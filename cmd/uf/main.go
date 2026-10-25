// Command uf plans a Rockchip "uf" firmware upgrade for an RKFW/RKAF update
// image.
//
// This build (M2) implements the offline plan only: `uf --dry-run <image>`
// parses the container, verifies its checksums and prints every phase and
// partition write it would perform. It never opens a USB device, never writes
// or erases flash and never resets the board. The actual writer lands in M3.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
)

const (
	toolName      = "uf"
	versionString = "M2 (offline plan / --dry-run only)"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(toolName, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		dryRun       = fs.Bool("dry-run", false, "print the flash plan and exit without touching a device (required in this build)")
		jsonOut      = fs.Bool("json", false, "emit the plan as JSON")
		noVerify     = fs.Bool("no-verify", false, "skip the streaming MD5 / RKAF CRC checks")
		noReset      = fs.Bool("noreset", false, "plan without the final device reset (rd)")
		flashSectors = fs.Uint64("flash-sectors", 0, "flash size in 512-byte sectors (default: derive from the parameter table)")
		showVersion  = fs.Bool("version", false, "print the tool version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s --dry-run [options] <update.img>\n\n", toolName)
		fmt.Fprintln(stderr, "M2 implements the offline plan only: no USB device is opened and no flash write,")
		fmt.Fprintln(stderr, "erase or reset is performed.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Options:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", toolName, versionString)
		return 0
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if !*dryRun {
		fmt.Fprintf(stderr, "%s: writing is not implemented in this build; re-run with --dry-run\n", toolName)
		return 2
	}
	imagePath := fs.Arg(0)

	img, err := rkfw.Open(imagePath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}
	if img.Parameter == nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", toolName, imagePath, rkfw.ErrNoParameter)
		return 2
	}
	paramBytes, err := img.ReadItem(*img.Parameter, maxParameterBytes)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}
	doc, err := parameter.Parse(paramBytes)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}

	ver := VerificationInfo{}
	if doc.CRCAvailable {
		ver.Parameter = &rkfw.CRCResult{Stored: doc.CRCStored, Computed: doc.CRCComputed, OK: doc.CRCOK}
	}
	if *noVerify {
		ver.Skipped = true
	} else {
		if !*jsonOut {
			fmt.Fprintf(stderr, "%s: verifying MD5 and RKAF CRC (streaming, this reads the image once)\n", toolName)
		}
		md5res, err := img.VerifyMD5()
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
			return 2
		}
		ver.MD5 = &md5res
		crc, err := img.VerifyRKAFCRC()
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
			return 2
		}
		ver.RKAFCRC = &crc
	}

	rep, err := BuildReport(img, doc, ver, Options{FlashSectors: *flashSectors, NoReset: *noReset})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "%s: encode JSON: %v\n", toolName, err)
			return 2
		}
	} else {
		writeText(stdout, rep)
	}

	if ver.Failed() {
		return 1
	}
	return 0
}

func writeText(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "%s --dry-run: %s\n", toolName, rep.Image)
	writeKeyValue(w, rep)
	writeStages(w, rep)
	writePartitions(w, rep)
	writeUncovered(w, rep)
	writeList(w, "warnings", rep.Warnings)
	writeList(w, "notes", rep.Notes)
	fmt.Fprintf(w, "\ndry run: nothing was written and no device was opened.\n")
}

func writeKeyValue(w io.Writer, rep *Report) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "size\t%s (%s bytes)\n", humanBytes(rep.FileSize), groupDigits(uint64(rep.FileSize)))

	if rep.Container.HasRKFW {
		var bits []string
		bits = append(bits, "RKFW")
		if rep.Container.Chip != "" {
			bits = append(bits, rep.Container.Chip)
		}
		if rep.Container.Version != "" {
			bits = append(bits, "v"+rep.Container.Version)
		}
		if rep.Container.BuildTime != "" {
			bits = append(bits, "built "+rep.Container.BuildTime)
		}
		bits = append(bits, fmt.Sprintf("code 0x%08x", rep.Container.Code))
		fmt.Fprintf(tw, "container\t%s\n", strings.Join(bits, ", "))
		fmt.Fprintf(tw, "loader\toffset 0x%x, length %s\n", rep.Container.LoaderOffset, humanBytes(rep.Container.LoaderLength))
	} else {
		fmt.Fprintf(tw, "container\tbare RKAF image (no RKFW wrapper, no loader, no MD5 tail)\n")
	}

	arch := fmt.Sprintf("RKAF at 0x%x (%s), model=%q id=%q manufacturer=%q, version 0x%08x, %d items",
		rep.Container.RKAFOffset, humanBytes(rep.Container.RKAFLength),
		rep.Container.Model, rep.Container.ID, rep.Container.Manufacturer,
		rep.Container.ArchiveVersion, rep.Container.ItemCount)
	if rep.Container.LengthWrapped {
		arch += fmt.Sprintf(" (32-bit length field 0x%x, recovered from layout)", rep.Container.RKAFLengthField)
	}
	fmt.Fprintf(tw, "archive\t%s\n", arch)

	parm := fmt.Sprintf("%d partitions", rep.Parameter.PartitionCount)
	if rep.Parameter.Wrapped {
		parm = fmt.Sprintf("PARM wrapper, %d B content, %s", rep.Parameter.ContentBytes, parm)
	} else {
		parm = fmt.Sprintf("raw text, %d B, %s", rep.Parameter.ContentBytes, parm)
	}
	fmt.Fprintf(tw, "parameter\t%s\n", parm)
	fmt.Fprintf(tw, "verify\t%s\n", verificationLine(rep.Verification))
	tw.Flush()
}

func verificationLine(v VerificationInfo) string {
	if v.Skipped {
		return "skipped (--no-verify)"
	}
	var parts []string
	switch {
	case v.MD5 == nil:
		parts = append(parts, "MD5 n/a")
	case !v.MD5.Present:
		parts = append(parts, "MD5 n/a (bare RKAF image)")
	case v.MD5.OK:
		suffix := ""
		if v.MD5.Signed {
			suffix = ", signed tail"
		}
		parts = append(parts, fmt.Sprintf("MD5 ok %s at 0x%x%s", strings.ToLower(v.MD5.Expected), v.MD5.Offset, suffix))
	default:
		parts = append(parts, fmt.Sprintf("MD5 MISMATCH (stored %q, computed %q)", v.MD5.Expected, v.MD5.Computed))
	}
	if v.RKAFCRC == nil {
		parts = append(parts, "RKAF CRC n/a")
	} else if v.RKAFCRC.OK {
		parts = append(parts, fmt.Sprintf("RKAF CRC ok (0x%08x)", v.RKAFCRC.Stored))
	} else {
		parts = append(parts, fmt.Sprintf("RKAF CRC MISMATCH (stored 0x%08x, computed 0x%08x)", v.RKAFCRC.Stored, v.RKAFCRC.Computed))
	}
	if v.Parameter == nil {
		parts = append(parts, "PARM CRC n/a (unwrapped parameter)")
	} else if v.Parameter.OK {
		parts = append(parts, fmt.Sprintf("PARM CRC ok (0x%08x)", v.Parameter.Stored))
	} else {
		parts = append(parts, fmt.Sprintf("PARM CRC MISMATCH (stored 0x%08x, computed 0x%08x)", v.Parameter.Stored, v.Parameter.Computed))
	}
	return strings.Join(parts, "; ")
}

func writeStages(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "\nprojected stages (%s to transfer; dry run writes nothing):\n", humanBytes(rep.TotalBytes))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  phase\tstage\tdetail\tbytes\tprogress")
	for _, st := range rep.Stages {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.1f%%\n",
			st.Phase, st.Name, st.Detail, groupDigits(uint64(st.Bytes)), st.Progress)
	}
	tw.Flush()
}

func writePartitions(w io.Writer, rep *Report) {
	disk := "unknown"
	if rep.GPT.DiskKnown {
		disk = humanBytes(int64(rep.GPT.DiskSectors) * parameter.SectorSize)
	}
	fmt.Fprintf(w, "\npartition plan (%d partitions, flash %s):\n", len(rep.Partitions), disk)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  #\tname\tstart LBA\tend LBA\tsize\tpayload\tbytes\tcoverage")
	for _, p := range rep.Partitions {
		end := "-"
		if p.EndKnown {
			end = fmt.Sprintf("0x%08x", p.LastLBA)
		}
		size := "grow"
		switch {
		case p.SizeBytes > 0:
			size = humanBytes(p.SizeBytes)
		case !p.Grow:
			size = "unknown"
		}
		payload := "-"
		if len(p.PayloadItems) > 0 {
			payload = strings.Join(p.PayloadItems, ",")
		}
		coverage := "-"
		switch {
		case p.GPTOnly:
			coverage = "gpt-only"
		case p.CoveragePercent >= 0:
			coverage = fmt.Sprintf("%.1f%%", p.CoveragePercent)
		}
		fmt.Fprintf(tw, "  %d\t%s\t0x%08x\t%s\t%s\t%s\t%s\t%s\n",
			p.Index, p.Name, p.FirstLBA, end, size, payload, humanBytes(p.CoveredBytes), coverage)
	}
	tw.Flush()

	for _, p := range rep.Partitions {
		if p.Note != "" && !p.GPTOnly {
			fmt.Fprintf(w, "  %s: %s\n", p.Name, p.Note)
		}
	}
}

func writeUncovered(w io.Writer, rep *Report) {
	if len(rep.Uncovered) == 0 {
		return
	}
	fmt.Fprintln(w, "\nuncovered ranges (NOT written by this image; existing flash contents remain and whether")
	fmt.Fprintln(w, "they are erased depends on the device/loader, not on this image):")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, u := range rep.Uncovered {
		fmt.Fprintf(tw, "  %s\t%s (%s bytes) at LBA 0x%08x\tpartition %s, image covers %s\n",
			u.Partition, humanBytes(u.Bytes), groupDigits(uint64(u.Bytes)), u.FirstLBA,
			humanBytes(u.PartitionBytes), humanBytes(u.CoveredBytes))
	}
	tw.Flush()
}

func writeList(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, item := range items {
		if strings.HasPrefix(item, "- ") {
			fmt.Fprintf(w, "%s\n", item)
		} else {
			fmt.Fprintf(w, "  - %s\n", item)
		}
	}
}

// humanBytes formats a byte count with binary units.
func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// groupDigits inserts thousands separators.
func groupDigits(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		if i > pre {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

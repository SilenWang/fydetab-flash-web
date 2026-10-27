// Command uf plans and (against a mock device) executes a Rockchip "uf"
// firmware upgrade for an RKFW/RKAF update image.
//
// Two modes exist in this build:
//
//   - `uf --dry-run <image>` prints the M2 plan: every phase, partition write
//     and uncovered range. It opens no USB device and writes nothing.
//   - `uf --mock` / `uf --replay <trace>` run the M3a write state machine
//     against an in-memory device or a recorded trace. They exercise the same
//     rockusb.Transport the browser bridge will provide, so the state machine,
//     retries, failure abort and JSON log are verified without touching a board.
//
// Writing to a real device arrives with the WebUSB integration (M3b); this
// command refuses to pretend otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"

	"github.com/SilenWang/fydetab-flash-web/internal/imageplan"
	"github.com/SilenWang/fydetab-flash-web/internal/parameter"
	"github.com/SilenWang/fydetab-flash-web/internal/rkfw"
	"github.com/SilenWang/fydetab-flash-web/internal/uf"
)

const (
	toolName      = "uf"
	versionString = "M3e (GPT + parameter write stage; mock/replay unchanged)"
)

const (
	modeDryRun = "dry-run"
	modeMock   = "mock"
	modeReplay = "replay"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(toolName, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		dryRun       = fs.Bool("dry-run", false, "print the flash plan and exit without touching a device")
		mockRun      = fs.Bool("mock", false, "run the write state machine against an in-memory RK3588 device (no USB)")
		replayPath   = fs.String("replay", "", "run the write state machine against a recorded trace (JSON)")
		logPath      = fs.String("log", "", "write the NDJSON run log to this path ('-' = stdout)")
		jsonOut      = fs.Bool("json", false, "emit the plan (--dry-run) or the run report (--mock/--replay) as JSON")
		noVerify     = fs.Bool("no-verify", false, "skip the streaming MD5 / RKAF CRC checks")
		noReset      = fs.Bool("noreset", false, "skip the final device reset (rd)")
		flashSectors = fs.Uint64("flash-sectors", 0, "flash size in 512-byte sectors (default: derive from the parameter table)")
		eraseTail    = fs.String("erase-tail", string(uf.EraseNone), "what to do with partition tails the image does not fill: none|fill|erase-lba")
		loaderPath   = fs.String("loader", "", "use this loader file instead of the image's own (marked non-image in the report)")
		resumeFrom   = fs.String("resume-from", "", "resume at this item (skip every item before it)")
		chunkSize    = fs.Int64("chunk-size", 0, "write chunk in bytes, 1..4 MiB (progress/retry granularity)")
		retries      = fs.Int("retries", 1, "extra attempts for a timed out USB transfer")
		timeout      = fs.Duration("timeout", 0, "per USB transfer deadline (default 5s)")
		idbModel     = fs.String("idb-model", uf.ModelRK28RC4, "IDB byte model: "+strings.Join(uf.IDBModelNames(), "|"))
		rewriteIDB   = fs.Bool("rewrite-idb", false, "rebuild the IDB at LBA 64 even when the device already holds a valid one (default: keep it)")
		idbBackup    = fs.String("idb-backup", "", "write the original IDB bytes to this file before overwriting them")
		mockFault    = fs.String("mock-fault", "", "inject a mock failure: <timeout|failed|stall|disconnect>:<item|idb|lba:0x...>")
		showVersion  = fs.Bool("version", false, "print the tool version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s --dry-run [options] <update.img>\n", toolName)
		fmt.Fprintf(stderr, "       %s --mock [options] <update.img>\n", toolName)
		fmt.Fprintf(stderr, "       %s --replay <trace.json> [options] <update.img>\n\n", toolName)
		fmt.Fprintln(stderr, "--dry-run opens no USB device. --mock/--replay exercise the write path against an")
		fmt.Fprintln(stderr, "in-memory device; writing to real hardware lands with the WebUSB integration (M3b).")
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

	mode := ""
	switch {
	case *dryRun && (*mockRun || *replayPath != ""):
		fmt.Fprintf(stderr, "%s: --dry-run cannot be combined with --mock/--replay\n", toolName)
		return 2
	case *dryRun:
		mode = modeDryRun
	case *mockRun && *replayPath != "":
		fmt.Fprintf(stderr, "%s: --mock and --replay are mutually exclusive\n", toolName)
		return 2
	case *mockRun:
		mode = modeMock
	case *replayPath != "":
		mode = modeReplay
	default:
		fmt.Fprintf(stderr, "%s: writing to a real device is not part of this build; use --dry-run to plan, or --mock / --replay <trace> to exercise the write state machine\n", toolName)
		return 2
	}

	eraseMode, err := uf.ParseEraseMode(*eraseTail)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}
	model, err := uf.IDBModelByName(*idbModel)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
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

	loader, err := selectLoader(img, *loaderPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}

	rep, err := BuildReport(img, doc, ver, Options{FlashSectors: *flashSectors, NoReset: *noReset, RewriteIDB: *rewriteIDB})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}
	rep.Loader = loader
	applyEraseDecision(rep, eraseMode)
	if loader != nil && loader.ChipCheck == "mismatch" {
		warning := fmt.Sprintf("loader 与镜像 chip 不匹配（loader %s / 镜像 %s）：写入会在打开设备前被拒绝", loader.Chip, loader.ImageChip)
		rep.Warnings = append(rep.Warnings, warning)
	}

	if mode == modeDryRun {
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
		if rep.Data.Failed() {
			fmt.Fprintf(stderr, "%s: refusing to write: payload signature check failed (see the payload table / warnings)\n", toolName)
			return 1
		}
		return 0
	}

	if ver.Failed() {
		fmt.Fprintf(stderr, "%s: refusing to write: image verification failed (use --no-verify to override)\n", toolName)
		return 1
	}
	if rep.Data.Failed() {
		fmt.Fprintf(stderr, "%s: refusing to write: payload signature check failed; the item read offsets are not trustworthy\n", toolName)
		return 1
	}

	plan, err := imageplan.WriterPlan(rep, loader)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}

	conn, err := newConnector(mode, *replayPath, plan, *mockFault)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}

	src, err := os.Open(imagePath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", toolName, err)
		return 2
	}
	defer src.Close()

	opts := uf.Options{
		Timeout:   *timeout,
		Retries:   *retries,
		ChunkSize: *chunkSize,
		EraseTail: eraseMode,
		NoReset:   *noReset,
		StartItem: *resumeFrom,
		IDB:       model,
		// This CLI only runs mock/replay; a real write must exercise the
		// signature and read-back gates.
		SkipSignatureCheck: true,
		SkipReadback:       true,
		RewriteIDB:         *rewriteIDB,
		IDBBackupPath:      *idbBackup,
		Event:              func(ev uf.Event) { writeEvent(stderr, ev) },
	}
	var (
		logWriter io.WriteCloser
		rec       *uf.Recorder
	)
	if *logPath != "" {
		w, err := openLog(*logPath, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "%s: open log: %v\n", toolName, err)
			return 2
		}
		logWriter = w
		defer logWriter.Close()
		rec, err = uf.NewRecorder(w, plan, opts)
		if err != nil {
			fmt.Fprintf(stderr, "%s: log: %v\n", toolName, err)
			return 2
		}
		opts.Trace = rec.Transfer
		opts.Progress = rec.Progress
		opts.Event = func(ev uf.Event) {
			rec.Event(ev)
			writeEvent(stderr, ev)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	res, runErr := uf.Run(ctx, plan, src, conn, opts)
	if rec != nil {
		if err := rec.Result(res); err != nil {
			fmt.Fprintf(stderr, "%s: log: %v\n", toolName, err)
		}
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintf(stderr, "%s: encode JSON: %v\n", toolName, err)
			return 2
		}
	} else {
		for _, line := range runResultSummary(res) {
			fmt.Fprintln(stdout, line)
		}
		if *logPath != "" {
			fmt.Fprintf(stdout, "log: %s\n", *logPath)
		}
	}
	if runErr != nil {
		return 1
	}
	return 0
}

// selectLoader picks the Download Boot blob.
//
// The default is the loader the image itself carries in its RKFW header: that
// is what the vendor's upgrade_tool used on the same board and image, and it is
// the only combination that did not stop at index 0. --loader overrides it and
// the report marks the override as non-image.
func selectLoader(img *rkfw.Image, path string) (*imageplan.EmbeddedLoader, error) {
	chip := ""
	if img.HasRKFW {
		chip = img.RKFW.ChipName()
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read --loader %s: %w", path, err)
		}
		return imageplan.FromFile(path, data, chip), nil
	}
	loader, err := imageplan.FromImage(img)
	if errors.Is(err, imageplan.ErrNoImageLoader) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return loader, nil
}

// newConnector is buildConnector behind a variable so a test can assert that
// --dry-run never asks for a device at all.
var newConnector = buildConnector

// buildConnector selects the mock device a run talks to.
func buildConnector(mode, replayPath string, plan *uf.Plan, mockFault string) (uf.Connector, error) {
	switch mode {
	case modeMock:
		faults, err := parseMockFault(mockFault, plan)
		if err != nil {
			return nil, err
		}
		return mockConnector(plan, faults), nil
	case modeReplay:
		f, err := os.Open(replayPath)
		if err != nil {
			return nil, fmt.Errorf("open trace: %w", err)
		}
		defer f.Close()
		trace, err := uf.LoadTrace(f)
		if err != nil {
			return nil, err
		}
		return uf.NewReplayConnector(uf.ReplaySession{Mode: trace.Mode(), Frames: trace.Frames}), nil
	}
	return nil, fmt.Errorf("no device for mode %q", mode)
}

func writeEvent(w io.Writer, ev uf.Event) {
	if ev.Level == "info" && ev.Message == "" {
		return
	}
	prefix := "  "
	switch ev.Level {
	case "warn":
		prefix = "warning: "
	case "error":
		prefix = "error: "
	}
	fmt.Fprintf(w, "%s%s\n", prefix, ev.Message)
}

func writeText(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "%s --dry-run: %s\n", toolName, rep.Image)
	writeKeyValue(w, rep)
	writeLoader(w, rep)
	writePayloads(w, rep)
	writeStages(w, rep)
	writeGPTStage(w, rep)
	writePartitions(w, rep)
	writeUncovered(w, rep)
	writeErase(w, rep)
	writeList(w, "warnings", rep.Warnings)
	writeList(w, "notes", rep.Notes)
	fmt.Fprintf(w, "\ndry run: nothing was written and no device was opened.\n")
}

// writePayloads prints where every item's bytes really are and what the first
// bytes at that offset look like. This is the check that would have caught the
// SIL-515 write: 7/7 partitions "succeeded" while the parser was reading zeros
// from the wrong offset of an archive with a 4 GiB hole.
func writePayloads(w io.Writer, rep *Report) {
	if rep.Data == nil {
		return
	}
	fmt.Fprintf(w, "\npayload offsets and signatures (hole %s at logical 0x%x):\n",
		humanBytes(rep.Data.Hole), rep.Data.HoleStart)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  item\tpos\tdataOffset\tsize\tmagic\texpected\tstatus")
	for _, ip := range rep.Items {
		if ip.Size == 0 {
			continue
		}
		expected := ip.ExpectedMagic
		if expected == "" {
			expected = "-"
		}
		status := "ok"
		switch {
		case ip.ExpectedMagic == "":
			status = "reported"
		case !ip.MagicOK:
			status = "MISMATCH"
		}
		shift := ""
		if ip.Shifted {
			shift = " (+hole)"
		}
		fmt.Fprintf(tw, "  %s\t0x%x\t0x%x%s\t%s\t%s\t%s\t%s\n",
			ip.Name, ip.Pos, ip.DataOffset, shift, humanBytes(ip.Size), ip.Magic, expected, status)
	}
	tw.Flush()
	if !rep.Data.Decided {
		fmt.Fprintf(w, "  ✗ 洞的位置无法用 payload 签名确认：item 偏移未经验证，拒绝写入。\n")
	}
	if rep.Data.FailedCount > 0 {
		fmt.Fprintf(w, "  ✗ %d 项读不到预期签名：拒绝写入（见 warnings）。\n", rep.Data.FailedCount)
	}
}

// writeLoader prints the Download Boot blob's identity. The default is the one
// the image carries; a --loader override is called out explicitly because a
// loader from another package is what real hardware rejected at index 0.
func writeLoader(w io.Writer, rep *Report) {
	if rep.Loader == nil {
		fmt.Fprintf(w, "loader\t(none: bare RKAF image; maskrom cannot be bootstrapped from it)\n")
		return
	}
	fmt.Fprintf(w, "\nloader for Download Boot:\n")
	for _, line := range rep.Loader.Summary() {
		fmt.Fprintf(w, "  %s\n", line)
	}
	if rep.Loader.ChipCheck == "mismatch" {
		fmt.Fprintf(w, "  ✗ 拒绝发送 db：loader 的 supportChip 与镜像 chip 不一致\n")
	}
}

// writeErase prints the explicit answer to "is the uncovered tail erased?", so
// the choice is visible next to the ranges it applies to.
func writeErase(w io.Writer, rep *Report) {
	if rep.Erase == nil {
		return
	}
	fmt.Fprintf(w, "\nerase semantics (--erase-tail=%s): %s\n", rep.Erase.Mode, rep.Erase.Detail)
	if rep.Erase.Tails > 0 {
		fmt.Fprintf(w, "  %d tail(s), %s total (not counted in the transfer total unless erased)\n",
			rep.Erase.Tails, humanBytes(rep.Erase.Bytes))
	}
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

// writeGPTStage prints where the partition table and the parameter block go.
// The dry run must show them because they are written before any payload, and
// the backup table's position depends on the flash size the device reports.
func writeGPTStage(w io.Writer, rep *Report) {
	if rep.ParameterWrite == nil {
		return
	}
	p := rep.ParameterWrite
	fmt.Fprintf(w, "\ngpt stage (partition table + parameter, %s + %s):\n",
		humanBytes(GPTPrimaryBytes+GPTBackupBytes), humanBytes(p.TotalBytes()))
	fmt.Fprintf(w, "  primary GPT   LBA 0, %d sectors (protective MBR + header + entry array)\n", GPTPrimarySectors)
	if rep.GPT != nil && rep.GPT.DiskKnown {
		fmt.Fprintf(w, "  backup GPT    LBA 0x%x, %d sectors (disk %s)\n",
			rep.GPT.BackupEntriesLBA, GPTBackupSectors, humanBytes(int64(rep.GPT.DiskSectors)*512))
	} else {
		fmt.Fprintf(w, "  backup GPT    at the end of the flash (size unknown to this dry run)\n")
	}
	fmt.Fprintf(w, "  parameter     %s from image offset 0x%x, %d copies of %s (first-4-access %t):\n",
		humanBytes(p.Bytes), p.FileOffset, p.Copies, humanBytes(p.CopyBytes), p.First4Access)
	for _, r := range p.Replicas {
		fmt.Fprintf(w, "    copy %d  LBA 0x%x  %d sectors\n", r.Index, r.LBA, r.Sectors)
	}
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
func humanBytes(n int64) string { return imageplan.HumanBytes(n) }

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

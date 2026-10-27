// Package imageplan is the one planner for a Rockchip update image.
//
// It maps an image's RKAF items onto the parameter partition table, projects
// the `uf` phases and records what is *not* covered. cmd/uf renders it as the
// M2 dry-run report and cmd/wasm feeds it the same bytes through
// rkfw.OpenReaderAt, so the CLI and the browser cannot disagree about which
// LBAs a write would touch.
package imageplan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SilenWang/fydetab-flash-web/internal/gpt"
	"github.com/SilenWang/fydetab-flash-web/internal/parameter"
	"github.com/SilenWang/fydetab-flash-web/internal/rkfw"
	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
	"github.com/SilenWang/fydetab-flash-web/internal/uf"
)

// ModelNameFallback is only used in the warning above.
const ModelNameFallback = "RC4(loader)"

// buildIDB returns the exact New IDB length for the image's loader. It uses the
// same model the runner will, so the dry-run stage and the written bytes agree.
func buildIDB(img *rkfw.Image) (int64, error) {
	if img == nil || !img.HasRKFW || img.LoaderLength <= 0 {
		return 0, nil
	}
	data, err := img.ReadRange(img.LoaderOffset, img.LoaderLength)
	if err != nil {
		return 0, err
	}
	parsed, err := rockusb.ParseLoaderImage(data)
	if err != nil {
		return 0, err
	}
	idb, err := uf.NewRKBOOTModel().Build(uf.LoaderInput{Container: data, Parsed: parsed})
	if err != nil {
		return 0, err
	}
	return int64(len(idb)), nil
}

func imgLoaderRC4(img *rkfw.Image) bool {
	if img == nil || !img.HasRKFW || img.LoaderLength <= 0 {
		return false
	}
	data, err := img.ReadRange(img.LoaderOffset, img.LoaderLength)
	if err != nil {
		return false
	}
	parsed, err := rockusb.ParseLoaderImage(data)
	return err == nil && parsed.RC4Flag != 0
}

// MaxParameterBytes bounds the parameter item a caller is willing to load.
// Real parameter partitions are 64 KiB or less.
const MaxParameterBytes = 4 << 20

const (
	// IDBSector is where Rockchip's loader expects the IDB copy on flash.
	// Same convention as SD_Firmware_Tool / rk_image_tool.
	IDBSector = 64
)

// Item actions in the plan.
const (
	ActionWrite = "write"
	// ActionParameter marks the parameter entry: its bytes are written by the
	// gpt stage (replicated across its reserved area), never as a partition
	// payload at an item target.
	ActionParameter = "parameter"
	ActionReserved  = "reserved"
	ActionNoTarget  = "no-target"
	ActionEmpty     = "zero-size"
)

// GPTPrimaryBytes and GPTBackupBytes are the two transfers the gpt stage makes
// for the partition table itself. They do not depend on the partition table:
// the layout is 128 fixed-size entries either way.
const (
	GPTPrimaryBytes = int64(gpt.PrimarySectors * parameter.SectorSize)
	GPTBackupBytes  = int64(gpt.BackupSectors * parameter.SectorSize)
)

// Options are the knobs of the dry-run plan.
type Options struct {
	// FlashSectors overrides the flash size in 512-byte sectors. Zero derives
	// the smallest disk that fits the parameter table.
	FlashSectors uint64
	// NoReset mirrors uf's --noreset: the plan then skips the rd stage.
	NoReset bool
	// RewriteIDB mirrors uf's --rewrite-idb: the IDB stage then rebuilds the
	// block instead of keeping the one already on the device.
	RewriteIDB bool
	// NoFirst4Access places the parameter copies at the parameter entry's own
	// flash offset instead of the first-4-access offset. The reference device
	// advertises the capability, so the default is to use it.
	NoFirst4Access bool
}

// VerificationInfo carries the checksum results for the report.
type VerificationInfo struct {
	MD5       *rkfw.MD5Result `json:"md5,omitempty"`
	RKAFCRC   *rkfw.CRCResult `json:"rkafCrc,omitempty"`
	Parameter *rkfw.CRCResult `json:"parameterCrc,omitempty"`
	Skipped   bool            `json:"skipped,omitempty"`
}

// Failed reports whether any checked checksum mismatched.
func (v VerificationInfo) Failed() bool {
	if v.MD5 != nil && v.MD5.Present && !v.MD5.OK {
		return true
	}
	if v.RKAFCRC != nil && !v.RKAFCRC.OK {
		return true
	}
	if v.Parameter != nil && !v.Parameter.OK {
		return true
	}
	return false
}

// ContainerInfo summarises the parsed container for humans and JSON.
type ContainerInfo struct {
	HasRKFW         bool   `json:"hasRkfw"`
	Version         string `json:"version,omitempty"`
	Code            uint32 `json:"code,omitempty"`
	Chip            string `json:"chip,omitempty"`
	BuildTime       string `json:"buildTime,omitempty"`
	LoaderOffset    int64  `json:"loaderOffset,omitempty"`
	LoaderLength    int64  `json:"loaderLength,omitempty"`
	RKAFOffset      int64  `json:"rkafOffset"`
	RKAFLength      int64  `json:"rkafLength"`
	RKAFLengthField uint32 `json:"rkafLengthField"`
	LengthWrapped   bool   `json:"lengthWrapped"`
	// DataHole is the number of bytes packed into the RKAF archive that its
	// 32-bit item table cannot describe (the STATE slot's reserved space).
	// DataHoleStart is the logical archive offset where it begins.
	DataHole       int64  `json:"dataHole,omitempty"`
	DataHoleStart  int64  `json:"dataHoleStart,omitempty"`
	Model          string `json:"model,omitempty"`
	ID             string `json:"id,omitempty"`
	Manufacturer   string `json:"manufacturer,omitempty"`
	ArchiveVersion uint32 `json:"archiveVersion"`
	ItemCount      int    `json:"itemCount"`
}

// ParameterInfo summarises the parameter partition.
type ParameterInfo struct {
	Wrapped        bool `json:"wrapped"`
	ContentBytes   int  `json:"contentBytes"`
	PartitionCount int  `json:"partitionCount"`
}

// Stage is one phase of the projected uf run.
type Stage struct {
	Phase    string  `json:"phase"` // ul | gpt | wl | rd
	Name     string  `json:"stage"`
	Detail   string  `json:"detail"`
	Bytes    int64   `json:"bytes"`
	Progress float64 `json:"progressPercent"`
}

// ItemPlan is one RKAF item mapped onto the flash.
type ItemPlan struct {
	Name string `json:"name"`
	// Pos is the 32-bit payload offset stored in the item table, relative to
	// the RKAF archive start.
	Pos int64 `json:"pos"`
	// DataOffset is where the payload really is in the file. It equals
	// RKAFOffset+Pos unless the archive carries a hole, in which case every
	// item after the hole is stored DataHole bytes further along.
	DataOffset int64 `json:"dataOffset"`
	// Shifted marks an item stored after the archive hole.
	Shifted bool `json:"shifted,omitempty"`
	// Magic is the leading signature actually read at DataOffset: CHROMEOS,
	// FDT, FAT, RSCE, zero, ... It is a measurement, so a wrong offset shows
	// up here instead of only on the device.
	Magic string `json:"magic"`
	// ExpectedMagic is the signature this item must carry, empty when the
	// table has no recorded expectation for that name.
	ExpectedMagic string `json:"expectedMagic,omitempty"`
	// MagicOK reports whether ExpectedMagic was found (true when there is no
	// expectation to check).
	MagicOK     bool    `json:"magicOk"`
	Size        int64   `json:"size"`
	PaddedSize  int64   `json:"paddedSize"`
	NandAddr    *uint32 `json:"nandAddr,omitempty"`
	TargetLBA   uint64  `json:"targetLba,omitempty"`
	TargetKnown bool    `json:"targetKnown"`
	Partition   string  `json:"partition,omitempty"`
	Action      string  `json:"action"`
	Detail      string  `json:"detail,omitempty"`
}

// PartitionPlan is one GPT partition with its payload coverage.
type PartitionPlan struct {
	Index           int      `json:"index"`
	Name            string   `json:"name"`
	FirstLBA        uint64   `json:"firstLba"`
	LastLBA         uint64   `json:"lastLba"`
	EndKnown        bool     `json:"endKnown"`
	SizeBytes       int64    `json:"sizeBytes"` // -1 when unknown (grow, no flash size)
	Grow            bool     `json:"grow"`
	Flags           []string `json:"flags,omitempty"`
	PayloadItems    []string `json:"payloadItems,omitempty"`
	PayloadBytes    int64    `json:"payloadBytes"`
	CoveredBytes    int64    `json:"coveredBytes"` // payload plus 512-byte padding
	UncoveredBytes  int64    `json:"uncoveredBytes"`
	CoveragePercent float64  `json:"coveragePercent"` // -1 when unknown
	GPTOnly         bool     `json:"gptOnly"`
	Note            string   `json:"note,omitempty"`
}

// UncoveredRange is the tail of a partition that the image does not fill.
type UncoveredRange struct {
	Partition      string `json:"partition"`
	FirstLBA       uint64 `json:"firstLba"`
	Bytes          int64  `json:"bytes"`
	PartitionBytes int64  `json:"partitionBytes"`
	CoveredBytes   int64  `json:"coveredBytes"`
}

// ParameterPlan is the parameter-partition write the gpt stage performs: the
// PARM block the image carries, replicated across the entry's reserved area so
// the device always finds an intact partition table.
type ParameterPlan struct {
	// Item is the archive entry the bytes come from ("parameter").
	Item string `json:"item"`
	// FileOffset/Bytes locate the PARM block in the image; CopyBytes is the
	// sector-aligned length written per copy (the block plus its slot padding).
	FileOffset int64 `json:"fileOffset"`
	Bytes      int64 `json:"bytes"`
	CopyBytes  int64 `json:"copyBytes"`
	// Copies and the layout of the reserved area.
	Copies       int                 `json:"copies"`
	First4Access bool                `json:"first4Access"`
	Replicas     []parameter.Replica `json:"replicas"`
	// CRCOK records whether the block's own PARM checksum verified.
	CRCOK bool `json:"crcOk"`
}

// TotalBytes is the transfer size of all copies.
func (p *ParameterPlan) TotalBytes() int64 {
	if p == nil {
		return 0
	}
	return parameter.ReplicaBytes(p.Replicas)
}

// DataCheck is the per-item payload audit: where each item's bytes really are
// (dataOffset), and what the first bytes at that offset look like. It exists so
// a wrong offset is visible in the plan instead of only on flash: SIL-515 wrote
// 7/7 partitions "successfully" from zeros because the parser read the 32-bit
// pos of an archive with a 4 GiB hole.
type DataCheck struct {
	// Hole is the byte count derived from the file layout (0 when none).
	Hole int64 `json:"hole"`
	// HoleStart is the logical archive offset the hole begins at.
	HoleStart int64 `json:"holeStart,omitempty"`
	// Decided is false when no payload signature could settle where the hole
	// starts; the item offsets are then unverified and a write is refused.
	Decided bool `json:"decided"`
	// Checked/Failed count items with an expected signature.
	Checked     int `json:"checked"`
	FailedCount int `json:"failed"`
	// Missing lists the items whose expected magic was not found.
	Missing []string `json:"missing,omitempty"`
}

// Failed reports whether any item's expected signature was not found, or the
// hole position could not be verified.
func (c *DataCheck) Failed() bool {
	if c == nil {
		return false
	}
	return c.FailedCount > 0 || (c.Hole > 0 && !c.Decided)
}

// Report is the complete dry-run plan.
type Report struct {
	Image        string           `json:"image"`
	FileSize     int64            `json:"fileSize"`
	Container    ContainerInfo    `json:"container"`
	Parameter    ParameterInfo    `json:"parameter"`
	Verification VerificationInfo `json:"verification"`
	// Data is the payload offset/magic audit (see DataCheck).
	Data *DataCheck `json:"data,omitempty"`
	// Loader is the Download Boot blob's identity (image's own or a file the
	// user picked). Filled by the caller after BuildReport, because picking
	// the loader is a user choice, not a property of the image.
	Loader *EmbeddedLoader `json:"loader,omitempty"`
	GPT    *gpt.Layout     `json:"gpt"`
	// ParameterWrite is the parameter-partition write the gpt stage performs,
	// when the image carries a parameter entry.
	ParameterWrite *ParameterPlan   `json:"parameterWrite,omitempty"`
	Stages         []Stage          `json:"stages"`
	Items          []ItemPlan       `json:"items"`
	Partitions     []PartitionPlan  `json:"partitions"`
	Uncovered      []UncoveredRange `json:"uncovered,omitempty"`
	// Erase is the explicit --erase-tail decision, filled by applyEraseDecision.
	Erase   *EraseDecision `json:"erase,omitempty"`
	NoReset bool           `json:"noReset,omitempty"`
	// IDBRewrite is the explicit --rewrite-idb decision; false means the run
	// reads LBA 64 first and keeps an existing valid IDB.
	IDBRewrite bool `json:"idbRewrite,omitempty"`
	// IDBBytes is the IDB model's size for this image's loader (0 when the
	// image carries none). It is part of the gpt stage, not the whole stage.
	IDBBytes   int64    `json:"idbBytes,omitempty"`
	TotalBytes int64    `json:"totalBytes"`
	Warnings   []string `json:"warnings,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// EraseDecision records what a run would do with partition tails the image does
// not fill. The question is unresolved for the Fydetab Duo image (STATE is 5 GiB
// but only ~1 GiB is payload), so it is a visible choice rather than an implicit
// side effect of the writer.
type EraseDecision struct {
	Mode   string `json:"mode"`   // none | fill | erase-lba
	Detail string `json:"detail"` // human sentence, same text the run logs
	Tails  int    `json:"tails"`
	Bytes  int64  `json:"bytes"`
}

// BuildReport maps image items onto parameter partitions and projects the uf
// execution phases. It performs no I/O.
func BuildReport(img *rkfw.Image, doc *parameter.Document, ver VerificationInfo, opts Options) (*Report, error) {
	if len(doc.Partitions) == 0 {
		return nil, fmt.Errorf("parameter has no mtdparts partitions; cannot build a flash plan")
	}

	rep := &Report{
		Image:        img.Path,
		FileSize:     img.Size,
		Verification: ver,
		NoReset:      opts.NoReset,
		IDBRewrite:   opts.RewriteIDB,
		Container: ContainerInfo{
			HasRKFW:         img.HasRKFW,
			LoaderOffset:    img.LoaderOffset,
			LoaderLength:    img.LoaderLength,
			RKAFOffset:      img.RKAFOffset,
			RKAFLength:      img.RKAFLength,
			RKAFLengthField: img.RKAFLengthField,
			LengthWrapped:   img.LengthWrapped,
			DataHole:        img.DataHole,
			DataHoleStart:   img.DataHoleStart,
			Model:           img.Archive.Model,
			ID:              img.Archive.ID,
			Manufacturer:    img.Archive.Manufacturer,
			ArchiveVersion:  img.Archive.Version,
			ItemCount:       len(img.Items),
		},
		Parameter: ParameterInfo{
			Wrapped:        doc.Wrapped,
			ContentBytes:   len(doc.Raw),
			PartitionCount: len(doc.Partitions),
		},
		Warnings: append([]string(nil), img.Warnings...),
	}
	rep.Warnings = append(rep.Warnings, doc.Warnings...)

	if img.HasRKFW {
		rep.Container.Version = img.RKFW.VersionString()
		rep.Container.Code = img.RKFW.Code
		rep.Container.Chip = img.RKFW.ChipName()
		if t, ok := img.RKFW.ReleaseTime(); ok {
			rep.Container.BuildTime = t.Format("2006-01-02 15:04:05 UTC")
		}
	}

	lay, err := gpt.Build(doc.Partitions, opts.FlashSectors)
	if err != nil {
		return nil, err
	}
	rep.GPT = lay
	rep.Warnings = append(rep.Warnings, lay.Warnings...)
	rep.Notes = append(rep.Notes,
		"- dry run: no USB device is opened and no flash write, erase or reset is performed.",
		fmt.Sprintf("- GPT geometry: protective MBR at LBA %d, primary header at LBA %d, entry array at LBA %d..%d, first usable LBA %d.",
			gpt.ProtectiveMBRLBA, gpt.PrimaryHeaderLBA, gpt.PrimaryEntriesLBA, gpt.PrimaryEntriesLBA+gpt.EntriesSectors-1, gpt.FirstUsableLBA),
		"- the device's loader creates GPT from the parameter partition; this table reproduces the parameter-derived LBAs and still has to be diffed against the on-device GPT.",
	)
	if lay.DiskDerived {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"- flash size was not supplied; %s (%d sectors) is the smallest disk that fits the parameter table. Use --flash-sectors for a plan against the real device.",
			HumanBytes(int64(lay.DiskSectors)*parameter.SectorSize), lay.DiskSectors))
	}
	if !lay.DiskKnown {
		rep.Notes = append(rep.Notes, "- the parameter table has a grow partition and no flash size was supplied, so grow ends and the backup GPT locations are unknown.")
	}

	// Map every RKAF item to a parameter partition.
	itemPlans := make([]ItemPlan, len(img.Items))
	targetPart := make([]int, len(img.Items))
	for i := range targetPart {
		targetPart[i] = -1
	}

	for i, it := range img.Items {
		ip := ItemPlan{
			Name:       it.Name,
			Pos:        int64(it.Pos),
			DataOffset: img.ItemOffset(it),
			Shifted:    img.DataHole > 0 && int64(it.Pos) >= img.DataHoleStart,
			Size:       int64(it.Size64),
			// The vendor writes entry->size rounded to a sector; the archive
			// slot may include alignment padding (and, for a full partition,
			// 1.5 KiB past its end). Never write the slot: it can spill into
			// the next partition.
			PaddedSize: alignUp(int64(it.Size64), int64(rkfw.SectorSize)),
		}
		if it.HasData() {
			sig := img.ItemSignature(it)
			ip.Magic = sig.String()
			ip.ExpectedMagic = rkfw.ExpectedSignature(it.Name)
			// "unreadable" means the source could not be sampled (an
			// in-memory plan fixture, or a truncated file). It is not a
			// mismatch, but it is also not proof; the write path still reads
			// and verifies the bytes it sends.
			ip.MagicOK = ip.ExpectedMagic == "" || sig.Name == "unreadable" ||
				(sig.Known && strings.EqualFold(sig.Name, ip.ExpectedMagic))
		}
		if it.NandAddr != rkfw.NoAddress {
			addr := it.NandAddr
			ip.NandAddr = &addr
		}

		nameIdx := findPartitionByName(doc.Partitions, it.Name)
		addrIdx := -1
		if ip.NandAddr != nil {
			addrIdx = findPartitionByLBA(lay.Entries, uint64(*ip.NandAddr))
		}

		switch {
		case img.Parameter != nil && &img.Items[i] == img.Parameter:
			// The parameter block is not a partition payload: the gpt stage
			// writes it (replicated) together with the partition table. It has
			// no target LBA of its own, so it never becomes a wl item.
			ip.Action = ActionParameter
			ip.Detail = "the parameter block is written by the gpt stage (replicated), not as a partition payload"
			ip.TargetKnown = false
			itemPlans[i] = ip
			continue
		case it.Reserved():
			ip.Action = ActionReserved
			ip.Detail = "no payload in the archive; the device generates this partition during flashing"
			if nameIdx >= 0 {
				targetPart[i] = nameIdx
			}
			itemPlans[i] = ip
			continue
		case it.Size64 == 0:
			ip.Action = ActionEmpty
			ip.Detail = "zero-sized payload; nothing to write"
			if nameIdx >= 0 {
				targetPart[i] = nameIdx
			}
			itemPlans[i] = ip
			continue
		}

		chosen := -1
		switch {
		case addrIdx >= 0 && nameIdx >= 0 && addrIdx != nameIdx:
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"item %q: nand_addr 0x%x points at partition %q, but the item name matches %q; using nand_addr",
				it.Name, *ip.NandAddr, doc.Partitions[addrIdx].Name, doc.Partitions[nameIdx].Name))
			chosen = addrIdx
		case addrIdx >= 0:
			chosen = addrIdx
		case nameIdx >= 0:
			chosen = nameIdx
			if ip.NandAddr != nil {
				ip.Detail = fmt.Sprintf("nand_addr 0x%x matches no partition; target taken from the item name", *ip.NandAddr)
			} else {
				ip.Detail = "target taken from the item name (nand_addr is unset)"
			}
		default:
			ip.Action = ActionNoTarget
			ip.Detail = "no nand_addr and no matching parameter partition; not placed in the plan"
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"item %q has no matching parameter partition (nand_addr=%s); it is not written by this plan",
				it.Name, formatAddr(it.NandAddr)))
			itemPlans[i] = ip
			continue
		}

		p := doc.Partitions[chosen]
		lba := p.First
		if ip.NandAddr != nil {
			lba = uint64(*ip.NandAddr)
		}
		ip.Action = ActionWrite
		ip.TargetKnown = true
		ip.TargetLBA = lba
		ip.Partition = p.Name
		if lba != p.First {
			if off, ok := p.End(); ok && lba < off {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf(
					"item %q is placed at LBA 0x%x, %d sectors into partition %q; writing starts there, not at the partition start",
					it.Name, lba, lba-p.First, p.Name))
			}
		}
		if it.NandSize != rkfw.NoAddress && !p.Grow && uint64(it.NandSize) != p.Sectors {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"item %q declares a %d-sector partition but parameter %q is %d sectors",
				it.Name, it.NandSize, p.Name, p.Sectors))
		}
		targetPart[i] = chosen
		itemPlans[i] = ip
	}
	rep.Items = itemPlans

	// Payload audit: record what the first bytes at each dataOffset look like,
	// so a wrong offset shows up in the plan (and refuses the write) instead
	// of only on flash.
	dc := &DataCheck{
		Hole:      img.DataHole,
		HoleStart: img.DataHoleStart,
		Decided:   img.DataHole == 0 || img.DataHoleDecided,
	}
	for _, ip := range itemPlans {
		if ip.ExpectedMagic == "" {
			continue
		}
		dc.Checked++
		if !ip.MagicOK {
			dc.FailedCount++
			dc.Missing = append(dc.Missing, fmt.Sprintf("%s: expected %s at dataOffset 0x%x, read %s",
				ip.Name, ip.ExpectedMagic, ip.DataOffset, ip.Magic))
		}
	}
	if dc.FailedCount > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d item(s) do not carry their expected payload magic: %s — the read offset is wrong or the image is damaged; refusing to write",
			dc.FailedCount, strings.Join(dc.Missing, "; ")))
	}
	if !dc.Decided {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"the RKAF archive has a %s hole but no payload signature settles where it starts: item data offsets are unverified, refusing to write",
			HumanBytes(img.DataHole)))
	}
	rep.Data = dc

	// Partition coverage.
	for idx, e := range lay.Entries {
		pp := PartitionPlan{
			Index:    e.Index,
			Name:     e.Name,
			FirstLBA: e.FirstLBA,
			LastLBA:  e.LastLBA,
			EndKnown: e.Known,
			Grow:     e.Grow,
			Flags:    e.Flags,
		}
		if e.Known {
			pp.SizeBytes = int64(e.Sectors) * parameter.SectorSize
		} else {
			pp.SizeBytes = -1
			pp.CoveragePercent = -1
		}
		for i, it := range img.Items {
			if targetPart[i] != idx {
				continue
			}
			switch itemPlans[i].Action {
			case ActionWrite:
				pp.PayloadItems = append(pp.PayloadItems, it.Name)
				pp.PayloadBytes += int64(it.Size64)
				pp.CoveredBytes += itemPlans[i].PaddedSize
			case ActionReserved:
				if pp.Note == "" {
					pp.Note = "device-generated: the image carries no payload (RESERVED item)"
				}
			case ActionEmpty:
				if pp.Note == "" {
					pp.Note = "payload item is empty"
				}
			}
		}
		if len(pp.PayloadItems) == 0 {
			pp.GPTOnly = true
			if pp.Note == "" {
				pp.Note = "GPT entry only: no payload item in the image"
			}
		}
		if pp.SizeBytes > 0 {
			if pp.CoveredBytes > pp.SizeBytes {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf(
					"partition %q is %s but its payload plus padding is %s; the write would overrun the partition by %s",
					pp.Name, HumanBytes(pp.SizeBytes), HumanBytes(pp.CoveredBytes), HumanBytes(pp.CoveredBytes-pp.SizeBytes)))
				pp.CoveragePercent = 100
			} else {
				pp.UncoveredBytes = pp.SizeBytes - pp.CoveredBytes
				pp.CoveragePercent = float64(pp.CoveredBytes) / float64(pp.SizeBytes) * 100
			}
			if pp.UncoveredBytes > 0 && !pp.GPTOnly {
				uncoveredLBA := pp.FirstLBA + uint64(pp.CoveredBytes)/parameter.SectorSize
				rep.Uncovered = append(rep.Uncovered, UncoveredRange{
					Partition:      pp.Name,
					FirstLBA:       uncoveredLBA,
					Bytes:          pp.UncoveredBytes,
					PartitionBytes: pp.SizeBytes,
					CoveredBytes:   pp.CoveredBytes,
				})
			}
		}
		rep.Partitions = append(rep.Partitions, pp)
	}

	var gptOnly []string
	for _, pp := range rep.Partitions {
		if pp.GPTOnly {
			gptOnly = append(gptOnly, pp.Name)
		}
	}
	if len(gptOnly) > 0 {
		rep.Notes = append(rep.Notes, "- partitions with a GPT entry but no payload to write: "+strings.Join(gptOnly, ", "))
	}

	// Projected uf phases: ul -> gpt -> wl per item -> rd.
	loader := img.LoaderLength
	if !img.HasRKFW {
		loader = 0
	}
	if loader > 0 {
		rep.appendStage("ul", "Download Boot", fmt.Sprintf("loader %s from image offset 0x%x into device RAM",
			HumanBytes(loader), img.LoaderOffset), loader)
	}
	idb := alignUp(loader, parameter.SectorSize)
	idbDetail := fmt.Sprintf("RC4(loader) per 512 B sector, %s written at LBA %d", HumanBytes(idb), IDBSector)
	if built, err := buildIDB(img); err == nil && built > 0 {
		idb = built
		idbDetail = fmt.Sprintf("New IDB (FlashHead+FlashData+FlashBoot, 2 KiB aligned%s), %s at LBA %d",
			map[bool]string{true: ", RC4 per 512 B", false: ""}[imgLoaderRC4(img)], HumanBytes(idb), IDBSector)
	} else if err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("could not build the New IDB from the image loader (%v); planning the legacy %s size", err, ModelNameFallback))
	}
	rep.IDBBytes = idb
	// The gpt stage is the partition-table stage: the IDB (when it has to be
	// rewritten), the GUID partition table itself and the parameter block the
	// device reads. None of it is optional on a device whose table can no
	// longer be trusted, so the byte count covers all three parts.
	parm, err := buildParameterPlan(img, doc, opts)
	if err != nil {
		return nil, err
	}
	rep.ParameterWrite = parm
	if parm == nil {
		rep.Warnings = append(rep.Warnings, "the image carries no parameter entry: the gpt stage can only write the IDB, not the partition table")
	}
	if parm != nil {
		if doc != nil && doc.CRCAvailable && !parm.CRCOK {
			rep.Warnings = append(rep.Warnings, "the parameter block's own checksum does not verify; the gpt stage will refuse to write it")
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"- gpt stage also writes the partition table: primary GPT %s at LBA 0, backup GPT %s at LBA N-33 (N = flash sectors; 233 GiB on the reference device), and %d parameter copies of %s at LBA 0x%x, step 0x%x.",
			HumanBytes(GPTPrimaryBytes), HumanBytes(GPTBackupBytes), parm.Copies, HumanBytes(parm.CopyBytes),
			parm.Replicas[0].LBA, parm.Replicas[1].LBA-parm.Replicas[0].LBA))
		if !rep.GPT.DiskKnown {
			rep.Notes = append(rep.Notes, "- the backup GPT location is derived here; a real write uses the flash size the device reports.")
		}
	}
	gptBytes := int64(0)
	if parm != nil {
		gptBytes = GPTPrimaryBytes + GPTBackupBytes + parm.TotalBytes()
	}
	if idb > 0 || parm != nil {
		var parts []string
		if idb > 0 {
			parts = append(parts, idbDetail)
		}
		if parm != nil {
			parts = append(parts, fmt.Sprintf("write GPT %s (primary 34 + backup 33 sectors) at LBA 0 and 0x%x, then the parameter block %d x %s",
				HumanBytes(GPTPrimaryBytes+GPTBackupBytes), rep.GPT.BackupEntriesLBA, parm.Copies, HumanBytes(parm.CopyBytes)))
		}
		detail := strings.Join(parts, "; ")
		if idb > 0 {
			prefix := fmt.Sprintf("read LBA %d first and identify the device's IDB (RKNS/RK28/empty); write only when it is absent or --rewrite-idb is given",
				IDBSector)
			if opts.RewriteIDB {
				prefix = fmt.Sprintf("--rewrite-idb: read LBA %d, back the original IDB up, rebuild in the identified format", IDBSector)
			}
			detail = prefix + "; " + detail
		}
		rep.appendStage("gpt", "Prepare/Download IDB, GPT and parameter", detail, idb+gptBytes)
	}

	writeable := make([]int, 0, len(img.Items))
	for i := range img.Items {
		if itemPlans[i].Action == ActionWrite && itemPlans[i].TargetKnown {
			writeable = append(writeable, i)
		}
	}
	sort.SliceStable(writeable, func(a, b int) bool {
		ia, ib := writeable[a], writeable[b]
		if itemPlans[ia].TargetLBA != itemPlans[ib].TargetLBA {
			return itemPlans[ia].TargetLBA < itemPlans[ib].TargetLBA
		}
		return itemPlans[ia].Name < itemPlans[ib].Name
	})
	for _, i := range writeable {
		ip := itemPlans[i]
		detail := fmt.Sprintf("image 0x%x, %s -> LBA 0x%x (%s)", ip.DataOffset, HumanBytes(ip.Size), ip.TargetLBA, ip.Partition)
		if pad := ip.PaddedSize - ip.Size; pad > 0 {
			detail += fmt.Sprintf(", %s zero padding", HumanBytes(pad))
		}
		rep.appendStage("wl", ip.Name, detail, ip.PaddedSize)
	}

	if opts.NoReset {
		rep.appendStage("rd", "Reset device", "--noreset: skipped", 0)
	} else {
		rep.appendStage("rd", "Reset device", "RD command after the last write", 0)
	}

	var total int64
	for _, st := range rep.Stages {
		total += st.Bytes
	}
	rep.TotalBytes = total
	var cumulative int64
	for i := range rep.Stages {
		cumulative += rep.Stages[i].Bytes
		if total > 0 {
			rep.Stages[i].Progress = float64(cumulative) / float64(total) * 100
		} else {
			rep.Stages[i].Progress = 100
		}
	}

	return rep, nil
}

func (rep *Report) appendStage(phase, name, detail string, bytes int64) {
	rep.Stages = append(rep.Stages, Stage{Phase: phase, Name: name, Detail: detail, Bytes: bytes})
}

func findPartitionByName(parts []parameter.Partition, name string) int {
	for i, p := range parts {
		if strings.EqualFold(p.Name, name) {
			return i
		}
	}
	return -1
}

func findPartitionByLBA(entries []gpt.Entry, lba uint64) int {
	for i, e := range entries {
		if e.FirstLBA == lba {
			return i
		}
	}
	for i, e := range entries {
		if e.Known && lba > e.FirstLBA && lba <= e.LastLBA {
			return i
		}
	}
	return -1
}

func alignUp(v, align int64) int64 {
	if align <= 1 {
		return v
	}
	return (v + align - 1) / align * align
}

func formatAddr(a uint32) string {
	if a == rkfw.NoAddress {
		return "none"
	}
	return fmt.Sprintf("0x%x", a)
}

// HumanBytes formats a byte count with binary units. It lives here because the
// planner's notes and the CLI's tables must use the same units.
func HumanBytes(n int64) string {
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

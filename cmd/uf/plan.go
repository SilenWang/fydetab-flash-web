package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SilenWang/rk3588-webflasher/internal/gpt"
	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
)

const (
	// maxParameterBytes bounds the parameter item we are willing to load.
	// Real parameter partitions are 64 KiB or less.
	maxParameterBytes = 4 << 20
	// idbSector is where Rockchip's loader expects the IDB copy on flash.
	// Same convention as SD_Firmware_Tool / rk_image_tool.
	idbSector = 64
)

// Item actions in the plan.
const (
	actionWrite    = "write"
	actionReserved = "reserved"
	actionNoTarget = "no-target"
	actionEmpty    = "zero-size"
)

// Options are the knobs of the dry-run plan.
type Options struct {
	// FlashSectors overrides the flash size in 512-byte sectors. Zero derives
	// the smallest disk that fits the parameter table.
	FlashSectors uint64
	// NoReset mirrors uf's --noreset: the plan then skips the rd stage.
	NoReset bool
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
	Model           string `json:"model,omitempty"`
	ID              string `json:"id,omitempty"`
	Manufacturer    string `json:"manufacturer,omitempty"`
	ArchiveVersion  uint32 `json:"archiveVersion"`
	ItemCount       int    `json:"itemCount"`
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
	Name        string  `json:"name"`
	FileOffset  int64   `json:"fileOffset"`
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

// Report is the complete dry-run plan.
type Report struct {
	Image        string           `json:"image"`
	FileSize     int64            `json:"fileSize"`
	Container    ContainerInfo    `json:"container"`
	Parameter    ParameterInfo    `json:"parameter"`
	Verification VerificationInfo `json:"verification"`
	GPT          *gpt.Layout      `json:"gpt"`
	Stages       []Stage          `json:"stages"`
	Items        []ItemPlan       `json:"items"`
	Partitions   []PartitionPlan  `json:"partitions"`
	Uncovered    []UncoveredRange `json:"uncovered,omitempty"`
	TotalBytes   int64            `json:"totalBytes"`
	Warnings     []string         `json:"warnings,omitempty"`
	Notes        []string         `json:"notes,omitempty"`
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
		Container: ContainerInfo{
			HasRKFW:         img.HasRKFW,
			LoaderOffset:    img.LoaderOffset,
			LoaderLength:    img.LoaderLength,
			RKAFOffset:      img.RKAFOffset,
			RKAFLength:      img.RKAFLength,
			RKAFLengthField: img.RKAFLengthField,
			LengthWrapped:   img.LengthWrapped,
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
			humanBytes(int64(lay.DiskSectors)*parameter.SectorSize), lay.DiskSectors))
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
			FileOffset: img.ItemOffset(it),
			Size:       int64(it.Size),
			PaddedSize: int64(it.PaddedSize),
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
		case it.Reserved():
			ip.Action = actionReserved
			ip.Detail = "no payload in the archive; the device generates this partition during flashing"
			if nameIdx >= 0 {
				targetPart[i] = nameIdx
			}
			itemPlans[i] = ip
			continue
		case it.Size == 0:
			ip.Action = actionEmpty
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
			ip.Action = actionNoTarget
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
		ip.Action = actionWrite
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
			case actionWrite:
				pp.PayloadItems = append(pp.PayloadItems, it.Name)
				pp.PayloadBytes += int64(it.Size)
				pp.CoveredBytes += int64(it.PaddedSize)
			case actionReserved:
				if pp.Note == "" {
					pp.Note = "device-generated: the image carries no payload (RESERVED item)"
				}
			case actionEmpty:
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
					pp.Name, humanBytes(pp.SizeBytes), humanBytes(pp.CoveredBytes), humanBytes(pp.CoveredBytes-pp.SizeBytes)))
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
			humanBytes(loader), img.LoaderOffset), loader)
	}
	idb := alignUp(loader, parameter.SectorSize)
	if idb > 0 {
		rep.appendStage("gpt", "Prepare/Download IDB", fmt.Sprintf("RC4(loader) per 512 B sector, %s written at LBA %d",
			humanBytes(idb), idbSector), idb)
	}

	writeable := make([]int, 0, len(img.Items))
	for i := range img.Items {
		if itemPlans[i].Action == actionWrite && itemPlans[i].TargetKnown {
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
		detail := fmt.Sprintf("image 0x%x, %s -> LBA 0x%x (%s)", ip.FileOffset, humanBytes(ip.Size), ip.TargetLBA, ip.Partition)
		if pad := ip.PaddedSize - ip.Size; pad > 0 {
			detail += fmt.Sprintf(", %s zero padding", humanBytes(pad))
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

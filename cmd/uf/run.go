package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
	"github.com/SilenWang/rk3588-webflasher/internal/uf"
)

// planFromReport projects the M2 dry-run report onto the writer's plan. This is
// the only bridge between planning and writing: item order, target LBAs, the
// IDB byte count and the uncovered tails all come from BuildReport, so
// `uf --dry-run` and `uf --mock` cannot disagree about what would be written.
func planFromReport(rep *Report) (*uf.Plan, error) {
	plan := &uf.Plan{
		Image:      rep.Image,
		Chip:       rep.Container.Chip,
		NoReset:    rep.NoReset,
		TotalBytes: rep.TotalBytes,
	}
	if rep.Container.HasRKFW && rep.Container.LoaderLength > 0 {
		plan.Loader = &uf.LoaderPlan{
			FileOffset: rep.Container.LoaderOffset,
			Length:     rep.Container.LoaderLength,
		}
	}
	for _, st := range rep.Stages {
		if st.Phase == "gpt" {
			plan.IDB = uf.IDBPlan{LBASector: uf.IDBDefaultLBA, Bytes: st.Bytes}
			break
		}
	}
	for _, it := range rep.Items {
		if it.Action != actionWrite || !it.TargetKnown {
			continue
		}
		plan.Items = append(plan.Items, uf.Item{
			Name:       it.Name,
			Partition:  it.Partition,
			FileOffset: it.FileOffset,
			Size:       it.Size,
			PaddedSize: it.PaddedSize,
			TargetLBA:  uint32(it.TargetLBA),
		})
	}
	for _, u := range rep.Uncovered {
		plan.Tails = append(plan.Tails, uf.Tail{
			Partition: u.Partition,
			FirstLBA:  u.FirstLBA,
			Bytes:     u.Bytes,
		})
	}
	if rep.GPT != nil {
		plan.DiskSectors = rep.GPT.DiskSectors
		plan.DiskKnown = rep.GPT.DiskKnown
		plan.DiskDerived = rep.GPT.DiskDerived
	}
	return plan, plan.Validate()
}

// applyEraseDecision makes the "erase or not" choice visible in the dry-run
// report itself, instead of leaving it to prose in the notes.
func applyEraseDecision(rep *Report, mode uf.EraseMode) {
	dec := &EraseDecision{Mode: string(mode), Detail: mode.Describe(), Tails: len(rep.Uncovered)}
	for _, u := range rep.Uncovered {
		dec.Bytes += u.Bytes
	}
	rep.Erase = dec
	rep.Notes = append(rep.Notes, fmt.Sprintf("- 擦除语义（--erase-tail=%s）：%s；未覆盖区间共 %s。", mode, mode.Describe(), humanBytes(dec.Bytes)))
}

// parseMockFault turns --mock-fault into a simulator fault tied to an item's
// first LBA, so the failure lands exactly where a real device would fail.
func parseMockFault(spec string, plan *uf.Plan) ([]uf.SimFault, error) {
	if spec == "" {
		return nil, nil
	}
	kindName, target, ok := strings.Cut(spec, ":")
	if !ok || target == "" {
		return nil, fmt.Errorf("--mock-fault wants <timeout|failed|stall|disconnect>:<item|lba:0x...|idb>, got %q", spec)
	}
	var kind uf.SimFaultKind
	switch kindName {
	case "timeout":
		kind = uf.SimTimeout
	case "failed":
		kind = uf.SimFailed
	case "stall":
		kind = uf.SimStall
	case "disconnect":
		kind = uf.SimDisconnect
	default:
		return nil, fmt.Errorf("unknown --mock-fault kind %q (want timeout, failed, stall or disconnect)", kindName)
	}

	var lba uint32
	switch {
	case target == "idb":
		if plan.IDB.Bytes == 0 {
			return nil, fmt.Errorf("--mock-fault %s:idb: the plan has no IDB stage", kindName)
		}
		lba = plan.IDB.LBASector
	case strings.HasPrefix(target, "lba:"):
		if _, err := fmt.Sscanf(target, "lba:0x%x", &lba); err != nil {
			if _, err := fmt.Sscanf(target, "lba:%d", &lba); err != nil {
				return nil, fmt.Errorf("--mock-fault: cannot parse LBA in %q", target)
			}
		}
	default:
		it, ok := plan.Item(target)
		if !ok {
			return nil, fmt.Errorf("--mock-fault: item %q is not written by this plan", target)
		}
		lba = it.TargetLBA
	}
	return []uf.SimFault{{Kind: kind, LBA: &lba}}, nil
}

// mockFlashSectors sizes the simulated flash so a plan fits, mirroring what a
// 5 GiB-class eMMC would report.
func mockFlashSectors(plan *uf.Plan) uint32 {
	if plan.DiskKnown && plan.DiskSectors > 0 && plan.DiskSectors <= 1<<32-1 {
		return uint32(plan.DiskSectors)
	}
	var end uint64
	if plan.IDB.Bytes > 0 {
		end = uint64(plan.IDB.LBASector) + uint64(plan.IDB.Bytes/uf.SectorSize)
	}
	for _, it := range plan.Items {
		if e := uint64(it.TargetLBA) + uint64(it.PaddedSize/uf.SectorSize); e > end {
			end = e
		}
	}
	for _, t := range plan.Tails {
		if e := t.FirstLBA + uint64(t.Bytes/uf.SectorSize); e > end {
			end = e
		}
	}
	end += 4096 // a little headroom, like a real disk
	if end > 1<<32-1 {
		end = 1<<32 - 1
	}
	return uint32(end)
}

// mockConnector builds the in-memory device a --mock run talks to. A plan with
// a loader starts in MASKROM so Download Boot and re-enumeration are exercised
// too; a bare RKAF plan starts directly in LOADER.
func mockConnector(plan *uf.Plan, faults []uf.SimFault) *uf.SimConnector {
	cfg := uf.SimConfig{
		Mode:         rockusb.ModeLoader,
		Chip:         plan.Chip,
		FlashSectors: mockFlashSectors(plan),
		Faults:       faults,
	}
	if plan.Loader != nil {
		cfg.Mode = rockusb.ModeMaskrom
	}
	return uf.NewSimConnector(cfg)
}

// runResultSummary renders the human-readable outcome of a write run.
func runResultSummary(rep *uf.Report) []string {
	lines := []string{fmt.Sprintf("uf: %s", rep.Image)}
	lines = append(lines, fmt.Sprintf("device: %s chip=%q flash=%d MiB", rep.Device.Mode, rep.Device.Chip, rep.Device.Flash.FlashSizeBytes>>20))
	for _, ph := range rep.Phases {
		note := ph.Note
		if note == "" {
			note = ph.Status
		}
		lines = append(lines, fmt.Sprintf("  %-5s %-24s %10s  %s", ph.Phase, ph.Name, humanBytes(ph.Bytes), note))
	}
	for _, it := range rep.Items {
		if it.Status == "ok" {
			lines = append(lines, fmt.Sprintf("  wl    %-24s %10s -> LBA 0x%x  ok", it.Name, humanBytes(it.Bytes), it.TargetLBA))
		}
	}
	if rep.IDB != nil {
		lines = append(lines, fmt.Sprintf("  gpt   IDB model=%s verified=%t lba=%d bytes=%d",
			rep.IDB.Model, rep.IDB.Verified, rep.IDB.LBA, rep.IDB.Bytes))
	}
	if rep.Erase != nil {
		lines = append(lines, fmt.Sprintf("  erase mode=%s tails=%d bytes=%d status=%s", rep.Erase.Mode, rep.Erase.Tails, rep.Erase.Bytes, rep.Erase.Status))
	}
	state := "ok"
	if !rep.OK {
		state = "FAILED"
		if rep.Cancelled {
			state = "cancelled"
		}
	}
	lines = append(lines, fmt.Sprintf("result: %s, wrote %s, erased %s, reset=%t, %s",
		state, humanBytes(rep.BytesWritten), humanBytes(rep.BytesErased), rep.Reset, durationText(rep.DurationMs)))
	if rep.NextItem != "" {
		lines = append(lines, fmt.Sprintf("next item: %s (resume with --resume-from %s)", rep.NextItem, rep.NextItem))
	}
	if rep.Hint != "" {
		lines = append(lines, "hint: "+rep.Hint)
	}
	for _, w := range rep.Warnings {
		lines = append(lines, "warning: "+w)
	}
	return lines
}

func durationText(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.1f ms", ms)
	}
	return fmt.Sprintf("%.2f s", ms/1000)
}

// openLog creates the NDJSON log file. "-" means stdout, which is useful when
// the caller already redirects the human summary.
func openLog(path string, fallback io.Writer) (io.WriteCloser, error) {
	if path == "" {
		return nil, nil
	}
	if path == "-" {
		return nopWriteCloser{fallback}, nil
	}
	return os.Create(path)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

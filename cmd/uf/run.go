package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SilenWang/fydetab-flash-web/internal/imageplan"
	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
	"github.com/SilenWang/fydetab-flash-web/internal/uf"
)

// planFromReport projects the M2 dry-run report onto the writer's plan using
// the loader the report says the image carries. It is the CLI-test entry point;
// run() calls imageplan.WriterPlan with the loader's full identity so the
// report can name its sha256 and build date.
func planFromReport(rep *Report) (*uf.Plan, error) {
	var loader *imageplan.EmbeddedLoader
	if rep.Container.HasRKFW && rep.Container.LoaderLength > 0 {
		loader = &imageplan.EmbeddedLoader{
			Source:     imageplan.LoaderSourceImage,
			FileOffset: rep.Container.LoaderOffset,
			Length:     rep.Container.LoaderLength,
		}
	}
	return imageplan.WriterPlan(rep, loader)
}

// applyEraseDecision makes the "erase or not" choice visible in the dry-run
// report itself, instead of leaving it to prose in the notes.
func applyEraseDecision(rep *Report, mode uf.EraseMode) { imageplan.ApplyEraseDecision(rep, mode) }

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
	if rep.GPT != nil {
		lines = append(lines, fmt.Sprintf("  gpt   GPT entries=%d primary=LBA 0x%x/%d B backup=LBA 0x%x/%d B verified=%t rebuilt=%t",
			rep.GPT.Entries, rep.GPT.PrimaryLBA, rep.GPT.PrimaryBytes, rep.GPT.BackupLBA, rep.GPT.BackupBytes,
			rep.GPT.Verified, rep.GPT.Rebuilt))
	}
	if rep.Parameter != nil {
		lines = append(lines, fmt.Sprintf("  gpt   parameter copies=%d written=%d skipped=%d bytes=%d base=0x%x stride=0x%x verified=%t",
			rep.Parameter.Copies, rep.Parameter.Written, rep.Parameter.Skipped, rep.Parameter.Bytes,
			rep.Parameter.BaseLBA, rep.Parameter.StrideLBA, rep.Parameter.Verified))
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
	if rep.Error != nil {
		// The code and the category are what a real failure has to be triaged
		// by ("first-block" is a loader/power-cycle problem, not a cable one),
		// so they belong in the human summary, not only in --json.
		detail := fmt.Sprintf("error: %s (%d)", rep.Error.CodeName, rep.Error.Code)
		if rep.Error.Category != "" {
			detail += fmt.Sprintf(" [%s]", rep.Error.Category)
		}
		lines = append(lines, detail+" "+rep.Error.Detail)
	}
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

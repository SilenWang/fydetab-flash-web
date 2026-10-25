package main

// End-to-end `uf --dry-run` against a sparse reconstruction of the real
// openFyde r132 image (sha256 01fe1c04…42670ecc), not the official FydeOS
// v23.0-SP1 image and not a synthetic fixture. Every expectation below is a
// measured property of that image.

import (
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
	"github.com/SilenWang/rk3588-webflasher/internal/testimage"
)

func r132Report(t *testing.T) *Report {
	t.Helper()
	path := testimage.R132().Write(t, t.TempDir())
	img, err := rkfw.Open(path)
	if err != nil {
		t.Fatalf("rkfw.Open: %v", err)
	}
	if img.Parameter == nil {
		t.Fatal("image has no parameter item")
	}
	raw, err := img.ReadItem(*img.Parameter, maxParameterBytes)
	if err != nil {
		t.Fatalf("ReadItem(parameter): %v", err)
	}
	doc, err := parameter.Parse(raw)
	if err != nil {
		t.Fatalf("parameter.Parse: %v", err)
	}
	// The checksum stages are covered by the rkfw tests; a partial
	// reconstruction cannot reproduce the real MD5 / RKAF CRC.
	rep, err := BuildReport(img, doc, VerificationInfo{Skipped: true}, Options{NoReset: true})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	return rep
}

func TestR132DryRunContainer(t *testing.T) {
	rep := r132Report(t)

	if !rep.Container.HasRKFW {
		t.Error("HasRKFW = false")
	}
	if got, want := rep.Container.Chip, "RK3588"; got != want {
		t.Errorf("chip = %q, want %q", got, want)
	}
	if got, want := rep.Container.ItemCount, 10; got != want {
		t.Errorf("item count = %d, want %d", got, want)
	}
	if got, want := rep.Container.RKAFOffset, int64(412198); got != want {
		t.Errorf("rkaf offset = %d, want %d", got, want)
	}
	if got, want := rep.Container.RKAFLengthField, uint32(4053098496); got != want {
		t.Errorf("rkaf length field = %d, want %d", got, want)
	}
	if rep.Container.LengthWrapped {
		t.Error("LengthWrapped = true; r132's length field does not wrap")
	}
	if got, want := rep.Parameter.PartitionCount, 12; got != want {
		t.Errorf("parameter partitions = %d, want %d", got, want)
	}
	if got, want := rep.Parameter.ContentBytes, 578; got != want {
		t.Errorf("parameter content = %d bytes, want %d", got, want)
	}
	// The 4 GiB of trailing padding must be reported, not swallowed.
	if !hasWarning(rep.Warnings, "after the RKAF archive") {
		t.Errorf("warnings = %v, want one about the trailing padding", rep.Warnings)
	}
}

func TestR132DryRunPartitions(t *testing.T) {
	rep := r132Report(t)

	if got, want := len(rep.Partitions), 12; got != want {
		t.Fatalf("partitions = %d, want %d", got, want)
	}
	wantNames := []string{"STATE", "KERN-A", "ROOT-A", "KERN-B", "ROOT-B", "KERN-C", "ROOT-C", "OEM", "reserved1", "resource", "uboot", "EFI-SYSTEM"}
	for i, want := range wantNames {
		if got := rep.Partitions[i].Name; got != want {
			t.Errorf("partition %d = %q, want %q", i, got, want)
		}
	}

	state := rep.Partitions[0]
	if got, want := state.FirstLBA, uint64(11542592); got != want {
		t.Errorf("STATE first LBA = %d, want %d", got, want)
	}
	if got, want := state.LastLBA, uint64(11542592+10485869-1); got != want {
		t.Errorf("STATE last LBA = %d, want %d", got, want)
	}
	if got, want := state.SizeBytes, int64(5368764928); got != want {
		t.Errorf("STATE size = %d bytes, want %d (5 GiB)", got, want)
	}
	if got, want := state.PayloadBytes, int64(1073797632); got != want {
		t.Errorf("STATE payload = %d bytes, want %d (1 GiB)", got, want)
	}

	// STATE: 5 GiB partition, 1 GiB payload -> the 4 GiB tail is uncovered and
	// must show up as such instead of being silently written or erased.
	if got, want := state.UncoveredBytes, int64(4294965760); got != want {
		t.Errorf("STATE uncovered = %d bytes, want %d", got, want)
	}
	var stateRange *UncoveredRange
	for i := range rep.Uncovered {
		if rep.Uncovered[i].Partition == "STATE" {
			stateRange = &rep.Uncovered[i]
		}
	}
	if stateRange == nil {
		t.Fatalf("no uncovered range for STATE; got %v", rep.Uncovered)
	}
	if got, want := stateRange.FirstLBA, uint64(13639856); got != want {
		t.Errorf("STATE uncovered starts at LBA %d, want %d", got, want)
	}

	// Partitions that only get a GPT entry, no payload item in the image.
	wantGPTOnly := map[string]bool{"KERN-B": true, "ROOT-B": true, "KERN-C": true, "ROOT-C": true, "reserved1": true}
	for _, p := range rep.Partitions {
		if p.GPTOnly != wantGPTOnly[p.Name] {
			t.Errorf("partition %q GPTOnly = %v, want %v", p.Name, p.GPTOnly, wantGPTOnly[p.Name])
		}
	}

	// resource is the second partially-filled partition (16 MiB partition,
	// 901 KiB payload).
	for _, p := range rep.Partitions {
		if p.Name != "resource" {
			continue
		}
		if got, want := p.SizeBytes, int64(16777216); got != want {
			t.Errorf("resource size = %d, want %d", got, want)
		}
		if got, want := p.UncoveredBytes, int64(15853568); got != want {
			t.Errorf("resource uncovered = %d, want %d", got, want)
		}
	}
}

func TestR132DryRunStages(t *testing.T) {
	rep := r132Report(t)

	var ul, gpt *Stage
	wl := 0
	for i := range rep.Stages {
		switch rep.Stages[i].Phase {
		case "ul":
			ul = &rep.Stages[i]
		case "gpt":
			gpt = &rep.Stages[i]
		case "wl":
			wl++
			if rep.Stages[i].Name == "STATE" && rep.Stages[i].Bytes != 1073799168 {
				t.Errorf("STATE write stage = %d bytes, want the 1073799168-byte archive slot", rep.Stages[i].Bytes)
			}
		}
	}
	if ul == nil || ul.Bytes != 412096 {
		t.Errorf("ul stage = %+v, want 412096 loader bytes", ul)
	}
	if gpt == nil || gpt.Bytes != 412160 {
		t.Errorf("gpt stage = %+v, want the 412160-byte loader padded to 512", gpt)
	}
	// package-file, bootloader and parameter have no mappable partition, so
	// seven payload items are written.
	if wl != 7 {
		t.Errorf("wl stages = %d, want 7", wl)
	}

	var want int64
	for _, s := range rep.Stages {
		want += s.Bytes
	}
	if rep.TotalBytes != want {
		t.Errorf("total = %d, want the sum of the stages %d", rep.TotalBytes, want)
	}
	last := rep.Stages[len(rep.Stages)-1]
	if last.Phase != "rd" || last.Detail == "" {
		t.Errorf("last stage = %+v, want a skipped rd stage (--noreset)", last)
	}
}

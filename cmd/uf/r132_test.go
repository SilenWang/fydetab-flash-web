package main

// End-to-end `uf --dry-run` against a sparse reconstruction of the real
// openFyde r132 image (sha256 01fe1c04…42670ecc), not the official FydeOS
// v23.0-SP1 image and not a synthetic fixture. Every expectation below is a
// measured property of that image.

import (
	"testing"

	gptpkg "github.com/SilenWang/rk3588-webflasher/internal/gpt"
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
	if !rep.Container.LengthWrapped {
		t.Error("LengthWrapped = false; the item table carries 64-bit high words beyond the 32-bit length field")
	}
	if got, want := rep.Parameter.PartitionCount, 12; got != want {
		t.Errorf("parameter partitions = %d, want %d", got, want)
	}
	if got, want := rep.Parameter.ContentBytes, 578; got != want {
		t.Errorf("parameter content = %d bytes, want %d", got, want)
	}
	// The 32-bit length field holds the low half; the real archive continues
	// past it. The parser must say so instead of inventing 4 GiB of padding.
	if !hasWarning(rep.Warnings, "recovered the true body length") {
		t.Errorf("warnings = %v, want one about the recovered body length", rep.Warnings)
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
	// The image carries the full 5 GiB STATE payload (its 64-bit size high
	// word is 1); there is no 4 GiB uncovered tail.
	if got, want := state.PayloadBytes, int64(5368764928); got != want {
		t.Errorf("STATE payload = %d bytes, want %d (full 5 GiB partition)", got, want)
	}
	if got, want := state.UncoveredBytes, int64(0); got != want {
		t.Errorf("STATE uncovered = %d bytes, want %d", got, want)
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
		if got, want := p.UncoveredBytes, int64(15854080); got != want {
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
			if rep.Stages[i].Name == "STATE" && rep.Stages[i].Bytes != 5368764928 {
				t.Errorf("STATE write stage = %d bytes, want the full 5368764928-byte payload", rep.Stages[i].Bytes)
			}
		}
	}
	if ul == nil || ul.Bytes != 412096 {
		t.Errorf("ul stage = %+v, want 412096 loader bytes", ul)
	}
	wantGPT := int64(412160) + int64(gptpkg.PrimarySectors+gptpkg.BackupSectors)*512 + parameter.ReplicaCount*2048
	if gpt == nil || gpt.Bytes != wantGPT {
		t.Errorf("gpt stage = %+v, want %d bytes (legacy IDB + GPT + parameter copies)", gpt, wantGPT)
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

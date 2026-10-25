package main

// End-to-end `uf --dry-run` against a sparse reconstruction of the official
// FyeOS v23.0-SP1 image (sha256 5a3f7991…fe7010) — the image SIL-515 flashed
// before the Fydetab Duo stopped booting. The capture has no payload bytes, so
// the plan must (a) report the 4 GiB hole, (b) flag the item offsets as
// unverified, and (c) refuse to write. The item table and parameter partition
// list are real measured values.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
	"github.com/SilenWang/rk3588-webflasher/internal/testimage"
)

func v23Report(t *testing.T) *Report {
	t.Helper()
	path := testimage.V23().Write(t, t.TempDir())
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
	rep, err := BuildReport(img, doc, VerificationInfo{Skipped: true}, Options{NoReset: true, FlashSectors: 488636416})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	return rep
}

func TestV23DryRunPlan(t *testing.T) {
	rep := v23Report(t)

	if got, want := rep.Container.Chip, "RK3588"; got != want {
		t.Errorf("chip = %q, want %q", got, want)
	}
	if got, want := rep.Container.ItemCount, 10; got != want {
		t.Errorf("items = %d, want %d", got, want)
	}
	if got, want := len(rep.Partitions), 12; got != want {
		t.Fatalf("partitions = %d, want %d", got, want)
	}
	// STATE is the 5 GiB partition with a 1 GiB payload: the uncovered 4 GiB
	// is what the packer reserved inside the archive.
	state := rep.Partitions[0]
	if state.Name != "STATE" {
		t.Fatalf("first partition = %q, want STATE", state.Name)
	}
	if got, want := state.PayloadBytes, int64(1073797632); got != want {
		t.Errorf("STATE payload = %d, want %d", got, want)
	}
	if got, want := state.UncoveredBytes, int64(4294965760); got != want {
		t.Errorf("STATE uncovered = %d, want %d", got, want)
	}

	// The 4 GiB hole must be in the plan, not silently folded into padding.
	if rep.Data == nil {
		t.Fatal("no payload audit in the report")
	}
	if got, want := rep.Data.Hole, int64(1)<<32; got != want {
		t.Errorf("data hole = %d, want %d", got, want)
	}
	if rep.Data.Decided {
		t.Error("Data.Decided = true; the capture cannot settle the hole position")
	}
	if !rep.Data.Failed() {
		t.Error("Data.Failed() = false; unverified offsets must refuse the write")
	}
	if !hasWarning(rep.Warnings, "no payload signature settles where it starts") {
		t.Errorf("warnings = %v, want the unverified-offset warning", rep.Warnings)
	}

	// The item table drives the plan: 7 payload items map onto partitions,
	// package-file/bootloader/parameter do not.
	var written []string
	for _, it := range rep.Items {
		if it.Action == actionWrite {
			written = append(written, it.Name)
		}
	}
	want := []string{"STATE", "KERN-A", "ROOT-A", "OEM", "resource", "uboot", "EFI-SYSTEM"}
	if strings.Join(written, ",") != strings.Join(want, ",") {
		t.Errorf("write items = %v, want %v", written, want)
	}
}

// The CLI must exit non-zero for this image until the payload offsets can be
// verified — that is the guard SIL-515 did not have.
func TestV23DryRunRefusesWithoutPayloadEvidence(t *testing.T) {
	path := testimage.V23().Write(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--dry-run", "--no-verify", "--flash-sectors", "488636416", path}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refused)\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "payload signature check failed") {
		t.Errorf("stderr = %q, want the refusal message", stderr.String())
	}
	// The payload table has to be in the output, with the hole named. No item
	// is marked "(+hole)" here because this capture cannot settle the split —
	// the table says so in the line below it instead.
	for _, want := range []string{"payload offsets and signatures", "dataOffset", "hole 4.00 GiB"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout does not mention %q:\n%s", want, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), "未经验证") {
		t.Errorf("stdout = %q, want the unverified-offset line", stdout.String())
	}
}

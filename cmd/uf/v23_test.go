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

	"github.com/SilenWang/fydetab-flash-web/internal/parameter"
	"github.com/SilenWang/fydetab-flash-web/internal/rkfw"
	"github.com/SilenWang/fydetab-flash-web/internal/testimage"
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
	// STATE carries its full 5 GiB payload (the size high word is 1).
	state := rep.Partitions[0]
	if state.Name != "STATE" {
		t.Fatalf("first partition = %q, want STATE", state.Name)
	}
	if got, want := state.PayloadBytes, int64(5368764928); got != want {
		t.Errorf("STATE payload = %d, want %d (full 5 GiB)", got, want)
	}
	if got, want := state.UncoveredBytes, int64(0); got != want {
		t.Errorf("STATE uncovered = %d, want %d", got, want)
	}

	// The high-word offsets are authoritative: no inferred hole. The sparse
	// capture still has no payload bytes, so the signature gate must refuse.
	if rep.Data == nil {
		t.Fatal("no payload audit in the report")
	}
	if got := rep.Data.Hole; got != 0 {
		t.Errorf("data hole = %d, want 0", got)
	}
	if !rep.Data.Decided {
		t.Error("Data.Decided = false; high-word offsets settle every item")
	}
	if !rep.Data.Failed() {
		t.Error("Data.Failed() = false; the sparse capture has no payload signatures")
	}
	if !hasWarning(rep.Warnings, "do not carry their expected payload magic") {
		t.Errorf("warnings = %v, want the payload-magic warning", rep.Warnings)
	}

	// The item table drives the plan: 7 payload items map onto partitions,
	// package-file/bootloader/parameter do not. The parameter entry is written
	// by the gpt stage instead.
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
	for _, it := range rep.Items {
		if it.Name != "parameter" {
			continue
		}
		if it.Action != actionParameter {
			t.Errorf("parameter action = %q, want %q", it.Action, actionParameter)
		}
	}

	// The gpt stage carries the partition table and the parameter block, on top
	// of the IDB: the stage's byte count must account for all three.
	var gptStage *Stage
	for i := range rep.Stages {
		if rep.Stages[i].Phase == "gpt" {
			gptStage = &rep.Stages[i]
		}
	}
	if gptStage == nil {
		t.Fatal("no gpt stage")
	}
	tableBytes := int64(67) * 512 // primary 34 + backup 33 sectors
	parm := rep.ParameterWrite
	if parm == nil {
		t.Fatal("the plan has no parameter write")
	}
	if parm.Copies != 8 {
		t.Errorf("parameter copies = %d, want 8", parm.Copies)
	}
	if got, want := parm.Replicas[0].LBA, uint64(0x2000); got != want {
		t.Errorf("first parameter copy at LBA 0x%x, want 0x%x", got, want)
	}
	if got, want := parm.Replicas[1].LBA-parm.Replicas[0].LBA, uint64(0x800); got != want {
		t.Errorf("parameter copy stride = 0x%x, want 0x%x", got, want)
	}
	if !parm.CRCOK {
		t.Error("the parameter block's checksum does not verify")
	}
	if want := rep.IDBBytes + tableBytes + parm.TotalBytes(); gptStage.Bytes != want {
		t.Errorf("gpt stage = %d bytes, want %d (IDB + table + parameter copies)", gptStage.Bytes, want)
	}
	// The payload is untouched by this stage: 7.79 GiB stays 7.79 GiB.
	if rep.TotalBytes < 8_300_000_000 || rep.TotalBytes > 8_400_000_000 {
		t.Errorf("total transfer = %d bytes, want the ~7.79 GiB payload plus a few MiB", rep.TotalBytes)
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
	// The payload table must show the high-word data offsets; the sparse
	// capture still has no payload bytes, so the signatures are MISMATCH.
	for _, want := range []string{"payload offsets and signatures", "dataOffset", "hole 0 B", "MISMATCH"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout does not mention %q:\n%s", want, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), "拒绝写入") {
		t.Errorf("stdout = %q, want the refusal line", stdout.String())
	}
}

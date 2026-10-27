package main

import (
	"math"
	"strings"
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/gpt"
	"github.com/SilenWang/fydetab-flash-web/internal/parameter"
	"github.com/SilenWang/fydetab-flash-web/internal/rkfw"
)

// syntheticCmdline is invented for the test; it is not the Fydetab Duo
// parameter table. The shape mirrors what uf has to handle: 6 partitions with
// payload and 5 that only get a GPT entry, including a 5 GiB STATE partition.
// The parameter block itself is not one of the mtdparts entries — the gpt stage
// writes it into its own reserved area.
const syntheticCmdline = `CMDLINE: mtdparts=rk29xxnand:` +
	`0x0000000c@0x00000044(uboot),` +
	`0x00000008@0x00000050(misc),` +
	`0x00000004@0x00000058(dtbo),` +
	`0x00000800@0x0000005c(boot),` +
	`0x00004000@0x0000085c(KERN-B),` +
	`0x00008000@0x0000485c(ROOT-B),` +
	`0x00004000@0x0000c85c(KERN-C),` +
	`0x00008000@0x0001085c(ROOT-C),` +
	`0x00000800@0x0001885c(rootfs),` +
	`0x00a00000@0x0001905c(state),` +
	`0x00001000@0x00b1905c(reserved1)`

const (
	loaderBytes = int64(100000)
	rkafStart   = int64(0x1000)
	// The parameter entry's reserved area, in sectors, as the reference image
	// carries it.
	paramPartSize = 0x4000
)

func testDocument(t *testing.T) *parameter.Document {
	t.Helper()
	doc, err := parameter.Parse([]byte(syntheticCmdline))
	if err != nil {
		t.Fatalf("parse synthetic parameter: %v", err)
	}
	if doc.PartitionCount != 11 {
		t.Fatalf("synthetic parameter has %d partitions, want 12", doc.PartitionCount)
	}
	return doc
}

func testImage() *rkfw.Image {
	pos := int64(0x800)
	add := func(items []rkfw.Item, name string, lba uint32, size int64) []rkfw.Item {
		items = append(items, rkfw.Item{
			Name:       name,
			Filename:   "Image/" + name,
			NandAddr:   lba,
			NandSize:   rkfw.NoAddress,
			Pos:        uint32(pos),
			Pos64:      uint64(pos),
			Size:       uint32(size),
			Size64:     uint64(size),
			PaddedSize: uint64(size),
		})
		pos += size
		return items
	}

	var items []rkfw.Item
	items = add(items, "parameter", 0, 2048)
	// The parameter entry's reserved area, in sectors, as the reference image
	// carries it; without it the copies cannot be placed.
	items[0].NandSize = paramPartSize
	items = add(items, "uboot", 0x44, 6144)
	items = add(items, "misc", 0x50, 4096)
	items = add(items, "dtbo", 0x58, 2048)
	items = add(items, "boot", 0x5c, 1<<20)
	items = add(items, "rootfs", 0x1885c, 1<<20)
	items = add(items, "state", 0x1905c, 1<<30) // 1 GiB payload into a 5 GiB partition

	rkafLen := pos + 4
	img := &rkfw.Image{
		Path:         "/tmp/synthetic-update.img",
		Size:         rkafStart + rkafLen,
		HasRKFW:      true,
		LoaderOffset: 0x66,
		LoaderLength: loaderBytes,
		RKAFOffset:   rkafStart,
		RKAFLength:   rkafLen,
		Items:        items,
	}
	img.RKFW.Version = 0x17000001
	img.RKFW.Code = 0x01030000
	img.RKFW.Chip = 0x33353838 // "8853" -> RK3588
	img.Archive.Model = "Synthetic Duo"
	img.Archive.ID = "rockchip"
	img.Archive.Manufacturer = "Rockchip"
	img.Parameter = &img.Items[0]
	return img
}

func TestBuildReportStagesAndCoverage(t *testing.T) {
	rep, err := BuildReport(testImage(), testDocument(t), VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if got, want := len(rep.Partitions), 11; got != want {
		t.Fatalf("partitions = %d, want %d", got, want)
	}
	if !rep.GPT.DiskDerived || !rep.GPT.DiskKnown {
		t.Fatalf("disk flags = derived:%v known:%v", rep.GPT.DiskDerived, rep.GPT.DiskKnown)
	}

	// The five GPT-only partitions named in the issue.
	var gptOnly []string
	for _, p := range rep.Partitions {
		if p.GPTOnly {
			gptOnly = append(gptOnly, p.Name)
		}
	}
	wantOnly := []string{"KERN-B", "ROOT-B", "KERN-C", "ROOT-C", "reserved1"}
	if strings.Join(gptOnly, ",") != strings.Join(wantOnly, ",") {
		t.Fatalf("gpt-only partitions = %v, want %v", gptOnly, wantOnly)
	}

	// Stage projection: ul, gpt, one wl per payload item sorted by LBA, rd.
	// The parameter entry has no wl stage: the gpt stage writes it.
	wantPhases := []string{"ul", "gpt", "wl", "wl", "wl", "wl", "wl", "wl", "rd"}
	var gotPhases []string
	for _, st := range rep.Stages {
		gotPhases = append(gotPhases, st.Phase)
	}
	if strings.Join(gotPhases, ",") != strings.Join(wantPhases, ",") {
		t.Fatalf("stage phases = %v, want %v", gotPhases, wantPhases)
	}
	var names []string
	for _, st := range rep.Stages[2 : len(rep.Stages)-1] {
		names = append(names, st.Name)
	}
	wantOrder := "uboot,misc,dtbo,boot,rootfs,state"
	if strings.Join(names, ",") != wantOrder {
		t.Fatalf("wl order = %v, want %s", names, wantOrder)
	}

	if got, want := rep.Stages[0].Bytes, loaderBytes; got != want {
		t.Fatalf("ul bytes = %d, want %d", got, want)
	}
	// The gpt stage covers the IDB, the primary and backup GPT, and every
	// parameter copy.
	idbWant := (loaderBytes + 511) / 512 * 512
	gptWant := idbWant + (gpt.PrimarySectors+gpt.BackupSectors)*512 + parameter.ReplicaCount*2048
	if got := rep.Stages[1].Bytes; got != gptWant {
		t.Fatalf("gpt bytes = %d, want %d (IDB + GPT + parameter copies)", got, gptWant)
	}
	if rep.IDBBytes != idbWant {
		t.Fatalf("IDB bytes = %d, want %d", rep.IDBBytes, idbWant)
	}
	if rep.ParameterWrite == nil {
		t.Fatal("the report has no parameter write plan")
	}
	if rep.ParameterWrite.Copies != parameter.ReplicaCount {
		t.Fatalf("parameter copies = %d, want %d", rep.ParameterWrite.Copies, parameter.ReplicaCount)
	}
	if got, want := rep.ParameterWrite.Replicas[0].LBA, uint64(0x2000); got != want {
		t.Fatalf("first parameter copy at LBA 0x%x, want 0x%x", got, want)
	}
	if got, want := rep.ParameterWrite.Replicas[1].LBA-rep.ParameterWrite.Replicas[0].LBA, uint64(0x800); got != want {
		t.Fatalf("parameter stride = 0x%x, want 0x%x", got, want)
	}
	if rep.Stages[len(rep.Stages)-1].Bytes != 0 {
		t.Fatal("rd must have zero transfer bytes")
	}
	if math.Abs(rep.Stages[len(rep.Stages)-1].Progress-100) > 1e-9 {
		t.Fatalf("final progress = %.3f, want 100", rep.Stages[len(rep.Stages)-1].Progress)
	}
	for i := 1; i < len(rep.Stages); i++ {
		if rep.Stages[i].Progress < rep.Stages[i-1].Progress {
			t.Fatalf("progress went backwards at stage %d: %v", i, rep.Stages)
		}
	}

	// Total transfer bytes = loader + the gpt stage + every padded payload.
	wantTotal := loaderBytes + gptWant
	for _, it := range testImage().Items {
		if it.Name == "parameter" {
			continue // written by the gpt stage, not as a payload
		}
		wantTotal += int64(it.PaddedSize)
	}
	if rep.TotalBytes != wantTotal {
		t.Fatalf("total bytes = %d, want %d", rep.TotalBytes, wantTotal)
	}

	// STATE: 5 GiB partition, 1 GiB payload, 4 GiB uncovered and explicitly
	// reported rather than silently dropped.
	state := rep.Partitions[9]
	if state.Name != "state" {
		t.Fatalf("partition 9 = %q", state.Name)
	}
	const gib = int64(1) << 30
	if state.SizeBytes != 5*gib {
		t.Fatalf("state size = %d, want %d", state.SizeBytes, 5*gib)
	}
	if state.CoveredBytes != gib {
		t.Fatalf("state covered = %d, want %d", state.CoveredBytes, gib)
	}
	if state.UncoveredBytes != 4*gib {
		t.Fatalf("state uncovered = %d, want %d", state.UncoveredBytes, 4*gib)
	}
	if state.CoveragePercent != 20 {
		t.Fatalf("state coverage = %v, want 20", state.CoveragePercent)
	}
	if len(rep.Uncovered) != 1 {
		t.Fatalf("uncovered ranges = %+v, want exactly the state tail", rep.Uncovered)
	}
	u := rep.Uncovered[0]
	if u.Partition != "state" || u.Bytes != 4*gib {
		t.Fatalf("uncovered = %+v", u)
	}
	wantLBA := state.FirstLBA + uint64(gib)/512
	if u.FirstLBA != wantLBA {
		t.Fatalf("state uncovered LBA = 0x%x, want 0x%x", u.FirstLBA, wantLBA)
	}

	// The synthetic parameter has no grow partition, so the plan has no
	// unknown-size entries.
	for _, p := range rep.Partitions {
		if !p.EndKnown || p.SizeBytes <= 0 || p.CoveragePercent < 0 {
			t.Fatalf("unexpected unknown partition: %+v", p)
		}
	}
}

func TestBuildReportNoTargetItem(t *testing.T) {
	img := testImage()
	size := int64(4096)
	img.Items = append(img.Items, rkfw.Item{
		Name:       "package-file",
		Filename:   "package-file",
		NandAddr:   rkfw.NoAddress,
		NandSize:   rkfw.NoAddress,
		Pos:        uint32(img.RKAFLength - 4 - size),
		Pos64:      uint64(img.RKAFLength - 4 - size),
		Size:       uint32(size),
		Size64:     uint64(size),
		PaddedSize: uint64(size),
	})
	rep, err := BuildReport(img, testDocument(t), VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	var found bool
	for _, it := range rep.Items {
		if it.Name == "package-file" {
			found = true
			if it.Action != actionNoTarget || it.TargetKnown {
				t.Fatalf("package-file plan = %+v", it)
			}
		}
	}
	if !found {
		t.Fatal("package-file missing from item plan")
	}
	if !hasWarning(rep.Warnings, "package-file") || !hasWarning(rep.Warnings, "not written") {
		t.Fatalf("warnings = %v", rep.Warnings)
	}
	for _, st := range rep.Stages {
		if st.Name == "package-file" {
			t.Fatal("an unmapped item must not get a wl stage")
		}
	}
}

func TestBuildReportNandAddrWinsOverName(t *testing.T) {
	img := testImage()
	// Name says uboot, address says boot (LBA 0x5c).
	for i := range img.Items {
		if img.Items[i].Name == "uboot" {
			img.Items[i].NandAddr = 0x5c
		}
	}
	rep, err := BuildReport(img, testDocument(t), VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if !hasWarning(rep.Warnings, "using nand_addr") {
		t.Fatalf("warnings = %v", rep.Warnings)
	}
	for _, it := range rep.Items {
		if it.Name == "uboot" {
			if it.Partition != "boot" || it.TargetLBA != 0x5c {
				t.Fatalf("uboot target = %+v", it)
			}
		}
	}
}

func TestBuildReportByNameFallback(t *testing.T) {
	img := testImage()
	for i := range img.Items {
		if img.Items[i].Name == "misc" {
			img.Items[i].NandAddr = rkfw.NoAddress
		}
	}
	rep, err := BuildReport(img, testDocument(t), VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	for _, it := range rep.Items {
		if it.Name == "misc" {
			if !it.TargetKnown || it.Partition != "misc" || it.TargetLBA != 0x50 {
				t.Fatalf("misc target = %+v", it)
			}
			if !strings.Contains(it.Detail, "item name") {
				t.Fatalf("misc detail = %q", it.Detail)
			}
		}
	}
}

func TestBuildReportPayloadOverrun(t *testing.T) {
	img := testImage()
	for i := range img.Items {
		if img.Items[i].Name == "misc" {
			img.Items[i].PaddedSize = 8 << 20 // partition is 2 MiB
			img.Items[i].Size = 8 << 20
			img.Items[i].Size64 = 8 << 20
		}
	}
	rep, err := BuildReport(img, testDocument(t), VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if !hasWarning(rep.Warnings, "overrun") {
		t.Fatalf("warnings = %v", rep.Warnings)
	}
}

func TestBuildReportNoReset(t *testing.T) {
	rep, err := BuildReport(testImage(), testDocument(t), VerificationInfo{}, Options{NoReset: true})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	last := rep.Stages[len(rep.Stages)-1]
	if last.Phase != "rd" || !strings.Contains(last.Detail, "--noreset") {
		t.Fatalf("last stage = %+v", last)
	}
}

func TestBuildReportExplicitFlashSectors(t *testing.T) {
	rep, err := BuildReport(testImage(), testDocument(t), VerificationInfo{}, Options{FlashSectors: 0xC00000})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if rep.GPT.DiskDerived {
		t.Fatal("explicit flash size must not be marked derived")
	}
	if rep.GPT.DiskSectors != 0xC00000 {
		t.Fatalf("disk sectors = %d", rep.GPT.DiskSectors)
	}
	if rep.GPT.LastUsableLBA != 0xC00000-34 {
		t.Fatalf("last usable = %d", rep.GPT.LastUsableLBA)
	}
}

func TestBuildReportRequiresPartitions(t *testing.T) {
	doc, err := parameter.Parse([]byte("FIRMWARE_VER: 1.0.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildReport(testImage(), doc, VerificationInfo{}, Options{}); err == nil {
		t.Fatal("expected an error for a parameter without partitions")
	}
}

func TestVerificationFailed(t *testing.T) {
	if (VerificationInfo{}).Failed() {
		t.Fatal("empty verification must not fail")
	}
	if !(VerificationInfo{MD5: &rkfw.MD5Result{Present: true, OK: false}}).Failed() {
		t.Fatal("MD5 mismatch must fail")
	}
	if !(VerificationInfo{RKAFCRC: &rkfw.CRCResult{OK: false}}).Failed() {
		t.Fatal("RKAF CRC mismatch must fail")
	}
	if !(VerificationInfo{Parameter: &rkfw.CRCResult{OK: false}}).Failed() {
		t.Fatal("PARM CRC mismatch must fail")
	}
	if (VerificationInfo{RKAFCRC: &rkfw.CRCResult{OK: true}}).Failed() {
		t.Fatal("matching CRC must not fail")
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

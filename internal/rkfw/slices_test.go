package rkfw

// This test replays any real image that was captured as slices, so a new image
// (the official FydeOS v23.0-SP1 one, for example) can be checked on a machine
// that does not have the multi-GiB file:
//
//	tools/extract-rk-slices.sh /path/to/update.img rk-slices   # on the machine with the image
//	RKFW_SLICES_DIR=$PWD/rk-slices pixi run test               # here
//
// It asserts only what the format guarantees and logs every measured number, so
// the output can be diffed against the r132 baseline in
// docs/rkfw-rkaf-format.md without inventing expectations. Turning the numbers
// into hard assertions is a separate, deliberate step per image.

import (
	"os"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/gpt"
	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/testimage"
)

func TestSlicesFromEnv(t *testing.T) {
	dir := os.Getenv("RKFW_SLICES_DIR")
	if dir == "" {
		t.Skip("set RKFW_SLICES_DIR to a directory produced by tools/extract-rk-slices.sh")
	}
	fixture, err := testimage.LoadSlices(dir)
	if err != nil {
		t.Fatalf("LoadSlices(%s): %v", dir, err)
	}
	t.Logf("image %s size=%d sha256=%s slices=%d", dir, fixture.Size, fixture.SHA256, len(fixture.Slices))

	path := fixture.Write(t, t.TempDir())
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Format guarantees, independent of the particular vendor image.
	if !img.HasRKFW {
		t.Error("HasRKFW = false")
	}
	if got, want := len(img.Items), int(img.Archive.NumParts); got != want {
		t.Errorf("items = %d, num_parts = %d", got, want)
	}
	if img.Parameter == nil {
		t.Fatal("no parameter item")
	}
	var slots uint64 = RKAFHeaderSize
	for _, it := range img.Items {
		if !it.Reserved() && it.Size64 > it.PaddedSize {
			t.Errorf("item %q payload %d exceeds its slot %d", it.Name, it.Size, it.PaddedSize)
		}
		slots += uint64(it.PaddedSize)
	}
	if want := uint64(img.RKAFLength - 4); slots != want {
		t.Errorf("item slots add up to %d, archive body is %d", slots, want)
	}

	raw, err := img.ReadItem(*img.Parameter, 64<<20)
	if err != nil {
		t.Fatalf("ReadItem(parameter): %v", err)
	}
	doc, err := parameter.Parse(raw)
	if err != nil {
		t.Fatalf("parameter.Parse: %v", err)
	}
	if doc.CRCAvailable && !doc.CRCOK {
		t.Errorf("PARM CRC mismatch: stored 0x%08x computed 0x%08x", doc.CRCStored, doc.CRCComputed)
	}
	if len(doc.Partitions) == 0 {
		t.Fatal("parameter has no mtdparts partitions")
	}
	lay, err := gpt.Build(doc.Partitions, 0)
	if err != nil {
		t.Fatalf("gpt.Build: %v", err)
	}

	// Measured values, for comparison with the r132 baseline.
	build := "unknown"
	if ts, ok := img.RKFW.ReleaseTime(); ok {
		build = ts.Format("2006-01-02 15:04:05 UTC")
	}
	t.Logf("rkfw head_len=%d chip=0x%08x (%s) version=%s build=%s",
		img.RKFW.HeadLen, img.RKFW.Chip, img.RKFW.ChipName(), img.RKFW.VersionString(), build)
	t.Logf("loader offset=%d length=%d", img.LoaderOffset, img.LoaderLength)
	t.Logf("rkaf offset=%d length=%d (field %d) wrapped=%v items=%d",
		img.RKAFOffset, img.RKAFLength, img.RKAFLengthField, img.LengthWrapped, len(img.Items))
	t.Logf("rkaf model=%q id=%q manufacturer=%q version=0x%08x", img.Archive.Model, img.Archive.ID, img.Archive.Manufacturer, img.Archive.Version)
	for _, it := range img.Items {
		t.Logf("item %-14s pos=%-12d nand_addr=%-12d +104=%-10d size=%-12d slot=%d",
			it.Name, it.Pos, it.NandAddr, it.PaddedBlocks, it.Size, it.PaddedSize)
	}
	t.Logf("parameter wrapped=%v declared=%d crc=0x%08x ok=%v partitions=%d",
		doc.Wrapped, doc.DeclaredLength, doc.CRCStored, doc.CRCOK, len(doc.Partitions))
	for _, p := range doc.Partitions {
		t.Logf("partition %-12s first=%-12d sectors=%-10d bytes=%d flags=%v", p.Name, p.First, p.Sectors, p.Sectors*parameter.SectorSize, p.Flags)
	}
	t.Logf("gpt disk=%d sectors derived=%v known=%v firstUsable=%d lastUsable=%d",
		lay.DiskSectors, lay.DiskDerived, lay.DiskKnown, lay.FirstUsableLBA, lay.LastUsableLBA)
	for _, w := range img.Warnings {
		t.Logf("warning: %s", w)
	}
	reportTail(t, img, path)
}

// reportTail logs the two checksum trailers, which are the values that decide
// whether the pre-flash verification policy has to change (r132 stores an
// all-zero RKAF CRC and a plain MD5 at EOF-32).
func reportTail(t *testing.T, img *Image, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var trailer [4]byte
	if _, err := f.ReadAt(trailer[:], img.RKAFOffset+img.RKAFLength-4); err == nil {
		t.Logf("rkaf crc trailer=% x", trailer)
	}
	md5res, err := img.VerifyMD5()
	if err != nil {
		t.Logf("md5: %v", err)
		return
	}
	t.Logf("md5 offset=%d signed=%v expected=%q", md5res.Offset, md5res.Signed, strings.TrimSpace(md5res.Expected))
}

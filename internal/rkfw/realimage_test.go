package rkfw

import (
	"bytes"
	"os"
	"testing"
)

// These tests require the real multi-GiB images; CI skips them. Run with:
//
//	RKFW_V23_IMAGE=/path/FydeOS_FydeTab_Duo_v23.0-SP1-com.img \
//	RKFW_R132_IMAGE=/path/openfyde.r132.img go test ./internal/rkfw
//
// The whole point is that a small synthetic fixture cannot exercise the
// 64-bit high-word encoding: the bug only appears when a payload crosses 4 GiB.

func realImage(t *testing.T, env string) *Image {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skipf("%s is not set; skipping the real-image regression", env)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s=%s: %v", env, path, err)
	}
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return img
}

type sigCheck struct {
	name   string
	pos    uint64
	size   uint64
	offset int64
	magic  []byte
}

func checkRealItems(t *testing.T, img *Image, checks []sigCheck) {
	t.Helper()
	byName := map[string]Item{}
	for _, it := range img.Items {
		byName[it.Name] = it
	}
	bodyEnd := uint64(img.RKAFLength - 4)
	for _, c := range checks {
		it, ok := byName[c.name]
		if !ok {
			t.Errorf("item %q not found", c.name)
			continue
		}
		if it.Pos64 != c.pos {
			t.Errorf("%s Pos64 = 0x%x, want 0x%x", c.name, it.Pos64, c.pos)
		}
		if it.Size64 != c.size {
			t.Errorf("%s Size64 = 0x%x, want 0x%x", c.name, it.Size64, c.size)
		}
		if end := it.Pos64 + it.Size64; end > bodyEnd {
			t.Errorf("%s ends at 0x%x, past the archive end 0x%x", c.name, end, bodyEnd)
		}
		off := img.ItemOffset(it) + c.offset
		buf := make([]byte, len(c.magic))
		if _, err := img.ReaderAt().ReadAt(buf, off); err != nil {
			t.Errorf("%s read signature at 0x%x: %v", c.name, off, err)
			continue
		}
		if !bytes.Equal(buf, c.magic) {
			t.Errorf("%s signature at 0x%x = %x, want %x", c.name, off, buf, c.magic)
		}
	}
}

func TestV23RealImageLayoutAndCRC(t *testing.T) {
	img := realImage(t, "RKFW_V23_IMAGE")
	checkRealItems(t, img, []sigCheck{
		{name: "STATE", pos: 0x69000, size: 0x14000da00, offset: 0x438, magic: []byte{0x53, 0xef}},
		{name: "KERN-A", pos: 0x140077000, size: 0x2000000, offset: 0, magic: []byte("CHROMEOS")},
		{name: "ROOT-A", pos: 0x142077000, size: 0xaa400000, offset: 0x438, magic: []byte{0x53, 0xef}},
		{name: "OEM", pos: 0x1ec477000, size: 0x1000000, offset: 0x438, magic: []byte{0x53, 0xef}},
		{name: "resource", pos: 0x1ed477000, size: 0xc5d200, offset: 0, magic: []byte("RSCE")},
		{name: "uboot", pos: 0x1ee0d4800, size: 0x400000, offset: 0, magic: []byte{0xd0, 0x0d, 0xfe, 0xed}},
		{name: "EFI-SYSTEM", pos: 0x1ee4d4800, size: 0x4000000, offset: 0, magic: []byte{0xeb}},
	})

	crc, err := img.VerifyRKAFCRC()
	if err != nil {
		t.Fatalf("VerifyRKAFCRC: %v", err)
	}
	// 6-A used to treat a zero trailer as "unset". The real trailer is not
	// zero: the earlier zero came from reading at the wrapped 32-bit offset.
	if crc.Stored != 0x296d5b32 {
		t.Fatalf("stored CRC = 0x%08x, want 0x296d5b32", crc.Stored)
	}
	if !crc.OK {
		t.Fatalf("RKAF CRC mismatch: stored 0x%08x computed 0x%08x", crc.Stored, crc.Computed)
	}
	md5, err := img.VerifyMD5()
	if err != nil {
		t.Fatalf("VerifyMD5: %v", err)
	}
	if !md5.OK {
		t.Fatalf("MD5 mismatch: stored %q computed %q", md5.Expected, md5.Computed)
	}
}

func TestR132RealImageLayout(t *testing.T) {
	img := realImage(t, "RKFW_R132_IMAGE")
	checkRealItems(t, img, []sigCheck{
		{name: "STATE", pos: 0x66800, size: 0x14000da00, offset: 0x438, magic: []byte{0x53, 0xef}},
		{name: "KERN-A", pos: 0x140074800, size: 0x2000000, offset: 0, magic: []byte("CHROMEOS")},
		{name: "ROOT-A", pos: 0x142074800, size: 0xaa400000, offset: 0x438, magic: []byte{0x53, 0xef}},
		{name: "resource", pos: 0x1ed474800, size: 0xe1600, offset: 0, magic: []byte("RSCE")},
		{name: "uboot", pos: 0x1ed556000, size: 0x400000, offset: 0, magic: []byte{0xd0, 0x0d, 0xfe, 0xed}},
	})
}

func TestHighWordItemPastArchiveFailsClosed(t *testing.T) {
	rkaf := buildRKAF(t, []fixturePart{item("boot", bytes.Repeat([]byte{1}, 512))})
	// Mark a 64-bit offset with high word 1: the item now points 4 GiB past
	// the archive, which must be rejected rather than read as a valid slot.
	entry := rkaf[140 : 140+RKAFItemSize]
	entry[32+50] = 'H'
	entry[32+51] = 1
	path := writeTemp(t, appendMD5(buildRKFWBody(nil, rkaf)))
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error for a high-word offset outside the archive")
	}
}

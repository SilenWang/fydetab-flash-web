package rkfw

// Synthetic tests for the archive hole. The real-image numbers live in
// r132_test.go / v23_test.go; these fixtures are constructed so each branch of
// the detection (signature settles the split, no signature at all, no hole)
// is reachable without a multi-GiB download.
//
// The layout mirrors what SIL-515 measured on FyeOS v23.0-SP1: an item table
// whose 32-bit pos values describe a contiguous archive, a hole of N bytes
// inserted after the STATE payload, and every later item stored N bytes further
// along than its pos says.

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/testimage"
)

// holeFixture describes one synthetic image with a hole.
type holeFixture struct {
	// hole is the number of bytes inserted after STATE.
	hole int64
	// payloads gives the leading bytes of each item's payload, by name.
	payloads map[string][]byte
	// sizes overrides payload sizes (default: len(payload of the name)).
	sizes map[string]int
}

type fixtureItem struct {
	name    string
	pos     int64
	size    int64
	nandLBA uint32
}

// buildHoleImage writes a sparse RKFW image with a hole and returns its path.
func buildHoleImage(t *testing.T, f holeFixture) string {
	t.Helper()
	const (
		stateSize = 1 << 20 // 1 MiB "STATE" payload; the hole is what matters
		kernSize  = 64 << 10
		resSize   = 8 << 10
		ubootSize = 16 << 10
		efiSize   = 32 << 10
		paramSize = 600
	)
	sizes := map[string]int64{
		"parameter": paramSize, "STATE": stateSize, "KERN-A": kernSize,
		"resource": resSize, "uboot": ubootSize, "EFI-SYSTEM": efiSize,
	}
	for name, n := range f.sizes {
		sizes[name] = int64(n)
	}

	// Logical layout: item table positions, contiguous by construction.
	var items []fixtureItem
	pos := int64(RKAFHeaderSize)
	for _, name := range []string{"parameter", "STATE", "KERN-A", "resource", "uboot", "EFI-SYSTEM"} {
		items = append(items, fixtureItem{name: name, pos: pos, size: sizes[name]})
		pos += alignTo(sizes[name], SectorSize)
	}
	declaredLen := pos // body length excluding the 4-byte CRC trailer

	holeStart := items[2].pos // KERN-A is the first item after the hole

	header := make([]byte, RKAFHeaderSize)
	copy(header[0:4], MagicRKAF[:])
	binary.LittleEndian.PutUint32(header[4:8], uint32(declaredLen))
	copy(header[8:42], "Fixture")
	binary.LittleEndian.PutUint32(header[136:140], uint32(len(items)))
	for i, it := range items {
		slot := header[140+i*RKAFItemSize : 140+(i+1)*RKAFItemSize]
		copy(slot[0:32], it.name)
		binary.LittleEndian.PutUint32(slot[92:96], uint32(it.size/SectorSize))
		binary.LittleEndian.PutUint32(slot[96:100], uint32(it.pos))
		binary.LittleEndian.PutUint32(slot[100:104], 0xFFFFFFFF)
		binary.LittleEndian.PutUint32(slot[108:112], uint32(it.size))
	}

	loader := make([]byte, 512)
	rkafOffset := int64(HeaderSize) + int64(len(loader))

	// Physical slices: everything before the hole at its logical offset,
	// everything after the hole shifted by f.hole.
	abs := func(logical int64) int64 {
		if logical >= holeStart {
			return rkafOffset + logical + f.hole
		}
		return rkafOffset + logical
	}
	slices := []testimage.Slice{
		{Name: "rkfw-head", Offset: 0, Data: writeRKFWHead(len(loader), rkafOffset, declaredLen+4)},
		{Name: "loader", Offset: HeaderSize, Data: loader},
		{Name: "rkaf-head", Offset: rkafOffset, Data: header},
	}
	for _, it := range items {
		data := make([]byte, it.size)
		if p := f.payloads[it.name]; len(p) > 0 {
			copy(data, p)
		}
		slices = append(slices, testimage.Slice{Name: it.name, Offset: abs(it.pos), Data: data})
	}
	// CRC trailer (zero: "not filled", as on both real images) and the ASCII
	// hex MD5 tail the hole detector keys on.
	crcOff := abs(declaredLen)
	slices = append(slices, testimage.Slice{Name: "crc", Offset: crcOff, Data: make([]byte, 4)})
	size := crcOff + 4 + 32
	slices = append(slices, testimage.Slice{
		Name: "md5", Offset: size - 32,
		Data: []byte(strings.Repeat("0123456789abcdef", 2)),
	})

	img := testimage.Image{Name: "hole-fixture", Size: size, Slices: slices}
	return img.Write(t, t.TempDir())
}

func alignTo(v, align int64) int64 {
	if align <= 1 {
		return v
	}
	return (v + align - 1) / align * align
}

func writeRKFWHead(loaderLen int, imageOffset, imageLength int64) []byte {
	head := make([]byte, HeaderSize)
	copy(head[0:4], MagicRKFW[:])
	binary.LittleEndian.PutUint16(head[4:6], HeaderSize)
	binary.LittleEndian.PutUint32(head[21:25], binary.LittleEndian.Uint32([]byte{'8', '8', '5', '3'}))
	binary.LittleEndian.PutUint32(head[25:29], HeaderSize)
	binary.LittleEndian.PutUint32(head[29:33], uint32(loaderLen))
	binary.LittleEndian.PutUint32(head[33:37], uint32(imageOffset))
	binary.LittleEndian.PutUint32(head[37:41], uint32(imageLength))
	return head
}

var (
	chromeOSMagic = []byte("CHROMEOS")
	fdtMagic      = []byte{0xD0, 0x0D, 0xFE, 0xED}
	rsceMagic     = []byte("RSCE")
)

// fatBootSector builds a minimal FAT32 boot sector that fatMagic accepts.
func fatBootSector() []byte {
	b := make([]byte, 512)
	b[0] = 0xEB
	copy(b[0x52:0x5A], "FAT32   ")
	b[0x1FE], b[0x1FF] = 0x55, 0xAA
	return b
}

// The marker payloads are what makes the split decidable; the sizes and
// offsets are the fixture's own, and the hole size is deliberately not 4 GiB
// so an accidental "+4 GiB constant" cannot pass this test.
func TestHoleDetectionWithSignatures(t *testing.T) {
	const hole = 8 << 20
	path := buildHoleImage(t, holeFixture{
		hole: hole,
		payloads: map[string][]byte{
			"parameter":  []byte("PARM"),
			"KERN-A":     chromeOSMagic,
			"resource":   rsceMagic,
			"uboot":      fdtMagic,
			"EFI-SYSTEM": fatBootSector(),
		},
	})
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, want := img.DataHole, int64(hole); got != want {
		t.Fatalf("DataHole = %d, want %d", got, want)
	}
	var kern Item
	for _, it := range img.Items {
		if it.Name == "KERN-A" {
			kern = it
		}
	}
	if kern.Name == "" {
		t.Fatal("KERN-A not parsed")
	}
	if got, want := img.DataHoleStart, int64(kern.Pos); got != want {
		t.Errorf("DataHoleStart = %d, want KERN-A pos %d", got, want)
	}
	// The shifted read is the correct one: CHROMEOS lives at pos+hole, and
	// pos itself is zeros.
	if got := img.ItemSignature(kern); got.Name != "CHROMEOS" {
		t.Errorf("KERN-A signature = %q, want CHROMEOS", got.Name)
	}
	raw := make([]byte, 8)
	if err := readAtFull(img.ReaderAt(), raw, img.RKAFOffset+int64(kern.Pos)); err != nil {
		t.Fatalf("read unshifted: %v", err)
	}
	if string(raw) == "CHROMEOS" {
		t.Error("unshifted pos also holds CHROMEOS; the fixture does not test the shift")
	}
	if got, want := img.DataOffset(kern), img.RKAFOffset+int64(kern.Pos)+hole; got != want {
		t.Errorf("DataOffset(KERN-A) = 0x%x, want 0x%x", got, want)
	}
	// Items before the hole are not shifted.
	param := *img.Parameter
	if got, want := img.DataOffset(param), img.RKAFOffset+int64(param.Pos); got != want {
		t.Errorf("DataOffset(parameter) = 0x%x, want 0x%x", got, want)
	}
}

// Without any signature the hole size is still derivable, but the parser must
// not pretend it knows where the hole starts.
func TestHoleDetectionWithoutSignatures(t *testing.T) {
	const hole = 4 << 20
	path := buildHoleImage(t, holeFixture{hole: hole})
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, want := img.DataHole, int64(hole); got != want {
		t.Fatalf("DataHole = %d, want %d", got, want)
	}
	if got, want := img.DataHoleStart, img.RKAFOffset+img.RKAFLength; got != want {
		t.Errorf("DataHoleStart = 0x%x, want the declared archive end 0x%x", got, want)
	}
	if !hasWarningString(img.Warnings, "no payload signature settles where it starts") {
		t.Errorf("warnings = %v, want the undetermined-position warning", img.Warnings)
	}
}

// A contiguous archive must not be reported as having a hole.
func TestNoHoleOnContiguousArchive(t *testing.T) {
	path := buildHoleImage(t, holeFixture{hole: 0})
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if img.DataHole != 0 {
		t.Errorf("DataHole = %d, want 0 for a contiguous archive", img.DataHole)
	}
}

// Two readings of the same item: the classifier must name the magic at the
// shifted offset and report zeros at the unshifted one. This is the check the
// dry-run prints per item.
func TestSignatureClassification(t *testing.T) {
	cases := []struct {
		data []byte
		want string
	}{
		{chromeOSMagic, "CHROMEOS"},
		{fdtMagic, "FDT"},
		{rsceMagic, "RSCE"},
		{fatBootSector(), "FAT"},
		{[]byte("PARM"), "PARM"},
		{make([]byte, 64), "zero"},
		{func() []byte { b := make([]byte, 64); for i := range b { b[i] = 0xFF }; return b }(), "blank"},
		{[]byte{0x53, 0xEF}, "unknown"},
	}
	for _, c := range cases {
		if got := ClassifySignature(c.data); got.Name != c.want {
			t.Errorf("ClassifySignature(% x) = %q, want %q", c.data[:min(len(c.data), 8)], got.Name, c.want)
		}
	}
	// ext4 magic sits at 0x438 and needs a 2 KiB sample.
	ext := make([]byte, 2048)
	ext[0x438], ext[0x439] = 0x53, 0xEF
	if got := ClassifySignature(ext); got.Name != "ext4" {
		t.Errorf("ext4 signature = %q, want ext4", got.Name)
	}
}

func TestExpectedSignature(t *testing.T) {
	cases := map[string]string{
		"KERN-A": "CHROMEOS", "KERN-B": "CHROMEOS", "uboot": "FDT",
		"EFI-SYSTEM": "FAT", "resource": "RSCE", "STATE": "", "parameter": "",
	}
	for name, want := range cases {
		if got := ExpectedSignature(name); got != want {
			t.Errorf("ExpectedSignature(%q) = %q, want %q", name, got, want)
		}
	}
}

func hasWarningString(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

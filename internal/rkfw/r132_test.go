package rkfw

// The expectations in this file are measured values from one real vendor image,
// not numbers produced by this parser and not numbers from a synthetic fixture:
//
//	openFyde r132 (Chromium 132 / platform 16093)
//	sha256 01fe1c0452606fc097d3816a73325c773013019954ed4f18fa55677842670ecc
//	size   8348478026 bytes
//
// It is NOT the official FydeOS v23.0-SP1 image, so nothing here may be
// relabelled as v23.0-SP1 data. The captured byte ranges live in
// internal/testimage/testdata/r132.
//
// Where the image disagrees with a previous assumption the test says so next to
// the number; the parser was changed to match the image, never the other way.

import (
	"os"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/testimage"
)

// r132ItemWant is one row of the r132 item table as measured on the image.
// slot is the number of bytes from pos to the next item's pos (the last item
// runs to the end of the archive body); it is derived from the measured pos
// column, not from the raw +104 field.
type r132ItemWant struct {
	name     string
	filename string
	nandSize uint32
	pos      uint32
	nandAddr uint32
	size     uint32
	slot     uint32
	field104 uint32 // the +104 field, verbatim
}

var r132Items = []r132ItemWant{
	{"package-file", "package-file", 0, 2048, NoAddress, 414, 2048, 1},
	{"bootloader", "Image/MiniLoaderAll.bin", 0, 4096, NoAddress, 412096, 413696, 202},
	{"parameter", "Image/parameter.txt", 16384, 417792, 0, 590, 2048, 1},
	// STATE: the partition is 5 GiB but the payload is 1 GiB.
	{"STATE", "Image/STATE.img", 10485869, 419840, 11542592, 1073797632, 1073799168, 2621468},
	{"KERN-A", "Image/KERN-A.img", 65536, 1074219008, 57408, 33554432, 33554432, 16384},
	{"ROOT-A", "Image/ROOT-A.img", 5578752, 1107773440, 5963840, 2856321024, 2856321024, 1394688},
	{"OEM", "Image/OEM.img", 32768, 3964094464, 188480, 16777216, 16777216, 8192},
	{"resource", "Image/resource.img", 32768, 3980871680, 24580, 923136, 923648, 451},
	{"uboot", "Image/uboot.img", 8192, 3981795328, 16384, 4194304, 4194304, 2048},
	{"EFI-SYSTEM", "Image/EFI-SYSTEM.img", 131072, 3985989632, 254016, 67108864, 67108864, 32768},
}

// r132Image writes the sparse reconstruction of the real image.
func r132Image(t *testing.T) (*Image, string) {
	t.Helper()
	path := testimage.R132().Write(t, t.TempDir())
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return img, path
}

func TestR132Container(t *testing.T) {
	img, _ := r132Image(t)
	const imageOffset = 412198
	const imageLength = 4053098500
	const rkafLength = 4053098496 // imageLength - 4

	if got, want := img.Size, testimage.R132Size; got != want {
		t.Errorf("size = %d, want %d", got, want)
	}
	if !img.HasRKFW {
		t.Fatal("HasRKFW = false; the image starts with RKFW")
	}
	h := img.RKFW
	checks := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"head_len", uint64(h.HeadLen), 102},
		{"chip", uint64(h.Chip), 0x33353838},
		{"loader_offset", uint64(h.LoaderOffset), 102},
		{"loader_length", uint64(h.LoaderLength), 412096},
		{"image_offset", uint64(h.ImageOffset), imageOffset},
		{"image_length", uint64(h.ImageLength), imageLength},
		{"archive version", uint64(img.Archive.Version), 0x01000000},
		{"num_parts", uint64(img.Archive.NumParts), 10},
		{"rkaf offset", uint64(img.RKAFOffset), imageOffset},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = 0x%x (%d), want 0x%x (%d)", c.name, c.got, c.got, c.want, c.want)
		}
	}
	if got := h.ChipName(); got != "RK3588" {
		t.Errorf("ChipName = %q, want RK3588", got)
	}
	if got := h.VersionString(); got != "1.0.0" {
		t.Errorf("VersionString = %q, want 1.0.0", got)
	}
	if ts, ok := h.ReleaseTime(); !ok || ts.Format("2006-01-02 15:04:05") != "2025-09-04 17:11:54" {
		t.Errorf("ReleaseTime = %v ok=%v", ts, ok)
	}
	// The header stores these strings with a leading space (" RK3588"), which is
	// as much a property of the image as the item-name garbage below.
	if got := img.Archive.Model; got != " RK3588" {
		t.Errorf("model = %q, want %q", got, " RK3588")
	}
	if got := img.Archive.ID; got != " 007" {
		t.Errorf("id = %q, want %q", got, " 007")
	}
	if got := img.Archive.Manufacturer; got != " RK3588" {
		t.Errorf("manufacturer = %q, want %q", got, " RK3588")
	}

	// The RKAF length field does not wrap in this image: the container's
	// image_length agrees with it (4053098500 = 4053098496 + 4), so the parser
	// must take the stored value instead of "recovering" a 4 GiB wrap from the
	// size of the file.
	if got, want := img.RKAFLengthField, uint32(rkafLength); got != want {
		t.Errorf("RKAFLengthField = %d, want %d", got, want)
	}
	if img.LengthWrapped {
		t.Error("LengthWrapped = true; this image's length field does not wrap")
	}
	if got, want := img.RKAFLength, int64(imageLength); got != want {
		t.Errorf("RKAFLength = %d, want %d (archive body + 4-byte trailer)", got, want)
	}

	// The file carries 2^32 bytes after the declared archive end plus the
	// 32-byte MD5 at EOF. SIL-515 proved on FyeOS v23 that those bytes are a
	// hole *inside* the packed archive (the packer reserves STATE's 5 GiB
	// slot), not trailing padding; the parser must derive the hole size from
	// the file layout and say so. This sparse reconstruction has no payload
	// bytes, so no signature can settle where the hole starts — the warning
	// has to say exactly that.
	trailing := img.Size - (img.RKAFOffset + img.RKAFLength)
	if want := int64(1)<<32 + 32; trailing != want {
		t.Errorf("trailing bytes after the archive = %d, want %d (2^32 hole + the MD5 tail)", trailing, want)
	}
	if got, want := img.DataHole, int64(1)<<32; got != want {
		t.Errorf("DataHole = %d, want %d (4 GiB)", got, want)
	}
	if !hasWarning(img.Warnings, "no payload signature settles where it starts") {
		t.Errorf("warnings = %v, want one saying the hole position is undetermined", img.Warnings)
	}
}

func TestR132ItemTable(t *testing.T) {
	img, _ := r132Image(t)
	if len(img.Items) != len(r132Items) {
		t.Fatalf("items = %d, want %d", len(img.Items), len(r132Items))
	}
	for i, want := range r132Items {
		got := img.Items[i]
		if got.Name != want.name {
			t.Errorf("item %d name = %q, want %q", i, got.Name, want.name)
		}
		if got.Filename != want.filename {
			t.Errorf("item %q filename = %q, want %q", want.name, got.Filename, want.filename)
		}
		if got.NandSize != want.nandSize {
			t.Errorf("item %q nand_size = %d, want %d", want.name, got.NandSize, want.nandSize)
		}
		if got.Pos != want.pos {
			t.Errorf("item %q pos = %d, want %d", want.name, got.Pos, want.pos)
		}
		if got.NandAddr != want.nandAddr {
			t.Errorf("item %q nand_addr = 0x%x, want 0x%x", want.name, got.NandAddr, want.nandAddr)
		}
		if got.Size != want.size {
			t.Errorf("item %q size = %d, want %d (the +108 field is the payload length)", want.name, got.Size, want.size)
		}
		if got.PaddedSize != want.slot {
			t.Errorf("item %q padded size = %d, want %d (bytes from pos to the next item)", want.name, got.PaddedSize, want.slot)
		}
		if got.PaddedBlocks != want.field104 {
			t.Errorf("item %q +104 field = %d, want %d", want.name, got.PaddedBlocks, want.field104)
		}
		// +104 is a count of 2 KiB pages, not a byte count: for every item but
		// STATE it covers exactly the archive slot. STATE records the 5 GiB
		// partition capacity instead, which is why it must never be used as a
		// length.
		if want.name != "STATE" {
			if pages := uint64(got.PaddedBlocks) * 2048; pages != uint64(want.slot) {
				t.Errorf("item %q: +104 * 2048 = %d, want the %d-byte slot", want.name, pages, want.slot)
			}
		}
	}
	if img.Parameter == nil || img.Parameter.Name != "parameter" {
		t.Fatalf("parameter item = %+v", img.Parameter)
	}

	// Sum of the slots covers the archive body exactly (header + payloads + pad).
	var sum uint64 = RKAFHeaderSize
	for _, it := range img.Items {
		sum += uint64(it.PaddedSize)
	}
	if got, want := sum, uint64(img.RKAFLength-4); got != want {
		t.Errorf("slots add up to %d, want the %d-byte archive body", got, want)
	}
}

// TestR132ItemNameStopsAtFirstNUL pins the measured quirk that made the stock
// extraction script miss the parameter item: the name/path fields are not
// zero-filled, and heap garbage follows the first NUL.
func TestR132ItemNameStopsAtFirstNUL(t *testing.T) {
	img, path := r132Image(t)
	it := img.Items[2]
	if it.Name != "parameter" {
		t.Fatalf("item 2 name = %q, want %q", it.Name, "parameter")
	}

	// Show the garbage is really in the file: the parameter slot is item 2, and
	// its name field is bytes 9..12 = 00 00 'e' 00.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var raw [12]byte
	if _, err := f.ReadAt(raw[:], img.RKAFOffset+140+2*RKAFItemSize); err != nil {
		t.Fatal(err)
	}
	if raw[9] != 0 || raw[11] != 'e' {
		t.Fatalf("name field bytes 9..12 = %x; the fixture no longer carries the post-NUL garbage", raw[9:12])
	}
}

func TestR132ParameterPayload(t *testing.T) {
	img, _ := r132Image(t)
	data, err := img.ReadItem(*img.Parameter, 0)
	if err != nil {
		t.Fatalf("ReadItem(parameter): %v", err)
	}
	if len(data) != 590 {
		t.Fatalf("parameter payload = %d bytes, want 590", len(data))
	}
	if !strings.HasPrefix(string(data), "PARM") {
		t.Fatalf("parameter payload starts with %q, want PARM", data[:4])
	}
	if !strings.Contains(string(data), "0xa0006d@0x00b02040(STATE)") {
		t.Fatal("parameter payload has no STATE mtdparts entry")
	}
}

func TestR132MD5Tail(t *testing.T) {
	img, _ := r132Image(t)
	res, err := img.VerifyMD5()
	if err != nil {
		t.Fatalf("VerifyMD5: %v", err)
	}
	if !res.Present {
		t.Fatal("Present = false")
	}
	// The digest sits at EOF-32, followed by nothing. The 4 GiB of padding
	// before it must not be mistaken for a signature blob.
	if res.Signed {
		t.Error("Signed = true; r132 has a plain 32-byte MD5 at EOF-32")
	}
	if want := img.Size - 32; res.Offset != want {
		t.Errorf("MD5 offset = %d, want %d", res.Offset, want)
	}
	if want := "a2329d6f0af98a7379bec6faafbdcfe1"; res.Expected != want {
		t.Errorf("MD5 tail = %q, want %q", res.Expected, want)
	}
	// The reconstructed file has holes where the real payloads are, so the MD5
	// cannot match; a match here would mean the slices cover the whole image.
	if res.OK {
		t.Errorf("MD5 matched a partial reconstruction (computed %q)", res.Computed)
	}
}

// TestR132RKAFCRCTrailer records what the real image holds at
// image_offset + rkaf.length: 00 00 00 00. See docs/rkfw-rkaf-format.md §5.1
// for what that means; the parser deliberately does not treat zero as "valid".
func TestR132RKAFCRCTrailer(t *testing.T) {
	img, path := r132Image(t)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var trailer [4]byte
	if _, err := f.ReadAt(trailer[:], img.RKAFOffset+img.RKAFLength-4); err != nil {
		t.Fatal(err)
	}
	if trailer != [4]byte{} {
		t.Fatalf("RKAF trailer = %x, want 00 00 00 00", trailer)
	}
}

func hasWarning(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// Package testimage reassembles sparse RKFW images from captured slices of a
// real vendor image, so parser and dry-run tests run against real bytes instead
// of only synthetic fixtures.
//
// The captured slices are tiny: the RKFW header, the RKAF header with its item
// table, the "parameter" item, the 4-byte RKAF CRC trailer and the trailing
// MD5 (2,766 bytes in total for the openFyde r132 fixture). Everything else —
// the loader and the multi-GiB payloads — stays a hole, so the sparse file has
// the real 8,348,478,026-byte size that the parser's layout checks depend on
// while occupying only a few blocks on disk.
//
// Nothing here writes to a device: the result is a plain file.
package testimage

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Slice is one captured byte range of a real image.
type Slice struct {
	// Name identifies the range, e.g. "rkaf-head".
	Name string
	// Offset is the absolute offset inside the image.
	Offset int64
	// Data is the captured content, verbatim.
	Data []byte
}

// Image is a real image described by its size and the byte ranges that were
// captured from it. Uncaptured ranges read back as zeros.
type Image struct {
	// Name identifies the source image. It never names a version the file is
	// not: "r132" is openFyde r132, not FydeOS v23.0-SP1.
	Name string
	// Size is the full size of the source image in bytes.
	Size int64
	// SHA256 is the digest of the complete source image, when known.
	SHA256 string
	// Slices are the captured ranges, ordered by offset.
	Slices []Slice
}

// Write lays the captured slices out at their absolute offsets in a new sparse
// file inside dir and returns its path. It fails when a slice does not fit in
// the declared size, and skips the test when the filesystem cannot store the
// image sparsely (a test must not silently materialise 8 GiB).
func (img Image) Write(t testing.TB, dir string) string {
	t.Helper()

	path := filepath.Join(dir, img.Name+".img")
	allocated, err := img.WriteTo(path)
	if err != nil {
		t.Fatalf("testimage: %v", err)
	}
	if allocated > img.Size/2 {
		if err := os.Remove(path); err != nil {
			t.Logf("testimage: remove %s: %v", path, err)
		}
		t.Skipf("testimage: %s is not sparse (%d bytes allocated for a %d-byte image); skipping the real-image fixture",
			path, allocated, img.Size)
	}
	return path
}

// WriteTo writes the image to path and returns how many bytes the filesystem
// allocated for it, so a caller can tell a sparse file from a materialised one.
func (img Image) WriteTo(path string) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	if err := f.Truncate(img.Size); err != nil {
		return 0, fmt.Errorf("size %s to %d bytes: %w", path, img.Size, err)
	}
	for _, s := range img.Slices {
		if s.Offset < 0 || int64(len(s.Data)) > img.Size-s.Offset {
			return 0, fmt.Errorf("slice %q [0x%x,0x%x) does not fit in a %d-byte image",
				s.Name, s.Offset, s.Offset+int64(len(s.Data)), img.Size)
		}
		if _, err := f.WriteAt(s.Data, s.Offset); err != nil {
			return 0, fmt.Errorf("write slice %q at 0x%x: %w", s.Name, s.Offset, err)
		}
	}

	st, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	allocated, _ := allocatedBytes(st)
	return allocated, nil
}

// openFyde r132 (Chromium 132 / platform 16093) measured constants. Every
// number below was read from the source image itself; see testdata/r132.
const (
	// R132Size is the size of /mnt/raid/openfyde/rk3588-image-maker/openfyde.r132.img.
	R132Size int64 = 8348478026
	// R132SHA256 is the digest of that complete source image.
	R132SHA256 = "01fe1c0452606fc097d3816a73325c773013019954ed4f18fa55677842670ecc"

	// r132ImageOffset is the RKFW image_offset field: the start of the RKAF archive.
	r132ImageOffset = 412198
	// r132RKAFLength is the RKAF length field: the archive body, excluding the
	// 4-byte CRC trailer.
	r132RKAFLength = 4053098496
	// r132Parameter is the absolute offset of the "parameter" item payload.
	r132Parameter = r132ImageOffset + 417792
	// r132Trailer is where the leader thinks the 4-byte RKAF CRC trailer is.
	r132Trailer = r132ImageOffset + r132RKAFLength
)

// Captured slices of the r132 image, cut on the test machine as described in
// SIL-520. The ranges are byte-for-byte what is on disk there, including the
// heap garbage that follows the first NUL in the item name/path fields.
var (
	//go:embed testdata/r132/rkfw-head.bin
	r132RKFWHead []byte
	//go:embed testdata/r132/rkaf-head.bin
	r132RKAFHead []byte
	//go:embed testdata/r132/parameter.bin
	r132ParameterItem []byte
	//go:embed testdata/r132/rkaf-crc-trailer.bin
	r132RKAFCrcTrailer []byte
	//go:embed testdata/r132/md5-tail.bin
	r132MD5Tail []byte
)

// R132 returns the openFyde r132 image rebuilt from its captured slices.
func R132() Image {
	return Image{
		Name:   "openfyde-r132",
		Size:   R132Size,
		SHA256: R132SHA256,
		Slices: []Slice{
			{Name: "rkfw-head", Offset: 0, Data: r132RKFWHead},
			{Name: "rkaf-head", Offset: r132ImageOffset, Data: r132RKAFHead},
			{Name: "parameter", Offset: r132Parameter, Data: r132ParameterItem},
			{Name: "rkaf-crc-trailer", Offset: r132Trailer, Data: r132RKAFCrcTrailer},
			{Name: "md5-tail", Offset: R132Size - 32, Data: r132MD5Tail},
		},
	}
}

// R132Offsets reports where the captured slices land, which keeps the numbers
// in a test readable and lets it assert the fixture itself.
func R132Offsets() map[string]int64 {
	out := make(map[string]int64, 5)
	for _, s := range R132().Slices {
		out[s.Name] = s.Offset
	}
	return out
}

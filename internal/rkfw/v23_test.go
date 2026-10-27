package rkfw

// The official FyeOS v23.0-SP1 image (Fydetab Duo), as captured by SIL-520 and
// re-measured on the machine that wrote it (SIL-515). Unlike r132, this is the
// image that was flashed when the board stopped booting, and it is where the
// 4 GiB archive hole was observed from the read-back: the bytes at KERN-A's
// table pos were zero, CHROMEOS was 4 GiB further along.
//
//	image  FydeOS_FydeTab_Duo_v23.0-SP1-com.img
//	size   8,360,540,746 bytes
//	sha256 5a3f79918a987160e355f680ad1e2b828c93d7f97a6c97aab57b04a197fe7010
//
// The capture holds only header/parameter/trailer/MD5 slices, so payload
// signatures cannot be sampled here. What the test can pin is the structure
// that was wrong before: the hole size (4 GiB), the item table, and the fact
// that the plan refuses to trust unverified offsets.

import (
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/testimage"
)

func v23Image(t *testing.T) *Image {
	t.Helper()
	path := testimage.V23().Write(t, t.TempDir())
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return img
}

func TestV23Container(t *testing.T) {
	img := v23Image(t)

	if got, want := img.Size, testimage.V23Size; got != want {
		t.Errorf("size = %d, want %d", got, want)
	}
	if !img.HasRKFW {
		t.Fatal("HasRKFW = false")
	}
	if got, want := img.RKFW.HeadLen, uint16(HeaderSize); got != want {
		t.Errorf("head_len = %d, want %d", got, want)
	}
	if got, want := img.RKFW.Chip, uint32(0x33353838); got != want {
		t.Errorf("chip = 0x%08x, want 0x33353838 (reversed ASCII \"8853\")", got)
	}
	if got, want := img.RKFW.ChipName(), "RK3588"; got != want {
		t.Errorf("chip name = %q, want %q", got, want)
	}
	if got, want := img.LoaderOffset, int64(102); got != want {
		t.Errorf("loader offset = %d, want %d", got, want)
	}
	if got, want := img.LoaderLength, int64(422336); got != want {
		t.Errorf("loader length = %d, want %d", got, want)
	}
	if got, want := img.RKAFOffset, int64(422438); got != want {
		t.Errorf("rkaf offset = %d, want %d", got, want)
	}
	// Both 32-bit length fields carry the same value and both wrapped: the
	// archive really is 4 GiB longer than they say (see DataHole below).
	if got, want := img.RKAFLengthField, uint32(4065150976); got != want {
		t.Errorf("rkaf length field = %d, want %d", got, want)
	}
	if !img.LengthWrapped {
		t.Error("LengthWrapped = false; the 64-bit high words extend the archive past the 32-bit length field")
	}

	// The high words are authoritative, so there is no inferred hole.
	if got := img.DataHole; got != 0 {
		t.Fatalf("DataHole = %d, want 0 (high-word offsets)", got)
	}
	if !img.DataHoleDecided {
		t.Error("DataHoleDecided = false; the high words settle every offset")
	}
	if got, want := img.DataOffset(img.Items[4]), img.RKAFOffset+int64(img.Items[4].Pos64); got != want {
		t.Errorf("KERN-A data offset = %d, want 0x%x", got, want)
	}
}

// v23ItemWant is the item table as measured on the official image.
type v23ItemWant struct {
	name     string
	filename string
	pos      uint32
	size     uint32
}

var v23Items = []v23ItemWant{
	{"package-file", "package-file", 2048, 414},
	{"bootloader", "Image/MiniLoaderAll.bin", 4096, 422336},
	{"parameter", "Image/parameter.txt", 428032, 590},
	{"STATE", "Image/STATE.img", 430080, 1073797632},
	{"KERN-A", "Image/KERN-A.img", 1074229248, 33554432},
	{"ROOT-A", "Image/ROOT-A.img", 1107783680, 2856321024},
	{"OEM", "Image/OEM.img", 3964104704, 16777216},
	{"resource", "Image/resource.img", 3980881920, 12964352},
	{"uboot", "Image/uboot.img", 3993847808, 4194304},
	{"EFI-SYSTEM", "Image/EFI-SYSTEM.img", 3998042112, 67108864},
}

func TestV23ItemTable(t *testing.T) {
	img := v23Image(t)
	if got, want := img.Archive.NumParts, uint32(10); got != want {
		t.Fatalf("num_parts = %d, want %d", got, want)
	}
	if got, want := len(img.Items), len(v23Items); got != want {
		t.Fatalf("items = %d, want %d", got, want)
	}
	for i, want := range v23Items {
		got := img.Items[i]
		if got.Name != want.name || got.Filename != want.filename {
			t.Errorf("item %d = %q/%q, want %q/%q", i, got.Name, got.Filename, want.name, want.filename)
		}
		if got.Pos != want.pos {
			t.Errorf("item %q pos = %d, want %d", want.name, got.Pos, want.pos)
		}
		if got.Size != want.size {
			t.Errorf("item %q size = %d, want %d", want.name, got.Size, want.size)
		}
	}
	// The last item's slot ends at the recovered archive body.
	last := img.Items[len(img.Items)-1]
	if got, want := last.End(), uint64(img.RKAFLength-4); got != want {
		t.Errorf("last item end = %d, want the archive body %d", got, want)
	}
	// STATE: 5 GiB partition and a full 5 GiB payload; the size high word is
	// what the low 32-bit field cannot express.
	state := img.Items[3]
	if got, want := state.Size, uint32(1073797632); got != want {
		t.Errorf("STATE low size = %d, want %d", got, want)
	}
	if got, want := state.Size64, uint64(5368764928); got != want {
		t.Errorf("STATE payload = %d, want %d (5 GiB)", got, want)
	}
	if got, want := state.NandSize, uint32(10485869); got != want {
		t.Errorf("STATE nand_size = %d, want %d sectors (5 GiB)", got, want)
	}
}

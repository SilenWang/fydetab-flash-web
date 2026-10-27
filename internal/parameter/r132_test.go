package parameter

// Expectations measured on the openFyde r132 image (sha256
// 01fe1c0452606fc097d3816a73325c773013019954ed4f18fa55677842670ecc), not on
// the official FydeOS v23.0-SP1 image, which was not available. The bytes come
// from internal/testimage/testdata/r132/parameter.bin.

import (
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/testimage"
)

// r132Partition is one mtdparts entry as written in the real parameter text.
type r132Partition struct {
	name    string
	first   uint64
	sectors uint64
	flags   []string
}

var r132Partitions = []r132Partition{
	{"STATE", 0x00b02040, 0xa0006d, nil},
	{"KERN-A", 0x0000e040, 0x00010000, nil},
	{"ROOT-A", 0x005b0040, 0x00552000, nil},
	{"KERN-B", 0x0001e040, 0x00010000, nil},
	{"ROOT-B", 0x0005e040, 0x00552000, nil},
	{"KERN-C", 0x00006000, 0x00000001, nil},
	{"ROOT-C", 0x00006001, 0x00000001, nil},
	{"OEM", 0x0002e040, 0x00008000, nil},
	{"reserved1", 0x00006002, 0x00000001, nil},
	{"resource", 0x00006004, 0x00008000, nil},
	{"uboot", 0x00004000, 0x00002000, nil},
	{"EFI-SYSTEM", 0x0003e040, 0x00020000, []string{"bootable"}},
}

func TestR132Parameter(t *testing.T) {
	var raw []byte
	for _, s := range testimage.R132().Slices {
		if s.Name == "parameter" {
			raw = s.Data
		}
	}
	if raw == nil {
		t.Fatal("fixture has no parameter slice")
	}

	doc, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !doc.Wrapped {
		t.Fatal("Wrapped = false; the item starts with PARM")
	}
	if want := uint32(578); doc.DeclaredLength != want {
		t.Errorf("declared length = %d, want %d", doc.DeclaredLength, want)
	}
	if doc.CRCAvailable && !doc.CRCOK {
		t.Errorf("PARM CRC mismatch: stored 0x%08x computed 0x%08x", doc.CRCStored, doc.CRCComputed)
	}
	if !doc.CRCAvailable {
		t.Error("CRCAvailable = false; the wrapper carries a trailer")
	}
	if got := doc.Fields["TYPE"]; got != "GPT" {
		t.Errorf("TYPE = %q, want GPT", got)
	}
	if got := doc.Fields["MACHINE_MODEL"]; got != "RK3588" {
		t.Errorf("MACHINE_MODEL = %q", got)
	}

	if len(doc.Partitions) != len(r132Partitions) {
		t.Fatalf("partitions = %d, want %d", len(doc.Partitions), len(r132Partitions))
	}
	for i, want := range r132Partitions {
		got := doc.Partitions[i]
		if got.Name != want.name {
			t.Errorf("partition %d name = %q, want %q", i, got.Name, want.name)
		}
		if got.First != want.first {
			t.Errorf("partition %q first = 0x%x (%d), want 0x%x", want.name, got.First, got.First, want.first)
		}
		if got.Grow {
			t.Errorf("partition %q is marked grow; the real table has no grow partition", want.name)
		}
		if got.Sectors != want.sectors {
			t.Errorf("partition %q sectors = 0x%x, want 0x%x", want.name, got.Sectors, want.sectors)
		}
		if len(got.Flags) != len(want.flags) {
			t.Errorf("partition %q flags = %v, want %v", want.name, got.Flags, want.flags)
		}
	}

	// STATE is ~5 GiB by sectors — exactly 0xa0006d sectors, which is 53,248
	// bytes more than 5 GiB — while the image only carries 1 GiB of payload.
	state := doc.Partitions[0]
	if got, want := state.Sectors*SectorSize, uint64(5368764928); got != want {
		t.Errorf("STATE size = %d bytes, want %d (0xa0006d sectors)", got, want)
	}
	if doc.PartitionCount != 12 {
		t.Errorf("PartitionCount = %d, want 12", doc.PartitionCount)
	}
	if len(doc.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", doc.Warnings)
	}
}

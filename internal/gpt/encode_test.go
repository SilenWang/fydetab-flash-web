package gpt

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/parameter"
)

// referenceDiskSectors is the flash size the reference device reports
// (233.0 GiB of eMMC). The partition table is built for the real flash, not for
// the size of an image.
const referenceDiskSectors = 488636416

// referenceTable is the partition table the vendor's own write produced on the
// reference device (12 entries, read back read-only after the device was
// restored). Start LBAs and sizes are the acceptance values for the encoder: a
// table that does not reproduce them is a different partition table.
var referenceTable = []struct {
	name    string
	first   uint64
	sectors uint64
}{
	{"STATE", 0xb02040, 0xa0006d},
	{"KERN-A", 0xe040, 0x10000},
	{"ROOT-A", 0x5b0040, 0x552000},
	{"KERN-B", 0x1e040, 0x10000},
	{"ROOT-B", 0x5e040, 0x552000},
	{"KERN-C", 0x6000, 1},
	{"ROOT-C", 0x6001, 1},
	{"OEM", 0x2e040, 0x8000},
	{"reserved1", 0x6002, 1},
	{"resource", 0x6004, 0x8000},
	{"uboot", 0x4000, 0x2000},
	{"EFI-SYSTEM", 0x3e040, 0x20000},
}

func referenceLayout(t *testing.T) *Layout {
	t.Helper()
	raw, err := os.ReadFile("../testimage/testdata/v23/parameter.bin")
	if err != nil {
		t.Fatalf("read the parameter slice: %v", err)
	}
	doc, err := parameter.Parse(raw)
	if err != nil {
		t.Fatalf("parse the parameter slice: %v", err)
	}
	if len(doc.Partitions) != len(referenceTable) {
		t.Fatalf("parameter has %d partitions, the reference table has %d", len(doc.Partitions), len(referenceTable))
	}
	lay, err := Build(doc.Partitions, referenceDiskSectors)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return lay
}

// The encoded bytes must reproduce the reference partition table entry by
// entry, and must be a GPT the UEFI rules accept: both headers, both entry
// arrays, both checksums.
func TestEncodeReproducesTheReferencePartitionTable(t *testing.T) {
	lay := referenceLayout(t)
	img, err := Encode(lay, EncodeOptions{Seed: "reference-device"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(img.Primary) != PrimarySectors*512 || len(img.Backup) != BackupSectors*512 {
		t.Fatalf("regions = %d/%d bytes, want %d/%d", len(img.Primary), len(img.Backup),
			PrimarySectors*512, BackupSectors*512)
	}
	if img.PrimaryLBA != 0 {
		t.Fatalf("primary LBA = %d, want 0", img.PrimaryLBA)
	}
	if want := uint64(referenceDiskSectors - BackupSectors); img.BackupLBA != want {
		t.Fatalf("backup LBA = 0x%x, want 0x%x (disk - 33)", img.BackupLBA, want)
	}

	parsed, err := Parse(img.Primary, img.Backup)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.ProtectiveMBR {
		t.Fatal("no protective MBR")
	}
	if parsed.Primary.MyLBA != 1 || parsed.Primary.AlternateLBA != referenceDiskSectors-1 {
		t.Fatalf("primary header lbas = %d/%d", parsed.Primary.MyLBA, parsed.Primary.AlternateLBA)
	}
	if parsed.Primary.FirstUsableLBA != 34 || parsed.Primary.LastUsableLBA != referenceDiskSectors-34 {
		t.Fatalf("usable range = %d..%d", parsed.Primary.FirstUsableLBA, parsed.Primary.LastUsableLBA)
	}
	if parsed.Backup == nil {
		t.Fatal("no backup header")
	}
	if parsed.Backup.MyLBA != referenceDiskSectors-1 || parsed.Backup.AlternateLBA != 1 {
		t.Fatalf("backup header lbas = %d/%d", parsed.Backup.MyLBA, parsed.Backup.AlternateLBA)
	}
	if parsed.Backup.PartitionEntryLBA != referenceDiskSectors-33 {
		t.Fatalf("backup entry array at %d, want %d", parsed.Backup.PartitionEntryLBA, referenceDiskSectors-33)
	}

	// The acceptance table, row by row, in parameter order.
	if len(parsed.Entries) != len(referenceTable) {
		t.Fatalf("decoded %d entries, want %d", len(parsed.Entries), len(referenceTable))
	}
	for i, want := range referenceTable {
		got := parsed.Entries[i]
		if !strings.EqualFold(got.Name, want.name) {
			t.Errorf("entry %d name = %q, want %q", i, got.Name, want.name)
		}
		if got.FirstLBA != want.first {
			t.Errorf("%s start LBA = 0x%x, want 0x%x", want.name, got.FirstLBA, want.first)
		}
		if got.Sectors() != want.sectors {
			t.Errorf("%s size = 0x%x sectors, want 0x%x", want.name, got.Sectors(), want.sectors)
		}
		if got.LastLBA != want.first+want.sectors-1 {
			t.Errorf("%s end LBA = 0x%x, want 0x%x", want.name, got.LastLBA, want.first+want.sectors-1)
		}
	}
	// Only the EFI system partition is bootable in the reference table.
	for _, e := range parsed.Entries {
		want := strings.EqualFold(e.Name, "EFI-SYSTEM")
		if e.Bootable() != want {
			t.Errorf("%s bootable = %t, want %t", e.Name, e.Bootable(), want)
		}
	}
}

// A retried write must be byte-identical, which is what makes the read-back
// comparison and a resumed run meaningful.
func TestEncodeIsDeterministic(t *testing.T) {
	lay := referenceLayout(t)
	a, err := Encode(lay, EncodeOptions{Seed: "same-seed"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	b, err := Encode(lay, EncodeOptions{Seed: "same-seed"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(a.Primary, b.Primary) || !bytes.Equal(a.Backup, b.Backup) {
		t.Fatal("the same layout and seed produced different bytes")
	}
	c, err := Encode(lay, EncodeOptions{Seed: "other-seed"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if bytes.Equal(a.Primary, c.Primary) {
		t.Fatal("a different seed produced the same GUIDs")
	}
}

// An unknown disk size means the backup table cannot be placed; the encoder
// must refuse rather than guess.
func TestEncodeRefusesAnUnknownDiskSize(t *testing.T) {
	lay := referenceLayout(t)
	lay.DiskKnown = false
	if _, err := Encode(lay, EncodeOptions{}); err == nil {
		t.Fatal("expected an error for an unknown disk size")
	}
	if _, err := Encode(nil, EncodeOptions{}); err == nil {
		t.Fatal("expected an error for a nil layout")
	}
}

// A corrupted header or entry array must not parse as a usable table.
func TestParseRejectsCorruption(t *testing.T) {
	lay := referenceLayout(t)
	img, err := Encode(lay, EncodeOptions{Seed: "corrupt"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	bad := append([]byte(nil), img.Primary...)
	bad[512+20] ^= 0xFF // reserved field of the header, covered by the header CRC
	if _, err := Parse(bad, img.Backup); err == nil {
		t.Fatal("a modified header parsed as valid")
	}
	bad = append([]byte(nil), img.Primary...)
	bad[2*512+128+32] ^= 0xFF // first byte of the second entry's start LBA
	if _, err := Parse(bad, img.Backup); err == nil {
		t.Fatal("a modified entry array parsed as valid")
	}
	bad = append([]byte(nil), img.Primary...)
	bad[510] = 0 // drop the MBR signature
	if _, err := Parse(bad, img.Backup); err == nil {
		t.Fatal("a table without the protective MBR parsed as valid")
	}
}

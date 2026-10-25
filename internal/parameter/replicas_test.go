package parameter

import (
	"os"
	"testing"
)

// The reference device reports a flash offset of 0 and a reserved area of
// 0x4000 sectors for its parameter entry. With the first-4-access capability
// (which it advertises) the copies start at LBA 0x2000 and are spaced one
// eighth of the reserved area apart.
func TestReplicaLayoutMatchesTheReferenceDevice(t *testing.T) {
	replicas, err := ReplicaLayout(0, 0x4000, true, 2048)
	if err != nil {
		t.Fatalf("ReplicaLayout: %v", err)
	}
	if len(replicas) != ReplicaCount {
		t.Fatalf("copies = %d, want %d", len(replicas), ReplicaCount)
	}
	if want := uint64(0x2000); replicas[0].LBA != want {
		t.Fatalf("first copy at LBA 0x%x, want 0x%x", replicas[0].LBA, want)
	}
	for i, r := range replicas {
		wantLBA := uint64(0x2000 + i*0x800)
		if r.Index != i || r.LBA != wantLBA {
			t.Fatalf("copy %d at LBA 0x%x (index %d), want 0x%x", i, r.LBA, r.Index, wantLBA)
		}
		if r.Sectors != 4 || r.End() != r.LBA+4 {
			t.Fatalf("copy %d covers %d sectors ending at 0x%x", i, r.Sectors, r.End())
		}
	}
	if got, want := ReplicaBytes(replicas), int64(ReplicaCount*2048); got != want {
		t.Fatalf("replica bytes = %d, want %d", got, want)
	}
}

// Without the capability the copies start at the entry's own flash offset.
func TestReplicaLayoutWithoutFirst4Access(t *testing.T) {
	replicas, err := ReplicaLayout(0x100, 0x4000, false, 512)
	if err != nil {
		t.Fatalf("ReplicaLayout: %v", err)
	}
	if replicas[0].LBA != 0x100 {
		t.Fatalf("first copy at LBA 0x%x, want 0x100", replicas[0].LBA)
	}
	if replicas[1].LBA-replicas[0].LBA != 0x800 {
		t.Fatalf("stride = 0x%x, want 0x800", replicas[1].LBA-replicas[0].LBA)
	}
}

// Everything the placement needs must be there; a guess would write the
// partition table somewhere the device does not read.
func TestReplicaLayoutRefusesIncompleteInput(t *testing.T) {
	cases := []struct {
		name      string
		offset    uint32
		partSize  uint32
		copyBytes int64
	}{
		{"no reserved area", 0, 0, 2048},
		{"reserved area unset", 0, noEntryValue, 2048},
		{"reserved area too small", 0, 4, 2048},
		{"flash offset unset", noEntryValue, 0x4000, 2048},
		{"empty copy", 0, 0x4000, 0},
		{"unsectoral copy", 0, 0x4000, 500},
	}
	for _, c := range cases {
		if _, err := ReplicaLayout(c.offset, c.partSize, true, c.copyBytes); err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
	}
}

// The block the image carries is written verbatim, so its own checksum has to
// verify; this pins the check the writer performs before replicating it.
func TestReferenceParameterBlockCarriesAValidChecksum(t *testing.T) {
	raw, err := os.ReadFile("../testimage/testdata/v23/parameter.bin")
	if err != nil {
		t.Fatalf("read the parameter slice: %v", err)
	}
	doc, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !doc.Wrapped {
		t.Fatal("the reference parameter block has no PARM wrapper")
	}
	if !doc.CRCOK {
		t.Fatalf("PARM CRC: stored 0x%08x, computed 0x%08x", doc.CRCStored, doc.CRCComputed)
	}
	if doc.PartitionCount != 12 {
		t.Fatalf("partitions = %d, want 12", doc.PartitionCount)
	}
}

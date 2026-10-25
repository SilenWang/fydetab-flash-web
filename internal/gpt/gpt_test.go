package gpt

import (
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
)

func part(name string, first, sectors uint64) parameter.Partition {
	return parameter.Partition{Name: name, First: first, Sectors: sectors}
}

func TestBuildExplicitDisk(t *testing.T) {
	parts := []parameter.Partition{
		part("uboot", 0x40, 0x2000),
		part("boot", 0x2040, 0x8000),
	}
	lay, err := Build(parts, 0x20000)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !lay.DiskKnown || lay.DiskDerived {
		t.Fatalf("disk flags = known:%v derived:%v", lay.DiskKnown, lay.DiskDerived)
	}
	if lay.FirstUsableLBA != 34 || lay.LastUsableLBA != 0x20000-34 {
		t.Fatalf("usable range = %d..%d", lay.FirstUsableLBA, lay.LastUsableLBA)
	}
	if lay.BackupEntriesLBA != 0x20000-33 || lay.BackupHeaderLBA != 0x1FFFF {
		t.Fatalf("backup GPT = entries %d header %d", lay.BackupEntriesLBA, lay.BackupHeaderLBA)
	}
	if len(lay.Entries) != 2 {
		t.Fatalf("entries = %d", len(lay.Entries))
	}
	e := lay.Entries[0]
	if e.FirstLBA != 0x40 || e.LastLBA != 0x40+0x2000-1 || e.Sectors != 0x2000 || !e.Known {
		t.Fatalf("uboot entry = %+v", e)
	}
}

func TestBuildDerivedDisk(t *testing.T) {
	parts := []parameter.Partition{
		part("uboot", 0x40, 0x2000),
		part("rootfs", 0x2040, 0x40000),
	}
	lay, err := Build(parts, 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !lay.DiskDerived {
		t.Fatal("expected DiskDerived")
	}
	wantEnd := uint64(0x2040 + 0x40000)
	if lay.DiskSectors != wantEnd+FirstUsableLBA {
		t.Fatalf("derived disk = %d, want %d", lay.DiskSectors, wantEnd+FirstUsableLBA)
	}
	if !lay.DiskKnown {
		t.Fatal("disk without grow partitions should be known")
	}
}

func TestBuildGrowLast(t *testing.T) {
	parts := []parameter.Partition{
		part("uboot", 0x40, 0x2000),
		{Name: "userdata", First: 0x2040, Grow: true},
	}
	lay, err := Build(parts, 10000)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	e := lay.Entries[1]
	if !e.Known || e.FirstLBA != 0x2040 || e.LastLBA != lay.LastUsableLBA {
		t.Fatalf("grow entry = %+v (last usable %d)", e, lay.LastUsableLBA)
	}
	if e.Sectors != lay.LastUsableLBA-0x2040+1 {
		t.Fatalf("grow sectors = %d", e.Sectors)
	}
}

func TestBuildGrowUnknownDisk(t *testing.T) {
	parts := []parameter.Partition{
		part("uboot", 0x40, 0x2000),
		{Name: "userdata", First: 0x2040, Grow: true},
	}
	lay, err := Build(parts, 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if lay.DiskKnown {
		t.Fatal("disk with a grow partition must be unknown without a flash size")
	}
	if lay.Entries[1].Known {
		t.Fatal("grow entry end must be unknown")
	}
	if lay.BackupHeaderLBA != 0 {
		t.Fatalf("backup header LBA = %d, want 0 (unknown)", lay.BackupHeaderLBA)
	}
}

func TestBuildErrors(t *testing.T) {
	cases := []struct {
		name  string
		parts []parameter.Partition
		disk  uint64
		want  string
	}{
		{
			name:  "overlap",
			parts: []parameter.Partition{part("a", 0x40, 0x1000), part("b", 0x800, 0x1000)},
			disk:  10000,
			want:  "overlap",
		},
		{
			name:  "past last usable",
			parts: []parameter.Partition{part("a", 100, 0x1000)},
			disk:  200,
			want:  "past the last usable",
		},
		{
			name:  "grow not last",
			parts: []parameter.Partition{{Name: "a", First: 0x40, Grow: true}, part("b", 0x2000, 0x100)},
			disk:  10000,
			want:  "grow partition",
		},
		{
			name:  "zero size",
			parts: []parameter.Partition{part("a", 0x40, 0)},
			disk:  10000,
			want:  "zero size",
		},
		{
			name:  "disk too small",
			parts: []parameter.Partition{part("a", 0x40, 0x100)},
			disk:  50,
			want:  "too small",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Build(c.parts, c.disk)
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestBuildTooManyEntries(t *testing.T) {
	parts := make([]parameter.Partition, EntryCount+1)
	for i := range parts {
		parts[i] = part("p", uint64(0x40+i*4), 1)
	}
	if _, err := Build(parts, 100000); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("error = %v, want entry-count error", err)
	}
}

func TestBuildBelowFirstUsableWarns(t *testing.T) {
	lay, err := Build([]parameter.Partition{part("early", 8, 0x100)}, 10000)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(lay.Warnings) == 0 || !strings.Contains(lay.Warnings[0], "below the first usable") {
		t.Fatalf("warnings = %v", lay.Warnings)
	}
}

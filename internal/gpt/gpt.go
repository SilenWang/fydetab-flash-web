// Package gpt derives the GPT layout a Rockchip loader is expected to create
// from the parameter partition.
//
// The firmware itself does not carry GPT bytes: upgrade_tool writes the
// parameter partition, and the loader on the device turns the mtdparts table
// into a protective MBR plus primary/backup GPT. This package reproduces that
// target layout so `uf --dry-run` can be checked against the device, without
// writing anything.
//
// Standard GPT geometry for a disk of N 512-byte sectors:
//
//	LBA 0            protective MBR
//	LBA 1            primary header
//	LBA 2..33        primary entry array (128 entries * 128 bytes = 16384 B)
//	LBA 34           first usable LBA
//	LBA N-34         last usable LBA
//	LBA N-33..N-2    backup entry array
//	LBA N-1          backup header
//
// The on-device loader may round the last usable LBA or partition ends to a
// coarser boundary; the plan reports the parameter-derived values verbatim and
// notes where that assumption still has to be confirmed against real hardware.
package gpt

import (
	"fmt"
	"sort"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
)

// GPT constants from the UEFI specification.
const (
	// ProtectiveMBRLBA holds the legacy MBR with a single 0xEE partition.
	ProtectiveMBRLBA = 0
	// PrimaryHeaderLBA holds the primary GPT header.
	PrimaryHeaderLBA = 1
	// PrimaryEntriesLBA is where the primary partition entry array starts.
	PrimaryEntriesLBA = 2
	// EntryCount is the fixed number of GPT entries.
	EntryCount = 128
	// EntrySize is sizeof(EFI_PARTITION_ENTRY).
	EntrySize = 128
	// EntriesBytes is the size of one entry array.
	EntriesBytes = EntryCount * EntrySize // 16384
	// EntriesSectors is how many 512-byte sectors one entry array occupies.
	EntriesSectors = EntriesBytes / parameter.SectorSize // 32
	// FirstUsableLBA is the first LBA after the primary entry array.
	FirstUsableLBA = PrimaryEntriesLBA + EntriesSectors // 34
)

// Entry is one planned GPT partition entry.
type Entry struct {
	Index    int      `json:"index"`
	Name     string   `json:"name"`
	Flags    []string `json:"flags,omitempty"`
	FirstLBA uint64   `json:"firstLba"`
	LastLBA  uint64   `json:"lastLba"` // inclusive
	Sectors  uint64   `json:"sectors"`
	Grow     bool     `json:"grow"`
	// Known is false for a grow partition on a disk whose size is unknown.
	Known bool `json:"known"`
}

// Layout is the planned GPT geometry.
type Layout struct {
	// DiskSectors is the flash size in 512-byte sectors.
	DiskSectors uint64 `json:"diskSectors"`
	// DiskKnown is false when a grow partition was present and the caller did
	// not supply a flash size; backup GPT locations and grow ends are then
	// unknown.
	DiskKnown bool `json:"diskKnown"`
	// DiskDerived reports that DiskSectors was inferred from the parameter
	// table instead of being supplied by the caller.
	DiskDerived bool `json:"diskDerived"`

	FirstUsableLBA uint64 `json:"firstUsableLba"`
	LastUsableLBA  uint64 `json:"lastUsableLba"`
	// BackupEntriesLBA and BackupHeaderLBA are zero when DiskKnown is false.
	BackupEntriesLBA uint64 `json:"backupEntriesLba"`
	BackupHeaderLBA  uint64 `json:"backupHeaderLba"`

	Entries  []Entry  `json:"entries"`
	Warnings []string `json:"warnings,omitempty"`
}

// Build lays out parts on a disk. Pass diskSectors=0 to derive the smallest
// disk that fits the parameter table (adds room for the backup GPT); the
// result is then marked DiskDerived.
func Build(parts []parameter.Partition, diskSectors uint64) (*Layout, error) {
	if len(parts) > EntryCount {
		return nil, fmt.Errorf("gpt: %d partitions exceed the %d GPT entry slots", len(parts), EntryCount)
	}

	lay := &Layout{Entries: make([]Entry, 0, len(parts))}
	for i, p := range parts {
		e := Entry{
			Index:    i,
			Name:     p.Name,
			Flags:    p.Flags,
			FirstLBA: p.First,
			Grow:     p.Grow,
			Known:    true,
		}
		if p.Grow {
			e.Sectors = 0
		} else {
			e.Sectors = p.Sectors
			e.LastLBA = p.First + p.Sectors - 1
		}
		lay.Entries = append(lay.Entries, e)
	}

	if diskSectors == 0 {
		lay.DiskDerived = true
		var maxEnd uint64
		for _, e := range lay.Entries {
			if !e.Grow && e.LastLBA+1 > maxEnd {
				maxEnd = e.LastLBA + 1
			}
		}
		if maxEnd > 0 {
			// Leave the standard 34-sector backup GPT region after the last
			// parameter partition.
			diskSectors = maxEnd + FirstUsableLBA
		}
		lay.DiskKnown = !hasGrow(lay.Entries) && diskSectors > 0
	} else {
		lay.DiskKnown = true
	}

	lay.DiskSectors = diskSectors
	if lay.DiskKnown {
		if diskSectors < FirstUsableLBA*2 {
			return nil, fmt.Errorf("gpt: disk of %d sectors is too small for a primary and backup GPT", diskSectors)
		}
		lay.FirstUsableLBA = FirstUsableLBA
		lay.LastUsableLBA = diskSectors - FirstUsableLBA
		lay.BackupEntriesLBA = diskSectors - EntriesSectors - 1
		lay.BackupHeaderLBA = diskSectors - 1
	}

	for i := range lay.Entries {
		e := &lay.Entries[i]
		if e.Grow {
			if lay.DiskKnown {
				e.FirstLBA = maxU64(e.FirstLBA, FirstUsableLBA)
				e.LastLBA = lay.LastUsableLBA
				if e.LastLBA < e.FirstLBA {
					return nil, fmt.Errorf("gpt: grow partition %q starts at LBA %d, past the last usable LBA %d",
						e.Name, e.FirstLBA, lay.LastUsableLBA)
				}
				e.Sectors = e.LastLBA - e.FirstLBA + 1
			} else {
				e.Known = false
			}
			continue
		}
		if e.Sectors == 0 {
			return nil, fmt.Errorf("gpt: partition %q has zero size", e.Name)
		}
		if e.FirstLBA < FirstUsableLBA {
			lay.warnf("partition %q starts at LBA %d, below the first usable LBA %d; the loader may clamp it",
				e.Name, e.FirstLBA, FirstUsableLBA)
		}
		if lay.DiskKnown && e.LastLBA > lay.LastUsableLBA {
			return nil, fmt.Errorf("gpt: partition %q ends at LBA %d, past the last usable LBA %d",
				e.Name, e.LastLBA, lay.LastUsableLBA)
		}
	}

	if err := checkOverlaps(lay); err != nil {
		return nil, err
	}
	return lay, nil
}

func hasGrow(entries []Entry) bool {
	for _, e := range entries {
		if e.Grow {
			return true
		}
	}
	return false
}

func checkOverlaps(lay *Layout) error {
	// A grow partition consumes everything to the end of the disk, so nothing
	// may follow it. Entries are in parameter order, which is also the order
	// the loader consumes them in. Check this before the interval sweep so the
	// error explains the actual mistake instead of a derived overlap.
	for i, e := range lay.Entries {
		if e.Grow && i != len(lay.Entries)-1 {
			return fmt.Errorf("gpt: grow partition %q is followed by %q; the grow end would overlap it",
				e.Name, lay.Entries[i+1].Name)
		}
	}

	known := make([]Entry, 0, len(lay.Entries))
	for _, e := range lay.Entries {
		if e.Known {
			known = append(known, e)
		}
	}
	sort.Slice(known, func(i, j int) bool {
		if known[i].FirstLBA != known[j].FirstLBA {
			return known[i].FirstLBA < known[j].FirstLBA
		}
		return known[i].LastLBA < known[j].LastLBA
	})
	for i := 1; i < len(known); i++ {
		prev, cur := known[i-1], known[i]
		if cur.FirstLBA <= prev.LastLBA {
			return fmt.Errorf("gpt: partitions %q [%d..%d] and %q [%d..%d] overlap",
				prev.Name, prev.FirstLBA, prev.LastLBA, cur.Name, cur.FirstLBA, cur.LastLBA)
		}
	}
	return nil
}

func (lay *Layout) warnf(format string, args ...any) {
	lay.Warnings = append(lay.Warnings, fmt.Sprintf(format, args...))
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

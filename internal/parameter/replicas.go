package parameter

// Placement of the parameter partition on flash.
//
// The parameter table is not a partition from mtdparts: the host writes the
// block itself into a reserved area at the beginning of the disk, and the
// device reads it to build the GUID partition table. The block is replicated
// because a single unreadable copy would leave the device unable to describe
// its own partitions.
//
// Layout of the reserved area (part_size of the parameter entry, in sectors):
//
//	base     = flash_offset + First4AccessOffset   (when the device has the
//	                                                 first-4-access capability)
//	stride   = part_size >> 3                      (one copy per eighth)
//	copies   = ReplicaCount
//
// With the sizes the reference device reports (flash_offset 0, part_size
// 0x4000) that is LBA 0x2000, 0x2800, ... 0x5800. The later copies land inside
// partitions that the same run rewrites, which is harmless: the parameter is
// written before the payloads. A writer must still check for anything it is
// not going to rewrite (see the overlap check in internal/uf).

import (
	"fmt"
)

const (
	// ReplicaCount is how many copies of the parameter block are written.
	ReplicaCount = 8
	// First4AccessOffset is added to the parameter entry's flash offset when
	// the device advertises the first-4-access capability.
	First4AccessOffset uint64 = 0x2000
	// noEntryValue is the "field carries no value" sentinel of the archive's
	// item table.
	noEntryValue uint32 = 0xFFFFFFFF
)

// Replica is one copy of the parameter block.
type Replica struct {
	Index   int    `json:"index"`
	LBA     uint64 `json:"lba"`
	Sectors uint64 `json:"sectors"`
}

// End is the exclusive end LBA of the copy.
func (r Replica) End() uint64 { return r.LBA + r.Sectors }

// ReplicaLayout places copies of a copyBytes-long parameter block. flashOffset
// and partSize are the parameter entry's own fields (the start of its reserved
// area and its length, both in sectors).
//
// It is fail-closed on everything it cannot derive: a reserved area with no
// readable size, or a zero stride, would put copies at the wrong place, and a
// wrong parameter block means a wrong partition table.
func ReplicaLayout(flashOffset, partSize uint32, first4Access bool, copyBytes int64) ([]Replica, error) {
	if copyBytes <= 0 || copyBytes%SectorSize != 0 {
		return nil, fmt.Errorf("parameter: a parameter copy is %d bytes, want a positive multiple of %d", copyBytes, SectorSize)
	}
	if partSize == 0 || partSize == noEntryValue {
		return nil, fmt.Errorf("parameter: the parameter entry has no reserved-area size (part_size 0x%08X)", partSize)
	}
	stride := uint64(partSize) >> 3
	if stride == 0 {
		return nil, fmt.Errorf("parameter: reserved area of %d sectors is too small to place %d copies", partSize, ReplicaCount)
	}
	base := uint64(flashOffset)
	if flashOffset == noEntryValue {
		return nil, fmt.Errorf("parameter: the parameter entry has no flash offset")
	}
	if first4Access {
		base += First4AccessOffset
	}

	sectors := uint64(copyBytes) / SectorSize
	out := make([]Replica, 0, ReplicaCount)
	for i := 0; i < ReplicaCount; i++ {
		lba := base + uint64(i)*stride
		if lba+sectors > uint64(1)<<32 {
			return nil, fmt.Errorf("parameter: copy %d at LBA 0x%x runs past the 32-bit LBA space", i, lba)
		}
		out = append(out, Replica{Index: i, LBA: lba, Sectors: sectors})
	}
	return out, nil
}

// ReplicaBytes is the total transfer size of a replica set.
func ReplicaBytes(rs []Replica) int64 {
	var total int64
	for _, r := range rs {
		total += int64(r.Sectors) * SectorSize
	}
	return total
}

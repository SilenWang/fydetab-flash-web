package gpt

// Parsing the GPT bytes a device holds. The write path compares the read-back
// bytes with what it sent, which is the strictest check; this decoder exists so
// tests (and a log reader) can assert the bytes really are a GPT with the
// expected table instead of only "the same blob came back".

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"unicode/utf16"
)

// Header is one parsed GPT header.
type Header struct {
	// Backup marks the copy that lives at the end of the disk.
	Backup            bool
	Revision          uint32
	HeaderSize        uint32
	CRC               uint32
	MyLBA             uint64
	AlternateLBA      uint64
	FirstUsableLBA    uint64
	LastUsableLBA     uint64
	DiskGUID          [16]byte
	PartitionEntryLBA uint64
	NumEntries        uint32
	EntrySize         uint32
	EntriesCRC        uint32
}

// ParsedEntry is one decoded partition entry.
type ParsedEntry struct {
	Index      int
	TypeGUID   [16]byte
	UniqueGUID [16]byte
	FirstLBA   uint64
	LastLBA    uint64
	Attributes uint64
	Name       string
}

// Sectors is the entry's size in sectors (LastLBA is inclusive).
func (e ParsedEntry) Sectors() uint64 {
	if e.LastLBA < e.FirstLBA {
		return 0
	}
	return e.LastLBA - e.FirstLBA + 1
}

// Bootable reports the "legacy BIOS bootable" attribute.
func (e ParsedEntry) Bootable() bool { return e.Attributes&AttributeBootable != 0 }

// Parsed is a decoded GPT: both headers, the protective MBR flag and the
// entries in table order.
type Parsed struct {
	ProtectiveMBR bool
	Primary       Header
	Backup        *Header
	Entries       []ParsedEntry
}

// Parse decodes the primary region (PrimarySectors sectors) and, when given,
// the backup region (BackupSectors sectors). Every checksum is verified, so a
// caller can treat a successful Parse as "this is a usable GPT".
func Parse(primary, backup []byte) (*Parsed, error) {
	if len(primary) < PrimarySectors*512 {
		return nil, fmt.Errorf("gpt: primary region is %d bytes, want at least %d", len(primary), PrimarySectors*512)
	}
	out := &Parsed{}

	// Protective MBR: boot signature and one 0xEE record.
	mbr := primary[:512]
	if binary.LittleEndian.Uint16(mbr[510:512]) != mbrSignature {
		return nil, fmt.Errorf("gpt: protective MBR has no 0x%04X signature", mbrSignature)
	}
	if mbr[PMBREntryOffset+4] != PMBRTypeGPT {
		return nil, fmt.Errorf("gpt: protective MBR partition type is 0x%02X, want 0x%02X", mbr[PMBREntryOffset+4], PMBRTypeGPT)
	}
	out.ProtectiveMBR = true

	hdr, err := parseHeader(primary[512:1024], false)
	if err != nil {
		return nil, fmt.Errorf("primary header: %w", err)
	}
	out.Primary = hdr

	entriesBytes := primary[2*512:]
	if got := crc32.ChecksumIEEE(entriesBytes[:int(hdr.NumEntries)*int(hdr.EntrySize)]); got != hdr.EntriesCRC {
		return nil, fmt.Errorf("gpt: primary entry array CRC 0x%08x, header says 0x%08x", got, hdr.EntriesCRC)
	}
	for i := 0; i < int(hdr.NumEntries); i++ {
		raw := entriesBytes[i*int(hdr.EntrySize):]
		e, ok, err := parseEntry(i, raw[:hdr.EntrySize])
		if err != nil {
			return nil, err
		}
		if ok {
			out.Entries = append(out.Entries, e)
		}
	}

	if len(backup) > 0 {
		if len(backup) < BackupSectors*512 {
			return nil, fmt.Errorf("gpt: backup region is %d bytes, want at least %d", len(backup), BackupSectors*512)
		}
		bh, err := parseHeader(backup[EntriesSectors*512:], true)
		if err != nil {
			return nil, fmt.Errorf("backup header: %w", err)
		}
		if bh.EntriesCRC != hdr.EntriesCRC {
			return nil, fmt.Errorf("gpt: backup entries CRC 0x%08x, primary 0x%08x", bh.EntriesCRC, hdr.EntriesCRC)
		}
		if !bytes.Equal(backup[:EntriesSectors*512], entriesBytes[:EntriesSectors*512]) {
			return nil, fmt.Errorf("gpt: backup entry array differs from the primary one")
		}
		if bh.EntrySize != hdr.EntrySize || bh.NumEntries != hdr.NumEntries {
			return nil, fmt.Errorf("gpt: backup header describes a different entry array")
		}
		out.Backup = &bh
	}
	return out, nil
}

func parseHeader(b []byte, backup bool) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("gpt: header is %d bytes, want %d", len(b), HeaderSize)
	}
	if binary.LittleEndian.Uint64(b[0:8]) != headerSignature {
		return Header{}, fmt.Errorf("gpt: header signature is not \"EFI PART\"")
	}
	h := Header{
		Backup:            backup,
		Revision:          binary.LittleEndian.Uint32(b[8:12]),
		HeaderSize:        binary.LittleEndian.Uint32(b[12:16]),
		CRC:               binary.LittleEndian.Uint32(b[16:20]),
		MyLBA:             binary.LittleEndian.Uint64(b[24:32]),
		AlternateLBA:      binary.LittleEndian.Uint64(b[32:40]),
		FirstUsableLBA:    binary.LittleEndian.Uint64(b[40:48]),
		LastUsableLBA:     binary.LittleEndian.Uint64(b[48:56]),
		PartitionEntryLBA: binary.LittleEndian.Uint64(b[72:80]),
		NumEntries:        binary.LittleEndian.Uint32(b[80:84]),
		EntrySize:         binary.LittleEndian.Uint32(b[84:88]),
		EntriesCRC:        binary.LittleEndian.Uint32(b[88:92]),
	}
	copy(h.DiskGUID[:], b[56:72])

	if h.Revision != headerRevision {
		return Header{}, fmt.Errorf("gpt: revision 0x%08x, want 0x%08x", h.Revision, uint32(headerRevision))
	}
	if h.HeaderSize != HeaderSize {
		return Header{}, fmt.Errorf("gpt: header size %d, want %d", h.HeaderSize, HeaderSize)
	}
	if h.NumEntries == 0 || h.EntrySize != EntrySize {
		return Header{}, fmt.Errorf("gpt: entry array %d x %d bytes, want %d x %d",
			h.NumEntries, h.EntrySize, EntryCount, EntrySize)
	}
	clone := append([]byte(nil), b[:HeaderSize]...)
	binary.LittleEndian.PutUint32(clone[16:20], 0)
	if got := crc32.ChecksumIEEE(clone); got != h.CRC {
		return Header{}, fmt.Errorf("gpt: header CRC 0x%08x, stored 0x%08x", got, h.CRC)
	}
	return h, nil
}

func parseEntry(index int, b []byte) (ParsedEntry, bool, error) {
	allZero := true
	for _, c := range b {
		if c != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ParsedEntry{}, false, nil
	}
	e := ParsedEntry{Index: index}
	copy(e.TypeGUID[:], b[0:16])
	copy(e.UniqueGUID[:], b[16:32])
	e.FirstLBA = binary.LittleEndian.Uint64(b[32:40])
	e.LastLBA = binary.LittleEndian.Uint64(b[40:48])
	e.Attributes = binary.LittleEndian.Uint64(b[48:56])
	if e.FirstLBA == 0 && e.LastLBA == 0 {
		return ParsedEntry{}, false, fmt.Errorf("gpt: entry %d has an all-zero type GUID but an LBA range", index)
	}
	var units []uint16
	for i := 56; i < 128; i += 2 {
		u := binary.LittleEndian.Uint16(b[i:])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	e.Name = string(utf16.Decode(units))
	return e, true, nil
}

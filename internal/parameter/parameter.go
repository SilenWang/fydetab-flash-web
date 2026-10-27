// Package parameter parses the Rockchip "parameter" partition: the PARM
// wrapper used inside update images and the kernel-style CMDLINE mtdparts
// table that defines the GPT layout.
//
// On-disk wrapper (as produced by Rockchip's packing tools):
//
//	0x00  "PARM"
//	0x04  uint32 little-endian content length
//	0x08  raw parameter text
//	...   uint32 little-endian RKCRC (Rockchip CRC-32) of the text
//
// A bare text file (no PARM magic) is also accepted, because some tooling
// writes the raw parameter.txt.
//
// The text is a list of "KEY: value" lines. The line that matters for flashing
// is CMDLINE, whose mtdparts= part looks like:
//
//	CMDLINE: mtdparts=rk29xxnand:0x00002000@0x00004000(uboot),-@0x0040000(rootfs:grow)
//
// Sizes and offsets are in 512-byte sectors. A size of "-" means "grow to the
// end of the disk"; a ":grow" flag means the same. Other flags (for example
// ":bootable") are preserved verbatim.
package parameter

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/SilenWang/fydetab-flash-web/internal/rkcrc"
)

const (
	// SectorSize is the unit used by mtdparts offsets and sizes.
	SectorSize = 512
	// WrapperHeader is len("PARM") + sizeof(uint32 length).
	WrapperHeader = 8
	// WrapperTrailer is sizeof(uint32 CRC).
	WrapperTrailer = 4
	// ParMagic tags a wrapped parameter.img.
	ParMagic = "PARM"
)

// Partition is one mtdparts entry.
type Partition struct {
	Name    string   `json:"name"`
	Flags   []string `json:"flags,omitempty"`
	First   uint64   `json:"firstLba"` // starting LBA (sector)
	Sectors uint64   `json:"sectors"`  // 0 when Grow
	Grow    bool     `json:"grow"`
}

// End returns the exclusive end LBA. The bool is false for a grow partition
// whose end depends on the device's flash size.
func (p Partition) End() (uint64, bool) {
	if p.Grow {
		return 0, false
	}
	return p.First + p.Sectors, true
}

// Bytes returns the partition size in bytes. The bool is false for grow.
func (p Partition) Bytes() (uint64, bool) {
	if p.Grow {
		return 0, false
	}
	return p.Sectors * SectorSize, true
}

// Document is a parsed parameter partition.
type Document struct {
	// Raw is the parameter text after stripping the PARM wrapper and NUL
	// padding.
	Raw string `json:"raw"`

	Wrapped        bool              `json:"wrapped"`
	DeclaredLength uint32            `json:"declaredLength"`
	CRCAvailable   bool              `json:"crcAvailable"`
	CRCStored      uint32            `json:"crcStored"`
	CRCComputed    uint32            `json:"crcComputed"`
	CRCOK          bool              `json:"crcOk"`
	PartitionCount int               `json:"partitionCount"`
	Fields         map[string]string `json:"fields"`
	Cmdline        string            `json:"cmdline"`
	UUIDs          map[string]string `json:"uuids,omitempty"`
	Partitions     []Partition       `json:"partitions"`
	Warnings       []string          `json:"warnings,omitempty"`
}

// Parse accepts either a PARM-wrapped parameter partition or raw text.
func Parse(data []byte) (*Document, error) {
	doc := &Document{Fields: map[string]string{}, UUIDs: map[string]string{}}

	content := data
	trimmed := trimNUL(data)
	if len(trimmed) >= len(ParMagic) && string(trimmed[:4]) == ParMagic {
		if len(trimmed) < WrapperHeader {
			return nil, fmt.Errorf("parameter: PARM wrapper is truncated (%d bytes, need at least %d)", len(trimmed), WrapperHeader)
		}
		doc.Wrapped = true
		doc.DeclaredLength = binary.LittleEndian.Uint32(trimmed[4:8])
		end := WrapperHeader + uint64(doc.DeclaredLength)
		if end+WrapperTrailer > uint64(len(trimmed)) {
			return nil, fmt.Errorf("parameter: PARM content length %d overruns the %d-byte item (need %d bytes)",
				doc.DeclaredLength, len(trimmed), end+WrapperTrailer)
		}
		content = trimmed[WrapperHeader:end]
		doc.CRCAvailable = true
		doc.CRCStored = binary.LittleEndian.Uint32(trimmed[end : end+WrapperTrailer])
		doc.CRCComputed = rkcrc.Checksum(content)
		doc.CRCOK = doc.CRCStored == doc.CRCComputed
	} else {
		content = trimmed
	}
	doc.Raw = string(content)

	fields, uuids := parseFields(doc.Raw)
	doc.Fields = fields
	doc.UUIDs = uuids
	doc.Cmdline = lookupField(fields, "CMDLINE")
	if doc.Cmdline == "" {
		if i := strings.Index(doc.Raw, "CMDLINE"); i >= 0 {
			doc.Cmdline = strings.TrimSpace(doc.Raw[i+len("CMDLINE"):])
			doc.Cmdline = strings.TrimPrefix(doc.Cmdline, ":")
		}
	}
	if strings.TrimSpace(doc.Cmdline) == "" {
		doc.Warnings = append(doc.Warnings, "parameter has no CMDLINE line; no partition table can be derived")
		return doc, nil
	}

	parts, err := parseMtdParts(doc.Cmdline)
	if err != nil {
		return nil, err
	}
	doc.Partitions = parts
	doc.PartitionCount = len(parts)
	if len(parts) == 0 {
		doc.Warnings = append(doc.Warnings, "CMDLINE has no mtdparts entries; no partition table can be derived")
	}
	for i, p := range parts {
		if p.Grow && i != len(parts)-1 {
			doc.Warnings = append(doc.Warnings, fmt.Sprintf("grow partition %q is not the last entry; its end is ambiguous", p.Name))
		}
	}
	return doc, nil
}

func trimNUL(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// parseFields splits "KEY: value" lines, joining backslash continuations.
func parseFields(text string) (map[string]string, map[string]string) {
	fields := map[string]string{}
	uuids := map[string]string{}
	var joined string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasSuffix(line, "\\") {
			joined += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		line = joined + line
		joined = ""
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" {
			continue
		}
		if _, exists := fields[key]; !exists {
			fields[key] = val
		}
		if strings.EqualFold(key, "uuid") {
			for _, pair := range strings.Split(val, ",") {
				k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
				if ok {
					uuids[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
		}
	}
	return fields, uuids
}

func lookupField(fields map[string]string, name string) string {
	for k, v := range fields {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// parseMtdParts extracts size@offset(name[:flags]) entries from a CMDLINE
// value.
func parseMtdParts(cmdline string) ([]Partition, error) {
	i := strings.Index(cmdline, "mtdparts=")
	if i < 0 {
		return nil, fmt.Errorf("parameter: CMDLINE has no mtdparts= entry")
	}
	rest := cmdline[i+len("mtdparts="):]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return nil, fmt.Errorf("parameter: mtdparts= is missing the device separator ':'")
	}
	list := rest[colon+1:]

	var parts []Partition
	seen := map[string]bool{}
	pos := 0
	for pos < len(list) {
		for pos < len(list) && (list[pos] == ',' || list[pos] == ' ' || list[pos] == '\t') {
			pos++
		}
		if pos >= len(list) || list[pos] == '\n' || list[pos] == '\r' {
			break
		}

		// size
		start := pos
		for pos < len(list) && list[pos] != '@' && list[pos] != '(' && list[pos] != ' ' {
			pos++
		}
		sizeField := strings.TrimSpace(list[start:pos])
		if sizeField == "" {
			return nil, fmt.Errorf("parameter: mtdparts entry %d has an empty size", len(parts)+1)
		}
		p := Partition{}
		if sizeField == "-" {
			p.Grow = true
		} else {
			v, err := parseSectors(sizeField)
			if err != nil {
				return nil, fmt.Errorf("parameter: mtdparts entry %d size %q: %w", len(parts)+1, sizeField, err)
			}
			p.Sectors = v
		}
		if pos >= len(list) || list[pos] != '@' {
			return nil, fmt.Errorf("parameter: mtdparts entry %d (%q) is missing '@offset'", len(parts)+1, sizeField)
		}
		pos++

		// offset
		start = pos
		for pos < len(list) && list[pos] != '(' && list[pos] != ' ' {
			pos++
		}
		offField := strings.TrimSpace(list[start:pos])
		if offField == "" {
			return nil, fmt.Errorf("parameter: mtdparts entry %d has an empty offset", len(parts)+1)
		}
		off, err := parseSectors(offField)
		if err != nil {
			return nil, fmt.Errorf("parameter: mtdparts entry %d offset %q: %w", len(parts)+1, offField, err)
		}
		p.First = off

		if pos >= len(list) || list[pos] != '(' {
			return nil, fmt.Errorf("parameter: mtdparts entry %d (%q@%q) is missing a (name)", len(parts)+1, sizeField, offField)
		}
		close := strings.IndexByte(list[pos:], ')')
		if close < 0 {
			return nil, fmt.Errorf("parameter: mtdparts entry %d has an unterminated (name)", len(parts)+1)
		}
		nameField := list[pos+1 : pos+close]
		pos += close + 1

		name, flags, _ := strings.Cut(nameField, ":")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("parameter: mtdparts entry %d has an empty partition name", len(parts)+1)
		}
		if seen[name] {
			return nil, fmt.Errorf("parameter: duplicate partition name %q in mtdparts", name)
		}
		seen[name] = true
		p.Name = name
		if flags != "" {
			for _, f := range strings.Split(flags, ":") {
				f = strings.TrimSpace(f)
				if f == "" {
					continue
				}
				p.Flags = append(p.Flags, f)
				if strings.EqualFold(f, "grow") {
					p.Grow = true
				}
			}
		}
		if p.Sectors == 0 && !p.Grow {
			return nil, fmt.Errorf("parameter: partition %q has zero size", name)
		}
		parts = append(parts, p)
	}
	return parts, nil
}

func parseSectors(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	base := 10
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		base = 16
		s = s[2:]
	}
	if s == "" {
		return 0, fmt.Errorf("empty number after 0x")
	}
	v, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		return 0, fmt.Errorf("not a sector count: %w", err)
	}
	return v, nil
}

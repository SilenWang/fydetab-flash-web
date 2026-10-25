// Package rkfw parses Rockchip update images: the RKFW container, its embedded
// RKAF archive and the RKAF item table.
//
// Wire layouts follow Rockchip's GPL-2.0 rkdeveloptool (RKImage.h, RKImage.cpp)
// and the two independent reimplementations cited in docs/rkfw-rkaf-format.md.
// Every field is little-endian and the structs are byte-packed.
//
// RKFW (a complete update.img):
//
//	0x00  RKFW header (0x66 bytes)
//	      loader_offset .. +loader_length   bootloader blob (MiniLoaderAll.bin)
//	      image_offset  .. +image_length    embedded RKAF archive
//	      ... 32-byte ASCII MD5 of every byte before it (at image end, or at
//	          EOF-32 for images without a signature trailer)
//
// RKAF (also a standalone update.img):
//
//	0x000  header (2048 bytes) with item table at 0x8C, 16 * 112 bytes
//	pos..  item payloads, each padded to a 512-byte boundary
//	end-4  RKCRC (Rockchip CRC-32) over everything before it
//
// The package is strictly read-only: there is no writer and no device access.
package rkfw

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Sizes and sentinels of the on-disk formats.
const (
	// HeaderSize is sizeof(STRUCT_RKIMAGE_HEAD): 0x66 bytes.
	HeaderSize = 0x66
	// RKAFHeaderSize covers the RKAF header and its 16-entry item table.
	RKAFHeaderSize = 2048
	// RKAFItemSize is sizeof(STRUCT_RKAF_ITEM).
	RKAFItemSize = 112
	// MaxItems is the number of RKAF item slots inside a 2048-byte header.
	MaxItems = 16
	// SectorSize is the Rockchip flash sector size used by the formats.
	SectorSize = 512
	// NoAddress marks an item field that carries no value (device-generated
	// items such as the backup partition use it for pos/size and for items
	// that are not in the parameter mtdparts list).
	NoAddress uint32 = 0xFFFFFFFF
)

// Image magics.
var (
	MagicRKFW = [4]byte{'R', 'K', 'F', 'W'}
	MagicRKAF = [4]byte{'R', 'K', 'A', 'F'}
)

// Header is the 0x66-byte RKFW header.
type Header struct {
	Tag          [4]byte // "RKFW"
	HeadLen      uint16  // sizeof(header), usually 0x66
	Version      uint32
	Code         uint32
	Year         uint16
	Month        uint8
	Day          uint8
	Hour         uint8
	Minute       uint8
	Second       uint8
	Chip         uint32 // 4 ASCII digits (reversed) or a legacy family code
	LoaderOffset uint32
	LoaderLength uint32
	ImageOffset  uint32
	ImageLength  uint32
	Unknown1     uint32
	Unknown2     uint32
	SystemFSType uint32
	BackupEndPos uint32
	Reserved     [0x2D]byte
}

// VersionString renders the header version the way Rockchip tools do:
// major from the most significant byte, patch from the low 16 bits.
func (h Header) VersionString() string {
	return fmt.Sprintf("%d.%d.%d", byte(h.Version>>24), byte(h.Version>>16), uint16(h.Version))
}

// ReleaseTime returns the build timestamp. The bool is false when the header
// does not hold a valid calendar time (some images leave it zeroed).
func (h Header) ReleaseTime() (time.Time, bool) {
	t := time.Date(int(h.Year), time.Month(h.Month), int(h.Day), int(h.Hour), int(h.Minute), int(h.Second), 0, time.UTC)
	if h.Year == 0 || t.Year() != int(h.Year) || int(t.Month()) != int(h.Month) || t.Day() != int(h.Day) {
		return time.Time{}, false
	}
	return t, true
}

// legacyChips maps the one-byte family code used by pre-RK356x headers.
var legacyChips = map[byte]string{
	0x19: "RV1109/RV1126",
	0x30: "PX30/RK3326",
	0x32: "RK3562",
	0x33: "RK3399/RK3399Pro",
	0x35: "RK3588/RK3588S",
	0x36: "RK3326",
	0x38: "RK3566/RK3568",
	0x39: "RK3528",
	0x41: "RK3368",
	0x48: "RK3308",
	0x50: "RK29xx",
	0x51: "RV1108",
	0x60: "RK30xx/RK3066",
	0x70: "RK31xx/RK3188",
	0x80: "RK32xx/RK3288",
}

// ChipName decodes the chip field. Newer images store the model number as four
// ASCII digits in reverse order ("8853" for RK3588); older ones store a family
// code in the first byte.
func (h Header) ChipName() string {
	b := [4]byte{byte(h.Chip), byte(h.Chip >> 8), byte(h.Chip >> 16), byte(h.Chip >> 24)}
	if isDigits(b[:]) {
		return "RK" + string([]byte{b[3], b[2], b[1], b[0]})
	}
	if name, ok := legacyChips[b[0]]; ok {
		return name
	}
	return fmt.Sprintf("unknown(0x%08x)", h.Chip)
}

func isDigits(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for _, b := range p {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// Archive describes the RKAF header. Length is the on-disk 32-bit field and
// excludes the 4-byte CRC trailer; the true body length, recovered for
// archives larger than 4 GiB, lives on Image.
type Archive struct {
	Magic        [4]byte // "RKAF"
	Length       uint32
	Model        string
	ID           string
	Manufacturer string
	Unknown1     uint32
	Version      uint32
	NumParts     uint32
	Reserved     []byte
}

// Item is one RKAF partition entry.
type Item struct {
	Name       string // parameter partition name, e.g. "parameter", "boot"
	Filename   string // path inside the packaging tree, e.g. "Image/boot.img"
	NandSize   uint32 // sectors; NoAddress for a grow/unknown partition
	Pos        uint32 // offset of the payload inside the RKAF archive
	NandAddr   uint32 // target flash offset in sectors; NoAddress if unknown
	PaddedSize uint32 // bytes occupied in the archive (size rounded to 512)
	Size       uint32 // payload bytes
}

// Reserved reports whether the entry carries no payload. Device-generated
// items (the backup partition is the common one) store NoAddress in pos/size.
func (it Item) Reserved() bool {
	return it.Pos == NoAddress || it.Size == NoAddress
}

// HasData reports whether the entry has readable payload bytes in the archive.
func (it Item) HasData() bool { return !it.Reserved() && it.Size > 0 }

// End returns Pos+PaddedSize as a 64-bit value; a u32 end would wrap for
// payloads that cross the 4 GiB boundary.
func (it Item) End() uint64 { return uint64(it.Pos) + uint64(it.PaddedSize) }

// Image is a parsed Rockchip update image. All fields are read-only metadata;
// use ItemReader or ReadItem to stream payload bytes.
type Image struct {
	Path string
	Size int64

	HasRKFW      bool
	RKFW         Header
	LoaderOffset int64
	LoaderLength int64

	// RKAFOffset is the absolute offset of the RKAF archive.
	RKAFOffset int64
	// RKAFLength is the archive length including the 4-byte CRC trailer.
	RKAFLength int64
	// RKAFLengthField is the 32-bit RKAF body length as stored.
	RKAFLengthField uint32
	// LengthWrapped reports that the stored 32-bit length had to be extended
	// by whole 4 GiB steps to match the bytes actually present.
	LengthWrapped bool

	Archive Archive
	Items   []Item

	// Parameter is the item named "parameter", if present.
	Parameter *Item

	Warnings []string
}

// Open parses path without reading payload bytes. Structural problems (bad
// magic, truncated file, item data outside the archive, more than 16 items)
// are errors; softer anomalies are collected in Image.Warnings.
func Open(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("rkfw: open %s: %w", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("rkfw: stat %s: %w", path, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("rkfw: %s is a directory", path)
	}
	size := st.Size()
	if size < int64(RKAFHeaderSize) {
		return nil, fmt.Errorf("rkfw: %s is only %d bytes; an RKAF header alone needs %d", path, size, RKAFHeaderSize)
	}

	var magic [4]byte
	if err := readAtFull(f, magic[:], 0); err != nil {
		return nil, fmt.Errorf("rkfw: read magic: %w", err)
	}

	img := &Image{Path: path, Size: size}
	rkafOffset := int64(0)

	switch magic {
	case MagicRKFW:
		img.HasRKFW = true
		head := make([]byte, HeaderSize)
		if err := readAtFull(f, head, 0); err != nil {
			return nil, fmt.Errorf("rkfw: read RKFW header: %w", err)
		}
		img.RKFW = parseHeader(head)
		if img.RKFW.HeadLen != HeaderSize {
			img.warnf("RKFW header length field is 0x%x, expected 0x%x; parsed the standard %d-byte header",
				img.RKFW.HeadLen, HeaderSize, HeaderSize)
		}
		img.LoaderOffset = int64(img.RKFW.LoaderOffset)
		img.LoaderLength = int64(img.RKFW.LoaderLength)
		if err := checkExtent("RKFW loader", img.LoaderOffset, img.LoaderLength, size); err != nil {
			return nil, err
		}
		if img.LoaderLength == 0 {
			img.warnf("RKFW loader is empty; Download Boot would have no payload")
		}
		rkafOffset = int64(img.RKFW.ImageOffset)
		if rkafOffset < int64(HeaderSize) || rkafOffset > size-int64(RKAFHeaderSize) {
			return nil, fmt.Errorf("rkfw: RKFW image offset 0x%x does not leave room for an RKAF header in the %d-byte file",
				rkafOffset, size)
		}
	case MagicRKAF:
		// Bare update.img: no RKFW wrapper, no loader, no MD5 tail.
	default:
		return nil, fmt.Errorf("rkfw: %s: unknown image magic %q (want %q or %q)",
			path, string(magic[:]), string(MagicRKFW[:]), string(MagicRKAF[:]))
	}

	raw := make([]byte, RKAFHeaderSize)
	if err := readAtFull(f, raw, rkafOffset); err != nil {
		return nil, fmt.Errorf("rkfw: read RKAF header at 0x%x: %w", rkafOffset, err)
	}
	arch, items, err := parseArchive(raw)
	if err != nil {
		return nil, err
	}

	// Bytes available for the RKAF body (header + payloads), excluding the
	// mandatory 4-byte CRC trailer.
	available := size - rkafOffset - 4
	if available < 0 {
		return nil, fmt.Errorf("rkfw: file ends before the RKAF CRC trailer at 0x%x", rkafOffset)
	}
	bodyLen := recoverTrueLength(uint64(arch.Length), uint64(available))
	if bodyLen == 0 && img.HasRKFW && img.RKFW.ImageLength >= 4 {
		bodyLen = recoverTrueLength(uint64(img.RKFW.ImageLength-4), uint64(available))
		img.warnf("RKAF header length is zero; using the RKFW image_length field instead")
	}
	if bodyLen == 0 {
		return nil, fmt.Errorf("rkfw: RKAF archive at 0x%x has a zero length field", rkafOffset)
	}
	if bodyLen > uint64(available) {
		return nil, fmt.Errorf("rkfw: RKAF body needs 0x%x bytes but only 0x%x are present after offset 0x%x (truncated image)",
			bodyLen, uint64(available), rkafOffset)
	}
	if bodyLen != uint64(arch.Length) {
		img.LengthWrapped = true
		img.warnf("RKAF 32-bit length field holds 0x%x; recovered the true body length 0x%x from the file layout (>4 GiB wrap)",
			arch.Length, bodyLen)
	}
	if img.HasRKFW && img.RKFW.ImageLength != 0 {
		if got, want := img.RKFW.ImageLength, uint32(bodyLen+4); got != want {
			img.warnf("RKFW image_length 0x%x disagrees with the RKAF-derived length 0x%x (0x%x incl. trailer); using the RKAF length",
				got, bodyLen, bodyLen+4)
		}
	}

	if err := validateItems(items, bodyLen, img); err != nil {
		return nil, err
	}

	img.Archive = arch
	img.RKAFOffset = rkafOffset
	img.RKAFLength = int64(bodyLen) + 4
	img.RKAFLengthField = arch.Length
	img.Items = items
	for i := range img.Items {
		if strings.EqualFold(img.Items[i].Name, "parameter") {
			if img.Parameter != nil {
				img.warnf("more than one RKAF item is named %q; using the first", "parameter")
				break
			}
			img.Parameter = &img.Items[i]
		}
	}
	return img, nil
}

func parseHeader(b []byte) Header {
	le := binary.LittleEndian
	var h Header
	copy(h.Tag[:], b[0:4])
	h.HeadLen = le.Uint16(b[4:6])
	h.Version = le.Uint32(b[6:10])
	h.Code = le.Uint32(b[10:14])
	h.Year = le.Uint16(b[14:16])
	h.Month = b[16]
	h.Day = b[17]
	h.Hour = b[18]
	h.Minute = b[19]
	h.Second = b[20]
	h.Chip = le.Uint32(b[21:25])
	h.LoaderOffset = le.Uint32(b[25:29])
	h.LoaderLength = le.Uint32(b[29:33])
	h.ImageOffset = le.Uint32(b[33:37])
	h.ImageLength = le.Uint32(b[37:41])
	h.Unknown1 = le.Uint32(b[41:45])
	h.Unknown2 = le.Uint32(b[45:49])
	h.SystemFSType = le.Uint32(b[49:53])
	h.BackupEndPos = le.Uint32(b[53:57])
	copy(h.Reserved[:], b[57:HeaderSize])
	return h
}

func parseArchive(b []byte) (Archive, []Item, error) {
	le := binary.LittleEndian
	var a Archive
	copy(a.Magic[:], b[0:4])
	if a.Magic != MagicRKAF {
		return a, nil, fmt.Errorf("rkfw: RKAF magic not found (got %q)", string(a.Magic[:]))
	}
	a.Length = le.Uint32(b[4:8])
	a.Model = cstring(b[8:42])
	a.ID = cstring(b[42:72])
	a.Manufacturer = cstring(b[72:128])
	a.Unknown1 = le.Uint32(b[128:132])
	a.Version = le.Uint32(b[132:136])
	a.NumParts = le.Uint32(b[136:140])
	a.Reserved = append([]byte(nil), b[1932:RKAFHeaderSize]...)
	if a.NumParts > MaxItems {
		return a, nil, fmt.Errorf("rkfw: RKAF header claims %d items, but a 2048-byte header only has room for %d", a.NumParts, MaxItems)
	}

	items := make([]Item, a.NumParts)
	for i := range items {
		off := 140 + i*RKAFItemSize
		p := b[off : off+RKAFItemSize]
		items[i] = Item{
			Name:       cstring(p[0:32]),
			Filename:   cstring(p[32:92]),
			NandSize:   le.Uint32(p[92:96]),
			Pos:        le.Uint32(p[96:100]),
			NandAddr:   le.Uint32(p[100:104]),
			PaddedSize: le.Uint32(p[104:108]),
			Size:       le.Uint32(p[108:112]),
		}
	}
	return a, items, nil
}

func validateItems(items []Item, bodyLen uint64, img *Image) error {
	type span struct {
		start, end uint64
		name       string
	}
	var spans []span

	for i := range items {
		it := &items[i]
		if it.Reserved() {
			if it.Pos != NoAddress || it.Size != NoAddress {
				img.warnf("item %q is only partially marked as reserved (pos=0x%x size=0x%x); treating it as device-generated",
					it.Name, it.Pos, it.Size)
			}
			continue
		}
		if it.Size == 0 {
			img.warnf("item %q has zero payload size; it will not be written", it.Name)
			continue
		}
		if it.PaddedSize < it.Size {
			return fmt.Errorf("rkfw: item %q padded size 0x%x is smaller than its payload size 0x%x",
				it.Name, it.PaddedSize, it.Size)
		}
		if uint64(it.Pos) < RKAFHeaderSize {
			return fmt.Errorf("rkfw: item %q payload starts at 0x%x, inside the %d-byte RKAF header",
				it.Name, it.Pos, RKAFHeaderSize)
		}
		if end := it.End(); end > bodyLen {
			return fmt.Errorf("rkfw: item %q payload range [0x%x,0x%x) exceeds the RKAF body end 0x%x (truncated file or corrupt item table)",
				it.Name, it.Pos, end, bodyLen)
		}
		if it.PaddedSize%SectorSize != 0 {
			img.warnf("item %q padded size 0x%x is not a multiple of %d bytes", it.Name, it.PaddedSize, SectorSize)
		}
		if it.Pos%SectorSize != 0 {
			img.warnf("item %q payload starts at 0x%x, which is not %d-byte aligned", it.Name, it.Pos, SectorSize)
		}
		if end := it.End(); end > 1<<32 {
			img.warnf("item %q ends at 0x%x, past the 4 GiB boundary; 32-bit RKAF offsets cannot describe payloads that large",
				it.Name, end)
		}
		spans = append(spans, span{uint64(it.Pos), it.End(), it.Name})
	}

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end < spans[j].end
	})
	for i := 1; i < len(spans); i++ {
		prev, cur := spans[i-1], spans[i]
		if cur.start >= prev.end {
			continue
		}
		if cur.start == prev.start && cur.end == prev.end {
			// Several header entries may intentionally point at the same
			// payload (e.g. an alias partition); that is not a conflict.
			continue
		}
		img.warnf("item payloads %q [0x%x,0x%x) and %q [0x%x,0x%x) overlap",
			prev.name, prev.start, prev.end, cur.name, cur.start, cur.end)
	}
	return nil
}

// recoverTrueLength reconstructs a length that may exceed 4 GiB from a 32-bit
// on-disk field. available is an upper bound taken from the actual file layout;
// the true length is the largest value congruent to stored modulo 2^32 that
// still fits.
func recoverTrueLength(stored, available uint64) uint64 {
	if available <= stored {
		return stored
	}
	return stored + ((available - stored) >> 32 << 32)
}

// ItemOffset is the absolute file offset of an item's payload.
func (img *Image) ItemOffset(it Item) int64 {
	return img.RKAFOffset + int64(it.Pos)
}

// ItemReader returns a streaming reader over an item's payload.
func (img *Image) ItemReader(it Item) (*io.SectionReader, func() error, error) {
	if !it.HasData() {
		return nil, nil, fmt.Errorf("rkfw: item %q has no payload in the archive", it.Name)
	}
	f, err := os.Open(img.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("rkfw: open %s: %w", img.Path, err)
	}
	r := io.NewSectionReader(f, img.ItemOffset(it), int64(it.Size))
	return r, f.Close, nil
}

// ReadItem reads an item payload into memory. maxBytes guards against
// accidentally loading a multi-GiB partition; pass 0 for no limit.
func (img *Image) ReadItem(it Item, maxBytes int64) ([]byte, error) {
	if !it.HasData() {
		return nil, fmt.Errorf("rkfw: item %q has no payload in the archive", it.Name)
	}
	if maxBytes > 0 && int64(it.Size) > maxBytes {
		return nil, fmt.Errorf("rkfw: item %q is %d bytes, above the %d-byte limit", it.Name, it.Size, maxBytes)
	}
	f, err := os.Open(img.Path)
	if err != nil {
		return nil, fmt.Errorf("rkfw: open %s: %w", img.Path, err)
	}
	defer f.Close()
	buf := make([]byte, it.Size)
	if err := readAtFull(f, buf, img.ItemOffset(it)); err != nil {
		return nil, fmt.Errorf("rkfw: read item %q: %w", it.Name, err)
	}
	return buf, nil
}

func (img *Image) warnf(format string, args ...any) {
	img.Warnings = append(img.Warnings, fmt.Sprintf(format, args...))
}

// ErrNoParameter is returned by helpers that need the parameter item.
var ErrNoParameter = errors.New("image has no RKAF item named \"parameter\"")

func cstring(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimRight(string(b), " \x00")
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func readAtFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(p) {
		return fmt.Errorf("short read: got %d of %d bytes at offset 0x%x", n, len(p), off)
	}
	return nil
}

func checkExtent(what string, off, length, size int64) error {
	if off < 0 || length < 0 {
		return fmt.Errorf("rkfw: %s has a negative offset or length", what)
	}
	if off > size || length > size-off {
		return fmt.Errorf("rkfw: %s range [0x%x,0x%x) is outside the %d-byte file",
			what, off, uint64(off)+uint64(length), size)
	}
	return nil
}

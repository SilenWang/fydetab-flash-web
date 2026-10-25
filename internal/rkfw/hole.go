package rkfw

// The 4 GiB hole inside a packed RKAF archive.
//
// Both real images this parser was checked against (openFyde r132 and FyeOS
// v23.0-SP1) are 8.3 GB while their RKAF archive claims only ~4.06 GB. The
// missing bytes are not trailing padding: the packer reserved the STATE
// partition's full slot (5 GiB) in the file even though the payload is 1 GiB,
// and every item after STATE is stored 4 GiB further along than its 32-bit
// `pos` says. Reading at `pos` returns zeros, and a write made from those bytes
// is silently wrong — that is what left the Fydetab Duo unbootable (SIL-515).
//
// The hole is derived from structure, never from a constant:
//
//	packed_end   = file size - trailing MD5 (32 B, when present)
//	declared_end = rkaf offset + rkaf body length + 4-byte CRC trailer
//	hole         = packed_end - declared_end
//
// The start of the hole is then decided per item by comparing what the payload
// looks like at `pos` and at `pos+hole`: an item whose leading bytes carry the
// expected magic (CHROMEOS, FDT, FAT, RSCE, ...) on exactly one side settles
// which side the item lives on. When no signature is available at all (a
// sparse test fixture, for instance) the parser stays conservative: the hole
// size is reported, no item is shifted, and the dry-run's signature check is
// what refuses the write.

import (
	"fmt"
	"io"
	"strings"
)

// Signature is what an item's leading bytes look like. It is a measurement,
// not an expectation; imageplan compares it with ExpectedSignature.
type Signature struct {
	// Name is the short label: CHROMEOS, FDT, FAT, RSCE, ext4, gzip, ...
	// "zero", "blank" and "" describe payloads with no recognisable magic.
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	// Known is true when Name is a magic this package recognises.
	Known bool `json:"known"`
	// Empty is true when the sampled bytes are all 0x00 or all 0xFF.
	Empty bool `json:"empty,omitempty"`
}

// String renders the signature the way the dry-run table prints it.
func (s Signature) String() string {
	if s.Name == "" {
		return "unknown"
	}
	return s.Name
}

// signatureSampleSize is how many leading bytes are sampled. 2048 covers the
// FAT boot sector's 512 bytes and the ext4 superblock magic at 0x438.
const signatureSampleSize = 2048

// ExpectedSignature maps an RKAF item name to the magic its payload must
// start with on real Rockchip images. An empty result means no expectation is
// recorded for that item, so the dry-run only reports what it saw.
//
// Only magics that have been observed on the hardware/image pair are listed;
// guessing an expectation would turn a valid image into a refused write.
func ExpectedSignature(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))
	switch {
	case strings.HasPrefix(n, "KERN"), n == "BOOT", n == "KERNEL":
		// ChromeOS kernel partition: begins with the ASCII magic CHROMEOS.
		return "CHROMEOS"
	case n == "UBOOT":
		// Rockchip's uboot.img is a FIT image (a flattened device tree).
		return "FDT"
	case n == "EFI-SYSTEM", n == "EFI":
		return "FAT"
	case n == "RESOURCE":
		return "RSCE"
	}
	return ""
}

// ClassifySignature labels the leading bytes of a payload.
func ClassifySignature(b []byte) Signature {
	if len(b) == 0 {
		return Signature{Name: "empty", Empty: true}
	}
	if allByte(b, 0x00) {
		return Signature{Name: "zero", Empty: true}
	}
	if allByte(b, 0xFF) {
		return Signature{Name: "blank", Empty: true}
	}

	if len(b) >= 8 && string(b[0:8]) == "CHROMEOS" {
		return Signature{Name: "CHROMEOS", Detail: "ChromeOS kernel", Known: true}
	}
	if len(b) >= 4 {
		switch {
		case b[0] == 0xD0 && b[1] == 0x0D && b[2] == 0xFE && b[3] == 0xED:
			return Signature{Name: "FDT", Detail: "flattened device tree", Known: true}
		case string(b[0:4]) == "RSCE":
			return Signature{Name: "RSCE", Detail: "Rockchip resource", Known: true}
		case string(b[0:4]) == "PARM":
			return Signature{Name: "PARM", Detail: "parameter wrapper", Known: true}
		case b[0] == 0x1F && b[1] == 0x8B:
			return Signature{Name: "gzip", Known: true}
		case b[0] == 0x3A && b[1] == 0xFF && b[2] == 0x26 && b[3] == 0xED:
			return Signature{Name: "sparse", Detail: "Android sparse image", Known: true}
		case string(b[0:4]) == "CrAU":
			return Signature{Name: "CrAU", Detail: "ChromeOS update payload", Known: true}
		case string(b[0:4]) == "ANDROID!":
			return Signature{Name: "ANDROID!", Detail: "Android boot image", Known: true}
		case string(b[0:4]) == "RKFW":
			return Signature{Name: "RKFW", Known: true}
		case string(b[0:4]) == "RKAF":
			return Signature{Name: "RKAF", Known: true}
		}
	}
	if fatMagic(b) {
		return Signature{Name: "FAT", Detail: "FAT filesystem boot sector", Known: true}
	}
	if len(b) >= 0x43A && b[0x438] == 0x53 && b[0x439] == 0xEF {
		return Signature{Name: "ext4", Detail: "ext2/3/4 superblock", Known: true}
	}
	return Signature{Name: "unknown"}
}

// fatMagic recognises a FAT12/16/32 boot sector: the filesystem type string at
// 0x36 or 0x52, or the 0x55AA boot signature when the type string is blank
// (mkfs.fat leaves FAT32's string empty in some versions).
func fatMagic(b []byte) bool {
	if len(b) >= 0x3E {
		switch string(b[0x36:0x3B]) {
		case "FAT12", "FAT16":
			return true
		}
	}
	if len(b) >= 0x5A && string(b[0x52:0x5A]) == "FAT32   " {
		return true
	}
	if len(b) >= 512 && b[0x1FE] == 0x55 && b[0x1FF] == 0xAA {
		// The 0x55AA signature alone is weak (MBRs have it too), so it only
		// counts when a DOS-era jump instruction opens the sector.
		return len(b) >= 3 && (b[0] == 0xEB || b[0] == 0xE9)
	}
	return false
}

func allByte(b []byte, v byte) bool {
	for _, c := range b {
		if c != v {
			return false
		}
	}
	return true
}

// signatureAt samples the leading bytes of a payload at an absolute file
// offset. A read that runs past the file or fails yields an empty signature.
func signatureAt(r io.ReaderAt, off, size int64) Signature {
	if off < 0 || size <= 0 {
		return Signature{Name: "empty", Empty: true}
	}
	n := int64(signatureSampleSize)
	if size < n {
		n = size
	}
	buf := make([]byte, n)
	if err := readAtFull(r, buf, off); err != nil {
		return Signature{Name: "unreadable", Detail: err.Error()}
	}
	return ClassifySignature(buf)
}

// ItemSignature samples an item at the offset the parser resolved for it.
func (img *Image) ItemSignature(it Item) Signature {
	return signatureAt(img.ReaderAt(), img.ItemOffset(it), int64(it.Size))
}

// holeCandidate is one item's evidence for the split point.
type holeCandidate struct {
	pos      int64
	expected string
	atPos    Signature
	atHole   Signature
}

// chooseHoleStart decides where the hole begins in the logical archive.
//
// decided is false when the payloads carry no usable signature (a sparse
// fixture, or an archive whose payloads are all zero); the caller then keeps
// every item at its table pos and says so in a warning.
func chooseHoleStart(img *Image, items []Item, hole, declaredEnd int64) (start int64, decided bool) {
	var cands []holeCandidate
	for _, it := range items {
		if !it.HasData() {
			continue
		}
		pos := int64(it.Pos)
		un := signatureAt(img.ReaderAt(), img.RKAFOffset+pos, int64(it.Size))
		sh := Signature{Name: "unreadable"}
		if off := img.RKAFOffset + pos + hole; off+int64(it.Size) <= img.Size {
			sh = signatureAt(img.ReaderAt(), off, int64(it.Size))
		}
		cands = append(cands, holeCandidate{
			pos:      pos,
			expected: ExpectedSignature(it.Name),
			atPos:    un,
			atHole:   sh,
		})
	}
	if len(cands) == 0 {
		return declaredEnd, false
	}

	// Strong evidence first: an item that carries its expected magic on one
	// side and not the other votes for that side.
	shiftedMin, haveShifted := int64(0), false
	unshiftedMax, haveUnshifted := int64(-1), false
	for _, c := range cands {
		if c.expected == "" {
			continue
		}
		switch {
		case signatureMatches(c.atPos, c.expected) && !signatureMatches(c.atHole, c.expected):
			if c.pos > unshiftedMax {
				unshiftedMax = c.pos
			}
			haveUnshifted = true
		case signatureMatches(c.atHole, c.expected) && !signatureMatches(c.atPos, c.expected):
			if !haveShifted || c.pos < shiftedMin {
				shiftedMin = c.pos
			}
			haveShifted = true
		}
	}
	if haveShifted && (!haveUnshifted || shiftedMin > unshiftedMax) {
		return shiftedMin, true
	}
	if !haveShifted && haveUnshifted {
		// Every item with an expectation read correctly in place: the extra
		// bytes sit after the last payload.
		return declaredEnd, true
	}
	// No item carries its expected magic on either side. That is the sparse
	// fixture case (payload bytes not captured) and also a genuinely corrupt
	// image; both are undecidable here, so do not guess. The caller keeps the
	// table pos and the dry-run's signature check is what refuses the write.
	return declaredEnd, false
}

// signatureScore weights how much a signature looks like real content.
func signatureScore(s Signature) int {
	switch {
	case s.Empty:
		return 0
	case s.Known:
		return 2
	case s.Name == "" || s.Name == "unknown" || s.Name == "unreadable":
		return 0
	default:
		return 1
	}
}

func signatureMatches(s Signature, want string) bool {
	return s.Known && strings.EqualFold(s.Name, want)
}

// asciiHexAt reports whether 32 bytes at off are ASCII hex, the shape of the
// RKFW trailing MD5. It is how the parser tells a real archive end from the
// inside of a developer-inserted hole.
func asciiHexAt(r io.ReaderAt, off int64, size int64) bool {
	if off < 0 || off+32 > size {
		return false
	}
	var tail [32]byte
	if err := readAtFull(r, tail[:], off); err != nil {
		return false
	}
	for _, c := range tail {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') && !(c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// detectDataHole derives the hole from the packed extent and locates its start
// with payload signatures. declaredEnd is where the 32-bit fields say the
// archive (including its CRC trailer) ends.
//
// decided reports whether a payload signature settled where the hole starts,
// so the caller can phrase its warning honestly instead of implying the item
// offsets were verified.
func detectDataHole(img *Image, rkafOffset, declaredEnd int64, items []Item) (hole, start int64, decided bool) {
	if declaredEnd <= 0 || declaredEnd >= img.Size {
		return 0, 0, false
	}
	// An ASCII MD5 right at the declared end means the archive really does
	// end there; anything after it is a signature blob, not archive content.
	if asciiHexAt(img.ReaderAt(), declaredEnd, img.Size) {
		return 0, 0, false
	}
	// The archive's own MD5 sits at EOF-32 on both real images. Only when
	// that is true can the bytes between the declared end and the digest be
	// read as archive content.
	packedEnd := img.Size
	if asciiHexAt(img.ReaderAt(), img.Size-32, img.Size) {
		packedEnd = img.Size - 32
	}
	hole = packedEnd - declaredEnd
	if hole <= 0 {
		return 0, 0, false
	}
	if hole%SectorSize != 0 {
		img.warnf("found %d unexpected bytes between the RKAF archive end and the trailing MD5; not a sector-aligned hole, so item offsets are left as the table states", hole)
		return 0, 0, false
	}
	start, decided = chooseHoleStart(img, items, hole, declaredEnd)
	if !decided {
		img.warnf("a %s (%d-byte) hole sits between the RKAF archive end 0x%x and the trailing MD5 at 0x%x, but no payload signature settles where it starts; items are read at their table pos and must be confirmed with `uf --dry-run` before writing",
			humanGiB(hole), hole, declaredEnd, packedEnd)
		return hole, declaredEnd, false
	}
	return hole, start, true
}

func humanGiB(n int64) string {
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/float64(int64(1)<<30))
}

// DataOffset returns the absolute file offset of an item's payload, taking the
// archive hole into account.
func (img *Image) DataOffset(it Item) int64 {
	off := img.RKAFOffset + int64(it.Pos)
	if img.DataHole > 0 && int64(it.Pos) >= img.DataHoleStart {
		off += img.DataHole
	}
	return off
}

// shifted reports whether an item's bytes live after the hole.
func (img *Image) shifted(it Item) bool {
	return img.DataHole > 0 && int64(it.Pos) >= img.DataHoleStart
}

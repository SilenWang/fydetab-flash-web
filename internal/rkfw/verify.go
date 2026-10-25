package rkfw

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/SilenWang/rk3588-webflasher/internal/rkcrc"
)

// verifyBufferSize is used for streaming multi-GiB image ranges.
const verifyBufferSize = 4 << 20

// CRCResult is the outcome of verifying a 32-bit Rockchip CRC.
type CRCResult struct {
	Stored   uint32 `json:"stored"`
	Computed uint32 `json:"computed"`
	OK       bool   `json:"ok"`
}

// MD5Result is the outcome of verifying the RKFW trailing MD5. Present is false
// for bare RKAF images, which carry no MD5.
type MD5Result struct {
	Present  bool   `json:"present"`
	Signed   bool   `json:"signed"` // MD5 sits at image end, followed by a signature blob
	Offset   int64  `json:"offset"`
	Expected string `json:"expected"`
	Computed string `json:"computed"`
	OK       bool   `json:"ok"`
}

// VerifyRKAFCRC streams the archive body through the Rockchip CRC-32 and
// compares it with the 4-byte trailer that follows it. Reading is bounded by
// RKAFLength, so a multi-GiB archive is never buffered.
func (img *Image) VerifyRKAFCRC() (CRCResult, error) {
	var res CRCResult
	bodyLen := img.RKAFLength - 4
	if bodyLen < 0 {
		return res, fmt.Errorf("rkfw: RKAF length %d is too small to hold a CRC trailer", img.RKAFLength)
	}

	f, err := os.Open(img.Path)
	if err != nil {
		return res, fmt.Errorf("rkfw: open %s: %w", img.Path, err)
	}
	defer f.Close()

	var tail [4]byte
	if err := readAtFull(f, tail[:], img.RKAFOffset+bodyLen); err != nil {
		return res, fmt.Errorf("rkfw: read RKAF CRC trailer: %w", err)
	}
	res.Stored = binary.LittleEndian.Uint32(tail[:])

	h := rkcrc.New()
	buf := make([]byte, verifyBufferSize)
	n, err := io.CopyBuffer(h, io.NewSectionReader(f, img.RKAFOffset, bodyLen), buf)
	if err != nil {
		return res, fmt.Errorf("rkfw: checksum RKAF body: %w", err)
	}
	if n != bodyLen {
		return res, fmt.Errorf("rkfw: RKAF body truncated: checksummed %d of %d bytes", n, bodyLen)
	}
	res.Computed = h.Sum32()
	res.OK = res.Stored == res.Computed
	return res, nil
}

// VerifyMD5 checks the 32-byte ASCII MD5 tail of an RKFW image. Rockchip tools
// place it either right after the embedded RKAF (signed images, followed by a
// signature blob) or at EOF-32.
//
// The position is chosen by content, not by arithmetic: the first candidate
// that holds 32 ASCII hex digits wins, with the position right after the
// archive preferred. That matters for images like openFyde r132, which carry
// 4 GiB of padding after the archive: the old "at least 160 bytes follow, so
// this must be a signature blob" rule placed the digest inside padding and
// reported a bogus mismatch.
func (img *Image) VerifyMD5() (MD5Result, error) {
	var res MD5Result
	if !img.HasRKFW {
		return res, nil
	}
	res.Present = true

	imageEnd := img.RKAFOffset + img.RKAFLength
	f, err := os.Open(img.Path)
	if err != nil {
		return res, fmt.Errorf("rkfw: open %s: %w", img.Path, err)
	}
	defer f.Close()

	type candidate struct {
		offset int64
		signed bool
	}
	var candidates []candidate
	switch {
	case imageEnd+32 == img.Size:
		// The archive ends exactly where the file does, so the digest is the
		// trailing one: no signature blob, nothing to prefer.
		candidates = append(candidates, candidate{img.Size - 32, false})
	case imageEnd+32 < img.Size:
		candidates = append(candidates, candidate{imageEnd, true})
	}
	if img.Size >= 32 && (len(candidates) == 0 || candidates[0].offset != img.Size-32) {
		candidates = append(candidates, candidate{img.Size - 32, false})
	}
	if len(candidates) == 0 {
		return res, fmt.Errorf("rkfw: file of %d bytes has no room for a 32-byte MD5 tail", img.Size)
	}

	var tail [32]byte
	chosen := candidate{offset: -1}
	for _, c := range candidates {
		if err := readAtFull(f, tail[:], c.offset); err != nil {
			continue
		}
		if isASCIIHex(tail[:]) {
			chosen = c
			break
		}
	}
	if chosen.offset < 0 {
		// No position holds an ASCII digest. Fall back to the layout heuristic
		// so a corrupt image reports a mismatch instead of a parse error.
		if img.Size-imageEnd >= 160 {
			chosen = candidates[0]
		} else {
			chosen = candidates[len(candidates)-1]
		}
		if err := readAtFull(f, tail[:], chosen.offset); err != nil {
			return res, fmt.Errorf("rkfw: read MD5 tail: %w", err)
		}
		res.Offset = chosen.offset
		res.Signed = chosen.signed
		res.Expected = string(tail[:])
		return res, nil
	}

	res.Offset = chosen.offset
	res.Signed = chosen.signed
	res.Expected = string(tail[:])

	h := md5.New()
	buf := make([]byte, verifyBufferSize)
	n, err := io.CopyBuffer(h, io.NewSectionReader(f, 0, res.Offset), buf)
	if err != nil {
		return res, fmt.Errorf("rkfw: hash image for MD5: %w", err)
	}
	if n != res.Offset {
		return res, fmt.Errorf("rkfw: image truncated: hashed %d of %d bytes", n, res.Offset)
	}
	res.Computed = hex.EncodeToString(h.Sum(nil))
	res.OK = equalFoldASCII(res.Expected, res.Computed)
	return res, nil
}

// isASCIIHex reports whether b is exactly 32 hexadecimal digits, the shape of
// an MD5 tail written by Rockchip's tools.
func isASCIIHex(b []byte) bool {
	for _, c := range b {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') && !(c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

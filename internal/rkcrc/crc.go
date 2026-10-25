// Package rkcrc implements the Rockchip checksum ("RKCRC") used to protect
// parameter, kernel and boot payloads inside Rockchip update images.
//
// It is a non-reflected CRC-32 with polynomial 0x04C10DB7, an initial value
// of zero and no final XOR. That differs from the IEEE (zlib) CRC-32 used by
// GPT, which is why the two are kept apart.
//
// The algorithm and table are transcribed from Rockchip's GPL-2.0
// rkdeveloptool (crc.cpp, "crc32 factor 0x04C10DB7"), cross-checked against
// the table generated here:
//
//	table[1] = 0x04C10DB7   table[2] = 0x09821B6E
//	table[128] = 0x6904C0EE table[255] = 0xB1F880B4
package rkcrc

// Polynomial is the Rockchip CRC-32 generator polynomial.
const Polynomial uint32 = 0x04C10DB7

var table = buildTable()

func buildTable() [256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 24
		for bit := 0; bit < 8; bit++ {
			if c&0x80000000 != 0 {
				c = (c << 1) ^ Polynomial
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

// CRC32 feeds p into the running Rockchip CRC-32 value. Pass 0 to start a new
// checksum.
func CRC32(crc uint32, p []byte) uint32 {
	for _, b := range p {
		crc = (crc << 8) ^ table[byte(crc>>24)^b]
	}
	return crc
}

// Checksum returns the Rockchip CRC-32 of p.
func Checksum(p []byte) uint32 { return CRC32(0, p) }

// Hash is a streaming Rockchip CRC-32 accumulator. It implements io.Writer so
// it can be dropped into io.Copy / io.CopyBuffer when checksumming multi-GiB
// image ranges without loading them into memory.
type Hash struct {
	crc uint32
}

// New returns a fresh accumulator.
func New() *Hash { return &Hash{} }

// Write implements io.Writer.
func (h *Hash) Write(p []byte) (int, error) {
	h.crc = CRC32(h.crc, p)
	return len(p), nil
}

// Sum32 returns the running checksum.
func (h *Hash) Sum32() uint32 { return h.crc }

// Reset clears the accumulator.
func (h *Hash) Reset() { h.crc = 0 }

// Size reports the checksum width in bytes.
func (h *Hash) Size() int { return 4 }

// BlockSize implements hash.Hash's optional interface.
func (h *Hash) BlockSize() int { return 1 }

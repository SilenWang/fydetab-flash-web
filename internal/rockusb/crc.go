package rockusb

// Checksums used by the Rockchip Download Boot path, reproduced bit-for-bit
// from Rockchip's GPL-2.0 rkdeveloptool (crc.cpp), including its quirks:
//
//   - CRC_CCITT: poly 0x1021, init 0xFFFF, MSB-first, no final XOR
//     (crc.cpp:342-370). Appended to every vendor-request payload.
//   - CRC_32: init 0, MSB-first, and poly 0x04C10DB7 — *not* the standard
//     0x04C11DB7. The upstream table comments "crc32 factor 0x04C10DB7"
//     (crc.cpp:8-9, 168-176), so a loader image written by boot_merger only
//     validates against the upstream value, not against standard CRC-32.
//
// Reproducing the typo is deliberate: using hash/crc32.IEEE here would reject
// every real loader file.

const (
	crcCCITTPoly uint16 = 0x1021
	crc32Poly    uint32 = 0x04C10DB7
)

// CRCCCITT is Rockchip's CRC_CCITT: init 0xFFFF, poly 0x1021, MSB-first.
func CRCCCITT(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ crcCCITTPoly
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// CRC32 is Rockchip's CRC_32: init 0, MSB-first, upstream polynomial
// 0x04C10DB7 (see the note above), no final XOR.
func CRC32(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc ^= uint32(b) << 24
		for bit := 0; bit < 8; bit++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ crc32Poly
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

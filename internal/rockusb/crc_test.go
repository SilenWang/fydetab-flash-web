package rockusb

import "testing"

func TestCRCCCITTCheckValue(t *testing.T) {
	// The canonical CRC-CCITT check value: "123456789" with init 0xFFFF and
	// poly 0x1021 yields 0x29B1. rkdeveloptool's CRC_CCITT is that exact
	// variant (crc.cpp:342-370), which is what every vendor-request payload
	// is checksummed with.
	if got := CRCCCITT([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("CRCCCITT(123456789) = 0x%04X, want 0x29B1", got)
	}
	if got := CRCCCITT(nil); got != 0xFFFF {
		t.Fatalf("CRCCCITT(empty) = 0x%04X, want the init value 0xFFFF", got)
	}
}

// crc32TableReference builds the MSB-first table with upstream's polynomial so
// the bitwise implementation in crc.go is checked against an independent code
// path, and the polynomial itself is pinned to values copied out of
// rkdeveloptool's crc.cpp table.
func crc32TableReference(poly uint32) [256]uint32 {
	var table [256]uint32
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for bit := 0; bit < 8; bit++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ poly
			} else {
				crc <<= 1
			}
		}
		table[i] = crc
	}
	return table
}

func TestCRC32UsesRockchipPolynomial(t *testing.T) {
	table := crc32TableReference(0x04C10DB7)
	// First entries of gTable_Crc32 in rkdeveloptool's crc.cpp.
	want := []uint32{0x00000000, 0x04C10DB7, 0x09821B6E, 0x0D4316D9, 0x130436DC}
	for i, w := range want {
		if table[i] != w {
			t.Fatalf("table[%d] = 0x%08X, want 0x%08X (rkdeveloptool crc.cpp)", i, table[i], w)
		}
	}
	// Standard CRC-32 (poly 0x04C11DB7) is a *different* table: if this ever
	// matches, someone "fixed" the polynomial and every real loader file
	// would start failing its CRC check.
	std := crc32TableReference(0x04C11DB7)
	if std[1] == table[1] {
		t.Fatal("Rockchip's non-standard CRC-32 polynomial must differ from CRC-32/MPEG-2")
	}

	data := []byte("rk3588_spl_loader_v1.06.111")
	var ref uint32
	for _, b := range data {
		ref = (ref << 8) ^ table[byte(ref>>24)^b]
	}
	if got := CRC32(data); got != ref {
		t.Fatalf("CRC32 = 0x%08X, want table reference 0x%08X", got, ref)
	}
	if got, zero := CRC32(nil), uint32(0); got != zero {
		t.Fatalf("CRC32(empty) = 0x%08X, want 0", got)
	}
}

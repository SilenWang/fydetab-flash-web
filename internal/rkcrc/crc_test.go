package rkcrc

import (
	"bytes"
	"testing"
)

// TestTable pins the generated table to the values in Rockchip's crc.cpp, so a
// change in polynomial or bit order cannot slip through.
func TestTable(t *testing.T) {
	cases := []struct {
		index int
		want  uint32
	}{
		{0, 0x00000000},
		{1, 0x04C10DB7},
		{2, 0x09821B6E},
		{128, 0x6904C0EE},
		{255, 0xB1F880B4},
	}
	for _, c := range cases {
		if got := table[c.index]; got != c.want {
			t.Errorf("table[%d] = 0x%08X, want 0x%08X", c.index, got, c.want)
		}
	}
}

// crc32Bitwise is the straightforward shift-register form of the same
// checksum; comparing against it keeps the table implementation honest.
func crc32Bitwise(crc uint32, p []byte) uint32 {
	for _, b := range p {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ Polynomial
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func TestCRC32BitwiseAgreement(t *testing.T) {
	input := make([]byte, 4096)
	for i := range input {
		input[i] = byte(i*37 + 11)
	}
	if got, want := Checksum(input), crc32Bitwise(0, input); got != want {
		t.Fatalf("table CRC = 0x%08X, bitwise = 0x%08X", got, want)
	}

	// A leading zero byte leaves this CRC at zero (init 0, MSB-first), but a
	// non-zero byte must not.
	if got := Checksum([]byte{1}); got != 0x04C10DB7 {
		t.Fatalf("CRC32 of {0x01} = 0x%08X, want 0x04C10DB7", got)
	}
}

func TestIncremental(t *testing.T) {
	a := []byte("Rockchip update image")
	b := []byte(" with a trailing MD5")
	whole := Checksum(append(append([]byte{}, a...), b...))
	split := CRC32(Checksum(a), b)
	if whole != split {
		t.Fatalf("incremental CRC = 0x%08X, one-shot = 0x%08X", split, whole)
	}
}

func TestHashWriter(t *testing.T) {
	input := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 1024)
	h := New()
	for i := 0; i < len(input); i += 7 {
		end := i + 7
		if end > len(input) {
			end = len(input)
		}
		if _, err := h.Write(input[i:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got, want := h.Sum32(), Checksum(input); got != want {
		t.Fatalf("streaming CRC = 0x%08X, one-shot = 0x%08X", got, want)
	}
	if h.Size() != 4 {
		t.Fatalf("Size() = %d, want 4", h.Size())
	}
	h.Reset()
	if h.Sum32() != 0 {
		t.Fatalf("after Reset Sum32 = 0x%08X, want 0", h.Sum32())
	}
}

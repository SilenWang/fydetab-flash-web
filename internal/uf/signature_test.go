package uf

import (
	"bytes"
	"strings"
	"testing"
)

func TestSignatureGateMatchesPayloadMagics(t *testing.T) {
	src := &memSource{data: make([]byte, 8192)}
	copy(src.data[0:], "CHROMEOS")
	src.data[0x438] = 0x53
	src.data[0x439] = 0xef

	cases := []struct {
		name string
		file int64
	}{
		{"KERN-A", 0},   // CHROMEOS
		{"ROOT-A", 0},   // ext4 @0x438
		{"STATE", 0},    // ext4 @0x438
		{"OEM", 0},      // ext4 @0x438
	}
	for _, c := range cases {
		if err := checkItemSignature(src, Item{Name: c.name, FileOffset: c.file}); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	fdt := make([]byte, 8192)
	copy(fdt, []byte{0xd0, 0x0d, 0xfe, 0xed})
	fdtSrc := &memSource{data: fdt}
	if err := checkItemSignature(fdtSrc, Item{Name: "uboot", FileOffset: 0}); err != nil {
		t.Errorf("uboot: %v", err)
	}

	res := make([]byte, 8192)
	copy(res, "RSCE")
	resSrc := &memSource{data: res}
	if err := checkItemSignature(resSrc, Item{Name: "resource", FileOffset: 0}); err != nil {
		t.Errorf("resource: %v", err)
	}

	fat := make([]byte, 8192)
	copy(fat[0x36:], "FAT16")
	fatSrc := &memSource{data: fat}
	if err := checkItemSignature(fatSrc, Item{Name: "EFI-SYSTEM", FileOffset: 0}); err != nil {
		t.Errorf("EFI-SYSTEM: %v", err)
	}

	if err := checkItemSignature(src, Item{Name: "KERN-A", FileOffset: 4096}); err == nil {
		t.Error("a zero region must fail the KERN-A signature")
	}
	if err := checkItemSignature(src, Item{Name: "vendor-misc", FileOffset: 0}); err == nil {
		t.Error("an item without a signature rule must fail closed")
	}
}

func TestIDBMagicGate(t *testing.T) {
	if err := checkIDBMagic(append([]byte("RKNS"), make([]byte, 12)...)); err != nil {
		t.Fatalf("valid New IDB rejected: %v", err)
	}
	if err := checkIDBMagic([]byte("LDR \x66\x00\x0b\x01")); err == nil || !strings.Contains(err.Error(), "RKNS") {
		t.Fatalf("legacy/whole-loader IDB must be rejected, got %v", err)
	}
}

func TestSignatureGateChecksAllPlanItems(t *testing.T) {
	src := &memSource{data: bytes.Repeat([]byte{0x11}, 8192)}
	n, err := checkPlanSignatures(src, []Item{{Name: "KERN-A", FileOffset: 0}})
	if err == nil {
		t.Fatal("expected a signature failure")
	}
	if n != 0 {
		t.Fatalf("checked = %d, want 0 before the first mismatch", n)
	}
}

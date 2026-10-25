package uf

import (
	"bytes"
	"fmt"
	"strings"
)

// Item signatures. The 64-bit high-word bug produced a plan whose offsets
// were "legal" (aligned, inside the file) but pointed at zeros or at another
// partition, so every length/bounds check passed. The only thing that catches
// that class of error is the payload's own magic, checked before a single USB
// transfer is sent.
type itemSignature struct {
	offset int64
	magic  []byte
	// contains is used for FAT, whose label sits at a variable offset.
	contains []byte
}

func signatureForItem(name string) (itemSignature, bool) {
	up := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(up, "KERN-"):
		return itemSignature{offset: 0, magic: []byte("CHROMEOS")}, true
	case strings.HasPrefix(up, "ROOT-"), up == "STATE", up == "OEM":
		// ext4 superblock magic at 1024+56.
		return itemSignature{offset: 0x438, magic: []byte{0x53, 0xef}}, true
	case up == "UBOOT":
		// FIT / device tree blob.
		return itemSignature{offset: 0, magic: []byte{0xd0, 0x0d, 0xfe, 0xed}}, true
	case up == "RESOURCE":
		return itemSignature{offset: 0, magic: []byte("RSCE")}, true
	case up == "EFI-SYSTEM":
		return itemSignature{offset: 0, contains: []byte("FAT")}, true
	default:
		return itemSignature{}, false
	}
}

// checkItemSignature reads the bytes the signature needs and compares them.
// Unknown item names fail closed: an unrecognised payload is exactly where a
// wrong offset would hide.
func checkItemSignature(src Source, it Item) error {
	sig, ok := signatureForItem(it.Name)
	if !ok {
		return fmt.Errorf("no signature rule for item %q; refusing to write an unverified payload", it.Name)
	}
	need := sig.offset + int64(len(sig.magic))
	if sig.contains != nil {
		need = 512
	}
	buf := make([]byte, need)
	n, err := src.ReadAt(buf, it.FileOffset)
	if err != nil && n != len(buf) {
		return fmt.Errorf("read signature for %q at 0x%x: %w", it.Name, it.FileOffset, err)
	}
	if n != len(buf) {
		return fmt.Errorf("short signature read for %q at 0x%x: %d of %d", it.Name, it.FileOffset, n, len(buf))
	}
	if sig.contains != nil {
		if !bytes.Contains(buf, sig.contains) {
			return fmt.Errorf("signature mismatch for %q: first 512 B do not contain %q", it.Name, sig.contains)
		}
		return nil
	}
	if !bytes.Equal(buf[sig.offset:], sig.magic) {
		return fmt.Errorf("signature mismatch for %q at offset 0x%x: got %x, want %x",
			it.Name, sig.offset, buf[sig.offset:], sig.magic)
	}
	return nil
}

// checkPlanSignatures is the pre-flight gate: every item that will be written
// must carry the magic its partition is expected to have.
func checkPlanSignatures(src Source, items []Item) (int, error) {
	checked := 0
	for _, it := range items {
		if err := checkItemSignature(src, it); err != nil {
			return checked, err
		}
		checked++
	}
	return checked, nil
}

// checkIDBMagic pins the New IDB header. The boot ROM reads this header before
// any partition, so a wrong IDB is unbootable even when every item is correct.
func checkIDBMagic(idb []byte) error {
	if len(idb) < 16 || string(idb[:4]) != "RKNS" {
		return fmt.Errorf("IDB does not start with the New IDB magic RKNS")
	}
	return nil
}

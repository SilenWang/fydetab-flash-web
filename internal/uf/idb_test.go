package uf

import (
	"bytes"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

func TestRC4MatchesTheClassicTestVector(t *testing.T) {
	// The canonical RC4 vector: key "Key", plaintext "Plaintext".
	got := []byte("Plaintext")
	rc4([]byte("Key"), got)
	want := []byte{0xBB, 0xF3, 0x16, 0xE8, 0xD9, 0x40, 0xAF, 0x0A, 0xD3}
	if !bytes.Equal(got, want) {
		t.Fatalf("RC4(key=Key, Plaintext) = %x, want %x", got, want)
	}
	// RC4 is symmetric: running it again restores the plaintext.
	rc4([]byte("Key"), got)
	if string(got) != "Plaintext" {
		t.Fatalf("second pass = %q, want Plaintext", got)
	}
}

func TestRK28RC4ModelAlignsAndEncryptsPerSector(t *testing.T) {
	loader := make([]byte, 1000)
	for i := range loader {
		loader[i] = byte(i)
	}
	model := NewRK28RC4Model()
	if model.Name() != ModelRK28RC4 || model.Verified() {
		t.Fatalf("model = %s verified=%t, want %s and unverified", model.Name(), model.Verified(), ModelRK28RC4)
	}

	idb, err := model.Build(LoaderInput{Container: loader})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(idb) != 1024 {
		t.Fatalf("IDB is %d bytes, want the loader rounded up to 1024", len(idb))
	}
	if bytes.Equal(idb[1000:], make([]byte, 24)) {
		t.Fatal("the zero padding was not encrypted with its sector")
	}

	// Decrypting sector by sector with the same key must give the loader back,
	// which is what proves the encryption is per 512-byte block and symmetric.
	plain := append([]byte(nil), idb...)
	for off := 0; off < len(plain); off += IDBBlockSize {
		rc4(rk28RC4Key, plain[off:off+IDBBlockSize])
	}
	if !bytes.Equal(plain[:len(loader)], loader) {
		t.Fatal("decrypting the IDB did not restore the loader")
	}
	if !bytes.Equal(plain[len(loader):], make([]byte, len(plain)-len(loader))) {
		t.Fatal("the zero padding was not restored by decrypting the last sector")
	}
	if bytes.Equal(idb[:512], loader[:512]) {
		t.Fatal("the first sector was not encrypted")
	}
}

func TestRawLoaderModelIsTheABOption(t *testing.T) {
	loader := bytes.Repeat([]byte{0x42}, 600)
	raw, err := NewRawLoaderModel().Build(LoaderInput{Container: loader})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(raw) != 1024 {
		t.Fatalf("raw loader IDB is %d bytes, want 1024", len(raw))
	}
	if !bytes.Equal(raw[:600], loader) {
		t.Fatal("the raw model must not transform the loader")
	}
	encrypted, err := NewRK28RC4Model().Build(LoaderInput{Container: loader})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if bytes.Equal(raw, encrypted) {
		t.Fatal("the two models produced identical bytes; the encryption step is missing")
	}
}

func TestIDBModelByEmptyLoaderAndByName(t *testing.T) {
	if _, err := NewRK28RC4Model().Build(LoaderInput{}); err == nil {
		t.Fatal("expected an error for an empty loader")
	}
	for _, name := range IDBModelNames() {
		m, err := IDBModelByName(name)
		if err != nil {
			t.Fatalf("IDBModelByName(%q): %v", name, err)
		}
		if m.Name() != name {
			t.Fatalf("IDBModelByName(%q).Name() = %q", name, m.Name())
		}
	}
	if _, err := IDBModelByName("nope"); err == nil {
		t.Fatal("expected an error for an unknown IDB model")
	}
	if IDBDefaultLBA != 64 {
		t.Fatalf("IDBDefaultLBA = %d, want the documented RK_IDB_SECTOR 64", IDBDefaultLBA)
	}
}

func TestIDBModelUsesParsedRKBOOTWhenAvailable(t *testing.T) {
	loader := rkbootLoader(t)
	img, err := rockusb.ParseLoaderImage(loader)
	if err != nil {
		t.Fatalf("parse loader fixture: %v", err)
	}
	idb, err := NewRK28RC4Model().Build(LoaderInput{Container: loader, Parsed: img})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(idb) != int(alignUp(int64(len(loader)), IDBBlockSize)) {
		t.Fatalf("IDB length = %d, want align512(%d)", len(idb), len(loader))
	}
}

func TestNewIDBModelAssemblesTheLoaderEntries(t *testing.T) {
	loader := rkbootLoader(t)
	idb, err := NewRKBOOTModel().Build(LoaderInput{Container: loader})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// FlashHead 2048 + FlashData 1024 + FlashBoot 512, each 2 KiB aligned.
	if len(idb) != 6144 {
		t.Fatalf("IDB length = %d, want 6144", len(idb))
	}
	// The fixture stores a plaintext FlashHead (ucRc4Flag=0), so the New IDB
	// starts with its magic directly. The real image uses ucRc4Flag=1 and the
	// same code path decrypts the stored segments; the real-image dry run
	// checks the resulting RKNS header.
	if string(idb[:4]) != "RKNS" {
		t.Fatalf("first sector starts %q, want RKNS", idb[:4])
	}
	legacy, err := NewRK28RC4Model().Build(LoaderInput{Container: loader})
	if err != nil {
		t.Fatalf("legacy Build: %v", err)
	}
	if len(legacy) == len(idb) {
		t.Fatal("legacy whole-loader model produced the New IDB length; models are not distinct")
	}
}

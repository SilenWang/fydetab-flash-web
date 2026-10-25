package rkfw

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/rkcrc"
)

// fixturePart describes one RKAF item for the synthetic images built below.
// None of the numbers come from a real vendor image; they only exercise the
// parser and its boundary checks.
type fixturePart struct {
	name     string
	data     []byte
	nandAddr uint32
	nandSize uint32
	reserved bool
}

func item(name string, data []byte) fixturePart {
	return fixturePart{name: name, data: data, nandAddr: NoAddress, nandSize: NoAddress}
}

func (p fixturePart) at(lba uint32) fixturePart       { p.nandAddr = lba; return p }
func (p fixturePart) size(sectors uint32) fixturePart { p.nandSize = sectors; return p }

// buildRKAF assembles header + payloads + RKCRC trailer.
func buildRKAF(t *testing.T, parts []fixturePart) []byte {
	t.Helper()
	if len(parts) > MaxItems {
		t.Fatalf("fixture has %d items, max %d", len(parts), MaxItems)
	}
	le := binary.LittleEndian
	header := make([]byte, RKAFHeaderSize)
	copy(header[0:4], MagicRKAF[:])
	copy(header[8:42], "Synthetic Duo")
	copy(header[42:72], "rockchip")
	copy(header[72:128], "Rockchip")
	le.PutUint32(header[128:132], 0)
	le.PutUint32(header[132:136], 0x01000000)
	le.PutUint32(header[136:140], uint32(len(parts)))

	body := append([]byte(nil), header...)
	pos := uint32(RKAFHeaderSize)
	for i, p := range parts {
		off := 140 + i*RKAFItemSize
		slot := body[off : off+RKAFItemSize]
		copy(slot[0:32], p.name)
		copy(slot[32:92], "Image/"+p.name)
		if p.reserved {
			le.PutUint32(slot[92:96], NoAddress)
			le.PutUint32(slot[96:100], NoAddress)
			le.PutUint32(slot[100:104], NoAddress)
			le.PutUint32(slot[104:108], 0)
			le.PutUint32(slot[108:112], NoAddress)
			continue
		}
		size := uint32(len(p.data))
		padded := (size + SectorSize - 1) / SectorSize * SectorSize
		le.PutUint32(slot[92:96], p.nandSize)
		le.PutUint32(slot[96:100], pos)
		le.PutUint32(slot[100:104], p.nandAddr)
		// "+104" counts 2 KiB pages, not bytes (measured on openFyde r132).
		le.PutUint32(slot[104:108], (padded+PageSize-1)/PageSize)
		le.PutUint32(slot[108:112], size)

		payload := make([]byte, padded)
		copy(payload, p.data)
		body = append(body, payload...)
		pos += padded
	}
	// RKAF header length excludes the 4-byte trailer.
	le.PutUint32(body[4:8], uint32(len(body)))
	trailer := make([]byte, 4)
	le.PutUint32(trailer, rkcrc.Checksum(body))
	return append(body, trailer...)
}

// buildRKFWBody wraps an RKAF archive in an RKFW header plus loader.
func buildRKFWBody(loader, rkaf []byte) []byte {
	le := binary.LittleEndian
	out := make([]byte, HeaderSize)
	copy(out[0:4], MagicRKFW[:])
	le.PutUint16(out[4:6], HeaderSize)
	le.PutUint32(out[6:10], 0x17000001) // 23.0.1
	le.PutUint32(out[10:14], 0x01030000)
	le.PutUint16(out[14:16], 2025)
	out[16] = 8 // month
	out[17] = 1 // day
	out[18] = 12
	out[19] = 34
	out[20] = 56
	// RK3588 is stored as the four ASCII digits "3588" reversed.
	le.PutUint32(out[21:25], binary.LittleEndian.Uint32([]byte{'8', '8', '5', '3'}))
	le.PutUint32(out[25:29], HeaderSize)
	le.PutUint32(out[29:33], uint32(len(loader)))
	le.PutUint32(out[33:37], uint32(HeaderSize+len(loader)))
	le.PutUint32(out[37:41], uint32(len(rkaf)))
	out = append(out, loader...)
	out = append(out, rkaf...)
	return out
}

func appendMD5(body []byte) []byte {
	sum := md5.Sum(body)
	return append(body, []byte(hex.EncodeToString(sum[:]))...)
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "update.img")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func fixtureImage(t *testing.T) (path string, loader, paramData, ubootData, bootData []byte) {
	t.Helper()
	loader = bytes.Repeat([]byte{0x5A}, 4096)
	paramData = []byte("PARM\x14\x00\x00\x00CMDLINE: mtdparts=x")
	for len(paramData) < 56 {
		paramData = append(paramData, 0)
	}
	ubootData = bytes.Repeat([]byte{0x11}, 700)
	bootData = bytes.Repeat([]byte{0x22}, 1500)
	parts := []fixturePart{
		item("parameter", paramData).at(0x40),
		item("uboot", ubootData).at(0x2000),
		item("boot", bootData).at(0x4000),
		{name: "backup", reserved: true},
	}
	rkaf := buildRKAF(t, parts)
	return writeTemp(t, appendMD5(buildRKFWBody(loader, rkaf))), loader, paramData, ubootData, bootData
}

func TestOpenRKFW(t *testing.T) {
	path, loader, paramData, _, _ := fixtureImage(t)
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !img.HasRKFW {
		t.Fatal("HasRKFW = false")
	}
	if got, want := img.LoaderLength, int64(len(loader)); got != want {
		t.Fatalf("loader length = %d, want %d", got, want)
	}
	if got := img.RKFW.ChipName(); got != "RK3588" {
		t.Fatalf("chip = %q, want RK3588", got)
	}
	if got := img.RKFW.VersionString(); got != "23.0.1" {
		t.Fatalf("version = %q", got)
	}
	ts, ok := img.RKFW.ReleaseTime()
	if !ok || !ts.Equal(time.Date(2025, 8, 1, 12, 34, 56, 0, time.UTC)) {
		t.Fatalf("release time = %v ok=%v", ts, ok)
	}
	if got, want := img.Archive.Model, "Synthetic Duo"; got != want {
		t.Fatalf("model = %q", got)
	}
	if len(img.Items) != 4 {
		t.Fatalf("items = %d", len(img.Items))
	}
	if img.Parameter == nil {
		t.Fatal("parameter item not found")
	}
	if got, want := img.Parameter.Name, "parameter"; got != want {
		t.Fatalf("parameter name = %q", got)
	}
	if got, want := img.Items[1].PaddedSize, uint32(1024); got != want {
		t.Fatalf("uboot padded size = %d, want %d", got, want)
	}
	if got, want := img.RKAFOffset, int64(HeaderSize+len(loader)); got != want {
		t.Fatalf("RKAF offset = %d, want %d", got, want)
	}

	data, err := img.ReadItem(*img.Parameter, 0)
	if err != nil {
		t.Fatalf("ReadItem: %v", err)
	}
	if !bytes.Equal(data, paramData) {
		t.Fatal("parameter payload mismatch")
	}
	if got := img.ItemOffset(img.Items[1]); got != img.RKAFOffset+int64(img.Items[1].Pos) {
		t.Fatalf("item offset = %d", got)
	}
	rd, closeFn, err := img.ItemReader(img.Items[2])
	if err != nil {
		t.Fatalf("ItemReader: %v", err)
	}
	defer closeFn()
	buf := make([]byte, img.Items[2].Size)
	if _, err := rd.Read(buf); err != nil {
		t.Fatalf("read section: %v", err)
	}
	if !bytes.HasPrefix(buf, bytes.Repeat([]byte{0x22}, 16)) {
		t.Fatalf("boot data starts with %x", buf[:16])
	}
	if len(img.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", img.Warnings)
	}
}

func TestVerifyMD5AndCRC(t *testing.T) {
	path, _, _, _, _ := fixtureImage(t)
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	md5res, err := img.VerifyMD5()
	if err != nil {
		t.Fatalf("VerifyMD5: %v", err)
	}
	if !md5res.Present || !md5res.OK || md5res.Signed {
		t.Fatalf("MD5 result = %+v", md5res)
	}
	if got, want := md5res.Offset, img.Size-32; got != want {
		t.Fatalf("MD5 offset = %d, want %d", got, want)
	}
	crc, err := img.VerifyRKAFCRC()
	if err != nil {
		t.Fatalf("VerifyRKAFCRC: %v", err)
	}
	if !crc.OK || crc.Stored != crc.Computed {
		t.Fatalf("CRC result = %+v", crc)
	}
}

func TestVerifyMD5Tampered(t *testing.T) {
	path, _, _, _, _ := fixtureImage(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one loader byte; the MD5 covers it and the loader is outside the
	// RKAF archive, so only the MD5 check should fail.
	data[HeaderSize] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	md5res, err := img.VerifyMD5()
	if err != nil {
		t.Fatalf("VerifyMD5: %v", err)
	}
	if md5res.OK {
		t.Fatal("MD5 check passed on a tampered image")
	}
	if md5res.Expected == md5res.Computed {
		t.Fatal("expected and computed MD5 are equal")
	}
	crc, err := img.VerifyRKAFCRC()
	if err != nil {
		t.Fatalf("VerifyRKAFCRC: %v", err)
	}
	if !crc.OK {
		t.Fatal("RKAF CRC must stay valid when only the loader changed")
	}
}

func TestVerifyRKAFCRCTampered(t *testing.T) {
	path, _, _, _, _ := fixtureImage(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a payload byte inside the RKAF archive.
	data[len(data)-64] ^= 0x01
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	crc, err := img.VerifyRKAFCRC()
	if err != nil {
		t.Fatalf("VerifyRKAFCRC: %v", err)
	}
	if crc.OK {
		t.Fatal("RKAF CRC passed on a tampered archive")
	}
	if crc.Stored == crc.Computed {
		t.Fatal("stored and computed CRC are equal")
	}
}

// Official Rockchip images (openFyde r132, FyeOS v23.0-SP1) carry a zero RKAF
// CRC trailer. That means "not filled in", so verification must not refuse to
// write such an image; the MD5 and PARM CRC checks still run.
func TestVerifyRKAFCRCUnset(t *testing.T) {
	path, _, _, _, _ := fixtureImage(t)
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	trailer := img.RKAFOffset + img.RKAFLength - 4

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 4; i++ {
		data[trailer+i] = 0
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	img, err = Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	crc, err := img.VerifyRKAFCRC()
	if err != nil {
		t.Fatalf("VerifyRKAFCRC: %v", err)
	}
	if !crc.Unset {
		t.Fatal("zero trailer must be reported as unset")
	}
	if !crc.OK {
		t.Fatal("an unset trailer must not fail verification")
	}
	if crc.Computed == 0 {
		t.Fatal("computed CRC must still be reported")
	}
}

func TestVerifyMD5SignedTail(t *testing.T) {
	loader := bytes.Repeat([]byte{0x33}, 1024)
	rkaf := buildRKAF(t, []fixturePart{item("parameter", []byte("PARM"))})
	body := appendMD5(buildRKFWBody(loader, rkaf))
	body = append(body, bytes.Repeat([]byte{0xA5}, 128)...) // signature blob
	path := writeTemp(t, body)

	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	res, err := img.VerifyMD5()
	if err != nil {
		t.Fatalf("VerifyMD5: %v", err)
	}
	if !res.Signed || !res.OK {
		t.Fatalf("signed MD5 result = %+v", res)
	}
	if got, want := res.Offset, img.RKAFOffset+img.RKAFLength; got != want {
		t.Fatalf("MD5 offset = %d, want image end %d", got, want)
	}
}

func TestOpenBareRKAF(t *testing.T) {
	rkaf := buildRKAF(t, []fixturePart{item("parameter", []byte("PARM")), item("boot", []byte{1, 2, 3})})
	path := writeTemp(t, rkaf)
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if img.HasRKFW {
		t.Fatal("HasRKFW = true for a bare RKAF")
	}
	if img.LoaderLength != 0 || img.RKAFOffset != 0 {
		t.Fatalf("loader=%d offset=%d", img.LoaderLength, img.RKAFOffset)
	}
	res, err := img.VerifyMD5()
	if err != nil {
		t.Fatalf("VerifyMD5: %v", err)
	}
	if res.Present {
		t.Fatal("bare RKAF must not report an MD5 tail")
	}
	crc, err := img.VerifyRKAFCRC()
	if err != nil {
		t.Fatalf("VerifyRKAFCRC: %v", err)
	}
	if !crc.OK {
		t.Fatalf("bare RKAF CRC failed: %+v", crc)
	}
}

func TestOpenShortRead(t *testing.T) {
	path := writeTemp(t, make([]byte, 100))
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error for a 100-byte file")
	} else if !strings.Contains(err.Error(), "only 100 bytes") {
		t.Fatalf("unexpected error: %v", err)
	}

	// A file that is large enough to start an RKAF header but ends inside it.
	short := append([]byte("RKAF"), make([]byte, 2044)...) // exactly one header
	if _, err := Open(writeTemp(t, short)); err == nil {
		t.Fatal("expected an error for a header with a zero length field")
	}

	// 2048 bytes of a claimed header plus a length field that runs past EOF.
	truncated := append([]byte("RKAF"), make([]byte, 2044)...)
	binary.LittleEndian.PutUint32(truncated[4:8], 4096)
	_, err := Open(writeTemp(t, truncated))
	if err == nil {
		t.Fatal("expected an error for a truncated RKAF body")
	}
	if !strings.Contains(err.Error(), "truncated image") && !strings.Contains(err.Error(), "zero length") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenTruncatedPayload(t *testing.T) {
	path, _, _, _, _ := fixtureImage(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Cut the last payload short but keep the 32-byte MD5 tail so the file is
	// structurally an image whose item table points past the data.
	cut := data[:len(data)-32-256]
	cut = append(cut, data[len(data)-32:]...)
	path = writeTemp(t, cut)
	_, err = Open(path)
	if err == nil {
		t.Fatal("expected an error for a truncated payload")
	}
	if !strings.Contains(err.Error(), "exceeds the RKAF body end") && !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenItemOverrun(t *testing.T) {
	rkaf := buildRKAF(t, []fixturePart{item("boot", []byte{1, 2, 3, 4}), item("uboot", bytes.Repeat([]byte{9}, 512))})
	// Make the first payload longer than the slot the next item's pos leaves
	// for it: the payload would run into uboot's data.
	binary.LittleEndian.PutUint32(rkaf[140+108:140+112], 4096)
	path := writeTemp(t, appendMD5(buildRKFWBody(nil, rkaf)))
	_, err := Open(path)
	if err == nil {
		t.Fatal("expected an error for a payload that outgrows its archive slot")
	}
	if !strings.Contains(err.Error(), "archive slot") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestOpenRaw104SmallerThanPayload pins the r132 quirk at unit level: the
// "+104" field is not a byte length, so a value smaller than the payload is
// normal and must not be rejected. The item's real slot comes from the
// neighbouring pos values.
func TestOpenRaw104SmallerThanPayload(t *testing.T) {
	rkaf := buildRKAF(t, []fixturePart{item("parameter", bytes.Repeat([]byte{1}, 590))})
	binary.LittleEndian.PutUint32(rkaf[140+104:140+108], 1) // r132 stores 1 here
	path := writeTemp(t, appendMD5(buildRKFWBody(nil, rkaf)))
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	it := img.Items[0]
	if it.PaddedBlocks != 1 {
		t.Errorf("PaddedBlocks = %d, want the raw field 1", it.PaddedBlocks)
	}
	// The builder pads the 590-byte payload to one 1024-byte slot; what matters
	// is that the slot comes from the layout, not from the raw "+104" value.
	if it.PaddedSize != 1024 {
		t.Errorf("PaddedSize = %d, want the 1024-byte archive slot", it.PaddedSize)
	}
}

func TestOpenTooManyItems(t *testing.T) {
	rkaf := buildRKAF(t, []fixturePart{item("boot", []byte{1})})
	binary.LittleEndian.PutUint32(rkaf[136:140], MaxItems+1)
	path := writeTemp(t, appendMD5(buildRKFWBody(nil, rkaf)))
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "room for 16") {
		t.Fatalf("error = %v, want item-count error", err)
	}
}

func TestOpenHeaderLengthWarning(t *testing.T) {
	loader := []byte{1, 2, 3}
	rkaf := buildRKAF(t, []fixturePart{item("parameter", []byte("PARM"))})
	body := buildRKFWBody(loader, rkaf)
	binary.LittleEndian.PutUint16(body[4:6], 0x70)
	path := writeTemp(t, appendMD5(body))
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(img.Warnings) == 0 || !strings.Contains(img.Warnings[0], "header length field") {
		t.Fatalf("warnings = %v", img.Warnings)
	}
}

func TestOpenImageLengthDisagreement(t *testing.T) {
	loader := []byte{1, 2, 3}
	rkaf := buildRKAF(t, []fixturePart{item("parameter", []byte("PARM"))})
	body := buildRKFWBody(loader, rkaf)
	binary.LittleEndian.PutUint32(body[37:41], uint32(len(rkaf)+512)) // lie
	path := writeTemp(t, appendMD5(body))
	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	found := false
	for _, w := range img.Warnings {
		if strings.Contains(w, "disagrees with the RKAF-derived length") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", img.Warnings)
	}
}

func TestRecoverTrueLength(t *testing.T) {
	const gib = uint64(1) << 32
	cases := []struct {
		name      string
		stored    uint64
		available uint64
		want      uint64
	}{
		{"fits", 0x1000, 0x1000, 0x1000},
		{"roomy but under 4 GiB", 0x1000, 0x2000, 0x1000},
		{"one wrap", 0x1000, 0x1000 + gib, 0x1000 + gib},
		{"wrap with slack", 0x1000, 0x1000 + gib + 0x800, 0x1000 + gib},
		{"two wraps", 0x100, 0x100 + 2*gib, 0x100 + 2*gib},
		{"zero", 0, 0x1000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := recoverTrueLength(c.stored, c.available); got != c.want {
				t.Fatalf("recoverTrueLength(0x%x, 0x%x) = 0x%x, want 0x%x", c.stored, c.available, got, c.want)
			}
		})
	}
}

// TestResolveArchiveLength pins the policy on real numbers: openFyde r132 has
// 4,053,098,496 archive bytes followed by 2^32 bytes of padding, and its RKAF
// length field (4053098496) agrees with the RKFW image_length (4053098500). The
// parser must take the stored value instead of recovering a phantom wrap, while
// a bare RKAF whose length really wrapped must still be recovered.
func TestResolveArchiveLength(t *testing.T) {
	const gib = uint64(1) << 32
	cases := []struct {
		name        string
		stored      uint64
		container   uint64
		hasStore    bool
		available   uint64
		want        uint64
		wantWrapped bool
	}{
		{
			name:      "r132: fields agree, file has 2^32 bytes of padding",
			stored:    4053098496,
			container: 4053098496,
			hasStore:  true,
			available: 8348065824,
			want:      4053098496,
		},
		{
			name:        "bare RKAF that really wrapped",
			stored:      0x1000,
			available:   0x1000 + gib,
			want:        0x1000 + gib,
			wantWrapped: true,
		},
		{
			name:      "no wrap and nothing extra",
			stored:    0x1000,
			available: 0x1000,
			want:      0x1000,
		},
		{
			name:        "container disagrees, wrap recovered from the layout",
			stored:      0x2000,
			container:   0x3000,
			hasStore:    true,
			available:   0x2000 + gib,
			want:        0x2000 + gib,
			wantWrapped: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, wrapped := resolveArchiveLength(c.stored, c.container, c.hasStore, c.available)
			if got != c.want || wrapped != c.wantWrapped {
				t.Fatalf("resolveArchiveLength(0x%x, 0x%x, %v, 0x%x) = 0x%x, %v; want 0x%x, %v",
					c.stored, c.container, c.hasStore, c.available, got, wrapped, c.want, c.wantWrapped)
			}
		})
	}
}

func TestChipName(t *testing.T) {
	cases := []struct {
		name string
		chip uint32
		want string
	}{
		{"rk3588 digits reversed", binary.LittleEndian.Uint32([]byte{'8', '8', '5', '3'}), "RK3588"},
		{"rk3566 digits reversed", binary.LittleEndian.Uint32([]byte{'6', '6', '5', '3'}), "RK3566"},
		{"legacy family", 0x50, "RK29xx"},
		{"unknown", 0xDEADBEEF, "unknown(0xdeadbeef)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := Header{Chip: c.chip}
			if got := h.ChipName(); got != c.want {
				t.Fatalf("ChipName() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestReleaseTimeInvalid(t *testing.T) {
	h := Header{Year: 2025, Month: 13, Day: 1}
	if _, ok := h.ReleaseTime(); ok {
		t.Fatal("month 13 must not parse")
	}
	h = Header{}
	if _, ok := h.ReleaseTime(); ok {
		t.Fatal("zeroed timestamp must not parse")
	}
}

func TestReservedItemMarkers(t *testing.T) {
	it := Item{Name: "backup", Pos: NoAddress, Size: NoAddress}
	if !it.Reserved() || it.HasData() {
		t.Fatal("both-NoAddress item must be reserved without data")
	}
	it = Item{Name: "backup", Pos: 0x800, Size: NoAddress}
	if !it.Reserved() || it.HasData() {
		t.Fatal("partially marked item must be treated as reserved")
	}
	it = Item{Name: "boot", Pos: 0x800, Size: 512, PaddedSize: 512}
	if it.Reserved() || !it.HasData() {
		t.Fatal("normal item must have data")
	}
}

// OpenReaderAt is the browser core's path into the parser: the page owns the
// image bytes and hands Go a ReaderAt, so every payload read and both checksum
// passes have to work without a filesystem path.
func TestOpenReaderAtReadsPayloadsWithoutAPath(t *testing.T) {
	loader := bytes.Repeat([]byte{0xA5}, 4096)
	rkaf := buildRKAF(t, []fixturePart{
		item("parameter", []byte("CMDLINE: mtdparts=rk29xxnand:0x4@0x40(parameter)")),
		item("boot", bytes.Repeat([]byte{0x07}, 1024)),
	})
	data := appendMD5(buildRKFWBody(loader, rkaf))

	img, err := OpenReaderAt(bytes.NewReader(data), int64(len(data)), "memory.img")
	if err != nil {
		t.Fatalf("OpenReaderAt: %v", err)
	}
	if img.Path != "memory.img" {
		t.Fatalf("Path = %q, want the supplied name", img.Path)
	}
	if !img.HasRKFW || img.LoaderLength != int64(len(loader)) {
		t.Fatalf("loader = %d bytes (rkfw=%v), want %d", img.LoaderLength, img.HasRKFW, len(loader))
	}

	raw, err := img.ReadItem(*img.Parameter, 4096)
	if err != nil {
		t.Fatalf("ReadItem: %v", err)
	}
	if got := string(raw); got != "CMDLINE: mtdparts=rk29xxnand:0x4@0x40(parameter)" {
		t.Fatalf("parameter payload = %q", got)
	}

	blob, err := img.ReadRange(img.LoaderOffset, img.LoaderLength)
	if err != nil {
		t.Fatalf("ReadRange(loader): %v", err)
	}
	if !bytes.Equal(blob, loader) {
		t.Fatalf("loader bytes differ from the source")
	}

	md5res, err := img.VerifyMD5()
	if err != nil {
		t.Fatalf("VerifyMD5: %v", err)
	}
	if !md5res.OK {
		t.Fatalf("VerifyMD5 = %+v", md5res)
	}
	crc, err := img.VerifyRKAFCRC()
	if err != nil {
		t.Fatalf("VerifyRKAFCRC: %v", err)
	}
	if !crc.OK {
		t.Fatalf("VerifyRKAFCRC = %+v", crc)
	}

	if _, err := OpenReaderAt(nil, int64(len(data)), "nil"); err == nil {
		t.Fatal("OpenReaderAt(nil) must fail")
	}
}

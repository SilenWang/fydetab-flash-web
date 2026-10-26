package rockusb

import (
	"encoding/binary"
	"testing"
)

func TestEncodeCBWLayout(t *testing.T) {
	cbw := EncodeCBW(0x11223344, OpReadChipInfo, 16)

	if len(cbw) != CBWLength {
		t.Fatalf("CBW length = %d, want %d", len(cbw), CBWLength)
	}
	if got := binary.LittleEndian.Uint32(cbw[0:4]); got != CBWSignature {
		t.Errorf("signature = 0x%08X, want 0x%08X", got, CBWSignature)
	}
	if got := binary.LittleEndian.Uint32(cbw[4:8]); got != 0x11223344 {
		t.Errorf("tag = 0x%08X, want 0x11223344", got)
	}
	if got := binary.LittleEndian.Uint32(cbw[8:12]); got != 16 {
		t.Errorf("transfer length = %d, want 16", got)
	}
	if cbw[12] != DirectionIn {
		t.Errorf("flags = 0x%02X, want 0x%02X (IN)", cbw[12], DirectionIn)
	}
	if cbw[14] != 0x06 {
		t.Errorf("cbwcb length = 0x%02X, want 0x06", cbw[14])
	}
	if cbw[15] != byte(OpReadChipInfo) {
		t.Errorf("opcode = 0x%02X, want 0x%02X", cbw[15], byte(OpReadChipInfo))
	}
	// Address/usLength must stay zero for read-info commands.
	for i := 16; i < CBWLength; i++ {
		if cbw[i] != 0 {
			t.Fatalf("byte %d = 0x%02X, want 0x00", i, cbw[i])
		}
	}
}

func TestDecodeCSW(t *testing.T) {
	b := make([]byte, CSWLength)
	binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
	binary.LittleEndian.PutUint32(b[4:8], 0xAABBCCDD)
	binary.LittleEndian.PutUint32(b[8:12], 0x12340000)
	b[12] = StatusPassed

	csw, err := DecodeCSW(b)
	if err != nil {
		t.Fatalf("DecodeCSW: %v", err)
	}
	if csw.Tag != 0xAABBCCDD || csw.Residue != 0x12340000 || csw.Status != StatusPassed {
		t.Fatalf("unexpected CSW: %+v", csw)
	}
	if code := csw.Validate(0xAABBCCDD); code != ErrSuccess {
		t.Errorf("Validate = %v, want ERR_SUCCESS", code)
	}
	if code := csw.Validate(0x00000001); code != ErrCmdNotMatch {
		t.Errorf("Validate(other tag) = %v, want ERR_CMD_NOTMATCH", code)
	}
}

func TestDecodeCSWRejectsShortAndBadSignature(t *testing.T) {
	if _, err := DecodeCSW(make([]byte, 12)); err == nil {
		t.Error("expected error for short CSW")
	}
	b := make([]byte, CSWLength)
	binary.LittleEndian.PutUint32(b[0:4], 0xDEADBEEF)
	if _, err := DecodeCSW(b); err == nil {
		t.Error("expected error for bad signature")
	}
}

func TestCSWStatusFailedMapsToErrFailed(t *testing.T) {
	b := make([]byte, CSWLength)
	binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
	binary.LittleEndian.PutUint32(b[4:8], 7)
	b[12] = StatusFailed

	csw, err := DecodeCSW(b)
	if err != nil {
		t.Fatalf("DecodeCSW: %v", err)
	}
	if code := csw.Validate(7); code != ErrFailed {
		t.Fatalf("Validate = %v, want %v", code, ErrFailed)
	}
	if ErrFailed.String() != "ERR_FAILED" {
		t.Errorf("ErrFailed.String() = %q", ErrFailed.String())
	}
}

func TestParseChipInfo(t *testing.T) {
	raw := make([]byte, 16)
	copy(raw, "3588")
	info, err := ParseChipInfo(raw)
	if err != nil {
		t.Fatalf("ParseChipInfo: %v", err)
	}
	if info.Text != "3588" {
		t.Errorf("chip text = %q, want %q", info.Text, "3588")
	}
	if _, err := ParseChipInfo(make([]byte, 15)); err == nil {
		t.Error("expected error for short chip info")
	}
}

func TestParseFlashInfo(t *testing.T) {
	b := make([]byte, 11)
	binary.LittleEndian.PutUint32(b[0:4], 0x00700000) // 7,340,032 sectors ≈ 3.5 GiB
	binary.LittleEndian.PutUint16(b[4:6], 0x0400)     // 1024 sectors per block
	b[6] = 0x08                                       // 4 KiB pages
	b[7] = 0x10
	b[8] = 0x0A
	b[9] = 0x01 // TOSHIBA
	b[10] = 0x03

	info, err := ParseFlashInfo(b)
	if err != nil {
		t.Fatalf("ParseFlashInfo: %v", err)
	}
	if info.FlashSizeSectors != 0x00700000 {
		t.Errorf("flash size sectors = %d", info.FlashSizeSectors)
	}
	if want := uint64(0x00700000) * 512; info.FlashSizeBytes != want {
		t.Errorf("flash size bytes = %d, want %d", info.FlashSizeBytes, want)
	}
	if info.BlockSizeBytes != 1024*512 {
		t.Errorf("block size bytes = %d", info.BlockSizeBytes)
	}
	if info.PageSizeBytes != 8*512 {
		t.Errorf("page size bytes = %d", info.PageSizeBytes)
	}
	if info.Manufacturer != "TOSHIBA" {
		t.Errorf("manufacturer = %q", info.Manufacturer)
	}
	if len(info.FlashCSList) != 2 || info.FlashCSList[0] != 0 || info.FlashCSList[1] != 1 {
		t.Errorf("flash CS list = %v, want [0 1]", info.FlashCSList)
	}
	if _, err := ParseFlashInfo(make([]byte, 10)); err == nil {
		t.Error("expected error for short flash info")
	}
}

func TestParseFlashInfoUnknownManufacturer(t *testing.T) {
	b := make([]byte, 11)
	b[9] = 0x42
	info, err := ParseFlashInfo(b)
	if err != nil {
		t.Fatalf("ParseFlashInfo: %v", err)
	}
	if info.Manufacturer != "UNKNOWN" {
		t.Errorf("manufacturer = %q, want UNKNOWN", info.Manufacturer)
	}
}

func TestParseCapability(t *testing.T) {
	b := []byte{0x0F, 0x01, 0, 0, 0, 0, 0, 0}
	cap, err := ParseCapability(b)
	if err != nil {
		t.Fatalf("ParseCapability: %v", err)
	}
	if !cap.DirectLBA || !cap.VendorStore || !cap.First4MAccess || !cap.ReadLBA || !cap.NewIDB {
		t.Fatalf("expected every known flag set, got %+v", cap)
	}
	if len(cap.Flags) != 5 {
		t.Errorf("flags = %v, want 5 entries", cap.Flags)
	}
	if _, err := ParseCapability(make([]byte, 7)); err == nil {
		t.Error("expected error for short capability")
	}
}

func TestParseStorage(t *testing.T) {
	b := []byte{0x02, 0x02, 0x00, 0x00} // bits 1 and 9 set: eMMC + SPI NOR
	st, err := ParseStorage(b)
	if err != nil {
		t.Fatalf("ParseStorage: %v", err)
	}
	if st.Bitmap != 0x202 {
		t.Errorf("bitmap = 0x%X", st.Bitmap)
	}
	if st.Current != 1 || st.CurrentName != "eMMC" {
		t.Errorf("current = %d (%q), want 1 (eMMC)", st.Current, st.CurrentName)
	}
	if len(st.Names) != 2 || st.Names[0] != "eMMC" || st.Names[1] != "SPI NOR" {
		t.Errorf("names = %v", st.Names)
	}
	if _, err := ParseStorage(make([]byte, 3)); err == nil {
		t.Error("expected error for short storage list")
	}
}

func TestParseStorageEmpty(t *testing.T) {
	st, err := ParseStorage([]byte{0, 0, 0, 0})
	if err != nil {
		t.Fatalf("ParseStorage: %v", err)
	}
	if st.Current != -1 || len(st.Available) != 0 {
		t.Errorf("expected no available storage, got %+v", st)
	}
	if st.CurrentName != "" {
		t.Errorf("current name = %q, want empty", st.CurrentName)
	}
}

func TestModeFromDescriptor(t *testing.T) {
	if got := ModeFromDescriptor(0x2207, PackBCDUSB(2, 0, 0)); got != ModeMaskrom {
		t.Errorf("bcdUSB 0x0200 => %v, want MASKROM", got)
	}
	if got := ModeFromDescriptor(0x2207, PackBCDUSB(2, 0, 1)); got != ModeLoader {
		t.Errorf("bcdUSB 0x0201 => %v, want LOADER", got)
	}
	if got := ModeFromDescriptor(0x1234, PackBCDUSB(2, 0, 0)); got != ModeUnknown {
		t.Errorf("foreign VID => %v, want UNKNOWN", got)
	}
}

// TestNoWritePath is the M0 acceptance guard: the command table must contain
// only the five read-only operations. If someone adds WRITE_LBA (0x15),
// ERASE_NORMAL (0x06), DEVICE_RESET (0xFF) and friends, this fails.
func TestNoWritePath(t *testing.T) {
	allowed := map[Op]bool{
		OpTestUnitReady:  true,
		OpReadFlashID:    true,
		OpReadLBA:        true,
		OpReadSDRAM:      true,
		OpReadFlashInfo:  true,
		OpReadChipInfo:   true,
		OpReadEfuse:      true,
		OpReadNewEfuse:   true,
		OpReadStorage:    true,
		OpReadCapability: true,
	}
	if len(Commands) != 5 {
		t.Fatalf("probe command table has %d entries, want 5", len(Commands))
	}
	for _, c := range Commands {
		if !allowed[c.Op] {
			t.Errorf("%s (0x%02X) is not a read-only opcode", c.Name, byte(c.Op))
		}
		if c.Op == 0x15 || c.Op == 0x05 || c.Op == 0x06 || c.Op == 0x0B || c.Op == 0xFF || c.Op == 0x2A {
			t.Errorf("%s is a write/erase/reset opcode and must not exist in the probe", c.Name)
		}
	}
}

func TestCommandForUnknownOp(t *testing.T) {
	if _, ok := CommandFor(Op(0x15)); ok {
		t.Fatal("WRITE_LBA must not resolve to a command")
	}
	if IsReadOnly(0x15) {
		t.Fatal("WRITE_LBA must not be read-only")
	}
	if got := Op(0x15).Name(); got != "UNKNOWN_OP_0x15" {
		t.Errorf("Name() = %q", got)
	}
}

// SIL-515: the loader parses dwAddress/usLength big-endian and READ_LBA has a
// 10-byte CBWCB. With little-endian fields the device answered CSW status 0x02
// and zero-filled data, which looked like an empty IDB at LBA 64.
func TestEncodeReadLBACBWMatchesLoaderWireFormat(t *testing.T) {
	cbw := EncodeReadLBACBW(0x01020304, 64, 8)
	if len(cbw) != CBWLength {
		t.Fatalf("CBW is %d bytes, want %d", len(cbw), CBWLength)
	}
	if cbw[12] != DirectionIn {
		t.Fatalf("flags = 0x%02X, want 0x%02X", cbw[12], DirectionIn)
	}
	if cbw[14] != 0x0a {
		t.Fatalf("cbwcb length = 0x%02X, want 0x0a for READ_LBA", cbw[14])
	}
	if cbw[15] != byte(OpReadLBA) {
		t.Fatalf("opcode = 0x%02X", cbw[15])
	}
	if got := binary.BigEndian.Uint32(cbw[17:21]); got != 64 {
		t.Fatalf("dwAddress = 0x%x, want 0x40 (big-endian)", got)
	}
	if got := binary.BigEndian.Uint16(cbw[22:24]); got != 8 {
		t.Fatalf("usLength = %d, want 8 (big-endian)", got)
	}
	if got := binary.LittleEndian.Uint32(cbw[8:12]); got != 8*512 {
		t.Fatalf("transfer length = %d, want %d (little-endian)", got, 8*512)
	}
}

func TestCSWPhaseErrorIsFailure(t *testing.T) {
	b := make([]byte, CSWLength)
	binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
	binary.LittleEndian.PutUint32(b[4:8], 9)
	b[12] = StatusPhaseError
	csw, err := DecodeCSW(b)
	if err != nil {
		t.Fatalf("DecodeCSW: %v", err)
	}
	if code := csw.Validate(9); code != ErrFailed {
		t.Fatalf("Validate(phase error) = %v, want %v", code, ErrFailed)
	}
}

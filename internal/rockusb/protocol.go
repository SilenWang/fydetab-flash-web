// Package rockusb implements the read-only subset of the Rockchip rockusb
// protocol used by the M0 probe.
//
// Wire format and command codes are transcribed from Rockchip's GPL-2.0
// rkdeveloptool (RKComm.h, RKComm.cpp, RKDevice.h). A command is a 31-byte
// UFI Command Block Wrapper written to the bulk OUT endpoint; for commands
// with a data stage the device answers on the bulk IN endpoint; the exchange
// is closed by a 13-byte Command Status Wrapper read from bulk IN.
//
// Only read-only operation codes are declared here on purpose: the probe must
// be unable to erase or write flash, so no write/erase/reset opcode exists in
// this package (see TestNoWritePath).
package rockusb

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Wire-level constants, RKComm.h.
const (
	CBWSignature uint32 = 0x43425355 // "USBC"
	CSWSignature uint32 = 0x53425355 // "USBS"

	// CBWLength is sizeof(CBW); cbwcb.ucCBWCBLength is 0x06 for the
	// read-info family of commands.
	CBWLength = 31
	// CSWLength is sizeof(CSW).
	CSWLength = 13

	DirectionIn  byte = 0x80
	DirectionOut byte = 0x00
)

// RockchipVendorID owns every VID/PID the probe asks for.
const RockchipVendorID = 0x2207

// CSW status byte values.
const (
	StatusPassed byte = 0x00
	StatusFailed byte = 0x01
)

// ErrorCode mirrors rkdeveloptool's ERR_* values, so a probe trace can be
// diffed against rkdeveloptool / upgrade_tool logs without translation.
type ErrorCode int

const (
	ErrSuccess           ErrorCode = 0
	ErrDeviceOpenFailed  ErrorCode = -1
	ErrDeviceWriteFailed ErrorCode = -3
	ErrDeviceReadFailed  ErrorCode = -4
	ErrCmdNotMatch       ErrorCode = -5
	ErrDeviceUnready     ErrorCode = -6
	ErrFailed            ErrorCode = -8
	ErrDeviceNotSupport  ErrorCode = -10
	ErrRequestFail       ErrorCode = -12
)

func (e ErrorCode) String() string {
	switch e {
	case ErrSuccess:
		return "ERR_SUCCESS"
	case ErrDeviceOpenFailed:
		return "ERR_DEVICE_OPEN_FAILED"
	case ErrDeviceWriteFailed:
		return "ERR_DEVICE_WRITE_FAILED"
	case ErrDeviceReadFailed:
		return "ERR_DEVICE_READ_FAILED"
	case ErrCmdNotMatch:
		return "ERR_CMD_NOTMATCH"
	case ErrDeviceUnready:
		return "ERR_DEVICE_UNREADY"
	case ErrFailed:
		return "ERR_FAILED"
	case ErrDeviceNotSupport:
		return "ERR_DEVICE_NOT_SUPPORT"
	case ErrRequestFail:
		return "ERR_REQUEST_FAIL"
	case ErrTimeout:
		return "ERR_TIMEOUT"
	default:
		return fmt.Sprintf("ERR_UNKNOWN(%d)", int(e))
	}
}

// Op is a USB_OPERATION_CODE (RKComm.h). Only read-only codes are listed.
type Op uint8

const (
	OpTestUnitReady  Op = 0x00 // TEST_UNIT_READY
	OpReadFlashID    Op = 0x01 // READ_FLASH_ID
	OpReadLBA        Op = 0x14 // READ_LBA
	OpReadSDRAM      Op = 0x17 // READ_SDRAM
	OpReadFlashInfo  Op = 0x1A // READ_FLASH_INFO
	OpReadChipInfo   Op = 0x1B // READ_CHIP_INFO
	OpReadEfuse      Op = 0x20 // READ_EFUSE
	OpReadNewEfuse   Op = 0x24 // READ_NEW_EFUSE
	OpReadStorage    Op = 0x2B // READ_STORAGE
	OpReadCapability Op = 0xAA // READ_CAPABILITY
)

// OpName returns the rkdeveloptool spelling of an opcode.
func (o Op) Name() string {
	for _, c := range Commands {
		if c.Op == o {
			return c.Name
		}
	}
	return fmt.Sprintf("UNKNOWN_OP_0x%02X", uint8(o))
}

// Command describes one read-only command and the shape of its transfer.
type Command struct {
	Op Op
	// Name is the rkdeveloptool API name; RKU_ReadStorageList is
	// upgrade_tool's name for the same READ_STORAGE command.
	Name string
	// TransferLength is what rkdeveloptool puts in dwCBWTransferLength.
	TransferLength uint32
	// DataLength is how many bytes rkdeveloptool expects back.
	DataLength int
	// AllowShort marks RKU_ReadFlashInfo, whose data stage is documented
	// as 1..512 bytes (rkdeveloptool reads 512 and accepts >= 11).
	AllowShort bool
	// Description is the human-readable purpose shown in the probe UI.
	Description string
}

// Commands is the complete set of commands the M0 probe may issue.
var Commands = []Command{
	{
		Op:             OpTestUnitReady,
		Name:           "RKU_TestDeviceReady",
		TransferLength: 0,
		DataLength:     0,
		Description:    "询问设备是否就绪（只读取状态）",
	},
	{
		Op:             OpReadChipInfo,
		Name:           "RKU_ReadChipInfo",
		TransferLength: 16,
		DataLength:     16,
		Description:    "读取芯片型号字符串（16 字节 ASCII）",
	},
	{
		Op:             OpReadFlashInfo,
		Name:           "RKU_ReadFlashInfo",
		TransferLength: 11,
		DataLength:     11,
		AllowShort:     true,
		Description:    "读取 flash 容量/块大小/页大小/ECC/厂商/片选",
	},
	{
		Op:             OpReadCapability,
		Name:           "RKU_ReadCapability",
		TransferLength: 8,
		DataLength:     8,
		Description:    "读取固件能力位（Direct LBA、Vendor Storage 等）",
	},
	{
		Op:             OpReadStorage,
		Name:           "RKU_ReadStorageList",
		TransferLength: 4,
		DataLength:     4,
		Description:    "读取可用存储介质位图及当前介质",
	},
}

// CommandFor looks up a command definition.
func CommandFor(op Op) (Command, bool) {
	for _, c := range Commands {
		if c.Op == op {
			return c, true
		}
	}
	return Command{}, false
}

// IsReadOnly reports whether op is in the probe's allow-list. Every transfer
// path must call it before touching the device.
func IsReadOnly(op Op) bool {
	_, ok := CommandFor(op)
	return ok
}

// EncodeReadLBACBW renders the CBW for READ_LBA: dwAddress is the first
// 512-byte sector and usLength is the sector count. Unlike the info commands,
// this one carries both fields and a bulk-IN data stage of sectors*512 bytes.
func EncodeReadLBACBW(tag uint32, lba uint32, sectors uint16) []byte {
	b := EncodeCBW(tag, OpReadLBA, uint32(sectors)*512)
	binary.LittleEndian.PutUint32(b[17:21], lba)
	binary.LittleEndian.PutUint16(b[22:24], sectors)
	return b
}

// EncodeCBW renders the 31-byte CBW for a read command. address and usLength
// stay zero: none of the read-info commands use them (rkdeveloptool only sets
// them for LBA/sector transfers).
func EncodeCBW(tag uint32, op Op, transferLength uint32) []byte {
	b := make([]byte, CBWLength)
	binary.LittleEndian.PutUint32(b[0:4], CBWSignature)
	binary.LittleEndian.PutUint32(b[4:8], tag)
	binary.LittleEndian.PutUint32(b[8:12], transferLength)
	b[12] = DirectionIn // CBW flags: device-to-host for every read command
	b[13] = 0           // LUN
	b[14] = 0x06        // cbwcb length
	b[15] = byte(op)    // cbwcb.ucOperCode
	// b[16] reserved, b[17:21] dwAddress, b[21] reserved2, b[22:24] usLength,
	// b[24:31] reserved3 — all zero for read-only info commands.
	return b
}

// CSW is a decoded Command Status Wrapper.
type CSW struct {
	Signature uint32
	Tag       uint32
	Residue   uint32
	Status    byte
}

// DecodeCSW parses the 13-byte status wrapper.
func DecodeCSW(b []byte) (CSW, error) {
	if len(b) < CSWLength {
		return CSW{}, fmt.Errorf("rockusb: CSW needs %d bytes, got %d", CSWLength, len(b))
	}
	csw := CSW{
		Signature: binary.LittleEndian.Uint32(b[0:4]),
		Tag:       binary.LittleEndian.Uint32(b[4:8]),
		Residue:   binary.LittleEndian.Uint32(b[8:12]),
		Status:    b[12],
	}
	if csw.Signature != CSWSignature {
		return csw, fmt.Errorf("rockusb: bad CSW signature 0x%08X", csw.Signature)
	}
	return csw, nil
}

// Validate checks the CSW against the tag of the CBW that opened the exchange
// and maps the status byte to an rkdeveloptool error code.
func (c CSW) Validate(tag uint32) ErrorCode {
	if c.Signature != CSWSignature || c.Tag != tag {
		return ErrCmdNotMatch
	}
	if c.Status == StatusFailed {
		return ErrFailed
	}
	return ErrSuccess
}

// ChipInfo is the decoded response to RKU_ReadChipInfo.
type ChipInfo struct {
	Raw  string `json:"rawHex"`
	Text string `json:"text"`
}

// ParseChipInfo decodes the 16-byte chip info buffer: an ASCII model string
// (e.g. "3588") padded with NULs.
func ParseChipInfo(b []byte) (ChipInfo, error) {
	if len(b) < 16 {
		return ChipInfo{}, fmt.Errorf("rockusb: chip info needs 16 bytes, got %d", len(b))
	}
	raw := b[:16]
	trimmed := strings.TrimRight(string(raw), "\x00 ")
	return ChipInfo{Raw: hexBytes(raw), Text: trimmed}, nil
}

// FlashInfo is the decoded response to RKU_ReadFlashInfo, matching
// STRUCT_FLASHINFO_CMD (RKDevice.h).
type FlashInfo struct {
	FlashSizeSectors uint32 `json:"flashSizeSectors"`
	FlashSizeBytes   uint64 `json:"flashSizeBytes"`
	BlockSizeSectors uint16 `json:"blockSizeSectors"`
	BlockSizeBytes   uint32 `json:"blockSizeBytes"`
	PageSizeSectors  uint8  `json:"pageSizeSectors"`
	PageSizeBytes    uint32 `json:"pageSizeBytes"`
	ECCBits          uint8  `json:"eccBits"`
	AccessTime       uint8  `json:"accessTime"`
	ManufacturerCode uint8  `json:"manufacturerCode"`
	Manufacturer     string `json:"manufacturer"`
	FlashCS          uint8  `json:"flashCS"`
	FlashCSList      []int  `json:"flashCSList"`
	Raw              string `json:"rawHex"`
}

// sectorSize is rkdeveloptool's assumed 512-byte sector (it divides flash size
// by 2 to report KiB).
const sectorSize = 512

// manufacturerNames mirrors rkdeveloptool's szManufName table (RKDevice.cpp).
var manufacturerNames = []string{
	"SAMSUNG", "TOSHIBA", "HYNIX", "INFINEON", "MICRON", "RENESAS", "ST", "INTEL",
}

// ParseFlashInfo decodes the 11-byte flash info structure.
func ParseFlashInfo(b []byte) (FlashInfo, error) {
	if len(b) < 11 {
		return FlashInfo{}, fmt.Errorf("rockusb: flash info needs 11 bytes, got %d", len(b))
	}
	info := FlashInfo{
		FlashSizeSectors: binary.LittleEndian.Uint32(b[0:4]),
		BlockSizeSectors: binary.LittleEndian.Uint16(b[4:6]),
		PageSizeSectors:  b[6],
		ECCBits:          b[7],
		AccessTime:       b[8],
		ManufacturerCode: b[9],
		FlashCS:          b[10],
		Raw:              hexBytes(b[:11]),
	}
	info.FlashSizeBytes = uint64(info.FlashSizeSectors) * sectorSize
	info.BlockSizeBytes = uint32(info.BlockSizeSectors) * sectorSize
	info.PageSizeBytes = uint32(info.PageSizeSectors) * sectorSize
	if int(info.ManufacturerCode) < len(manufacturerNames) {
		info.Manufacturer = manufacturerNames[info.ManufacturerCode]
	} else {
		info.Manufacturer = "UNKNOWN"
	}
	info.FlashCSList = bitIndices(uint32(info.FlashCS), 8)
	return info, nil
}

// Capability is the decoded response to RKU_ReadCapability. rkdeveloptool
// prints capability[0] bits 0..3 and capability[1] bit 0; the remaining bits
// are exposed raw because the upstream tests (20/40/80) look like a decimal
// typo for 0x20/0x40/0x80 and are not trusted here.
type Capability struct {
	Raw           string   `json:"rawHex"`
	Bytes         []uint8  `json:"bytes"`
	DirectLBA     bool     `json:"directLBA"`
	VendorStore   bool     `json:"vendorStorage"`
	First4MAccess bool     `json:"first4MAccess"`
	ReadLBA       bool     `json:"readLBA"`
	NewIDB        bool     `json:"newIDB"`
	Flags         []string `json:"flags"`
}

// ParseCapability decodes the 8-byte capability bitmap.
func ParseCapability(b []byte) (Capability, error) {
	if len(b) < 8 {
		return Capability{}, fmt.Errorf("rockusb: capability needs 8 bytes, got %d", len(b))
	}
	cap := Capability{
		Raw:           hexBytes(b[:8]),
		Bytes:         []uint8{b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7]},
		DirectLBA:     b[0]&0x01 != 0,
		VendorStore:   b[0]&0x02 != 0,
		First4MAccess: b[0]&0x04 != 0,
		ReadLBA:       b[0]&0x08 != 0,
		NewIDB:        b[1]&0x01 != 0,
	}
	if cap.DirectLBA {
		cap.Flags = append(cap.Flags, "Direct LBA")
	}
	if cap.VendorStore {
		cap.Flags = append(cap.Flags, "Vendor Storage")
	}
	if cap.First4MAccess {
		cap.Flags = append(cap.Flags, "First 4M Access")
	}
	if cap.ReadLBA {
		cap.Flags = append(cap.Flags, "Read LBA")
	}
	if cap.NewIDB {
		cap.Flags = append(cap.Flags, "New IDB")
	}
	return cap, nil
}

// Storage is the decoded response to RKU_ReadStorageList / READ_STORAGE: a
// 32-bit bitmap of available media plus the currently selected one.
type Storage struct {
	Raw         string   `json:"rawHex"`
	Bitmap      uint32   `json:"bitmap"`
	Available   []int    `json:"available"`
	Names       []string `json:"names"`
	Current     int      `json:"current"` // -1 when no bit is set
	CurrentName string   `json:"currentName"`
}

// storageNames follows rkdeveloptool's `cs` help text (1=EMMC, 2=SD,
// 9=SPINOR); ids Rockchip has never documented are reported as unknown.
var storageNames = map[int]string{
	1: "eMMC",
	2: "SD",
	3: "NAND",
	9: "SPI NOR",
}

// StorageName maps a storage id to its rkdeveloptool name.
func StorageName(id int) string {
	if n, ok := storageNames[id]; ok {
		return n
	}
	return fmt.Sprintf("UNKNOWN(%d)", id)
}

// ParseStorage decodes the 4-byte storage bitmap. The device reports available
// media as set bits; the lowest set bit is the active medium (this is exactly
// how rkdeveloptool's RKU_ReadStorage picks *storage).
func ParseStorage(b []byte) (Storage, error) {
	if len(b) < 4 {
		return Storage{}, fmt.Errorf("rockusb: storage list needs 4 bytes, got %d", len(b))
	}
	bitmap := binary.LittleEndian.Uint32(b[0:4])
	st := Storage{
		Raw:       hexBytes(b[:4]),
		Bitmap:    bitmap,
		Available: bitIndices(bitmap, 32),
		Current:   -1,
	}
	for _, id := range st.Available {
		st.Names = append(st.Names, StorageName(id))
	}
	if len(st.Available) > 0 {
		st.Current = st.Available[0]
		st.CurrentName = StorageName(st.Current)
	}
	return st, nil
}

// MediaMode is the rockusb operating mode a device reports.
type MediaMode int

const (
	ModeUnknown MediaMode = iota
	ModeMaskrom
	ModeLoader
)

func (m MediaMode) String() string {
	switch m {
	case ModeMaskrom:
		return "MASKROM"
	case ModeLoader:
		return "LOADER"
	default:
		return "UNKNOWN"
	}
}

// ModeFromDescriptor reproduces rkdeveloptool's mode detection (RKScan.cpp):
// for VID 0x2207, bit 0 of bcdUSB is cleared in MASKROM and set in LOADER.
// bcdUSB must be the packed value (major<<8 | minor<<4 | subminor).
func ModeFromDescriptor(vendorID uint16, bcdUSB uint32) MediaMode {
	if vendorID != RockchipVendorID {
		return ModeUnknown
	}
	if bcdUSB&0x0001 == 0 {
		return ModeMaskrom
	}
	return ModeLoader
}

// PackBCDUSB packs WebUSB's usbVersion triple into the raw bcdUSB value.
func PackBCDUSB(major, minor, subminor uint8) uint32 {
	return uint32(major)<<8 | uint32(minor)<<4 | uint32(subminor)
}

func bitIndices(bitmap uint32, width int) []int {
	out := make([]int, 0, 8)
	for i := 0; i < width; i++ {
		if bitmap&(1<<uint(i)) != 0 {
			out = append(out, i)
		}
	}
	return out
}

func hexBytes(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*3)
	for i, v := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[v>>4], digits[v&0x0f])
	}
	return string(out)
}

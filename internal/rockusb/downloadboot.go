package rockusb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// Download Boot ("db" in upgrade_tool) is how a bare MASKROM device is given a
// working usbplug: the host pushes the loader's DDR init and usbplug segments
// into device RAM over EP0 vendor control transfers. Only after that does the
// device re-enumerate as LOADER and start answering rockusb CBWs.
//
// Everything below is transcribed from Rockchip's GPL-2.0 rkdeveloptool:
//   - RKBoot.h / RKBoot.cpp: the RKBOOT ("BOOT" / "LDR ") container.
//   - RKComm.cpp:811-882 (RKU_DeviceRequest): the vendor transfer, its chunk
//     size and its CRC.
//   - RKDevice.cpp:510-580 (CRKDevice::DownloadBoot): 0x0471 entries first,
//     then 0x0472 entries, then a one second settle.
//
// The M0 probe shipped no loader and no vendor requests; this stage implements
// the download path but still never writes flash. A loader image is supplied
// by the user from their own machine and is never redistributed here.

// RKBOOT container constants (RKBoot.h, boot_merger.h).
const (
	// LoaderHeaderSize is sizeof(STRUCT_RKBOOT_HEAD): 102 bytes packed.
	LoaderHeaderSize = 102
	// LoaderEntrySize is sizeof(STRUCT_RKBOOT_ENTRY): 57 bytes packed.
	LoaderEntrySize = 57
	// LoaderTagBoot is "BOOT" (0x544F4F42 little-endian).
	LoaderTagBoot uint32 = 0x544F4F42
	// LoaderTagLoader is "LDR " (0x2052444C); upstream accepts both tags.
	LoaderTagLoader uint32 = 0x2052444C
	// RK3588ChipTag is boot_merger's convertChipType("RK3588"): the four
	// ASCII bytes "3588" packed big-endian ('3'<<24|'5'<<16|'8'<<8|'8').
	RK3588ChipTag uint32 = 0x33353838
	// MinLoaderSize is a header, one entry and the trailing CRC.
	MinLoaderSize = LoaderHeaderSize + LoaderEntrySize + 4
	// MaxLoaderSize keeps a corrupt or mistyped file from being streamed to
	// the device. Real RK3588 loaders are ~140 KiB.
	MaxLoaderSize = 16 << 20
	// DBEntry471 is the code471 entry type (DDR init / USB head).
	DBEntry471 uint32 = 1
	// DBEntry472 is the code472 entry type (usbplug).
	DBEntry472 uint32 = 2
	// DBSettle is the sleep upstream does after the last segment.
	DBSettle = time.Second
)

// LoaderEntry is one segment listed in an RKBOOT container.
type LoaderEntry struct {
	Type       uint32 `json:"type"`
	Name       string `json:"name"`
	DataOffset uint32 `json:"dataOffset"`
	DataSize   uint32 `json:"dataSize"`
	DataDelay  uint32 `json:"dataDelayMs"`
}

// LoaderImage is a parsed RKBOOT container. The caller owns the bytes and must
// not mutate them while the image is in use.
type LoaderImage struct {
	Tag           uint32        `json:"tag"`
	TagName       string        `json:"tagName"`
	Version       uint32        `json:"version"`
	MergeVersion  uint32        `json:"mergeVersion"`
	ChipType      uint32        `json:"chipType"`
	SignFlag      uint8         `json:"signFlag"`
	RC4Flag       uint8         `json:"rc4Flag"`
	CRCStored     uint32        `json:"crcStored"`
	CRCCalculated uint32        `json:"crcCalculated"`
	Entries471    []LoaderEntry `json:"entries471"`
	Entries472    []LoaderEntry `json:"entries472"`
	Size          int           `json:"size"`

	data []byte
}

// LoaderErrorKind classifies why a loader file was rejected. The UI turns this
// into a specific message; a bare "invalid loader" would hide the difference
// between a truncated download and a file built for another chip.
type LoaderErrorKind int

const (
	LoaderEmpty LoaderErrorKind = iota + 1
	LoaderTooShort
	LoaderTooLarge
	LoaderBadTag
	LoaderBadCRC
	LoaderBadEntry
	LoaderChipMismatch
	LoaderNoBootEntries
)

func (k LoaderErrorKind) String() string {
	switch k {
	case LoaderEmpty:
		return "LOADER_EMPTY"
	case LoaderTooShort:
		return "LOADER_TOO_SHORT"
	case LoaderTooLarge:
		return "LOADER_TOO_LARGE"
	case LoaderBadTag:
		return "LOADER_BAD_TAG"
	case LoaderBadCRC:
		return "LOADER_BAD_CRC"
	case LoaderBadEntry:
		return "LOADER_BAD_ENTRY"
	case LoaderChipMismatch:
		return "LOADER_CHIP_MISMATCH"
	case LoaderNoBootEntries:
		return "LOADER_NO_BOOT_ENTRIES"
	default:
		return fmt.Sprintf("LOADER_ERROR(%d)", int(k))
	}
}

// LoaderError reports an unusable loader file.
type LoaderError struct {
	Kind   LoaderErrorKind
	Detail string
}

func (e *LoaderError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Detail)
}

func loaderErrf(kind LoaderErrorKind, format string, args ...any) *LoaderError {
	return &LoaderError{Kind: kind, Detail: fmt.Sprintf(format, args...)}
}

// ParseLoaderImage validates and indexes an RKBOOT file. It deliberately
// re-checks the same things upstream does (tag, CRC-32, entry bounds) plus a
// few bounds upstream leaves to chance, because the bytes here come from a
// user's file picker rather than from a trusted path.
func ParseLoaderImage(data []byte) (*LoaderImage, error) {
	switch {
	case len(data) == 0:
		return nil, loaderErrf(LoaderEmpty, "loader file is empty")
	case len(data) < MinLoaderSize:
		return nil, loaderErrf(LoaderTooShort, "loader file is %d bytes, need at least %d", len(data), MinLoaderSize)
	case len(data) > MaxLoaderSize:
		return nil, loaderErrf(LoaderTooLarge, "loader file is %d bytes, over the %d byte sanity limit", len(data), MaxLoaderSize)
	}

	img := &LoaderImage{
		Tag:  binary.LittleEndian.Uint32(data[0:4]),
		Size: len(data),
	}
	switch img.Tag {
	case LoaderTagBoot:
		img.TagName = "BOOT"
	case LoaderTagLoader:
		img.TagName = "LDR "
	default:
		return nil, loaderErrf(LoaderBadTag, "tag 0x%08X is neither BOOT (0x%08X) nor LDR (0x%08X)", img.Tag, LoaderTagBoot, LoaderTagLoader)
	}

	img.Version = binary.LittleEndian.Uint32(data[6:10])
	img.MergeVersion = binary.LittleEndian.Uint32(data[10:14])
	img.ChipType = binary.LittleEndian.Uint32(data[21:25])
	img.SignFlag = data[43]
	img.RC4Flag = data[44]

	// Upstream CrcCheck(): CRC-32 of everything but the last four bytes.
	img.CRCStored = binary.LittleEndian.Uint32(data[len(data)-4:])
	img.CRCCalculated = CRC32(data[:len(data)-4])
	if img.CRCStored != img.CRCCalculated {
		return nil, loaderErrf(LoaderBadCRC, "stored CRC 0x%08X != calculated 0x%08X (truncated file?)", img.CRCStored, img.CRCCalculated)
	}

	entries471, err := parseEntryList(data, "code471", data[25], binary.LittleEndian.Uint32(data[26:30]), data[30], DBEntry471)
	if err != nil {
		return nil, err
	}
	entries472, err := parseEntryList(data, "code472", data[31], binary.LittleEndian.Uint32(data[32:36]), data[36], DBEntry472)
	if err != nil {
		return nil, err
	}
	if len(entries471) == 0 && len(entries472) == 0 {
		return nil, loaderErrf(LoaderNoBootEntries, "loader lists no 0x0471 or 0x0472 segment")
	}

	// A loader built for a different chip would be accepted by the BootROM
	// and then fail in a way that looks like a USB problem. Catch it first.
	if img.ChipType != 0 && img.ChipType != RK3588ChipTag {
		return nil, loaderErrf(LoaderChipMismatch, "loader is built for chip 0x%08X (%s), not RK3588 (0x%08X)",
			img.ChipType, chipTagString(img.ChipType), RK3588ChipTag)
	}

	img.Entries471 = entries471
	img.Entries472 = entries472
	img.data = data
	return img, nil
}

func parseEntryList(data []byte, label string, count uint8, offset uint32, stride uint8, wantType uint32) ([]LoaderEntry, error) {
	if count == 0 {
		return nil, nil
	}
	if stride < LoaderEntrySize {
		return nil, loaderErrf(LoaderBadEntry, "%s entry size %d is smaller than the %d byte struct", label, stride, LoaderEntrySize)
	}
	end := uint64(offset) + uint64(count)*uint64(stride)
	if offset < LoaderHeaderSize || end > uint64(len(data)) {
		return nil, loaderErrf(LoaderBadEntry, "%s table [%d,%d) is outside the %d byte file", label, offset, end, len(data))
	}
	out := make([]LoaderEntry, 0, count)
	for i := 0; i < int(count); i++ {
		base := int(offset) + i*int(stride)
		entry := LoaderEntry{
			Type:       binary.LittleEndian.Uint32(data[base+1 : base+5]),
			Name:       decodeUTF16Name(data[base+5 : base+45]),
			DataOffset: binary.LittleEndian.Uint32(data[base+45 : base+49]),
			DataSize:   binary.LittleEndian.Uint32(data[base+49 : base+53]),
			DataDelay:  binary.LittleEndian.Uint32(data[base+53 : base+57]),
		}
		if entry.Type != wantType {
			return nil, loaderErrf(LoaderBadEntry, "%s entry %d has type %d, want %d", label, i, entry.Type, wantType)
		}
		if entry.DataSize == 0 {
			return nil, loaderErrf(LoaderBadEntry, "%s entry %d (%q) has zero length", label, i, entry.Name)
		}
		segEnd := uint64(entry.DataOffset) + uint64(entry.DataSize)
		if entry.DataOffset < LoaderHeaderSize || segEnd > uint64(len(data)) {
			return nil, loaderErrf(LoaderBadEntry, "%s entry %d (%q) data [%d,%d) is outside the %d byte file",
				label, i, entry.Name, entry.DataOffset, segEnd, len(data))
		}
		out = append(out, entry)
	}
	return out, nil
}

// decodeUTF16Name mirrors upstream WCHAR_To_char: each uint16's low byte is
// the ASCII name, NUL terminated.
func decodeUTF16Name(raw []byte) string {
	cells := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		cells = append(cells, binary.LittleEndian.Uint16(raw[i:i+2]))
	}
	runes := utf16.Decode(cells)
	for i, r := range runes {
		if r == 0 {
			runes = runes[:i]
			break
		}
	}
	return strings.TrimSpace(string(runes))
}

func chipTagString(tag uint32) string {
	b := []byte{byte(tag >> 24), byte(tag >> 16), byte(tag >> 8), byte(tag)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return "?"
		}
	}
	return string(b)
}

// Segment returns the bytes of one entry.
func (img *LoaderImage) Segment(entry LoaderEntry) []byte {
	end := uint64(entry.DataOffset) + uint64(entry.DataSize)
	if end > uint64(len(img.data)) {
		return nil
	}
	return img.data[entry.DataOffset:end]
}

// Plan returns the transfer order: every 0x0471 segment in file order, then
// every 0x0472 segment (RKDevice.cpp:510).
func (img *LoaderImage) Plan() []DBSegment {
	out := make([]DBSegment, 0, len(img.Entries471)+len(img.Entries472))
	for _, e := range img.Entries471 {
		out = append(out, DBSegment{Request: RequestIndex471, Entry: e})
	}
	for _, e := range img.Entries472 {
		out = append(out, DBSegment{Request: RequestIndex472, Entry: e})
	}
	return out
}

// DBSegment is one loader entry paired with the vendor request carrying it.
type DBSegment struct {
	Request uint16      `json:"request"`
	Entry   LoaderEntry `json:"entry"`
}

// Summary is the human-readable description the UI shows after a file pick.
func (img *LoaderImage) Summary() []string {
	lines := []string{
		fmt.Sprintf("Loader: tag=%s version=0x%08X merge=0x%08X chip=0x%08X (%s) size=%d B",
			img.TagName, img.Version, img.MergeVersion, img.ChipType, chipTagString(img.ChipType), img.Size),
		fmt.Sprintf("CRC-32: 0x%08X (verified)", img.CRCStored),
	}
	for _, seg := range img.Plan() {
		lines = append(lines, fmt.Sprintf("  0x%04X %-28s %7d B (offset %d, delay %d ms)",
			seg.Request, seg.Entry.Name, seg.Entry.DataSize, seg.Entry.DataOffset, seg.Entry.DataDelay))
	}
	return lines
}

// VendorPlan is one segmented vendor request ready to be sent over EP0.
type VendorPlan struct {
	Request          uint16   `json:"request"`
	PayloadSize      int      `json:"payloadSize"`
	DataSize         int      `json:"dataSize"`
	CRCCCITT         uint16   `json:"crcCCITT"`
	Packets          [][]byte `json:"-"`
	PacketSizes      []int    `json:"packetSizes"`
	PendingZeroWrite bool     `json:"pendingZeroWrite"`
}

// PlanVendorRequest reproduces RKU_DeviceRequest (RKComm.cpp:811) exactly,
// including two quirks:
//
//   - a payload of 4095 mod 4096 bytes is padded with one byte so the CRC
//     starts in its own packet;
//   - a payload of 4094 mod 4096 bytes is followed by an extra one-byte
//     control transfer that upstream expects to write zero bytes.
//
// Neither quirk is reachable with the entry sizes of a stock RK3588 loader
// (2048 / 51200 / 88064 all leave 2048 mod 4096), but they are part of the
// protocol and are covered by unit tests so a future segment size cannot
// silently diverge.
func PlanVendorRequest(request uint16, payload []byte) (VendorPlan, error) {
	if request != RequestIndex471 && request != RequestIndex472 {
		return VendorPlan{}, fmt.Errorf("vendor request 0x%04X is not a Download Boot index (want 0x0471 or 0x0472)", request)
	}
	if len(payload) == 0 {
		return VendorPlan{}, fmt.Errorf("vendor request 0x%04X needs a non-empty payload", request)
	}

	plan := VendorPlan{Request: request, PayloadSize: len(payload)}
	buf := make([]byte, len(payload)+3) // +1 pad byte, +2 CRC, as upstream allocates +5
	copy(buf, payload)

	size := len(payload)
	switch size % VendorChunkSize {
	case VendorChunkSize - 1:
		size++
	case VendorChunkSize - 2:
		plan.PendingZeroWrite = true
	}

	crc := CRCCCITT(buf[:size])
	plan.CRCCCITT = crc
	buf[size] = byte(crc >> 8) // upstream writes the high byte first
	buf[size+1] = byte(crc)
	size += 2
	plan.DataSize = size

	for sent := 0; sent < size; {
		n := size - sent
		if n > VendorChunkSize {
			n = VendorChunkSize
		}
		packet := make([]byte, n)
		copy(packet, buf[sent:sent+n])
		plan.Packets = append(plan.Packets, packet)
		plan.PacketSizes = append(plan.PacketSizes, n)
		sent += n
	}
	return plan, nil
}

// DBErrorKind classifies a failed Download Boot. A single "-1" from the
// BootROM is useless on its own: "BootROM refused", "device is not in
// maskrom" and "loader does not match" need different user actions, so the
// error carries which one happened.
type DBErrorKind int

const (
	DBNotMaskrom DBErrorKind = iota + 1
	DBBootromRejected
	DBTransferFailed
	DBLoaderInvalid
	DBLoaderChipMismatch
)

func (k DBErrorKind) String() string {
	switch k {
	case DBNotMaskrom:
		return "DB_NOT_MASKROM"
	case DBBootromRejected:
		return "DB_BOOTROM_REJECTED"
	case DBTransferFailed:
		return "DB_TRANSFER_FAILED"
	case DBLoaderInvalid:
		return "DB_LOADER_INVALID"
	case DBLoaderChipMismatch:
		return "DB_LOADER_CHIP_MISMATCH"
	default:
		return fmt.Sprintf("DB_ERROR(%d)", int(k))
	}
}

// DBError is a Download Boot failure with enough context to tell the three
// real-world causes apart.
type DBError struct {
	Kind     DBErrorKind `json:"kind"`
	Code     ErrorCode   `json:"code"`
	CodeName string      `json:"codeName"`
	Detail   string      `json:"detail"`
	Request  uint16      `json:"request,omitempty"`
	Entry    int         `json:"entry"`
	Offset   int         `json:"offset"`
	Packet   int         `json:"packet"`
	Err      error       `json:"-"`
}

func (e *DBError) Error() string {
	return fmt.Sprintf("%s (%s): %s", e.Kind, e.Code, e.Detail)
}

func (e *DBError) Unwrap() error { return e.Err }

// Hint is the one-line next step shown to the user.
func (e *DBError) Hint() string {
	switch e.Kind {
	case DBNotMaskrom:
		return "设备不在 maskrom：Download Boot 只能在裸 maskrom 下执行；loader 态直接做 cd/sd/只读信息。"
	case DBBootromRejected:
		return "BootROM 拒绝了首包（0x0471 index 0）。先确认设备确为 maskrom（bcdUSB bit0=0），再确认 loader 与芯片匹配；不是 USB 线或浏览器的问题。"
	case DBTransferFailed:
		return "首包之后的数据传输失败：loader 文件可能被截断，或设备中途掉出总线；重新进 maskrom 后再试。"
	case DBLoaderInvalid:
		return "loader 文件无法解析（RKBOOT tag / CRC-32 / 段表越界）：确认选的是 rk3588_spl_loader_*.bin，而不是解压失败的半截文件。"
	case DBLoaderChipMismatch:
		return "loader 是为其他芯片构建的：RK3588 需要 chip=0x33353838（\"3588\"）的 loader。"
	default:
		return ""
	}
}

// AsDBError narrows a DownloadBoot failure to its classified form.
func AsDBError(err error) *DBError {
	var d *DBError
	if errors.As(err, &d) {
		return d
	}
	return nil
}

// DBOptions tunes a Download Boot run.
type DBOptions struct {
	// Progress is called before each segment and after the settle.
	Progress func(DBEvent)
}

// DBEvent is one progress step of a Download Boot run.
type DBEvent struct {
	Kind       string `json:"kind"` // segment / packet / settle / done
	Request    uint16 `json:"request,omitempty"`
	Entry      int    `json:"entry"`
	Name       string `json:"name,omitempty"`
	Offset     int    `json:"offset"`
	Length     int    `json:"length"`
	Packet     int    `json:"packet"`
	PacketSize int    `json:"packetSize"`
	Total      int    `json:"total,omitempty"`
	Sent       int    `json:"sent,omitempty"`
	Note       string `json:"note,omitempty"`
}

// DBReport summarises a successful Download Boot.
type DBReport struct {
	Segments    int      `json:"segments"`
	Segments471 int      `json:"segments471"`
	Segments472 int      `json:"segments472"`
	Packets     int      `json:"packets"`
	Bytes       int      `json:"bytes"`
	CRCs        []uint16 `json:"crcs"`
	DurationMs  float64  `json:"durationMs"`
	Names       []string `json:"names"`
}

// DownloadBoot pushes a parsed loader into maskrom RAM. It never touches flash
// and it deliberately does not retry: a partially delivered loader cannot be
// resumed, so the only safe recovery is to re-enter maskrom.
func (s *Session) DownloadBoot(img *LoaderImage, opts DBOptions) (*DBReport, error) {
	start := s.opts.now()
	if s.Link.Mode != ModeMaskrom {
		return nil, &DBError{
			Kind: DBNotMaskrom, Code: ErrDeviceNotSupport, CodeName: ErrDeviceNotSupport.String(),
			Entry: -1, Packet: -1,
			Detail: fmt.Sprintf("device mode is %s, Download Boot requires MASKROM", s.Link.Mode),
		}
	}
	if img == nil {
		return nil, &DBError{
			Kind: DBLoaderInvalid, Code: ErrFailed, CodeName: ErrFailed.String(),
			Entry: -1, Packet: -1, Detail: "no loader image was supplied",
		}
	}

	progress := opts.Progress
	if progress == nil {
		progress = func(DBEvent) {}
	}

	plan := img.Plan()
	report := &DBReport{Segments: len(plan), Segments471: len(img.Entries471), Segments472: len(img.Entries472)}
	segmentIndex := 0
	for _, seg := range plan {
		data := img.Segment(seg.Entry)
		if len(data) == 0 {
			return nil, &DBError{
				Kind: DBLoaderInvalid, Code: ErrFailed, CodeName: ErrFailed.String(),
				Request: seg.Request, Entry: segmentIndex, Packet: -1,
				Detail: fmt.Sprintf("segment %q has no data in the file", seg.Entry.Name),
			}
		}
		vplan, err := PlanVendorRequest(seg.Request, data)
		if err != nil {
			return nil, &DBError{
				Kind: DBLoaderInvalid, Code: ErrFailed, CodeName: ErrFailed.String(),
				Request: seg.Request, Entry: segmentIndex, Packet: -1, Err: err, Detail: err.Error(),
			}
		}

		report.Names = append(report.Names, seg.Entry.Name)
		report.CRCs = append(report.CRCs, vplan.CRCCCITT)
		progress(DBEvent{
			Kind: "segment", Request: seg.Request, Entry: segmentIndex, Name: seg.Entry.Name,
			Offset: int(seg.Entry.DataOffset), Length: len(data), Total: vplan.DataSize,
		})

		offset := 0
		for packetIndex, packet := range vplan.Packets {
			ok := s.vendorOut(vplan.Request, packet, &DBErrorContext{
				request: vplan.Request, entry: segmentIndex, packet: packetIndex, offset: offset,
				firstPacket: segmentIndex == 0 && packetIndex == 0,
			})
			if ok != nil {
				return nil, ok
			}
			report.Packets++
			report.Bytes += len(packet)
			progress(DBEvent{
				Kind: "packet", Request: vplan.Request, Entry: segmentIndex, Name: seg.Entry.Name,
				Offset: offset, Length: len(packet), Packet: packetIndex, PacketSize: len(packet),
			})
			offset += len(packet)
		}

		if vplan.PendingZeroWrite {
			if dbErr := s.vendorOutPendingZero(vplan.Request, &DBErrorContext{
				request: vplan.Request, entry: segmentIndex, packet: len(vplan.Packets), offset: offset,
			}); dbErr != nil {
				return nil, dbErr
			}
		}

		if seg.Entry.DataDelay > 0 {
			s.opts.sleep(time.Duration(seg.Entry.DataDelay) * time.Millisecond)
		}
		segmentIndex++
	}

	progress(DBEvent{Kind: "settle", Entry: -1, Note: fmt.Sprintf("waiting %s for the device to re-enumerate", DBSettle)})
	s.opts.sleep(DBSettle)
	progress(DBEvent{Kind: "done", Entry: -1, Length: report.Bytes, Sent: report.Packets})
	report.DurationMs = msSince(start, s.opts.now())
	return report, nil
}

// DBErrorContext locates a failed vendor transfer inside the download.
type DBErrorContext struct {
	request     uint16
	entry       int
	packet      int
	offset      int
	firstPacket bool
}

func (s *Session) vendorOut(request uint16, data []byte, ctx *DBErrorContext) *DBError {
	start := s.opts.now()
	n, _, err := s.call(fmt.Sprintf("vendor request 0x%04X", request), func() (int, []byte, error) {
		written, werr := s.transport.ControlOut(RequestTypeVendorOut, RequestDeviceRequest, 0, request, data)
		return written, nil, werr
	})
	s.trace(Transfer{
		Kind: "control-out", Endpoint: -1, Direction: "out",
		RequestType: int(RequestTypeVendorOut), Request: int(RequestDeviceRequest),
		Value: 0, Index: int(request),
		RequestedLength: len(data), Length: n, Status: statusFor(err, n == len(data)),
		DataHex: hexBytes(limit(data, 64)), DurationMs: msSince(start, s.opts.now()),
		Op: -1, Stage: "boot",
		Note: noteForDB(ctx, len(data), err),
	})
	if err != nil {
		return classifyDBError(err, ctx, len(data), true)
	}
	if n != len(data) {
		return classifyDBError(fmt.Errorf("control OUT accepted %d of %d bytes", n, len(data)), ctx, len(data), false)
	}
	return nil
}

func (s *Session) vendorOutPendingZero(request uint16, ctx *DBErrorContext) *DBError {
	start := s.opts.now()
	n, _, err := s.call(fmt.Sprintf("vendor request 0x%04X pending packet", request), func() (int, []byte, error) {
		written, werr := s.transport.ControlOut(RequestTypeVendorOut, RequestDeviceRequest, 0, request, []byte{0})
		return written, nil, werr
	})
	s.trace(Transfer{
		Kind: "control-out", Endpoint: -1, Direction: "out",
		RequestType: int(RequestTypeVendorOut), Request: int(RequestDeviceRequest),
		Value: 0, Index: int(request),
		RequestedLength: 1, Length: n,
		Status:  statusFor(err, n == 0),
		DataHex: "00", DurationMs: msSince(start, s.opts.now()),
		Op: -1, Stage: "boot", Note: "trailing pending packet (upstream expects bytesWritten=0)",
	})
	if err != nil {
		return classifyDBError(err, ctx, 1, false)
	}
	if n != 0 {
		return classifyDBError(fmt.Errorf("pending packet wrote %d bytes, upstream expects 0", n), ctx, 1, false)
	}
	return nil
}

func classifyDBError(err error, ctx *DBErrorContext, length int, transportErr bool) *DBError {
	code := ErrRequestFail
	if isTimeout(err) {
		code = ErrTimeout
	}
	kind := DBTransferFailed
	detail := fmt.Sprintf("vendor request 0x%04X entry %d packet %d (offset %d, %d bytes): %v",
		ctx.request, ctx.entry, ctx.packet, ctx.offset, length, err)
	if ctx.firstPacket && transportErr {
		// The stage-2 hardware finding: a bare maskrom refuses the very
		// first packet with a USB transfer error. A short write instead
		// means the device took the packet and stopped, which is a different
		// problem and must not be reported as "BootROM rejected".
		kind = DBBootromRejected
		detail = fmt.Sprintf("BootROM rejected the first packet of 0x0471 entry0 (offset 0, %d bytes): %v", length, err)
	}
	return &DBError{Kind: kind, Code: code, CodeName: code.String(), Request: ctx.request, Entry: ctx.entry, Packet: ctx.packet, Offset: ctx.offset, Err: err, Detail: detail}
}

func statusFor(err error, complete bool) string {
	switch {
	case err != nil:
		return "error"
	case !complete:
		return "short"
	default:
		return "ok"
	}
}

func noteForDB(ctx *DBErrorContext, length int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("entry %d packet %d offset %d", ctx.entry, ctx.packet, ctx.offset)
}

// VerifyLoaderFile is the UI's cheap pre-flight: it parses the file and
// returns the summary lines or the classified error, without touching a
// device.
func VerifyLoaderFile(data []byte) (*LoaderImage, []string, error) {
	img, err := ParseLoaderImage(data)
	if err != nil {
		return nil, nil, err
	}
	return img, img.Summary(), nil
}

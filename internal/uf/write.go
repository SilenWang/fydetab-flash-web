package uf

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
)

// Op is a rockusb opcode used by the write path. These are declared here, not
// in internal/rockusb, because that package's command table is the read-only
// allow-list that keeps the M1 page from ever writing flash.
//
// Values and CBW shapes are from rkdeveloptool's USB_OPERATION_CODE and
// CRKUsbComm::InitializeCBW (RKComm.h, RKComm.cpp:185-233).
type Op uint8

const (
	// OpTestUnitReady is reused by the state machine's "Test Device" phase.
	OpTestUnitReady Op = 0x00
	// OpReadLBA reads flash sectors: READ_LBA. It is a read command, but it
	// carries an address and a length, so it lives here next to WRITE_LBA
	// rather than in rockusb's address-free info-command table.
	OpReadLBA Op = 0x14
	// OpWriteLBA writes data to flash sectors: WRITE_LBA.
	OpWriteLBA Op = 0x15
	// OpEraseLBA erases flash sectors: ERASE_LBA.
	OpEraseLBA Op = 0x25
	// OpDeviceReset resets the device: DEVICE_RESET.
	OpDeviceReset Op = 0xFF
)

// Name is the rkdeveloptool API name, so a trace can be diffed against an
// upstream log without translation.
func (o Op) Name() string {
	switch o {
	case OpTestUnitReady:
		return "RKU_TestDeviceReady"
	case OpReadLBA:
		return "RKU_ReadLBA"
	case OpWriteLBA:
		return "RKU_WriteLBA"
	case OpEraseLBA:
		return "RKU_EraseLBA"
	case OpDeviceReset:
		return "RKU_DeviceReset"
	default:
		return fmt.Sprintf("UNKNOWN_OP_0x%02X", uint8(o))
	}
}

const (
	// SectorSize is the unit of every LBA in this protocol.
	SectorSize = 512
	// MaxTransferSectors is upstream's DEFAULT_RW_LBA: one WRITE_LBA data
	// stage carries at most 128 sectors (64 KiB). Larger writes are split.
	MaxTransferSectors = 128
	// MaxEraseSectors mirrors erase_partition in main.cpp: 1024*32 sectors
	// (16 MiB) per ERASE_LBA command.
	MaxEraseSectors = 1024 * 32
	// MaxLBA is the largest addressable sector: dwAddress is a DWORD.
	MaxLBA = 0xFFFFFFFF
	// ResetSubcodeNone is RST_NONE_SUBCODE.
	ResetSubcodeNone byte = 0
	// methodImage is RWMETHOD_IMAGE, the only write method upgrade_tool uses
	// for uf. RWMETHOD_LBA exists upstream for other flows.
	methodImage byte = 0
)

// EncodeCommandCBW renders the 31-byte CBW for one write-path command.
//
// The layout follows RKComm.h's packed CBW and InitializeCBW():
//
//	0..3   signature "USBC"
//	4..7   tag (CSW must echo it)
//	8..11  transfer length in bytes
//	12     flags (0x00 = host-to-device)
//	13     LUN
//	14     cbwcb length: 0x0a for WRITE_LBA/ERASE_LBA, 0x06 for TEST_UNIT_READY
//	       and DEVICE_RESET
//	15     opcode
//	16     ucReserved: RWMETHOD_IMAGE (write) / RST_NONE_SUBCODE (reset)
//	17..20 dwAddress: start LBA, big-endian
//	22..23 usLength: sector count, big-endian
//
// ERASE_LBA carries dwAddress/usLength but no data stage, so its transfer
// length stays zero.
func EncodeCommandCBW(tag uint32, op Op, lba uint32, sectors uint16, dataLength uint32, subCode byte) ([]byte, error) {
	b := make([]byte, rockusb.CBWLength)
	binary.LittleEndian.PutUint32(b[0:4], rockusb.CBWSignature)
	binary.LittleEndian.PutUint32(b[4:8], tag)
	b[13] = 0 // LUN

	switch op {
	case OpReadLBA:
		if sectors == 0 {
			return nil, fmt.Errorf("uf: READ_LBA needs at least one sector")
		}
		if sectors > MaxTransferSectors {
			return nil, fmt.Errorf("uf: READ_LBA of %d sectors exceeds the %d-sector transfer limit", sectors, MaxTransferSectors)
		}
		if int64(dataLength) != int64(sectors)*SectorSize {
			return nil, fmt.Errorf("uf: READ_LBA data length %d does not match %d sectors", dataLength, sectors)
		}
		b[12] = rockusb.DirectionIn
		b[14] = 0x0a
		b[16] = subCode
		binary.LittleEndian.PutUint32(b[8:12], dataLength)
	case OpWriteLBA:
		if sectors == 0 {
			return nil, fmt.Errorf("uf: WRITE_LBA needs at least one sector")
		}
		if int64(dataLength) != int64(sectors)*SectorSize {
			return nil, fmt.Errorf("uf: WRITE_LBA data length %d does not match %d sectors", dataLength, sectors)
		}
		if sectors > MaxTransferSectors {
			return nil, fmt.Errorf("uf: WRITE_LBA of %d sectors exceeds the %d-sector transfer limit", sectors, MaxTransferSectors)
		}
		b[14] = 0x0a
		b[16] = subCode // RWMETHOD_IMAGE
		binary.LittleEndian.PutUint32(b[8:12], dataLength)
	case OpEraseLBA:
		if sectors == 0 {
			return nil, fmt.Errorf("uf: ERASE_LBA needs at least one sector")
		}
		if sectors > MaxEraseSectors {
			return nil, fmt.Errorf("uf: ERASE_LBA of %d sectors exceeds the %d-sector limit", sectors, MaxEraseSectors)
		}
		b[14] = 0x0a
		b[8] = 0 // no data stage
	case OpTestUnitReady, OpDeviceReset:
		if lba != 0 || sectors != 0 || dataLength != 0 {
			return nil, fmt.Errorf("uf: %s takes no address, length or data", op.Name())
		}
		b[14] = 0x06
		b[16] = subCode
	default:
		return nil, fmt.Errorf("uf: opcode 0x%02X is not part of the write path", uint8(op))
	}

	// dwAddress and usLength are big-endian on the wire; the transfer length
	// at b[8:12] stays little-endian. The loader answers CSW phase error (and
	// zero data) when either field is byte-swapped (SIL-515).
	binary.BigEndian.PutUint32(b[17:21], lba)
	binary.BigEndian.PutUint16(b[22:24], sectors)
	b[15] = byte(op) // cbwcb.ucOperCode
	return b, nil
}

// Error is a write-path failure that carries the rkdeveloptool code plus the
// LBA it happened at, so a failed run can say which transfer to retry.
type Error struct {
	Code     rockusb.ErrorCode `json:"code"`
	CodeName string            `json:"codeName"`
	Op       Op                `json:"op"`
	OpName   string            `json:"opName"`
	Stage    string            `json:"stage"`
	LBA      uint32            `json:"lba"`
	Sectors  int               `json:"sectors"`
	Attempt  int               `json:"attempt"`
	Detail   string            `json:"detail"`
	Err      error             `json:"-"`
}

func (e *Error) Error() string {
	name := e.OpName
	if name == "" {
		name = e.Op.Name()
	}
	if e.Sectors > 0 {
		return fmt.Sprintf("%s: %s lba=0x%x sectors=%d (attempt %d): %s",
			CodeName(e.Code), name, e.LBA, e.Sectors, e.Attempt, e.Detail)
	}
	return fmt.Sprintf("%s: %s (attempt %d): %s", CodeName(e.Code), name, e.Attempt, e.Detail)
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf extracts the write-path error code, defaulting to ErrFailed so an
// unexpected Go error never looks like success.
func CodeOf(err error) rockusb.ErrorCode {
	if err == nil {
		return rockusb.ErrSuccess
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrCancelled
	}
	var werr *Error
	if errors.As(err, &werr) {
		return werr.Code
	}
	var terr *rockusb.TimeoutError
	if errors.As(err, &terr) {
		return rockusb.ErrTimeout
	}
	switch {
	case errors.Is(err, errStalled):
		return rockusb.ErrDeviceWriteFailed
	case errors.Is(err, errDisconnected):
		return rockusb.ErrDeviceReadFailed
	}
	return rockusb.ErrFailed
}

var (
	errStalled      = errors.New("usb transfer stalled")
	errDisconnected = errors.New("device disconnected")
)

func isTimeout(err error) bool {
	var terr *rockusb.TimeoutError
	return errors.As(err, &terr)
}

func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// tracer numbers transfers across the read session and the write path, so one
// log has one monotonic seq (the M1 shell does the same for its commands).
type tracer struct {
	seq int
	fn  func(rockusb.Transfer)
}

func (t *tracer) record(tr rockusb.Transfer) {
	if t == nil {
		return
	}
	t.seq++
	tr.Seq = t.seq
	if t.fn != nil {
		t.fn(tr)
	}
}

// writer issues write-path commands over one transport, with a per-transfer
// deadline and a timeout-only retry policy. It is the write counterpart of
// rockusb.Session, which deliberately refuses to send these opcodes.
type writer struct {
	transport rockusb.Transport
	link      rockusb.Link
	timeout   time.Duration
	retries   int
	tracer    *tracer
	now       func() time.Time
	tag       uint32
}

func newWriter(t rockusb.Transport, link rockusb.Link, opts Options, tr *tracer) *writer {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &writer{
		transport: t,
		link:      link,
		timeout:   opts.Timeout,
		retries:   opts.Retries,
		tracer:    tr,
		now:       now,
		tag:       0x52534B00, // "RSK" + counter, same base as the read session
	}
}

func (w *writer) nextTag() uint32 {
	w.tag++
	return 0x52534B00 | (w.tag & 0xFF)
}

// WriteLBAs writes data at lba. data must be a whole number of sectors and is
// split into WRITE_LBA exchanges of at most MaxTransferSectors sectors.
func (w *writer) WriteLBAs(ctx context.Context, lba uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data)%SectorSize != 0 {
		return fmt.Errorf("uf: write of %d bytes is not a multiple of %d", len(data), SectorSize)
	}
	if end := uint64(lba) + uint64(len(data)/SectorSize); end > uint64(MaxLBA)+1 {
		return fmt.Errorf("uf: write at LBA 0x%x of %d sectors runs past the 32-bit LBA space", lba, len(data)/SectorSize)
	}

	for off := 0; off < len(data); {
		n := len(data) - off
		if max := MaxTransferSectors * SectorSize; n > max {
			n = max
		}
		sectors := uint16(n / SectorSize)
		start := lba + uint32(off/SectorSize)
		if err := w.command(ctx, OpWriteLBA, start, sectors, data[off:off+n]); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// EraseLBAs erases sectors with ERASE_LBA, split into MaxEraseSectors commands.
func (w *writer) EraseLBAs(ctx context.Context, lba uint32, sectors uint64) error {
	if sectors == 0 {
		return nil
	}
	if end := uint64(lba) + sectors; end > uint64(MaxLBA)+1 {
		return fmt.Errorf("uf: erase at LBA 0x%x of %d sectors runs past the 32-bit LBA space", lba, sectors)
	}
	for done := uint64(0); done < sectors; done += MaxEraseSectors {
		n := sectors - done
		if n > MaxEraseSectors {
			n = MaxEraseSectors
		}
		if err := w.command(ctx, OpEraseLBA, lba+uint32(done), uint16(n), nil); err != nil {
			return err
		}
	}
	return nil
}

// ReadLBAs reads flash sectors with READ_LBA, split into MaxTransferSectors
// commands. The runner uses it to identify the IDB already on the device
// before deciding whether to overwrite it.
func (w *writer) ReadLBAs(ctx context.Context, lba uint32, sectors int) ([]byte, error) {
	if sectors <= 0 {
		return nil, fmt.Errorf("uf: read of %d sectors is empty", sectors)
	}
	if end := uint64(lba) + uint64(sectors); end > uint64(MaxLBA)+1 {
		return nil, fmt.Errorf("uf: read at LBA 0x%x of %d sectors runs past the 32-bit LBA space", lba, sectors)
	}
	var out []byte
	for done := 0; done < sectors; {
		n := sectors - done
		if n > MaxTransferSectors {
			n = MaxTransferSectors
		}
		data, err := w.readChunk(ctx, lba+uint32(done), uint16(n))
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
		done += n
	}
	return out, nil
}

// readChunk runs one READ_LBA exchange, retrying only timeouts.
func (w *writer) readChunk(ctx context.Context, lba uint32, sectors uint16) ([]byte, error) {
	attempts := w.retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		data, err := w.readExchange(ctx, lba, sectors, attempt)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !isTimeout(err) || attempt == attempts {
			return nil, err
		}
	}
	return nil, lastErr
}

func (w *writer) readExchange(ctx context.Context, lba uint32, sectors uint16, attempt int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, w.errFor(OpReadLBA, "cancel", lba, sectors, attempt, err)
	}
	length := int(sectors) * SectorSize
	cbw, err := EncodeCommandCBW(w.nextTag(), OpReadLBA, lba, sectors, uint32(length), methodImage)
	if err != nil {
		return nil, &Error{Code: rockusb.ErrDeviceNotSupport, CodeName: rockusb.ErrDeviceNotSupport.String(),
			Op: OpReadLBA, OpName: OpReadLBA.Name(), Stage: "encode", LBA: lba, Sectors: int(sectors),
			Attempt: attempt, Detail: err.Error(), Err: err}
	}
	tag := binary.LittleEndian.Uint32(cbw[4:8])

	if err := w.bulkOut(ctx, OpReadLBA, "cbw", attempt, lba, sectors, cbw); err != nil {
		return nil, err
	}
	data, err := w.bulkIn(ctx, OpReadLBA, "data", attempt, lba, sectors, length)
	if err != nil {
		return nil, err
	}
	cswBytes, err := w.bulkIn(ctx, OpReadLBA, "csw", attempt, lba, sectors, rockusb.CSWLength)
	if err != nil {
		return nil, err
	}
	csw, err := rockusb.DecodeCSW(cswBytes)
	if err != nil {
		return nil, &Error{Code: rockusb.ErrCmdNotMatch, CodeName: rockusb.ErrCmdNotMatch.String(),
			Op: OpReadLBA, OpName: OpReadLBA.Name(), Stage: "csw", LBA: lba, Sectors: int(sectors),
			Attempt: attempt, Detail: err.Error(), Err: err}
	}
	if code := csw.Validate(tag); code != rockusb.ErrSuccess {
		return nil, &Error{Code: code, CodeName: code.String(),
			Op: OpReadLBA, OpName: OpReadLBA.Name(), Stage: "csw", LBA: lba, Sectors: int(sectors),
			Attempt: attempt, Detail: fmt.Sprintf("CSW tag=0x%08X status=0x%02X (want tag=0x%08X)", csw.Tag, csw.Status, tag)}
	}
	return data, nil
}

// Reset issues DEVICE_RESET (`rd`). The device drops off the bus afterwards.
func (w *writer) Reset(ctx context.Context) error {
	return w.command(ctx, OpDeviceReset, 0, 0, nil)
}

// command runs one exchange, retrying only timeouts. The retry repeats the
// whole CBW/data/CSW sequence: a timed out transfer leaves the device with a
// half-consumed command, so re-sending it is the only way back to a known
// state. WRITE_LBA is idempotent for the same LBA and bytes.
func (w *writer) command(ctx context.Context, op Op, lba uint32, sectors uint16, payload []byte) error {
	attempts := w.retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := w.exchange(ctx, op, lba, sectors, payload, attempt)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isTimeout(err) || attempt == attempts {
			return err
		}
	}
	return lastErr
}

func (w *writer) exchange(ctx context.Context, op Op, lba uint32, sectors uint16, payload []byte, attempt int) error {
	if err := ctx.Err(); err != nil {
		return w.errFor(op, "cancel", lba, sectors, attempt, err)
	}

	subCode := methodImage
	if op == OpDeviceReset {
		subCode = ResetSubcodeNone
	}
	cbw, err := EncodeCommandCBW(w.nextTag(), op, lba, sectors, uint32(len(payload)), subCode)
	if err != nil {
		return &Error{Code: rockusb.ErrDeviceNotSupport, CodeName: rockusb.ErrDeviceNotSupport.String(),
			Op: op, OpName: op.Name(), Stage: "encode", LBA: lba, Sectors: int(sectors), Attempt: attempt,
			Detail: err.Error(), Err: err}
	}
	tag := binary.LittleEndian.Uint32(cbw[4:8])

	if err := w.bulkOut(ctx, op, "cbw", attempt, lba, sectors, cbw); err != nil {
		return w.errFor(op, "cbw", lba, sectors, attempt, err)
	}
	if len(payload) > 0 {
		if err := w.bulkOut(ctx, op, "data", attempt, lba, sectors, payload); err != nil {
			return w.errFor(op, "data", lba, sectors, attempt, err)
		}
	}
	cswBytes, err := w.bulkIn(ctx, op, "csw", attempt, lba, sectors, rockusb.CSWLength)
	if err != nil {
		return w.errFor(op, "csw", lba, sectors, attempt, err)
	}
	csw, err := rockusb.DecodeCSW(cswBytes)
	if err != nil {
		return &Error{Code: rockusb.ErrCmdNotMatch, CodeName: rockusb.ErrCmdNotMatch.String(),
			Op: op, OpName: op.Name(), Stage: "csw", LBA: lba, Sectors: int(sectors), Attempt: attempt,
			Detail: err.Error(), Err: err}
	}
	if code := csw.Validate(tag); code != rockusb.ErrSuccess {
		// A failed CSW is the device answering "no"; retrying the same bytes
		// cannot help, so this is not routed through the retry loop.
		return &Error{Code: code, CodeName: code.String(),
			Op: op, OpName: op.Name(), Stage: "csw", LBA: lba, Sectors: int(sectors), Attempt: attempt,
			Detail: fmt.Sprintf("CSW tag=0x%08X status=0x%02X (want tag=0x%08X)", csw.Tag, csw.Status, tag)}
	}
	return nil
}

func (w *writer) errFor(op Op, stage string, lba uint32, sectors uint16, attempt int, err error) *Error {
	code := rockusb.ErrDeviceWriteFailed
	switch {
	case isTimeout(err):
		code = rockusb.ErrTimeout
	case isCancelled(err):
		code = ErrCancelled
	case stage == "csw":
		code = rockusb.ErrDeviceReadFailed
	}
	return &Error{Code: code, CodeName: CodeName(code), Op: op, OpName: op.Name(), Stage: stage,
		LBA: lba, Sectors: int(sectors), Attempt: attempt, Detail: err.Error(), Err: err}
}

func (w *writer) bulkOut(ctx context.Context, op Op, stage string, attempt int, lba uint32, sectors uint16, data []byte) error {
	start := w.now()
	n, _, err := w.call(ctx, fmt.Sprintf("%s %s", op.Name(), stage), func() (int, []byte, error) {
		written, werr := w.transport.BulkOut(w.link.BulkOut, data)
		return written, nil, werr
	})
	entry := rockusb.Transfer{
		Kind: "bulk-out", Endpoint: int(w.link.BulkOut), Direction: "out",
		RequestType: -1, Request: -1, Value: -1, Index: -1,
		RequestedLength: len(data), Length: n, DataHex: hexLimit(data, 64),
		DurationMs: msSince(start, w.now()), Op: int(op), OpName: op.Name(),
		Stage: stage, Attempt: attempt,
	}
	if err != nil {
		entry.Status = "error"
		entry.Note = err.Error()
		w.tracer.record(entry)
		return err
	}
	if n != len(data) {
		entry.Status = "short"
		entry.Note = fmt.Sprintf("bytesWritten=%d want %d", n, len(data))
		w.tracer.record(entry)
		return fmt.Errorf("bulk OUT wrote %d of %d bytes", n, len(data))
	}
	entry.Status = "ok"
	if stage == "data" {
		entry.Note = fmt.Sprintf("lba=0x%x sectors=%d", lba, sectors)
	}
	w.tracer.record(entry)
	return nil
}

func (w *writer) bulkIn(ctx context.Context, op Op, stage string, attempt int, lba uint32, sectors uint16, length int) ([]byte, error) {
	start := w.now()
	_, data, err := w.call(ctx, fmt.Sprintf("%s %s", op.Name(), stage), func() (int, []byte, error) {
		b, rerr := w.transport.BulkIn(w.link.BulkIn, length)
		return len(b), b, rerr
	})
	entry := rockusb.Transfer{
		Kind: "bulk-in", Endpoint: int(w.link.BulkIn), Direction: "in",
		RequestType: -1, Request: -1, Value: -1, Index: -1,
		RequestedLength: length, Length: len(data), DataHex: hexLimit(data, 64),
		DurationMs: msSince(start, w.now()), Op: int(op), OpName: op.Name(),
		Stage: stage, Attempt: attempt,
	}
	if err != nil {
		entry.Status = "error"
		entry.Note = err.Error()
		w.tracer.record(entry)
		return nil, err
	}
	if len(data) < length {
		entry.Status = "short"
		entry.Note = fmt.Sprintf("got %d of %d bytes", len(data), length)
		w.tracer.record(entry)
		return nil, fmt.Errorf("bulk IN returned %d of %d bytes", len(data), length)
	}
	entry.Status = "ok"
	w.tracer.record(entry)
	return data, nil
}

// call runs one transport operation under the per-transfer deadline and the
// caller's context. On either timeout the pipe is aborted so the next attempt
// starts clean.
func (w *writer) call(ctx context.Context, what string, fn func() (int, []byte, error)) (int, []byte, error) {
	type outcome struct {
		n   int
		b   []byte
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		n, b, err := fn()
		done <- outcome{n, b, err}
	}()

	timer := time.NewTimer(w.timeout)
	defer timer.Stop()
	select {
	case out := <-done:
		return out.n, out.b, out.err
	case <-ctx.Done():
		_ = w.transport.Reset()
		return 0, nil, ctx.Err()
	case <-timer.C:
		_ = w.transport.Reset()
		return 0, nil, &rockusb.TimeoutError{What: what, Timeout: w.timeout}
	}
}

func hexLimit(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
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

func msSince(start, end time.Time) float64 {
	return float64(end.Sub(start).Microseconds()) / 1000
}

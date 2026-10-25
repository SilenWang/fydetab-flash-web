package rockusb

import (
	"errors"
	"fmt"
	"time"
)

// This file is the transport side of the protocol core: one UFI command
// exchange (CBW → data → CSW) over an abstract USB transport, with a per
// transfer timeout and an explicit retry policy.
//
// The transport is implemented twice: by the browser bridge in cmd/wasm (every
// call goes through the WebUSB bridge on the page's main thread) and by the
// mock transports in the tests. Nothing here knows which one it talks to.

// DefaultTimeout is the per-transfer deadline. 5 s mirrors rkdeveloptool's
// CMD_TIMEOUT for the read commands; a real rockusb device answers in
// milliseconds, so a transfer that takes longer is a stuck pipe, not a slow
// device.
const DefaultTimeout = 5 * time.Second

// Vendor request constants for the Download Boot path (RKComm.cpp:811).
const (
	// RequestTypeVendorOut is bmRequestType for a vendor host-to-device
	// control transfer: direction OUT (0x00) | vendor (0x40).
	RequestTypeVendorOut uint16 = 0x40
	// RequestDeviceRequest is bRequest for RKU_DeviceRequest (0x0C).
	RequestDeviceRequest uint16 = 0x0C
	// RequestIndex471 carries the DDR / USB head segments into maskrom RAM.
	RequestIndex471 uint16 = 0x0471
	// RequestIndex472 carries the usbplug segment.
	RequestIndex472 uint16 = 0x0472
	// VendorChunkSize is the largest payload of one vendor control transfer.
	VendorChunkSize = 4096
)

// ErrTimeout is a local code: rkdeveloptool blocks forever on a stuck pipe,
// so it has no ERR_* value for "the host gave up". It is only ever produced by
// this package's own deadline logic.
const ErrTimeout ErrorCode = -13

// Transport is the USB surface the protocol core needs. The browser bridge
// implements it on top of WebUSB; tests implement it in memory.
type Transport interface {
	// ControlOut performs a vendor host-to-device control transfer and
	// reports how many bytes the device accepted. requestType is always
	// RequestTypeVendorOut for the Download Boot path.
	ControlOut(requestType, request, value, index uint16, data []byte) (int, error)
	// BulkOut writes to a bulk OUT endpoint and reports bytesWritten.
	BulkOut(endpoint uint8, data []byte) (int, error)
	// BulkIn reads up to length bytes from a bulk IN endpoint.
	BulkIn(endpoint uint8, length int) ([]byte, error)
	// Reset aborts a stuck transfer (WebUSB: device.reset()).
	Reset() error
}

// Link describes the claimed rockusb interface and the device it belongs to.
type Link struct {
	InterfaceNumber  int       `json:"interfaceNumber"`
	AlternateSetting int       `json:"alternateSetting"`
	InterfaceClass   uint16    `json:"interfaceClass"`
	BulkIn           uint8     `json:"bulkIn"`
	BulkOut          uint8     `json:"bulkOut"`
	VendorID         uint16    `json:"vendorId"`
	ProductID        uint16    `json:"productId"`
	Mode             MediaMode `json:"mode"`
}

// Transfer is one recorded USB transfer, shaped after the M0 probe trace so
// both stages export the same schema. A negative request field means "not
// applicable for this transfer kind".
type Transfer struct {
	Seq             int     `json:"seq"`
	Kind            string  `json:"kind"` // bulk-in / bulk-out / control-out / control-in
	Endpoint        int     `json:"endpoint"`
	Direction       string  `json:"direction"`
	RequestType     int     `json:"requestType"`
	Request         int     `json:"request"`
	Value           int     `json:"value"`
	Index           int     `json:"index"`
	RequestedLength int     `json:"requestedLength"`
	Length          int     `json:"length"`
	Status          string  `json:"status"`
	DataHex         string  `json:"dataHex,omitempty"`
	DurationMs      float64 `json:"durationMs"`
	Op              int     `json:"op"`
	OpName          string  `json:"opName,omitempty"`
	Stage           string  `json:"stage,omitempty"`
	Attempt         int     `json:"attempt,omitempty"`
	Note            string  `json:"note,omitempty"`
}

// Options tunes one session.
type Options struct {
	// Timeout bounds every single USB transfer. Zero means DefaultTimeout.
	Timeout time.Duration
	// Retries is how many *additional* attempts a command may make after a
	// timeout. Only timeouts are retried: a CSW failure, a bad tag or a
	// STALL on bulk OUT are definitive device answers and are never retried
	// (the M0 probe promised exactly that, and the stage-2 maskrom trace
	// shows every one of those five commands answering once and being
	// recorded).
	Retries int
	// Trace receives every transfer, successful or not.
	Trace func(Transfer)
	// Tags, when set, supplies CBW tags; the tests use it to replay a real
	// trace byte for byte. nil means the default counter.
	Tags func() uint32
	// Now and Sleep are test hooks; nil means time.Now / time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return DefaultTimeout
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	if o.Sleep != nil {
		o.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Error is a protocol or transport failure with the rkdeveloptool error code
// that a trace consumer expects to see.
type Error struct {
	Code    ErrorCode
	Op      Op
	OpName  string
	Stage   string
	Attempt int
	Detail  string
	Err     error
}

func (e *Error) Error() string {
	name := e.OpName
	if name == "" {
		name = fmt.Sprintf("op 0x%02X", byte(e.Op))
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s: %s", e.Code, name, e.Detail)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, name, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, name)
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf extracts the rockusb error code from an error, defaulting to
// ErrFailed so an unexpected Go error never masquerades as success.
func CodeOf(err error) ErrorCode {
	var rerr *Error
	if errors.As(err, &rerr) {
		return rerr.Code
	}
	var terr *TimeoutError
	if errors.As(err, &terr) {
		return ErrTimeout
	}
	if err == nil {
		return ErrSuccess
	}
	return ErrFailed
}

// TimeoutError reports that a single transfer exceeded the deadline.
type TimeoutError struct {
	What    string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.What, e.Timeout)
}

// Session runs rockusb commands against one open device.
type Session struct {
	transport Transport
	Link      Link
	opts      Options

	seq int
	tag uint32
}

// NewSession wraps an opened device.
func NewSession(t Transport, link Link, opts Options) *Session {
	return &Session{transport: t, Link: link, opts: opts}
}

// NextTag returns the CBW tag for the next exchange. rockusb only requires
// that the CSW echoes it, so a counter is enough; starting from a fixed base
// keeps unit-test frames stable.
func (s *Session) NextTag() uint32 {
	if s.opts.Tags != nil {
		return s.opts.Tags()
	}
	s.tag++
	return 0x52534B00 | (s.tag & 0xFF) // "RSK" + low byte
}

// TransferError wraps the transport-level failure of one transfer.
func (s *Session) transferError(op Command, stage string, attempt int, err error) *Error {
	code := ErrDeviceWriteFailed
	if stage == "data" || stage == "csw" {
		code = ErrDeviceReadFailed
	}
	if isTimeout(err) {
		code = ErrTimeout
	}
	return &Error{Code: code, Op: op.Op, OpName: op.Name, Stage: stage, Attempt: attempt, Err: err}
}

func isTimeout(err error) bool {
	var terr *TimeoutError
	return errors.As(err, &terr)
}

// call runs one transport operation under the per-transfer deadline.
func (s *Session) call(what string, fn func() (int, []byte, error)) (int, []byte, error) {
	type outcome struct {
		n  int
		b  []byte
		er error
	}
	done := make(chan outcome, 1)
	go func() {
		n, b, err := fn()
		done <- outcome{n: n, b: b, er: err}
	}()

	timeout := s.opts.timeout()
	if timeout <= 0 {
		out := <-done
		return out.n, out.b, out.er
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case out := <-done:
		return out.n, out.b, out.er
	case <-timer.C:
		// Abort the stuck pipe so the next attempt starts clean. Best
		// effort: the device may already be gone.
		_ = s.transport.Reset()
		return 0, nil, &TimeoutError{What: what, Timeout: timeout}
	}
}

func (s *Session) trace(t Transfer) {
	s.seq++
	t.Seq = s.seq
	if s.opts.Trace != nil {
		s.opts.Trace(t)
	}
}

// bulkOut sends one buffer and records it.
func (s *Session) bulkOut(op Command, stage string, attempt int, data []byte) error {
	start := s.opts.now()
	n, _, err := s.call(fmt.Sprintf("%s bulk OUT", op.Name), func() (int, []byte, error) {
		written, werr := s.transport.BulkOut(s.Link.BulkOut, data)
		return written, nil, werr
	})
	entry := Transfer{
		Kind: "bulk-out", Endpoint: int(s.Link.BulkOut), Direction: "out",
		RequestType: -1, Request: -1, Value: -1, Index: -1,
		RequestedLength: len(data), Length: n, DataHex: hexBytes(limit(data, 64)),
		DurationMs: msSince(start, s.opts.now()), Op: int(op.Op), OpName: op.Name,
		Stage: stage, Attempt: attempt,
	}
	if err != nil {
		entry.Status = "error"
		entry.Note = err.Error()
		s.trace(entry)
		return err
	}
	entry.Status = "ok"
	if n != len(data) {
		entry.Status = "short"
		entry.Note = fmt.Sprintf("bytesWritten=%d want %d", n, len(data))
		s.trace(entry)
		return fmt.Errorf("bulk OUT wrote %d of %d bytes", n, len(data))
	}
	s.trace(entry)
	return nil
}

// bulkIn reads exactly length bytes and records it.
func (s *Session) bulkIn(op Command, stage string, attempt int, length int) ([]byte, error) {
	start := s.opts.now()
	_, data, err := s.call(fmt.Sprintf("%s bulk IN", op.Name), func() (int, []byte, error) {
		b, rerr := s.transport.BulkIn(s.Link.BulkIn, length)
		return len(b), b, rerr
	})
	entry := Transfer{
		Kind: "bulk-in", Endpoint: int(s.Link.BulkIn), Direction: "in",
		RequestType: -1, Request: -1, Value: -1, Index: -1,
		RequestedLength: length, Length: len(data), DataHex: hexBytes(limit(data, 64)),
		DurationMs: msSince(start, s.opts.now()), Op: int(op.Op), OpName: op.Name,
		Stage: stage, Attempt: attempt,
	}
	if err != nil {
		entry.Status = "error"
		entry.Note = err.Error()
		s.trace(entry)
		return nil, err
	}
	if len(data) < length {
		entry.Status = "short"
		entry.Note = fmt.Sprintf("got %d of %d bytes", len(data), length)
		s.trace(entry)
		return data, fmt.Errorf("bulk IN returned %d of %d bytes", len(data), length)
	}
	entry.Status = "ok"
	s.trace(entry)
	return data, nil
}

// Result is the outcome of one command, shaped for both the unit tests and the
// JSON the wasm shell hands back to the page.
type Result struct {
	OpName      string    `json:"opName"`
	Op          Op        `json:"op"`
	Description string    `json:"description"`
	OK          bool      `json:"ok"`
	Code        ErrorCode `json:"code"`
	CodeName    string    `json:"codeName"`
	Detail      string    `json:"detail,omitempty"`
	Attempts    int       `json:"attempts"`
	DurationMs  float64   `json:"durationMs"`
	DataHex     string    `json:"dataHex,omitempty"`
	Data        []byte    `json:"-"`
	CSW         *CSW      `json:"csw,omitempty"`
	Decoded     any       `json:"decoded,omitempty"`
}

// Exec runs one read-info command, retrying only on timeout.
func (s *Session) Exec(cmd Command) Result {
	res := Result{
		OpName:      cmd.Name,
		Op:          cmd.Op,
		Description: cmd.Description,
	}
	start := s.opts.now()
	// Defence in depth: only the command table defines a legal exchange, so
	// an opcode that is not in it never reaches the transport.
	if !IsReadOnly(cmd.Op) {
		res.Code = ErrDeviceNotSupport
		res.CodeName = res.Code.String()
		res.Detail = fmt.Sprintf("opcode 0x%02X is not in the read-only command table; refusing to send it", byte(cmd.Op))
		return res
	}
	attempts := s.opts.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		res.Attempts = attempt
		data, csw, err := s.exchange(cmd, attempt)
		if err == nil {
			res.OK = true
			res.Code = ErrSuccess
			res.CodeName = ErrSuccess.String()
			res.Data = data
			res.CSW = csw
			res.DataHex = hexBytes(data)
			res.Decoded = decodeData(cmd.Op, data)
			break
		}
		lastErr = err
		if !isTimeout(err) || attempt == attempts {
			break
		}
	}
	if !res.OK {
		res.Code = CodeOf(lastErr)
		res.CodeName = res.Code.String()
		res.Detail = lastErr.Error()
	}
	res.DurationMs = msSince(start, s.opts.now())
	return res
}

func (s *Session) exchange(cmd Command, attempt int) ([]byte, *CSW, error) {
	tag := s.NextTag()
	cbw := EncodeCBW(tag, cmd.Op, cmd.TransferLength)
	if err := s.bulkOut(cmd, "cbw", attempt, cbw); err != nil {
		return nil, nil, s.transferError(cmd, "cbw", attempt, err)
	}

	var data []byte
	if cmd.DataLength > 0 {
		// rkdeveloptool reads the CSW at ucCBWTransferLength; the flash
		// info command is the one place where a device may legally answer
		// short, so only that one tolerates a short read.
		n := int(cmd.TransferLength)
		if n == 0 {
			n = cmd.DataLength
		}
		got, err := s.bulkIn(cmd, "data", attempt, n)
		if err != nil {
			if cmd.AllowShort && len(got) >= cmd.DataLength {
				data = got
			} else {
				return nil, nil, s.transferError(cmd, "data", attempt, err)
			}
		} else {
			data = got
		}
	}

	cswBytes, err := s.bulkIn(cmd, "csw", attempt, CSWLength)
	if err != nil {
		return nil, nil, s.transferError(cmd, "csw", attempt, err)
	}
	csw, err := DecodeCSW(cswBytes)
	if err != nil {
		return nil, nil, &Error{Code: ErrCmdNotMatch, Op: cmd.Op, OpName: cmd.Name, Stage: "csw", Attempt: attempt, Err: err}
	}
	if code := csw.Validate(tag); code != ErrSuccess {
		if cmd.Op == OpTestUnitReady && code == ErrFailed {
			code = ErrDeviceUnready
		}
		return nil, &csw, &Error{
			Code: code, Op: cmd.Op, OpName: cmd.Name, Stage: "csw", Attempt: attempt,
			Detail: fmt.Sprintf("CSW tag=0x%08X status=0x%02X (want tag=0x%08X)", csw.Tag, csw.Status, tag),
		}
	}
	return data, &csw, nil
}

func decodeData(op Op, data []byte) any {
	switch op {
	case OpReadChipInfo:
		if info, err := ParseChipInfo(data); err == nil {
			return info
		}
	case OpReadFlashInfo:
		if info, err := ParseFlashInfo(data); err == nil {
			return info
		}
	case OpReadCapability:
		if cap, err := ParseCapability(data); err == nil {
			return cap
		}
	case OpReadStorage:
		if st, err := ParseStorage(data); err == nil {
			return st
		}
	}
	if len(data) == 0 {
		return nil
	}
	return map[string]string{"rawHex": hexBytes(data)}
}

func limit(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func msSince(start, end time.Time) float64 {
	d := end.Sub(start)
	return float64(d.Microseconds()) / 1000
}

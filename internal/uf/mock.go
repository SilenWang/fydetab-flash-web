package uf

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// This file holds the two mock transports the write path is developed against:
//
//   - ReplayTransport answers from a recorded trace (docs/uf-write-path.md
//     describes the JSON), so a real device session can be re-run offline;
//   - Simulator is a small LOADER-mode (or MASKROM + Download Boot) device that
//     keeps a flash image in memory, so a whole `uf` run can be asserted.
//
// Neither type is used by the production path: both implement
// rockusb.Transport, which is the only thing the runner knows about a device.

// Trace schema and frame kinds.
const (
	TraceSchema = "uf-trace/1"

	// FrameCBW is a 31-byte bulk OUT command block.
	FrameCBW = "cbw"
	// FrameWriteData is the payload bulk OUT of WRITE_LBA.
	FrameWriteData = "write-data"
	// FrameReadData is a bulk IN data stage (chip/flash/capability/storage).
	FrameReadData = "read-data"
	// FrameCSW is the 13-byte bulk IN status wrapper.
	FrameCSW = "csw"
	// FrameControlOut is a vendor control transfer (Download Boot).
	FrameControlOut = "control-out"

	// FrameStatusOK is the default: the transfer succeeds.
	FrameStatusOK = ""
	// FrameStatusFailed makes the CSW report ucCSWStatus = 1.
	FrameStatusFailed = "csw-failed"
	// FrameStatusStall makes the USB transfer STALL.
	FrameStatusStall = "stall"
	// FrameStatusTimeout makes the transfer exceed the deadline.
	FrameStatusTimeout = "timeout"
	// FrameStatusDisconnect makes the device drop off the bus.
	FrameStatusDisconnect = "disconnect"
)

// Frame is one expected USB transfer plus its recorded answer. Optional fields
// (Op/LBA/Sectors/Length/Request) are asserted when present; the CBW tag is
// never stored because it is random per run and the CSW is synthesised with the
// tag of the CBW it answers.
type Frame struct {
	Kind       string  `json:"kind"`
	Op         *int    `json:"op,omitempty"`
	LBA        *uint32 `json:"lba,omitempty"`
	Sectors    *int    `json:"sectors,omitempty"`
	Length     *int    `json:"length,omitempty"`
	Request    *int    `json:"request,omitempty"`
	RespondHex string  `json:"respondHex,omitempty"`
	Status     string  `json:"status,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// Trace is a recorded device session.
type Trace struct {
	Schema string `json:"schema"`
	Device struct {
		Mode string `json:"mode"`
	} `json:"device"`
	Frames []Frame `json:"frames"`
}

// LoadTrace parses and validates a trace file.
func LoadTrace(r io.Reader) (*Trace, error) {
	if r == nil {
		return nil, errors.New("uf: nil trace reader")
	}
	var tr Trace
	if err := json.NewDecoder(r).Decode(&tr); err != nil {
		return nil, fmt.Errorf("uf: parse trace: %w", err)
	}
	if tr.Schema != "" && tr.Schema != TraceSchema {
		return nil, fmt.Errorf("uf: trace schema %q is not %q", tr.Schema, TraceSchema)
	}
	if len(tr.Frames) == 0 {
		return nil, errors.New("uf: trace has no frames")
	}
	for i, f := range tr.Frames {
		switch f.Kind {
		case FrameCBW, FrameWriteData, FrameReadData, FrameCSW, FrameControlOut:
		default:
			return nil, fmt.Errorf("uf: trace frame %d has unknown kind %q", i+1, f.Kind)
		}
		switch f.Status {
		case FrameStatusOK, FrameStatusFailed, FrameStatusStall, FrameStatusTimeout, FrameStatusDisconnect:
		default:
			return nil, fmt.Errorf("uf: trace frame %d has unknown status %q", i+1, f.Status)
		}
		if f.Kind == FrameReadData && f.RespondHex == "" {
			return nil, fmt.Errorf("uf: trace frame %d is a read data stage with no respondHex", i+1)
		}
	}
	return &tr, nil
}

func traceMode(tr *Trace) rockusb.MediaMode {
	switch strings.ToUpper(strings.TrimSpace(tr.Device.Mode)) {
	case "MASKROM":
		return rockusb.ModeMaskrom
	case "LOADER", "":
		return rockusb.ModeLoader
	default:
		return rockusb.ModeUnknown
	}
}

// Mode is the device mode the trace was recorded in.
func (t *Trace) Mode() rockusb.MediaMode { return traceMode(t) }

// ReplayTransport feeds recorded frames back to the writer/read session. Every
// mismatch is sticky: the transport refuses further calls once a frame does not
// match, so a test cannot silently pass on a drifted byte sequence.
type ReplayTransport struct {
	mu      sync.Mutex
	frames  []Frame
	pos     int
	lastTag uint32
	calls   int
	err     error
	seen    []Frame
}

// NewReplayTransport wraps a frame list.
func NewReplayTransport(frames []Frame) *ReplayTransport {
	return &ReplayTransport{frames: frames}
}

// Done reports whether every frame was consumed.
func (t *ReplayTransport) Done() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pos == len(t.frames)
}

// Remaining is how many frames were not consumed.
func (t *ReplayTransport) Remaining() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.frames) - t.pos
}

// Err is the first mismatch or transport error.
func (t *ReplayTransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Seen returns the frames the transport matched, in order.
func (t *ReplayTransport) Seen() []Frame {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Frame(nil), t.seen...)
}

func (t *ReplayTransport) next(kinds ...string) (Frame, error) {
	if t.err != nil {
		return Frame{}, t.err
	}
	if t.pos >= len(t.frames) {
		t.err = fmt.Errorf("replay: transfer %d has no recorded frame (%d frames consumed)", t.calls+1, t.pos)
		return Frame{}, t.err
	}
	f := t.frames[t.pos]
	matched := false
	for _, k := range kinds {
		if f.Kind == k {
			matched = true
			break
		}
	}
	if !matched {
		t.err = fmt.Errorf("replay: transfer %d is %q, expected %s", t.calls+1, f.Kind, strings.Join(kinds, "/"))
		return Frame{}, t.err
	}
	t.calls++
	t.pos++
	t.seen = append(t.seen, f)
	return f, nil
}

func (t *ReplayTransport) mismatch(format string, args ...any) error {
	t.err = fmt.Errorf("replay: transfer %d: %s", t.calls, fmt.Sprintf(format, args...))
	return t.err
}

// ControlOut replays a Download Boot vendor transfer.
func (t *ReplayTransport) ControlOut(requestType, request, value, index uint16, data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := t.next(FrameControlOut)
	if err != nil {
		return 0, err
	}
	if f.Request != nil && int(index) != *f.Request {
		return 0, t.mismatch("control request 0x%04X, recorded 0x%04X", index, *f.Request)
	}
	if f.Length != nil && len(data) != *f.Length {
		return 0, t.mismatch("control payload %d bytes, recorded %d", len(data), *f.Length)
	}
	if err := frameError(f); err != nil {
		return 0, err
	}
	return len(data), nil
}

// BulkOut replays a CBW or a write data stage.
func (t *ReplayTransport) BulkOut(endpoint uint8, data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	kind := FrameWriteData
	var op byte
	var lba uint32
	var sectors uint16
	if len(data) == rockusb.CBWLength && binary.LittleEndian.Uint32(data[0:4]) == rockusb.CBWSignature {
		kind = FrameCBW
		op = data[15]
		lba = binary.LittleEndian.Uint32(data[17:21])
		sectors = binary.LittleEndian.Uint16(data[22:24])
		t.lastTag = binary.LittleEndian.Uint32(data[4:8])
	}

	f, err := t.next(kind)
	if err != nil {
		return 0, err
	}
	if f.Op != nil && int(op) != *f.Op {
		return 0, t.mismatch("opcode 0x%02X, recorded 0x%02X", op, *f.Op)
	}
	if f.LBA != nil && lba != *f.LBA {
		return 0, t.mismatch("LBA 0x%x, recorded 0x%x", lba, *f.LBA)
	}
	if f.Sectors != nil && int(sectors) != *f.Sectors {
		return 0, t.mismatch("%d sectors, recorded %d", sectors, *f.Sectors)
	}
	if f.Length != nil && len(data) != *f.Length {
		return 0, t.mismatch("%d bytes, recorded %d", len(data), *f.Length)
	}
	if err := frameError(f); err != nil {
		return 0, err
	}
	return len(data), nil
}

// BulkIn replays a read data stage or a CSW.
func (t *ReplayTransport) BulkIn(endpoint uint8, length int) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := t.next(FrameReadData, FrameCSW)
	if err != nil {
		return nil, err
	}
	if f.Length != nil && length != *f.Length {
		return nil, t.mismatch("bulk IN asked for %d bytes, recorded %d", length, *f.Length)
	}
	if err := frameError(f); err != nil {
		return nil, err
	}
	if f.Kind == FrameCSW && f.RespondHex == "" {
		status := rockusb.StatusPassed
		if f.Status == FrameStatusFailed {
			status = rockusb.StatusFailed
		}
		return cswBytes(t.lastTag, status), nil
	}
	raw, err := decodeHex(f.RespondHex)
	if err != nil {
		return nil, t.mismatch("bad respondHex on frame %d: %v", t.pos, err)
	}
	return raw, nil
}

// Reset is a no-op: the replay trace has no reset acknowledgement.
func (t *ReplayTransport) Reset() error { return nil }

func frameError(f Frame) error {
	switch f.Status {
	case FrameStatusOK:
		return nil
	case FrameStatusTimeout:
		return &rockusb.TimeoutError{What: "replayed transfer", Timeout: 0}
	case FrameStatusStall:
		return errStalled
	case FrameStatusDisconnect:
		return errDisconnected
	case FrameStatusFailed:
		// csw-failed is carried in the CSW, not in the transport.
		return nil
	}
	return fmt.Errorf("replay: unknown status %q", f.Status)
}

func decodeHex(s string) ([]byte, error) {
	clean := strings.NewReplacer(" ", "", "\n", "", "\t", "", "0x", "").Replace(s)
	return hex.DecodeString(clean)
}

func cswBytes(tag uint32, status byte) []byte {
	b := make([]byte, rockusb.CSWLength)
	binary.LittleEndian.PutUint32(b[0:4], rockusb.CSWSignature)
	binary.LittleEndian.PutUint32(b[4:8], tag)
	b[12] = status
	return b
}

// ReplaySession is one enumeration: the frames of that device instance plus its
// mode. A Download Boot session is followed by a LOADER session.
type ReplaySession struct {
	Mode   rockusb.MediaMode
	Frames []Frame
}

// ReplayConnector hands out one ReplayTransport per Open, in order. Once the
// sessions are exhausted Open fails, which is what a device that never
// re-enumerated looks like to the runner.
type ReplayConnector struct {
	mu       sync.Mutex
	sessions []ReplaySession
	next     int
	opened   []*ReplayTransport
	closes   int
}

// NewReplayConnector builds a connector from recorded sessions.
func NewReplayConnector(sessions ...ReplaySession) *ReplayConnector {
	return &ReplayConnector{sessions: sessions}
}

// Open returns the next recorded device.
func (c *ReplayConnector) Open(ctx context.Context) (rockusb.Transport, rockusb.Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, rockusb.Link{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= len(c.sessions) {
		return nil, rockusb.Link{}, fmt.Errorf("replay: no device session left after %d open(s)", c.next)
	}
	s := c.sessions[c.next]
	c.next++
	t := NewReplayTransport(s.Frames)
	c.opened = append(c.opened, t)
	return t, simLink(s.Mode), nil
}

// Close counts releases; the replay device has no interface to release.
func (c *ReplayConnector) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

// Transports returns every transport handed out so far.
func (c *ReplayConnector) Transports() []*ReplayTransport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*ReplayTransport(nil), c.opened...)
}

// Err returns the first transport-level mismatch.
func (c *ReplayConnector) Err() error {
	for _, t := range c.Transports() {
		if err := t.Err(); err != nil {
			return err
		}
	}
	return nil
}

// --- in-memory device -------------------------------------------------------

// SimFaultKind is a transport failure the simulator can inject.
type SimFaultKind string

const (
	// SimTimeout makes one transfer exceed the deadline, which the writer
	// retries.
	SimTimeout SimFaultKind = "timeout"
	// SimFailed makes the CSW report failure, which the writer does not retry.
	SimFailed SimFaultKind = "failed"
	// SimStall makes the bulk transfer STALL.
	SimStall SimFaultKind = "stall"
	// SimDisconnect makes the device drop off the bus.
	SimDisconnect SimFaultKind = "disconnect"
)

// SimFault injects a failure on matching commands.
type SimFault struct {
	Kind SimFaultKind `json:"kind"`
	// LBA matches the command's start sector when set.
	LBA *uint32 `json:"lba,omitempty"`
	// Op matches the opcode when set.
	Op *Op `json:"op,omitempty"`
	// Times is how many matching transfers the fault fires on. Zero means
	// one; a negative value means every matching transfer.
	Times int `json:"times,omitempty"`

	used int
}

// SimConfig configures the simulated device.
type SimConfig struct {
	// Mode defaults to LOADER. MASKROM makes the first simulator answer
	// nothing (like a real bare BootROM) until Download Boot data arrives,
	// after which Open returns a LOADER device.
	Mode rockusb.MediaMode
	// Chip is the READ_CHIP_INFO text; default "3588".
	Chip string
	// FlashSectors is READ_FLASH_INFO's uiFlashSize; default 2 MiB worth
	// (4096 sectors).
	FlashSectors uint32
	// Capability is the 8-byte READ_CAPABILITY reply; default Direct LBA +
	// First 4M Access.
	Capability [8]byte
	// IDB seeds the flash at LBA 64, so a test can present an existing New
	// IDB ("RKNS") or legacy one ("RK28") the runner must not overwrite.
	IDB []byte
	// Faults are transport failures to inject.
	Faults []SimFault
}

// SimWrite is one applied WRITE_LBA.
type SimWrite struct {
	LBA     uint32 `json:"lba"`
	Sectors int    `json:"sectors"`
	Bytes   int    `json:"bytes"`
}

// SimErase is one applied ERASE_LBA.
type SimErase struct {
	LBA     uint32 `json:"lba"`
	Sectors int    `json:"sectors"`
}

// SimControl is one vendor control transfer (Download Boot).
type SimControl struct {
	Request uint16 `json:"request"`
	Value   uint16 `json:"value"`
	Length  int    `json:"length"`
}

type simFlash struct {
	mu   sync.Mutex
	data []byte
}

type simCommand struct {
	op      Op
	lba     uint32
	sectors int
	dataLen int
	tag     uint32
	needCSW bool
	failCSW bool
}

// Simulator is an in-memory rockusb device. It implements rockusb.Transport.
type Simulator struct {
	mu   sync.Mutex
	cfg  SimConfig
	mode rockusb.MediaMode
	link rockusb.Link
	fl   *simFlash

	pending  *simCommand
	lastTag  uint32
	booted   bool
	reset    bool
	faults   []SimFault
	writes   []SimWrite
	erases   []SimErase
	controls []SimControl
	failures []string
}

func newSimulator(cfg SimConfig, mode rockusb.MediaMode, fl *simFlash) *Simulator {
	if cfg.Chip == "" {
		cfg.Chip = "3588"
	}
	if cfg.FlashSectors == 0 {
		cfg.FlashSectors = 4096 // 2 MiB
	}
	if cfg.Capability == [8]byte{} {
		cfg.Capability = [8]byte{0x0F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	}
	if fl == nil {
		fl = &simFlash{data: make([]byte, int(cfg.FlashSectors)*SectorSize)}
	}
	if len(cfg.IDB) > 0 {
		off := int(IDBDefaultLBA) * SectorSize
		if off+len(cfg.IDB) <= len(fl.data) {
			copy(fl.data[off:], cfg.IDB)
		}
	}
	sim := &Simulator{cfg: cfg, mode: mode, fl: fl, link: simLink(mode), faults: append([]SimFault(nil), cfg.Faults...)}
	return sim
}

func simLink(mode rockusb.MediaMode) rockusb.Link {
	return rockusb.Link{
		InterfaceNumber: 0, InterfaceClass: 0xff,
		BulkIn: 0x81, BulkOut: 0x02,
		VendorID: rockusb.RockchipVendorID, ProductID: 0x350b,
		Mode: mode,
	}
}

// Mode reports the simulated device mode.
func (s *Simulator) Mode() rockusb.MediaMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// Booted reports whether Download Boot data reached the maskrom simulator.
func (s *Simulator) Booted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.booted
}

// ResetSent reports whether DEVICE_RESET was issued.
func (s *Simulator) ResetSent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reset
}

// Writes, Erases, Controls and Failures are the recorded device activity.
func (s *Simulator) Writes() []SimWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SimWrite(nil), s.writes...)
}

func (s *Simulator) Erases() []SimErase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SimErase(nil), s.erases...)
}

func (s *Simulator) Controls() []SimControl {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SimControl(nil), s.controls...)
}

func (s *Simulator) InjectedFailures() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.failures...)
}

// FlashAt returns a copy of the flash contents at lba.
func (s *Simulator) FlashAt(lba uint32, length int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	off := int(lba) * SectorSize
	s.fl.mu.Lock()
	defer s.fl.mu.Unlock()
	if off < 0 || off+length > len(s.fl.data) {
		return nil, fmt.Errorf("sim: flash range LBA 0x%x + %d bytes is outside the %d-byte flash", lba, length, len(s.fl.data))
	}
	return append([]byte(nil), s.fl.data[off:off+length]...), nil
}

// ControlOut accepts Download Boot vendor transfers and marks the device as
// booted; a real maskrom would do the same after a valid loader.
func (s *Simulator) ControlOut(requestType, request, value, index uint16, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != rockusb.ModeMaskrom {
		return 0, &Error{Code: rockusb.ErrDeviceNotSupport, CodeName: rockusb.ErrDeviceNotSupport.String(),
			Stage: "control", Detail: "Download Boot vendor requests are maskrom-only"}
	}
	s.controls = append(s.controls, SimControl{Request: index, Value: value, Length: len(data)})
	s.booted = true
	return len(data), nil
}

// BulkOut accepts a CBW or a write data stage.
func (s *Simulator) BulkOut(endpoint uint8, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reset {
		return 0, errDisconnected
	}
	if len(data) == rockusb.CBWLength && binary.LittleEndian.Uint32(data[0:4]) == rockusb.CBWSignature {
		if s.mode != rockusb.ModeLoader {
			// A bare maskrom STALLs CBWs (stage-2 hardware finding).
			s.failures = append(s.failures, "maskrom stalled a CBW")
			return 0, errStalled
		}
		op := Op(data[15])
		lba := binary.LittleEndian.Uint32(data[17:21])
		sectors := int(binary.LittleEndian.Uint16(data[22:24]))
		s.lastTag = binary.LittleEndian.Uint32(data[4:8])
		s.pending = &simCommand{op: op, lba: lba, sectors: sectors,
			dataLen: int(binary.LittleEndian.Uint32(data[8:12])), tag: s.lastTag}

		switch op {
		case OpWriteLBA:
			// The data stage decides success.
		case OpEraseLBA:
			if err := s.inject(op, lba); err != nil {
				s.pending = nil
				return 0, err
			}
			if err := s.applyErase(lba, sectors); err != nil {
				s.pending = nil
				return 0, err
			}
			s.pending.needCSW = true
		case OpDeviceReset:
			s.pending.needCSW = true
		case OpTestUnitReady:
			s.pending.needCSW = true
		default:
			// Read commands produce a data stage first.
			s.pending.needCSW = false
		}
		return len(data), nil
	}

	if s.pending == nil || s.pending.op != OpWriteLBA || s.pending.dataLen != len(data) {
		return 0, fmt.Errorf("sim: unexpected %d-byte bulk OUT", len(data))
	}
	if err := s.inject(OpWriteLBA, s.pending.lba); err != nil {
		s.pending = nil
		return 0, err
	}
	if err := s.applyWrite(s.pending.lba, data); err != nil {
		s.pending = nil
		return 0, err
	}
	s.pending.needCSW = true
	return len(data), nil
}

// BulkIn returns a read data stage or the CSW.
func (s *Simulator) BulkIn(endpoint uint8, length int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reset {
		return nil, errDisconnected
	}
	if s.pending == nil {
		return nil, fmt.Errorf("sim: bulk IN without a command")
	}

	if !s.pending.needCSW && length != rockusb.CSWLength {
		data, err := s.readData(s.pending.op, s.pending.lba, length)
		if err != nil {
			return nil, err
		}
		s.pending.needCSW = true
		return data, nil
	}
	if length != rockusb.CSWLength {
		return nil, fmt.Errorf("sim: %s has no %d-byte data stage", s.pending.op.Name(), length)
	}
	if err := s.inject(s.pending.op, s.pending.lba); err != nil {
		s.pending = nil
		return nil, err
	}
	status := rockusb.StatusPassed
	if s.pending.failCSW {
		status = rockusb.StatusFailed
	}
	csw := cswBytes(s.pending.tag, status)
	op := s.pending.op
	if op == OpDeviceReset {
		s.reset = true
	}
	s.pending = nil
	return csw, nil
}

// Reset aborts the current command (transport level, not DEVICE_RESET).
func (s *Simulator) Reset() error {
	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()
	return nil
}

func (s *Simulator) inject(op Op, lba uint32) error {
	for i := range s.faults {
		f := &s.faults[i]
		if f.Op != nil && *f.Op != op {
			continue
		}
		if f.LBA != nil && *f.LBA != lba {
			continue
		}
		limit := f.Times
		if limit == 0 {
			limit = 1 // zero means "once"; a negative value means every match
		}
		if limit > 0 && f.used >= limit {
			continue
		}
		f.used++
		s.failures = append(s.failures, fmt.Sprintf("%s at op 0x%02X lba 0x%x", f.Kind, uint8(op), lba))
		switch f.Kind {
		case SimTimeout:
			return &rockusb.TimeoutError{What: "simulated transfer", Timeout: 0}
		case SimStall:
			return errStalled
		case SimDisconnect:
			return errDisconnected
		case SimFailed:
			if s.pending != nil {
				s.pending.failCSW = true
			}
			return nil
		}
	}
	return nil
}

func (s *Simulator) applyWrite(lba uint32, data []byte) error {
	off := int64(lba) * SectorSize
	s.fl.mu.Lock()
	defer s.fl.mu.Unlock()
	if off < 0 || off+int64(len(data)) > int64(len(s.fl.data)) {
		return fmt.Errorf("sim: write at LBA 0x%x of %d bytes is outside the %d-byte flash", lba, len(data), len(s.fl.data))
	}
	copy(s.fl.data[off:], data)
	s.writes = append(s.writes, SimWrite{LBA: lba, Sectors: len(data) / SectorSize, Bytes: len(data)})
	return nil
}

func (s *Simulator) applyErase(lba uint32, sectors int) error {
	off := int64(lba) * SectorSize
	n := int64(sectors) * SectorSize
	s.fl.mu.Lock()
	defer s.fl.mu.Unlock()
	if off < 0 || off+n > int64(len(s.fl.data)) {
		return fmt.Errorf("sim: erase at LBA 0x%x of %d sectors is outside the %d-byte flash", lba, sectors, len(s.fl.data))
	}
	for i := off; i < off+n; i++ {
		s.fl.data[i] = 0xFF
	}
	s.erases = append(s.erases, SimErase{LBA: lba, Sectors: sectors})
	return nil
}

func (s *Simulator) readData(op Op, lba uint32, length int) ([]byte, error) {
	switch rockusb.Op(op) {
	case rockusb.OpReadChipInfo:
		return padTo([]byte(s.cfg.Chip), length), nil
	case rockusb.OpReadFlashInfo:
		b := make([]byte, 11)
		binary.LittleEndian.PutUint32(b[0:4], s.cfg.FlashSectors)
		binary.LittleEndian.PutUint16(b[4:6], 128) // block size in sectors
		b[6] = 8                                   // page size in sectors
		b[7] = 4                                   // ECC bits
		b[8] = 0                                   // access time
		b[9] = 1                                   // TOSHIBA
		b[10] = 0x01                               // CS 0
		return padTo(b, length), nil
	case rockusb.OpReadCapability:
		capability := s.cfg.Capability
		return padTo(capability[:], length), nil
	case rockusb.OpReadStorage:
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, 1) // eMMC
		return padTo(b, length), nil
	case rockusb.OpReadLBA:
		off := int64(lba) * SectorSize
		end := off + int64(length)
		s.fl.mu.Lock()
		defer s.fl.mu.Unlock()
		if off < 0 || end > int64(len(s.fl.data)) {
			return nil, fmt.Errorf("sim: READ_LBA at LBA 0x%x of %d bytes is outside the %d-byte flash", lba, length, len(s.fl.data))
		}
		out := make([]byte, length)
		copy(out, s.fl.data[off:end])
		return out, nil
	case rockusb.OpTestUnitReady:
		return nil, fmt.Errorf("sim: TEST_UNIT_READY has no data stage")
	}
	return nil, fmt.Errorf("sim: opcode 0x%02X has no simulated data at lba 0x%x", uint8(op), lba)
}

func padTo(b []byte, length int) []byte {
	if len(b) >= length {
		return b[:length]
	}
	out := make([]byte, length)
	copy(out, b)
	return out
}

// SimConnector hands out one simulator at a time and shares the flash between
// the maskrom and loader instances of the same board.
type SimConnector struct {
	mu     sync.Mutex
	cfg    SimConfig
	flash  *simFlash
	opened int
	first  *Simulator
	sims   []*Simulator
	closes int
}

// NewSimConnector builds a simulator for a whole run.
func NewSimConnector(cfg SimConfig) *SimConnector {
	sectors := cfg.FlashSectors
	if sectors == 0 {
		sectors = 4096
	}
	return &SimConnector{cfg: cfg, flash: &simFlash{data: make([]byte, int(sectors)*SectorSize)}}
}

// Open returns the maskrom instance first (when configured), then LOADER
// instances. While the maskrom instance has not accepted Download Boot data,
// Open fails, which is what a device that has not re-enumerated looks like.
func (c *SimConnector) Open(ctx context.Context) (rockusb.Transport, rockusb.Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, rockusb.Link{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	mode := c.cfg.Mode
	if mode == rockusb.ModeUnknown {
		mode = rockusb.ModeLoader
	}
	if c.opened == 0 {
		sim := newSimulator(c.cfg, mode, c.flash)
		c.first = sim
		c.sims = append(c.sims, sim)
		c.opened++
		return sim, sim.link, nil
	}
	if mode == rockusb.ModeMaskrom && (c.first == nil || !c.first.Booted()) {
		return nil, rockusb.Link{}, fmt.Errorf("sim: device has not re-enumerated yet")
	}
	sim := newSimulator(c.cfg, rockusb.ModeLoader, c.flash)
	c.sims = append(c.sims, sim)
	c.opened++
	return sim, sim.link, nil
}

// Close counts releases.
func (c *SimConnector) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

// Simulators returns every instance handed out.
func (c *SimConnector) Simulators() []*Simulator {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*Simulator(nil), c.sims...)
}

// Last is the most recently opened simulator (the LOADER one after a boot).
func (c *SimConnector) Last() *Simulator {
	sims := c.Simulators()
	if len(sims) == 0 {
		return nil
	}
	return sims[len(sims)-1]
}

// FlashAt reads the shared flash image at lba.
func (c *SimConnector) FlashAt(lba uint32, length int) ([]byte, error) {
	off := int(lba) * SectorSize
	c.flash.mu.Lock()
	defer c.flash.mu.Unlock()
	if off < 0 || off+length > len(c.flash.data) {
		return nil, fmt.Errorf("sim: flash range LBA 0x%x + %d bytes is outside the %d-byte flash", lba, length, len(c.flash.data))
	}
	return append([]byte(nil), c.flash.data[off:off+length]...), nil
}

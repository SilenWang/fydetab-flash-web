package rockusb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"
)

func loaderLink() Link {
	return Link{BulkIn: 1, BulkOut: 2, InterfaceNumber: 0, InterfaceClass: 0xff, VendorID: 0x2207, ProductID: 0x350b, Mode: ModeLoader}
}

func maskromLink() Link {
	l := loaderLink()
	l.Mode = ModeMaskrom
	return l
}

func TestExecReadsChipInfo(t *testing.T) {
	data := make([]byte, 16)
	copy(data, "3588")
	dev := newScriptedDevice(map[Op][]byte{OpReadChipInfo: data})
	var trace []Transfer
	s := NewSession(dev, loaderLink(), Options{Trace: func(tr Transfer) { trace = append(trace, tr) }})

	cmd, _ := CommandFor(OpReadChipInfo)
	res := s.Exec(cmd)

	if !res.OK {
		t.Fatalf("Exec failed: %s", res.Detail)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", res.Attempts)
	}
	info, ok := res.Decoded.(ChipInfo)
	if !ok || info.Text != "3588" {
		t.Fatalf("decoded = %#v, want chip text 3588", res.Decoded)
	}
	if len(trace) != 3 {
		t.Fatalf("trace has %d transfers, want cbw + data + csw", len(trace))
	}
	wantStages := []string{"cbw", "data", "csw"}
	for i, want := range wantStages {
		if trace[i].Stage != want {
			t.Errorf("trace[%d].Stage = %q, want %q", i, trace[i].Stage, want)
		}
		if trace[i].Status != "ok" {
			t.Errorf("trace[%d].Status = %q, want ok", i, trace[i].Status)
		}
	}
	if trace[0].Length != CBWLength {
		t.Errorf("CBW length = %d", trace[0].Length)
	}
	// Only the CBW goes out; the data and CSW stages are IN transfers.
	outs := dev.outbound()
	if len(outs) != 1 || len(outs[0].data) != CBWLength {
		t.Fatalf("outbound transfers = %d, want exactly one %d-byte CBW", len(outs), CBWLength)
	}
}

func TestExecCSWTagMismatchIsCmdNotMatch(t *testing.T) {
	dev := newScriptedDevice(map[Op][]byte{OpReadChipInfo: make([]byte, 16)})
	dev.csw = func(_ uint32, _ Op) []byte {
		b := make([]byte, CSWLength)
		binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
		binary.LittleEndian.PutUint32(b[4:8], 0xDEADBEEF) // wrong tag
		return b
	}
	s := NewSession(dev, loaderLink(), Options{})
	cmd, _ := CommandFor(OpReadChipInfo)
	res := s.Exec(cmd)
	if res.Code != ErrCmdNotMatch {
		t.Fatalf("code = %v, want %v (%s)", res.Code, ErrCmdNotMatch, res.Detail)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", res.Attempts)
	}
}

func TestExecCSWFailedMapsToDeviceUnreadyForTestUnitReady(t *testing.T) {
	dev := newScriptedDevice(nil)
	dev.csw = func(tag uint32, _ Op) []byte {
		b := make([]byte, CSWLength)
		binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
		binary.LittleEndian.PutUint32(b[4:8], tag)
		b[12] = StatusFailed
		return b
	}
	s := NewSession(dev, loaderLink(), Options{})

	ready, _ := CommandFor(OpTestUnitReady)
	if res := s.Exec(ready); res.Code != ErrDeviceUnready {
		t.Errorf("TestUnitReady code = %v, want %v", res.Code, ErrDeviceUnready)
	}

	chip, _ := CommandFor(OpReadChipInfo)
	if res := s.Exec(chip); res.Code != ErrFailed {
		t.Errorf("ReadChipInfo code = %v, want %v", res.Code, ErrFailed)
	}
}

func TestStallIsNeverRetried(t *testing.T) {
	dev := newScriptedDevice(nil)
	stall := errors.New("Failed to execute 'transferOut': A transfer error has occurred")
	dev.bulkOutFn = func(_ uint8, _ []byte) (int, error) { return 0, stall }

	s := NewSession(dev, loaderLink(), Options{Retries: 3, Timeout: 50 * time.Millisecond})
	cmd, _ := CommandFor(OpReadChipInfo)
	res := s.Exec(cmd)

	if res.Code != ErrDeviceWriteFailed {
		t.Fatalf("code = %v, want %v", res.Code, ErrDeviceWriteFailed)
	}
	if res.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1: a STALL is a definitive answer, not a transient one", res.Attempts)
	}
	if got := len(dev.outbound()); got != 1 {
		t.Fatalf("outbound transfers = %d, want 1", got)
	}
}

func TestTimeoutIsRetriedAndReported(t *testing.T) {
	dev := newScriptedDevice(map[Op][]byte{OpReadChipInfo: make([]byte, 16)})
	block := make(chan struct{})
	defer close(block)
	var calls int
	dev.bulkOutFn = func(_ uint8, buf []byte) (int, error) {
		calls++
		if calls == 1 {
			<-block // first attempt never completes
		}
		dev.pendingOp = Op(buf[15])
		dev.pendingTag = binary.LittleEndian.Uint32(buf[4:8])
		dev.pendingLen = binary.LittleEndian.Uint32(buf[8:12])
		dev.sentData = false
		return len(buf), nil
	}

	s := NewSession(dev, loaderLink(), Options{Timeout: 20 * time.Millisecond, Retries: 1})
	cmd, _ := CommandFor(OpReadChipInfo)
	res := s.Exec(cmd)

	if !res.OK {
		t.Fatalf("expected the retry to succeed, got %v: %s", res.Code, res.Detail)
	}
	if res.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", res.Attempts)
	}
	if dev.resets() == 0 {
		t.Error("a timed-out transfer must reset the pipe before retrying")
	}
}

func TestTimeoutExhaustsRetries(t *testing.T) {
	dev := newScriptedDevice(nil)
	block := make(chan struct{})
	defer close(block)
	dev.bulkOutFn = func(_ uint8, _ []byte) (int, error) {
		<-block
		return 0, nil
	}

	s := NewSession(dev, loaderLink(), Options{Timeout: 10 * time.Millisecond, Retries: 1})
	cmd, _ := CommandFor(OpReadChipInfo)
	res := s.Exec(cmd)

	if res.Code != ErrTimeout {
		t.Fatalf("code = %v, want %v (%s)", res.Code, ErrTimeout, res.Detail)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (1 + 1 retry)", res.Attempts)
	}
	if res.CodeName != "ERR_TIMEOUT" {
		t.Errorf("code name = %q", res.CodeName)
	}
	if dev.resets() != 2 {
		t.Errorf("resets = %d, want one per timed-out attempt", dev.resets())
	}
}

func TestShortDataRead(t *testing.T) {
	dev := newScriptedDevice(map[Op][]byte{OpReadChipInfo: make([]byte, 16)})
	dev.bulkInFn = func(_ uint8, length int) ([]byte, error) {
		if length == 16 {
			return make([]byte, 4), nil
		}
		b := make([]byte, CSWLength)
		binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
		binary.LittleEndian.PutUint32(b[4:8], dev.pendingTag)
		return b, nil
	}
	s := NewSession(dev, loaderLink(), Options{})
	chip, _ := CommandFor(OpReadChipInfo)
	if res := s.Exec(chip); res.Code != ErrDeviceReadFailed {
		t.Fatalf("chip info short read code = %v, want %v", res.Code, ErrDeviceReadFailed)
	}

	// Flash info is the documented short-read command: 11 bytes are enough
	// even though the CBW asks for 11 and the device may answer with more.
	dev2 := newScriptedDevice(map[Op][]byte{OpReadFlashInfo: make([]byte, 11)})
	dev2.bulkInFn = func(_ uint8, length int) ([]byte, error) {
		if length == 11 {
			return make([]byte, 11), nil
		}
		b := make([]byte, CSWLength)
		binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
		binary.LittleEndian.PutUint32(b[4:8], dev2.pendingTag)
		return b, nil
	}
	s2 := NewSession(dev2, loaderLink(), Options{})
	flash, _ := CommandFor(OpReadFlashInfo)
	if res := s2.Exec(flash); !res.OK {
		t.Fatalf("flash info should accept a short data stage: %s", res.Detail)
	}
}

func TestExecRefusesOpcodeOutsideTheTable(t *testing.T) {
	dev := newScriptedDevice(nil)
	s := NewSession(dev, loaderLink(), Options{})
	// EncodeCBW would happily encode any opcode; Exec must not, because the
	// command table is the only thing that defines a legal exchange.
	res := s.Exec(Command{Op: Op(0x15), Name: "WRITE_LBA", TransferLength: 512})
	if res.OK || res.Code != ErrDeviceNotSupport {
		t.Fatalf("res = %+v, want a refusal with %v", res, ErrDeviceNotSupport)
	}
	if got := len(dev.outbound()); got != 0 {
		t.Fatalf("no transfer should have happened, got %d", got)
	}
	if got := len(dev.controlOuts()); got != 0 {
		t.Fatalf("no control transfer should have happened, got %d", got)
	}
}

func TestCodeOf(t *testing.T) {
	cases := []struct {
		err  error
		want ErrorCode
	}{
		{nil, ErrSuccess},
		{&Error{Code: ErrCmdNotMatch}, ErrCmdNotMatch},
		{&TimeoutError{What: "x", Timeout: time.Second}, ErrTimeout},
		{fmt.Errorf("boom"), ErrFailed},
	}
	for _, c := range cases {
		if got := CodeOf(c.err); got != c.want {
			t.Errorf("CodeOf(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestErrorCodeNamesMatchRkdeveloptool(t *testing.T) {
	want := map[ErrorCode]string{
		0: "ERR_SUCCESS", -1: "ERR_DEVICE_OPEN_FAILED", -3: "ERR_DEVICE_WRITE_FAILED",
		-4: "ERR_DEVICE_READ_FAILED", -5: "ERR_CMD_NOTMATCH", -6: "ERR_DEVICE_UNREADY",
		-8: "ERR_FAILED", -10: "ERR_DEVICE_NOT_SUPPORT", -12: "ERR_REQUEST_FAIL",
	}
	for code, name := range want {
		if got := code.String(); got != name {
			t.Errorf("ErrorCode(%d).String() = %q, want %q", int(code), got, name)
		}
	}
	if got := ErrTimeout.String(); got != "ERR_TIMEOUT" {
		t.Errorf("ErrTimeout.String() = %q", got)
	}
	if got := ErrorCode(-99).String(); got != "ERR_UNKNOWN(-99)" {
		t.Errorf("unknown code = %q", got)
	}
}

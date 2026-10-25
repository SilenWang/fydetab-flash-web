package rockusb

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The stage-2 hardware run on a real Fydetab Duo (RK3588, 2207:350b, maskrom)
// sent exactly five 31-byte CBWs and got a USB transfer error for each: the
// bare BootROM does not implement rockusb bulk CBW at all. That trace is the
// regression fixture for this stage: the frames must stay byte-identical, and
// the failure must stay "one attempt, error -3, nothing else sent".
const maskromTracePath = "testdata/maskrom-cbw-stall.json"

type replayTrace struct {
	Schema   string `json:"schema"`
	ReadOnly bool   `json:"readOnly"`
	Device   struct {
		VendorID  string `json:"vendorId"`
		ProductID string `json:"productId"`
		BCDUSB    string `json:"bcdUSB"`
		Mode      string `json:"mode"`
	} `json:"device"`
	Link struct {
		InterfaceNumber  int   `json:"interfaceNumber"`
		AlternateSetting int   `json:"alternateSetting"`
		InterfaceClass   int   `json:"interfaceClass"`
		BulkIn           uint8 `json:"bulkIn"`
		BulkOut          uint8 `json:"bulkOut"`
	} `json:"link"`
	Transfers []replayTransfer `json:"transfers"`
	Errors    []struct {
		Code   ErrorCode `json:"code"`
		Name   string    `json:"name"`
		Detail string    `json:"detail"`
	} `json:"errors"`
	ZeroWriteAssertions struct {
		BulkOutTransfers        int `json:"bulkOutTransfers"`
		DataPayloadOutTransfers int `json:"dataPayloadOutTransfers"`
	} `json:"zeroWriteAssertions"`
}

type replayTransfer struct {
	Seq             int    `json:"seq"`
	Kind            string `json:"kind"`
	Endpoint        int    `json:"endpoint"`
	Direction       string `json:"direction"`
	RequestedLength int    `json:"requestedLength"`
	Length          int    `json:"length"`
	Status          string `json:"status"`
	DataHex         string `json:"dataHex"`
	Op              int    `json:"op"`
	OpName          string `json:"opName"`
	Stage           string `json:"stage"`
	Note            string `json:"note"`
}

func loadMaskromTrace(t *testing.T) replayTrace {
	t.Helper()
	raw, err := os.ReadFile(maskromTracePath)
	if err != nil {
		t.Fatalf("read trace fixture: %v", err)
	}
	var trace replayTrace
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatalf("parse trace fixture: %v", err)
	}
	if len(trace.Transfers) == 0 {
		t.Fatal("trace fixture has no transfers")
	}
	return trace
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex in trace fixture: %v", err)
	}
	return raw
}

func TestReplayEncodesTheSameCBWsAsHardware(t *testing.T) {
	trace := loadMaskromTrace(t)
	if len(trace.Transfers) != 5 {
		t.Fatalf("fixture has %d transfers, want the 5 recorded CBWs", len(trace.Transfers))
	}

	for _, tr := range trace.Transfers {
		if tr.Kind != "bulk-out" {
			t.Fatalf("transfer %d kind = %q, want bulk-out", tr.Seq, tr.Kind)
		}
		raw := mustHex(t, tr.DataHex)
		if len(raw) != CBWLength {
			t.Fatalf("transfer %d is %d bytes, want %d", tr.Seq, len(raw), CBWLength)
		}

		tag := binary.LittleEndian.Uint32(raw[4:8])
		op := Op(raw[15])
		transferLength := binary.LittleEndian.Uint32(raw[8:12])

		cmd, ok := CommandFor(op)
		if !ok {
			t.Fatalf("recorded opcode 0x%02X is not in the command table", byte(op))
		}
		if transferLength != cmd.TransferLength {
			t.Errorf("%s transfer length = %d, table says %d", cmd.Name, transferLength, cmd.TransferLength)
		}
		if got := EncodeCBW(tag, op, transferLength); !bytes.Equal(got, raw) {
			t.Errorf("%s CBW drifted:\n got %x\nwant %x", cmd.Name, got, raw)
		}
		if tr.Op != int(op) || tr.OpName != cmd.Name {
			t.Errorf("fixture op bookkeeping disagrees for %s", cmd.Name)
		}
	}

	// The hardware run's own zero-write assertion: every outbound transfer
	// was a CBW, so nothing but a 31-byte frame ever left the host.
	if trace.ZeroWriteAssertions.DataPayloadOutTransfers != 0 {
		t.Fatal("fixture claims a data payload was written; it must not have been")
	}
	if trace.ZeroWriteAssertions.BulkOutTransfers != len(trace.Transfers) {
		t.Fatalf("fixture counts %d bulk OUT transfers but lists %d", trace.ZeroWriteAssertions.BulkOutTransfers, len(trace.Transfers))
	}
}

// TestReplayAgainstMaskromTransport drives the real command table through a
// transport that behaves like the device in the fixture: every bulk OUT is
// rejected. It pins three things a refactor could quietly break:
//
//  1. the frame we put on the wire is byte-identical to the recorded one;
//  2. each command fails with ERR_DEVICE_WRITE_FAILED (-3), like the probe;
//  3. a STALL is never retried, even when retries are enabled.
func TestReplayAgainstMaskromTransport(t *testing.T) {
	trace := loadMaskromTrace(t)

	dev := &mockTransport{}
	cursor := 0
	var attempts int
	dev.bulkOutFn = func(_ uint8, data []byte) (int, error) {
		attempts++
		if cursor >= len(trace.Transfers) {
			t.Fatalf("more outbound transfers than the %d recorded", len(trace.Transfers))
		}
		want := mustHex(t, trace.Transfers[cursor].DataHex)
		if !bytes.Equal(data, want) {
			t.Fatalf("outbound frame %d differs from hardware:\n got %x\nwant %x", cursor, data, want)
		}
		cursor++
		// WebUSB reports a STALL/transfer error like this.
		return 0, errors.New("Failed to execute 'transferOut' on 'USBDevice': A transfer error has occurred.")
	}

	s := NewSession(dev, Link{BulkIn: trace.Link.BulkIn, BulkOut: trace.Link.BulkOut, Mode: ModeMaskrom},
		Options{Retries: 3, Timeout: 100 * time.Millisecond, Tags: func() uint32 {
			if cursor >= len(trace.Transfers) {
				return 0
			}
			raw := mustHex(t, trace.Transfers[cursor].DataHex)
			return binary.LittleEndian.Uint32(raw[4:8])
		}})

	for _, tr := range trace.Transfers {
		cmd, ok := CommandFor(Op(tr.Op))
		if !ok {
			t.Fatalf("%s is not a read-only command", tr.OpName)
		}
		res := s.Exec(cmd)
		if res.Code != ErrDeviceWriteFailed {
			t.Errorf("%s code = %v, want %v (hardware saw %s)", cmd.Name, res.Code, ErrDeviceWriteFailed, tr.Note)
		}
		if res.CodeName != "ERR_DEVICE_WRITE_FAILED" {
			t.Errorf("%s code name = %q", cmd.Name, res.CodeName)
		}
		if res.Attempts != 1 {
			t.Errorf("%s attempts = %d, want 1", cmd.Name, res.Attempts)
		}
		if res.CSW != nil {
			t.Errorf("%s produced a CSW, but the device never answered", cmd.Name)
		}
	}

	if attempts != len(trace.Transfers) {
		t.Fatalf("outbound transfers = %d, want %d", attempts, len(trace.Transfers))
	}
	if outs := dev.outbound(); len(outs) != 5 {
		t.Fatalf("recorded %d bulk OUT transfers", len(outs))
	} else {
		for i, out := range outs {
			if len(out.data) != CBWLength {
				t.Errorf("outbound %d is %d bytes", i, len(out.data))
			}
			if !IsReadOnly(Op(out.data[15])) {
				t.Errorf("outbound %d carries non read-only opcode 0x%02X", i, out.data[15])
			}
		}
	}
	if len(dev.controlOuts()) != 0 {
		t.Error("replaying read commands must not issue control transfers")
	}
}

// TestReplayDeviceDescriptorMatchesMaskrom pins the companion descriptor
// fixture: the same hardware run that produced the CBW trace also recorded
// 2207:350b with bcdUSB 0x0200 (bit0=0 ⇒ MASKROM) and the bulk pair 0x81/0x02.
func TestReplayDeviceDescriptorMatchesMaskrom(t *testing.T) {
	raw, err := os.ReadFile("testdata/maskrom.device.json")
	if err != nil {
		t.Fatalf("read device fixture: %v", err)
	}
	var desc struct {
		Mode string `json:"mode"`
		Link struct {
			BulkIn  uint8 `json:"bulkIn"`
			BulkOut uint8 `json:"bulkOut"`
		} `json:"link"`
		USBDescriptor struct {
			VendorID string `json:"vendorId"`
			BcdUSB   string `json:"bcdUSB"`
			Mode     string `json:"mode"`
		} `json:"usbDescriptor"`
	}
	if err := json.Unmarshal(raw, &desc); err != nil {
		t.Fatalf("parse device fixture: %v", err)
	}
	if desc.USBDescriptor.VendorID != "0x2207" {
		t.Errorf("vendor = %q", desc.USBDescriptor.VendorID)
	}
	if desc.USBDescriptor.BcdUSB != "0x0200" {
		t.Errorf("bcdUSB = %q, want 0x0200", desc.USBDescriptor.BcdUSB)
	}
	if desc.Mode != "MASKROM" || desc.USBDescriptor.Mode != "MASKROM" {
		t.Errorf("mode = %q / %q, want MASKROM", desc.Mode, desc.USBDescriptor.Mode)
	}
	bcd := uint32(0x0200)
	if mode := ModeFromDescriptor(RockchipVendorID, bcd); mode != ModeMaskrom {
		t.Errorf("fixture descriptor maps to %v, want MASKROM", mode)
	}
	if desc.Link.BulkIn != 1 || desc.Link.BulkOut != 2 {
		t.Errorf("link = in %d / out %d, want 1/2", desc.Link.BulkIn, desc.Link.BulkOut)
	}
}

// TestReplayTraceErrorsMatchRecordedCodes checks the fixture's error list
// against the package's code names, so a future stage cannot renumber them.
func TestReplayTraceErrorsMatchRecordedCodes(t *testing.T) {
	trace := loadMaskromTrace(t)
	if len(trace.Errors) != len(trace.Transfers) {
		t.Fatalf("fixture errors = %d, transfers = %d", len(trace.Errors), len(trace.Transfers))
	}
	for i, e := range trace.Errors {
		if e.Code != ErrDeviceWriteFailed {
			t.Errorf("error %d code = %d, want %d", i, e.Code, ErrDeviceWriteFailed)
		}
		if e.Name != ErrDeviceWriteFailed.String() {
			t.Errorf("error %d name = %q, our name is %q", i, e.Name, ErrDeviceWriteFailed.String())
		}
		if e.Detail == "" {
			t.Errorf("error %d has no detail", i)
		}
	}
}

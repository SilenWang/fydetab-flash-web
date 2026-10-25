package uf

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// loaderSessionFrames records what a full uf run against a LOADER device looks
// like: Test Device, Check Chip, Get FlashInfo, the IDB write, every item, and
// reset. failLBA (when set) makes that write's CSW report failure.
func loaderSessionFrames(t *testing.T, plan *Plan, failLBA *uint32) []Frame {
	t.Helper()
	le := binary.LittleEndian
	var frames []Frame

	addRead := func(op rockusb.Op, length int, payload []byte) {
		frames = append(frames,
			Frame{Kind: FrameCBW, Op: intPtr(int(op))},
			Frame{Kind: FrameReadData, Length: intPtr(length), RespondHex: hex.EncodeToString(payload)},
			Frame{Kind: FrameCSW},
		)
	}

	// Test Device has no data stage.
	frames = append(frames, Frame{Kind: FrameCBW, Op: intPtr(int(rockusb.OpTestUnitReady))}, Frame{Kind: FrameCSW})

	chip := padTo([]byte("3588"), 16)
	addRead(rockusb.OpReadChipInfo, 16, chip)

	flash := make([]byte, 11)
	le.PutUint32(flash[0:4], uint32(plan.DiskSectors))
	le.PutUint16(flash[4:6], 128)
	flash[6] = 8
	flash[7] = 4
	flash[9] = 1
	flash[10] = 1
	addRead(rockusb.OpReadFlashInfo, 11, flash)

	addWrite := func(lba uint32, length int) {
		for off := 0; off < length; off += MaxTransferSectors * SectorSize {
			n := length - off
			if n > MaxTransferSectors*SectorSize {
				n = MaxTransferSectors * SectorSize
			}
			start := lba + uint32(off/SectorSize)
			status := FrameStatusOK
			if failLBA != nil && start == *failLBA {
				status = FrameStatusFailed
			}
			frames = append(frames,
				Frame{Kind: FrameCBW, Op: intPtr(int(OpWriteLBA)), LBA: u32Ptr(start), Sectors: intPtr(n / SectorSize)},
				Frame{Kind: FrameWriteData, Length: intPtr(n)},
				Frame{Kind: FrameCSW, Status: status},
			)
		}
	}

	if plan.IDB.Bytes > 0 {
		addWrite(plan.IDB.LBASector, int(plan.IDB.Bytes))
	}
	for _, it := range plan.Items {
		addWrite(it.TargetLBA, int(it.PaddedSize))
	}

	frames = append(frames, Frame{Kind: FrameCBW, Op: intPtr(int(OpDeviceReset))}, Frame{Kind: FrameCSW})
	return frames
}

func TestLoadTraceRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", `{"frames":[]}`},
		{"unknown kind", `{"frames":[{"kind":"nope"}]}`},
		{"unknown status", `{"frames":[{"kind":"cbw","status":"weird"}]}`},
		{"read data without payload", `{"frames":[{"kind":"read-data"}]}`},
		{"wrong schema", `{"schema":"other","frames":[{"kind":"cbw"}]}`},
	}
	for _, c := range cases {
		if _, err := LoadTrace(strings.NewReader(c.in)); err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
	}
}

func TestReplayFullLoaderSession(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	frames := loaderSessionFrames(t, plan, nil)
	conn := NewReplayConnector(ReplaySession{Mode: rockusb.ModeLoader, Frames: frames})

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err != nil {
		t.Fatalf("replayed run: %v (report %+v)", err, rep)
	}
	if !rep.OK || !rep.Reset {
		t.Fatalf("report = %+v", rep)
	}
	tr := conn.Transports()[0]
	if !tr.Done() {
		t.Fatalf("%d recorded frames were not consumed", tr.Remaining())
	}
	if err := tr.Err(); err != nil {
		t.Fatalf("replay mismatch: %v", err)
	}
	if rep.BytesWritten != plan.IDB.Bytes+layout.bootBytes+layout.stateBytes {
		t.Fatalf("bytes written = %d", rep.BytesWritten)
	}
}

func TestReplayDownloadBootThenLoaderSession(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	sessions := []ReplaySession{
		{Mode: rockusb.ModeMaskrom, Frames: maskromBootFrames(t, layout.loader)},
		{Mode: rockusb.ModeLoader, Frames: loaderSessionFrames(t, plan, nil)},
	}
	conn := NewReplayConnector(sessions...)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err != nil {
		t.Fatalf("replayed boot+write run: %v (report %+v)", err, rep)
	}
	if !rep.OK || rep.Device.ReenumAttempts != 1 {
		t.Fatalf("report = %+v", rep)
	}
	transports := conn.Transports()
	if len(transports) != 2 {
		t.Fatalf("transports = %d, want a maskrom and a loader session", len(transports))
	}
	for i, tr := range transports {
		if !tr.Done() {
			t.Fatalf("session %d did not consume every frame (%d left)", i, tr.Remaining())
		}
		if err := tr.Err(); err != nil {
			t.Fatalf("session %d replay error: %v", i, err)
		}
	}
}

func TestReplayMismatchIsSticky(t *testing.T) {
	// The runner sends TEST_UNIT_READY first; the trace claims a write.
	frames := []Frame{
		{Kind: FrameCBW, Op: intPtr(int(OpWriteLBA)), LBA: u32Ptr(0), Sectors: intPtr(1)},
		{Kind: FrameWriteData, Length: intPtr(512)},
		{Kind: FrameCSW},
	}
	tr := NewReplayTransport(frames)
	w := newWriter(tr, simLink(rockusb.ModeLoader), testOptions(), &tracer{})
	err := w.WriteLBAs(context.Background(), 0x40, make([]byte, 512))
	if err == nil {
		t.Fatal("a frame mismatch must fail the transfer")
	}
	if tr.Err() == nil {
		t.Fatal("the replay transport did not record the mismatch")
	}
	if err := w.WriteLBAs(context.Background(), 0x40, make([]byte, 512)); err == nil {
		t.Fatal("the mismatch must be sticky")
	}
}

func TestReplayFailedCSWAbortsAtTheRightItem(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.Loader = nil
	plan.IDB = IDBPlan{}
	failLBA := plan.Items[0].TargetLBA
	frames := loaderSessionFrames(t, plan, &failLBA)
	conn := NewReplayConnector(ReplaySession{Mode: rockusb.ModeLoader, Frames: frames})

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("a failed CSW must abort the run")
	}
	if rep.Error == nil || rep.Error.Item != "boot" {
		t.Fatalf("failure = %+v, want item boot", rep.Error)
	}
	if rep.NextItem != "boot" || !strings.Contains(rep.Hint, "--resume-from boot") {
		t.Fatalf("resume info = next %q hint %q", rep.NextItem, rep.Hint)
	}
	if rep.Reset {
		t.Fatal("a failed run must not reset")
	}
}

func TestReplayShorterThanThePlanIsVisible(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	frames := loaderSessionFrames(t, plan, nil)
	// Cut the recording short: once the frames run out, the next call must
	// fail and be sticky, never be silently skipped.
	tr := NewReplayTransport(frames[:len(frames)-4])
	w := newWriter(tr, simLink(rockusb.ModeLoader), testOptions(), &tracer{})
	if err := w.Reset(context.Background()); err == nil {
		t.Fatal("expected a mismatch when the trace is shorter than the plan")
	}
	if tr.Err() == nil || tr.Remaining() == 0 {
		t.Fatal("the recording-short condition is not observable")
	}
}

func TestFrameStatusMapping(t *testing.T) {
	if err := frameError(Frame{Status: FrameStatusTimeout}); !isTimeout(err) {
		t.Fatalf("timeout status mapped to %v, want a TimeoutError", err)
	}
	for _, status := range []string{FrameStatusStall, FrameStatusDisconnect} {
		if err := frameError(Frame{Status: status}); err == nil {
			t.Fatalf("%s status mapped to nil", status)
		}
	}
	if err := frameError(Frame{Status: FrameStatusFailed}); err != nil {
		t.Fatalf("csw-failed must stay in the CSW, got %v", err)
	}
	if !bytes.Equal(cswBytes(0x1234, rockusb.StatusFailed)[12:13], []byte{1}) {
		t.Fatal("cswBytes did not encode the failed status")
	}
}

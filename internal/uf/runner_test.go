package uf

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

func runPlan(t *testing.T, plan *Plan, layout *testLayout, conn Connector, opts Options) (*Report, error) {
	t.Helper()
	return Run(context.Background(), plan, &memSource{data: layout.source}, conn, opts)
}

func TestRunSuccessWritesIDBAndItemsThenResets(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	conn := NewSimConnector(testSimConfig(plan))
	prog := &progressLog{}
	opts := testOptions()
	opts.Progress = prog.record

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err != nil {
		t.Fatalf("run: %v (report %+v)", err, rep)
	}
	if !rep.OK || rep.Cancelled {
		t.Fatalf("report not ok: %+v", rep)
	}

	// IDB: the default model's bytes at LBA 64.
	wantIDB, err := NewRK28RC4Model().Build(LoaderInput{Container: layout.loader})
	if err != nil {
		t.Fatalf("build idb: %v", err)
	}
	gotIDB, err := conn.FlashAt(IDBDefaultLBA, len(wantIDB))
	if err != nil {
		t.Fatalf("read IDB from flash: %v", err)
	}
	if !bytes.Equal(gotIDB, wantIDB) {
		t.Fatalf("flash at IDB LBA does not hold the %s bytes", ModelRK28RC4)
	}
	if rep.IDB == nil || rep.IDB.LBA != IDBDefaultLBA || rep.IDB.Model != ModelRK28RC4 || rep.IDB.Verified {
		t.Fatalf("IDB report = %+v", rep.IDB)
	}

	// Items: exact payloads at their target LBAs, padding included.
	for _, it := range plan.Items {
		want := layout.source[it.FileOffset : it.FileOffset+it.PaddedSize]
		got, err := conn.FlashAt(it.TargetLBA, len(want))
		if err != nil {
			t.Fatalf("read %s: %v", it.Name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("flash for item %q does not match the image payload", it.Name)
		}
	}

	if !conn.Last().ResetSent() {
		t.Fatal("rd was not issued")
	}
	if !rep.Reset {
		t.Fatal("report does not record the reset")
	}
	wantWritten := plan.IDB.Bytes + layout.bootBytes + layout.stateBytes
	if rep.BytesWritten != wantWritten {
		t.Fatalf("bytes written = %d, want %d", rep.BytesWritten, wantWritten)
	}
	if got := prog.last(); got.Percent != 100 {
		t.Fatalf("final progress = %.1f%%, want 100%%", got.Percent)
	}
	for i := 1; i < len(prog.events); i++ {
		if prog.events[i].Done < prog.events[i-1].Done {
			t.Fatalf("progress went backwards at event %d: %d -> %d", i, prog.events[i-1].Done, prog.events[i].Done)
		}
	}
	if len(rep.Items) != 2 || rep.Items[0].Status != "ok" || rep.Items[1].Status != "ok" {
		t.Fatalf("item reports = %+v", rep.Items)
	}
	if !reflect.DeepEqual(rep.Completed, []string{"boot", "state"}) {
		t.Fatalf("completed items = %v", rep.Completed)
	}
	for _, ph := range rep.Phases {
		if ph.Status != "ok" {
			t.Fatalf("phase %s/%s status = %s", ph.Phase, ph.Name, ph.Status)
		}
	}
}

func TestRunMaskromBootsThenReenumerates(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	cfg := testSimConfig(plan)
	cfg.Mode = rockusb.ModeMaskrom
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err != nil {
		t.Fatalf("run: %v (report %+v)", err, rep)
	}
	sims := conn.Simulators()
	if len(sims) < 2 {
		t.Fatalf("expected a maskrom and a loader device instance, got %d", len(sims))
	}
	if len(sims[0].Controls()) == 0 {
		t.Fatal("Download Boot sent no vendor control transfers")
	}
	if !sims[0].Booted() {
		t.Fatal("maskrom instance did not accept the bootloader")
	}
	if rep.Device.ReenumAttempts != 1 {
		t.Fatalf("re-enumeration attempts = %d, want 1", rep.Device.ReenumAttempts)
	}
	if !conn.Last().ResetSent() {
		t.Fatal("rd was not issued on the loader instance")
	}
}

func TestRunNoResetSkipsRd(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	conn := NewSimConnector(testSimConfig(plan))
	opts := testOptions()
	opts.NoReset = true

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if conn.Last().ResetSent() || rep.Reset {
		t.Fatal("--noreset still issued DEVICE_RESET")
	}
	var rd *PhaseReport
	for i := range rep.Phases {
		if rep.Phases[i].Phase == PhaseReset {
			rd = &rep.Phases[i]
		}
	}
	if rd == nil || rd.Status != "skipped" {
		t.Fatalf("rd phase = %+v, want skipped", rd)
	}
	if !strings.Contains(strings.Join(rep.Notes, " "), "--noreset") {
		t.Fatalf("notes do not mention --noreset: %v", rep.Notes)
	}
}

func TestRunFailedWriteAbortsWithResumeHint(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	stateLBA := plan.Items[1].TargetLBA
	cfg := testSimConfig(plan)
	cfg.Faults = []SimFault{{Kind: SimFailed, LBA: &stateLBA, Times: -1}}
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("a failed CSW must abort the run")
	}
	if rep.OK || rep.Error == nil {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Error.Code != rockusb.ErrFailed {
		t.Fatalf("error code = %s, want ERR_FAILED", rep.Error.CodeName)
	}
	if rep.Error.Phase != PhaseWrite || rep.Error.Item != "state" {
		t.Fatalf("failure located at %s/%s, want wl/state", rep.Error.Phase, rep.Error.Item)
	}
	if !reflect.DeepEqual(rep.Completed, []string{"boot"}) {
		t.Fatalf("completed = %v, want [boot]", rep.Completed)
	}
	if rep.NextItem != "state" {
		t.Fatalf("next item = %q, want state", rep.NextItem)
	}
	if !strings.Contains(rep.Hint, "--resume-from state") {
		t.Fatalf("hint does not say where to resume: %q", rep.Hint)
	}
	if conn.Last().ResetSent() {
		t.Fatal("a failed run must not reset the device")
	}
	if rep.Items[len(rep.Items)-1].Status != "failed" {
		t.Fatalf("item report = %+v", rep.Items[len(rep.Items)-1])
	}
}

func TestRunResumeFromSkipsEarlierItems(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	conn := NewSimConnector(testSimConfig(plan))
	opts := testOptions()
	opts.StartItem = "state"

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !rep.Resumed || rep.StartItem != "state" {
		t.Fatalf("resume flags = %v/%q", rep.Resumed, rep.StartItem)
	}
	if !reflect.DeepEqual(rep.Skipped, []string{"boot"}) {
		t.Fatalf("skipped = %v, want [boot]", rep.Skipped)
	}
	for _, w := range conn.Last().Writes() {
		if w.LBA >= 0x1000 && w.LBA < 0x1000+uint32(layout.bootBytes/SectorSize) {
			t.Fatalf("boot item was written during a --resume-from state run: %+v", w)
		}
	}
	if rep.BytesWritten != plan.IDB.Bytes+layout.stateBytes {
		t.Fatalf("bytes written = %d, want IDB + state only", rep.BytesWritten)
	}
	if !conn.Last().ResetSent() {
		t.Fatal("resumed run did not reset")
	}
}

func TestRunResumeFromUnknownItemFailsFast(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	conn := NewSimConnector(testSimConfig(plan))
	opts := testOptions()
	opts.StartItem = "nope"
	if _, err := runPlan(t, plan, layout, conn, opts); err == nil {
		t.Fatal("expected an error for an unknown --resume-from item")
	}
	if len(conn.Simulators()) != 0 {
		t.Fatal("the device was opened before the plan was validated")
	}
}

func TestRunTimeoutIsRetriedAndRunContinues(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	bootLBA := plan.Items[0].TargetLBA
	cfg := testSimConfig(plan)
	cfg.Faults = []SimFault{{Kind: SimTimeout, LBA: &bootLBA, Times: 1}}
	conn := NewSimConnector(cfg)
	opts := testOptions()
	opts.Retries = 1

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err != nil {
		t.Fatalf("run after a retried timeout: %v", err)
	}
	if !rep.OK {
		t.Fatalf("report = %+v", rep)
	}
	if got := len(conn.Last().InjectedFailures()); got != 1 {
		t.Fatalf("injected failures = %d, want exactly one timeout", got)
	}
	want := layout.source[layout.bootOff : layout.bootOff+layout.bootBytes]
	got, err := conn.FlashAt(bootLBA, len(want))
	if err != nil {
		t.Fatalf("read boot: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the retried write did not land the payload")
	}
}

func TestRunDisconnectAbortsWithResumeHint(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	stateLBA := plan.Items[1].TargetLBA
	cfg := testSimConfig(plan)
	cfg.Faults = []SimFault{{Kind: SimDisconnect, LBA: &stateLBA, Times: 1}}
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("a disconnect must abort the run")
	}
	if rep.Error == nil {
		t.Fatalf("report has no RunError: %+v", rep)
	}
	if rep.Error.Code != rockusb.ErrDeviceWriteFailed {
		t.Fatalf("error code = %s, want ERR_DEVICE_WRITE_FAILED", rep.Error.CodeName)
	}
	if rep.NextItem != "state" || !strings.Contains(rep.Hint, "--resume-from state") {
		t.Fatalf("recovery info = next %q hint %q", rep.NextItem, rep.Hint)
	}
	if !strings.Contains(rep.Hint, "授权") {
		t.Fatalf("hint does not mention re-authorizing the device: %q", rep.Hint)
	}
}

func TestRunCancelledMidItemStopsWithoutReset(t *testing.T) {
	const itemBytes = 2 << 20
	src := make([]byte, itemBytes)
	for i := range src {
		src[i] = byte(i)
	}
	plan := &Plan{
		Image:       "big.img",
		Items:       []Item{{Name: "big", FileOffset: 0, Size: itemBytes, PaddedSize: itemBytes, TargetLBA: 0x100}},
		DiskSectors: 0x8000,
		DiskKnown:   true,
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("plan: %v", err)
	}
	conn := NewSimConnector(SimConfig{FlashSectors: 0x8000})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := testOptions()
	chunks := 0
	opts.Progress = func(p Progress) {
		if p.Phase == PhaseWrite && p.ItemDone > 0 {
			chunks++
			cancel()
		}
	}
	rep, err := Run(ctx, plan, &memSource{data: src}, conn, opts)
	if err == nil {
		t.Fatal("a cancelled run must return an error")
	}
	if !rep.Cancelled {
		t.Fatalf("report is not marked cancelled: %+v", rep)
	}
	if got := CodeOf(err); got != ErrCancelled {
		t.Fatalf("error code = %s, want ERR_CANCELLED", CodeName(got))
	}
	if rep.NextItem != "big" {
		t.Fatalf("next item = %q, want big", rep.NextItem)
	}
	if conn.Last().ResetSent() {
		t.Fatal("a cancelled run must not reset the device")
	}
	if chunks != 1 {
		t.Fatalf("progress events before cancellation = %d, want 1", chunks)
	}
	if rep.Items[0].Status != "cancelled" {
		t.Fatalf("item status = %q, want cancelled", rep.Items[0].Status)
	}
}

func TestRunChipMismatchAbortsBeforeWriting(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	cfg := testSimConfig(plan)
	cfg.Chip = "3566"
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("a chip mismatch must abort")
	}
	if rep.Error == nil || rep.Error.Phase != PhaseChip {
		t.Fatalf("failure = %+v, want the chip phase", rep.Error)
	}
	for _, w := range conn.Last().Writes() {
		t.Fatalf("the device was written despite the chip mismatch: %+v", w)
	}
}

func TestRunFlashTooSmallAbortsBeforeWriting(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	cfg := testSimConfig(plan)
	cfg.FlashSectors = 0x1000 // smaller than the state item's LBA
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("a target LBA outside the flash must abort")
	}
	if rep.Error == nil || rep.Error.Phase != PhaseFlash {
		t.Fatalf("failure = %+v, want the flash phase", rep.Error)
	}
	if len(conn.Last().Writes()) != 0 {
		t.Fatalf("writes happened before the bounds check: %+v", conn.Last().Writes())
	}
}

func TestRunIDBLengthMismatchWarnsButWrites(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.IDB.Bytes += 512 // dry-run plan and model disagree on the byte count
	conn := NewSimConnector(testSimConfig(plan))

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(strings.Join(rep.Warnings, " "), "IDB") {
		t.Fatalf("warnings do not report the IDB size mismatch: %v", rep.Warnings)
	}
	if rep.IDB.PlannedBytes != plan.IDB.Bytes {
		t.Fatalf("IDB planned bytes = %d, want %d", rep.IDB.PlannedBytes, plan.IDB.Bytes)
	}
}

func TestRunEraseTailModes(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	tail := plan.Tails[0]
	tailLBA := uint32(tail.FirstLBA)

	t.Run("none leaves the tail alone", func(t *testing.T) {
		conn := NewSimConnector(testSimConfig(plan))
		rep, err := runPlan(t, plan, layout, conn, testOptions())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if rep.Erase == nil || rep.Erase.Status != "skipped" || rep.Erase.Mode != EraseNone {
			t.Fatalf("erase report = %+v", rep.Erase)
		}
		if len(conn.Last().Erases()) != 0 {
			t.Fatal("EraseNone issued ERASE_LBA")
		}
		for _, w := range conn.Last().Writes() {
			if w.LBA >= tailLBA && w.LBA < tailLBA+uint32(tail.Bytes/SectorSize) {
				t.Fatalf("EraseNone wrote the uncovered tail: %+v", w)
			}
		}
		if !strings.Contains(strings.Join(rep.Notes, " "), "未覆盖") {
			t.Fatalf("notes do not record the uncovered tail decision: %v", rep.Notes)
		}
	})

	t.Run("fill writes 0xFF", func(t *testing.T) {
		conn := NewSimConnector(testSimConfig(plan))
		opts := testOptions()
		opts.EraseTail = EraseFill
		rep, err := runPlan(t, plan, layout, conn, opts)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if rep.Erase == nil || rep.Erase.Status != "ok" || rep.Erase.Mode != EraseFill {
			t.Fatalf("erase report = %+v", rep.Erase)
		}
		if rep.BytesErased != tail.Bytes {
			t.Fatalf("bytes erased = %d, want %d", rep.BytesErased, tail.Bytes)
		}
		got, err := conn.FlashAt(tailLBA, int(tail.Bytes))
		if err != nil {
			t.Fatalf("read tail: %v", err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{0xFF}, int(tail.Bytes))) {
			t.Fatal("the tail was not filled with 0xFF")
		}
	})

	t.Run("erase-lba erases the tail", func(t *testing.T) {
		conn := NewSimConnector(testSimConfig(plan))
		opts := testOptions()
		opts.EraseTail = EraseLBA
		rep, err := runPlan(t, plan, layout, conn, opts)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		erases := conn.Last().Erases()
		if len(erases) != 1 {
			t.Fatalf("erase commands = %+v, want one", erases)
		}
		if erases[0].LBA != tailLBA || erases[0].Sectors != int(tail.Bytes/SectorSize) {
			t.Fatalf("erase = %+v, want LBA 0x%x sectors %d", erases[0], tailLBA, tail.Bytes/SectorSize)
		}
		if rep.Erase.Status != "ok" {
			t.Fatalf("erase report = %+v", rep.Erase)
		}
	})
}

func TestRunMaskromWithoutLoaderFails(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.Loader = nil
	plan.IDB = IDBPlan{}
	cfg := testSimConfig(plan)
	cfg.Mode = rockusb.ModeMaskrom
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("a maskrom device without a loader must fail")
	}
	if rep.Error == nil || rep.Error.Phase != PhaseBoot {
		t.Fatalf("failure = %+v, want the ul phase", rep.Error)
	}
}

func TestRunReenumerationTimeoutExplainsReauthorization(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)

	frames := maskromBootFrames(t, layout.loader)
	conn := NewReplayConnector(ReplaySession{Mode: rockusb.ModeMaskrom, Frames: frames})

	now := time.Unix(1700000000, 0)
	opts := testOptions()
	opts.Now = func() time.Time { return now }
	opts.Sleep = func(d time.Duration) { now = now.Add(d) }
	opts.ReenumTimeout = 2 * time.Second
	opts.ReenumPoll = 100 * time.Millisecond

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err == nil {
		t.Fatal("expected a re-enumeration timeout")
	}
	if rep.Error == nil || rep.Error.Phase != PhaseBoot {
		t.Fatalf("failure = %+v", rep.Error)
	}
	if !strings.Contains(rep.Hint, "授权") {
		t.Fatalf("hint does not explain browser re-authorization: %q", rep.Hint)
	}
	if err := conn.Err(); err != nil {
		t.Fatalf("the boot trace itself failed to replay: %v", err)
	}
	if !conn.Transports()[0].Done() {
		t.Fatalf("%d boot frames were not consumed", conn.Transports()[0].Remaining())
	}
}

// maskromBootFrames records the control transfers Download Boot will send for a
// loader, so a replay can stand in for a bare maskrom.
func maskromBootFrames(t *testing.T, loader []byte) []Frame {
	t.Helper()
	img, err := rockusb.ParseLoaderImage(loader)
	if err != nil {
		t.Fatalf("parse loader: %v", err)
	}
	var frames []Frame
	for _, seg := range img.Plan() {
		plan, err := rockusb.PlanVendorRequest(seg.Request, img.Segment(seg.Entry))
		if err != nil {
			t.Fatalf("plan vendor request: %v", err)
		}
		for _, packet := range plan.Packets {
			frames = append(frames, Frame{
				Kind:    FrameControlOut,
				Request: intPtr(int(seg.Request)),
				Length:  intPtr(len(packet)),
			})
		}
		if plan.PendingZeroWrite {
			frames = append(frames, Frame{
				Kind:    FrameControlOut,
				Request: intPtr(int(seg.Request)),
				Length:  intPtr(1),
			})
		}
	}
	return frames
}

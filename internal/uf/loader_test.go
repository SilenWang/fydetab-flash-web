package uf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	deviceapi "github.com/SilenWang/rk3588-webflasher/internal/device"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// --- loader self-check ------------------------------------------------------

func TestRunReportsLoaderIdentity(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	cfg := testSimConfig(plan)
	cfg.Mode = rockusb.ModeMaskrom
	conn := NewSimConnector(cfg)

	opts := testOptions()
	var events []Event
	opts.Event = func(ev Event) { events = append(events, ev) }

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Loader == nil {
		t.Fatal("report has no loader identity")
	}
	sum := sha256.Sum256(layout.loader)
	if rep.Loader.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want the digest of the loader the run sent", rep.Loader.SHA256)
	}
	if rep.Loader.Source != "image" || rep.Loader.NonImage {
		t.Errorf("source = %q nonImage = %v, want image/false", rep.Loader.Source, rep.Loader.NonImage)
	}
	if rep.Loader.Length != int64(len(layout.loader)) || rep.Loader.Chip != "RK3588" {
		t.Errorf("loader report = %+v", rep.Loader)
	}
	if rep.Loader.ChipCheck != "ok" || rep.Loader.ChipExpected != "RK3588" {
		t.Errorf("chip check = %q (expected %q), want ok/RK3588", rep.Loader.ChipCheck, rep.Loader.ChipExpected)
	}
	if rep.Loader.BuildTime != "unknown" {
		// The fixture leaves the RKBOOT timestamp zeroed; a zero date must be
		// reported as unknown, never as year 0.
		t.Errorf("build time = %q, want unknown for a zeroed timestamp", rep.Loader.BuildTime)
	}
	found := false
	for _, ev := range events {
		if strings.Contains(ev.Message, "loader 自检") {
			found = true
		}
	}
	if !found {
		t.Error("the run log should state the loader self-check result")
	}
}

func TestRunRefusesLoaderForAnotherChipBeforeTouchingTheDevice(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.Chip = "RK3566" // image for another chip than the fixture loader
	cfg := testSimConfig(plan)
	cfg.Mode = rockusb.ModeMaskrom
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("a loader/image chip mismatch must be refused")
	}
	if rep == nil || rep.Error == nil {
		t.Fatalf("report = %+v, want a classified failure", rep)
	}
	if rep.Error.Code != ErrLoaderMismatch || rep.Error.CodeName != "ERR_LOADER_MISMATCH" {
		t.Errorf("code = %s (%d), want ERR_LOADER_MISMATCH", rep.Error.CodeName, rep.Error.Code)
	}
	if rep.Error.Category != "loader-chip-mismatch" || rep.Error.Phase != PhaseBoot {
		t.Errorf("category/phase = %q/%q", rep.Error.Category, rep.Error.Phase)
	}
	if rep.Loader == nil || rep.Loader.ChipCheck != "mismatch" {
		t.Fatalf("loader report = %+v, want a recorded mismatch", rep.Loader)
	}
	if !strings.Contains(rep.Error.Hint, "loader") {
		t.Errorf("hint should point at the loader: %q", rep.Error.Hint)
	}
	// The check runs before Open, so no device instance was ever created and
	// not one control transfer was sent.
	if sims := conn.Simulators(); len(sims) != 0 {
		t.Fatalf("the device was opened despite the mismatch: %d instance(s)", len(sims))
	}
}

func TestRunRefusalMentionsANonImageLoader(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.Chip = "RK3566"
	// A user-picked loader is marked NonImage; the hint must send the user
	// back to the image's own loader rather than to a cable hunt.
	plan.Loader = &LoaderPlan{
		Source:   "file",
		Name:     "rk3588_spl_loader_v1.06.111.bin",
		Length:   int64(len(layout.loader)),
		Bytes:    layout.loader,
		Chip:     "RK3588",
		NonImage: true,
	}
	conn := NewSimConnector(testSimConfig(plan))

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if rep.Loader == nil || !rep.Loader.NonImage || rep.Loader.Source != "file" {
		t.Fatalf("loader report = %+v, want the non-image source recorded", rep.Loader)
	}
	if !strings.Contains(rep.Error.Hint, "镜像自带") {
		t.Errorf("hint should tell the user to use the image's own loader: %q", rep.Error.Hint)
	}
}

// --- Download Boot failure split -------------------------------------------

// rejectControl wraps a transport and makes the first n vendor control
// transfers fail, which is what a BootROM rejection looks like on the wire.
type rejectControl struct {
	rockusb.Transport
	after int // fail on the (after+1)-th control transfer
	n     int
	err   error
}

func (r *rejectControl) ControlOut(requestType, request, value, index uint16, data []byte) (int, error) {
	r.n++
	if r.n > r.after {
		return 0, r.err
	}
	return r.Transport.ControlOut(requestType, request, value, index, data)
}

type rejectConnector struct {
	inner Connector
	after int
	err   error
}

func (c *rejectConnector) Open(ctx context.Context) (rockusb.Transport, rockusb.Link, error) {
	tr, link, err := c.inner.Open(ctx)
	if err != nil {
		return nil, rockusb.Link{}, err
	}
	return &rejectControl{Transport: tr, after: c.after, err: c.err}, link, nil
}

func (c *rejectConnector) Close() error { return c.inner.Close() }

func TestRunSplitsFirstBlockRejectionFromMidTransfer(t *testing.T) {
	layout := newTestLayout(t)

	t.Run("first block (index 0)", func(t *testing.T) {
		plan := layout.plan(t)
		cfg := testSimConfig(plan)
		cfg.Mode = rockusb.ModeMaskrom
		conn := &rejectConnector{
			inner: NewSimConnector(cfg),
			after: 0,
			err:   errors.New("Failed to execute 'controlOut': A transfer error has occurred."),
		}

		rep, err := runPlan(t, plan, layout, conn, testOptions())
		if err == nil {
			t.Fatal("expected Download Boot to fail")
		}
		if rep.DB == nil {
			t.Fatalf("report has no db failure split: %+v", rep)
		}
		if rep.DB.Category != "first-block" || rep.DB.Index != 0 {
			t.Errorf("db split = %+v, want first-block/index 0", rep.DB)
		}
		if rep.Error.Category != "first-block" {
			t.Errorf("run error category = %q, want first-block", rep.Error.Category)
		}
		if !strings.Contains(rep.Error.Hint, "index 0") || !strings.Contains(rep.Error.Hint, "干净的上电周期") {
			t.Errorf("hint should separate loader mismatch from the power-on cycle: %q", rep.Error.Hint)
		}
		if rep.Phases[0].Status != "failed" && rep.Phases[0].Phase != PhaseBoot {
			t.Errorf("the boot phase should be reported as failed: %+v", rep.Phases)
		}
	})

	t.Run("mid transfer", func(t *testing.T) {
		plan := layout.plan(t)
		cfg := testSimConfig(plan)
		cfg.Mode = rockusb.ModeMaskrom
		conn := &rejectConnector{
			inner: NewSimConnector(cfg),
			after: 1, // let the first block through, fail the next one
			err:   errors.New("device disappeared"),
		}

		rep, err := runPlan(t, plan, layout, conn, testOptions())
		if err == nil {
			t.Fatal("expected Download Boot to fail")
		}
		if rep.DB == nil || rep.DB.Category != "mid-transfer" {
			t.Fatalf("db split = %+v, want mid-transfer", rep.DB)
		}
		if rep.DB.Index != 1 {
			t.Errorf("failed index = %d, want 1 (the second data block)", rep.DB.Index)
		}
		if rep.Error.Category != "mid-transfer" {
			t.Errorf("run error category = %q", rep.Error.Category)
		}
		if !strings.Contains(rep.Error.Hint, "首块已经被接受") {
			t.Errorf("hint should say the first block was accepted: %q", rep.Error.Hint)
		}
	})
}

// --- device.Bridge binding --------------------------------------------------

// simBridge is device.Bridge over a SimConnector: it reports the simulator's
// current mode the way a WebUSB listing would (bcdUSB bit0) and opens the
// instance the SimConnector hands out. It is the smallest honest stand-in for
// cmd/wasm's jsBridge.
type simBridge struct {
	conn *SimConnector
	// handles lets a test pin one of two devices by handle.
	handles map[string]bool
}

func (b *simBridge) List() ([]deviceapi.RawInfo, error) {
	if b.handles != nil {
		out := make([]deviceapi.RawInfo, 0, len(b.handles))
		for handle := range b.handles {
			out = append(out, b.raw(handle, b.mode()))
		}
		return out, nil
	}
	return []deviceapi.RawInfo{b.raw("sim", b.mode())}, nil
}

func (b *simBridge) mode() rockusb.MediaMode {
	sims := b.conn.Simulators()
	if len(sims) == 0 {
		if b.conn.cfg.Mode == rockusb.ModeMaskrom {
			return rockusb.ModeMaskrom
		}
		return rockusb.ModeLoader
	}
	if sims[0].Booted() {
		return rockusb.ModeLoader
	}
	return sims[0].Mode()
}

func (b *simBridge) raw(handle string, mode rockusb.MediaMode) deviceapi.RawInfo {
	sub := uint8(0)
	if mode == rockusb.ModeLoader {
		sub = 1
	}
	return deviceapi.RawInfo{
		VendorID: rockusb.RockchipVendorID, ProductID: 0x350b,
		USBVersionMajor: 2, USBVersionMinor: 0, USBVersionSubminor: sub,
		ProductName: "sim", InterfaceNumber: 0, InterfaceClass: 0xff,
		BulkIn: 0x81, BulkOut: 0x02, Handle: handle,
	}
}

func (b *simBridge) Open(devNo int, handle string) (rockusb.Transport, error) {
	tr, _, err := b.conn.Open(context.Background())
	return tr, err
}

func (b *simBridge) Close(devNo int) error { return b.conn.Close() }

func TestBridgeConnectorDrivesTheRealWritePath(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	cfg := testSimConfig(plan)
	cfg.Mode = rockusb.ModeMaskrom
	sim := NewSimConnector(cfg)

	conn := &BridgeConnector{Bridge: &simBridge{conn: sim}, Handle: "sim", MaskromFirst: true}
	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err != nil {
		t.Fatalf("run over the bridge failed: %v (connector saw %v)", err, conn.lastErr)
	}
	if conn.OpenCount() != 2 {
		t.Fatalf("opens = %d, want maskrom then loader", conn.OpenCount())
	}
	if rep.Device.Mode != "LOADER" {
		t.Errorf("device mode after boot = %q, want LOADER", rep.Device.Mode)
	}
	if !sim.Last().ResetSent() {
		t.Error("rd was not sent through the bridge transport")
	}
	// The flash really changed: the first item landed at its target LBA.
	first := plan.Items[0]
	want := layout.source[first.FileOffset : first.FileOffset+first.PaddedSize]
	got, err := sim.FlashAt(first.TargetLBA, len(want))
	if err != nil {
		t.Fatalf("FlashAt: %v", err)
	}
	if string(got) != string(want) {
		t.Error("flash content does not match the image payload written through the bridge")
	}
}

func TestBridgeConnectorExplainsAMissingDevice(t *testing.T) {
	empty := &emptyBridge{}
	conn := &BridgeConnector{Bridge: empty, MaskromFirst: true}
	if _, _, err := conn.Open(context.Background()); err == nil {
		t.Fatal("Open with no authorized device must fail")
	} else {
		if !strings.Contains(err.Error(), "没有处于 MASKROM") || !strings.Contains(err.Error(), "none") {
			t.Errorf("error should name the expected mode and what was listed: %v", err)
		}
	}
	listed, lerr := conn.LastSeen()
	if len(listed) != 0 || lerr == nil {
		t.Errorf("LastSeen = %v/%v, want the failure recorded", listed, lerr)
	}
	if conn.OpenCount() != 0 {
		t.Error("nothing was opened")
	}
}

type emptyBridge struct{}

func (emptyBridge) List() ([]deviceapi.RawInfo, error) { return nil, nil }
func (emptyBridge) Open(int, string) (rockusb.Transport, error) {
	return nil, errors.New("unreachable")
}
func (emptyBridge) Close(int) error { return nil }

// SIL-515 started with the board already in LOADER. The old MaskromFirst path
// failed the first Open in that state, so the runner never reached
// bootIfNeeded and "already in Loader, skip Download Boot" was unreachable.
func TestBridgeConnectorStartsFromLoaderWithoutDownloadBoot(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	cfg := testSimConfig(plan)
	cfg.Mode = rockusb.ModeLoader
	sim := NewSimConnector(cfg)

	conn := &BridgeConnector{Bridge: &simBridge{conn: sim}, Handle: "sim", MaskromFirst: true}
	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err != nil {
		t.Fatalf("run from LOADER failed: %v (connector saw %v)", err, conn.lastErr)
	}
	if conn.OpenCount() != 1 {
		t.Fatalf("opens = %d, want the already-running LOADER device", conn.OpenCount())
	}
	if rep.Device.Mode != "LOADER" {
		t.Errorf("device mode = %q, want LOADER", rep.Device.Mode)
	}
	for _, ph := range rep.Phases {
		if ph.Phase == PhaseBoot && ph.Status == "ok" {
			t.Errorf("Download Boot ran even though the device was already in LOADER: %+v", ph)
		}
	}
	if got := sim.Last().Controls(); len(got) != 0 {
		t.Errorf("Download Boot control transfers = %d, want 0", len(got))
	}
	if !rep.OK || !sim.Last().ResetSent() {
		t.Errorf("run did not finish: ok=%t reset=%t", rep.OK, sim.Last().ResetSent())
	}
}

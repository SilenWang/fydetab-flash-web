package uf

import (
	"context"
	"fmt"
	"strings"
	"sync"

	deviceapi "github.com/SilenWang/rk3588-webflasher/internal/device"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// BridgeConnector binds the write state machine to the real transport.
//
// It is the M3b half of `internal/uf`: the runner already speaks only to a
// rockusb.Transport, so the same state machine that the simulator and the
// replay trace exercise drives the browser-authorized device through
// deviceapi.Bridge (the page's WebUSB host, via cmd/wasm's jsBridge). Nothing in
// the runner changes; the mock and replay connectors stay as they are.
//
// Two things this layer owns that the runner cannot see:
//
//   - device selection: the page authorizes one *instance*, so Open pins the
//     opaque Handle the page listed rather than an index that can reorder;
//   - re-enumeration: after Download Boot the old instance disappears. The
//     first Open looks for MASKROM, every later Open for LOADER, which is
//     exactly the sequence the runner performs.
type BridgeConnector struct {
	Bridge deviceapi.Bridge
	// Handle pins the physical device the user authorized. Empty means "the
	// first Rockchip device in the expected mode".
	Handle string
	// MaskromFirst makes the first Open look for a maskrom device and every
	// later one for a LOADER device.
	MaskromFirst bool

	mu      sync.Mutex
	opens   int
	devNo   int
	last    []deviceapi.Info
	lastErr error
}

// Open lists the authorized devices, picks one in the mode this step expects
// and claims its interface.
func (c *BridgeConnector) Open(ctx context.Context) (rockusb.Transport, rockusb.Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, rockusb.Link{}, err
	}
	if c.Bridge == nil {
		return nil, rockusb.Link{}, fmt.Errorf("uf: no device bridge bound")
	}

	c.mu.Lock()
	want := rockusb.ModeLoader
	maskromFirst := false
	if c.MaskromFirst && c.opens == 0 {
		want = rockusb.ModeMaskrom
		maskromFirst = true
	}
	c.mu.Unlock()

	raw, err := c.Bridge.List()
	if err != nil {
		c.record(nil, err)
		return nil, rockusb.Link{}, fmt.Errorf("列出设备失败: %w", err)
	}
	listed := make([]deviceapi.Info, 0, len(raw))
	for i, item := range raw {
		listed = append(listed, item.Info(i+1))
	}
	c.record(listed, nil)

	target, ok := c.pick(listed, want)
	if !ok && maskromFirst {
		// The board is already booted into LOADER (for example it came back
		// from a previous session): Download Boot is unnecessary, and the
		// state machine skips it once it sees Link.Mode == LOADER. Falling
		// back here keeps "start directly from LOADER" working; a maskrom
		// device still takes the maskrom path above.
		target, ok = c.pick(listed, rockusb.ModeLoader)
	}
	if !ok {
		err := fmt.Errorf("没有处于 %s 的 Rockchip 设备（已看到：%s）", want, describeModes(listed))
		c.record(listed, err)
		return nil, rockusb.Link{}, err
	}

	tr, err := c.Bridge.Open(target.DevNo, target.Handle)
	if err != nil {
		c.record(listed, err)
		return nil, rockusb.Link{}, err
	}

	c.mu.Lock()
	c.opens++
	c.devNo = target.DevNo
	c.mu.Unlock()

	return tr, linkOf(target), nil
}

// Close releases the interface of the device that was opened.
func (c *BridgeConnector) Close() error {
	c.mu.Lock()
	devNo := c.devNo
	c.devNo = 0
	c.mu.Unlock()
	if devNo == 0 {
		return nil
	}
	return c.Bridge.Close(devNo)
}

// LastSeen reports the devices the previous List returned and the error, if
// any. The page shows this when authorization was revoked by re-enumeration,
// which is the most common reason an Open fails mid-run.
func (c *BridgeConnector) LastSeen() ([]deviceapi.Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]deviceapi.Info(nil), c.last...), c.lastErr
}

// OpenCount reports how many times a device was opened.
func (c *BridgeConnector) OpenCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

func (c *BridgeConnector) record(listed []deviceapi.Info, err error) {
	c.mu.Lock()
	c.last = listed
	c.lastErr = err
	c.mu.Unlock()
}

// pick prefers the pinned handle, then any device in the wanted mode. A device
// that reappears in the other mode keeps its DevNo (see device.Registry) but
// gets a new instance, so the handle cannot be trusted across a boot.
func (c *BridgeConnector) pick(listed []deviceapi.Info, want rockusb.MediaMode) (deviceapi.Info, bool) {
	if c.Handle != "" {
		for _, d := range listed {
			if d.Handle == c.Handle && d.Mode == want {
				return d, true
			}
		}
	}
	for _, d := range listed {
		if d.Mode == want && d.VendorID == rockusb.RockchipVendorID {
			return d, true
		}
	}
	return deviceapi.Info{}, false
}

// linkOf derives the transfer endpoints from the listing, so the page's
// descriptor scan stays the only place that knows about interfaces/endpoints.
func linkOf(info deviceapi.Info) rockusb.Link {
	return rockusb.Link{
		InterfaceNumber: info.InterfaceNumber,
		InterfaceClass:  info.InterfaceClass,
		BulkIn:          info.BulkIn,
		BulkOut:         info.BulkOut,
		VendorID:        info.VendorID,
		ProductID:       info.ProductID,
		Mode:            info.Mode,
	}
}

func describeModes(listed []deviceapi.Info) string {
	if len(listed) == 0 {
		return "none（浏览器没有已授权设备）"
	}
	parts := make([]string, 0, len(listed))
	for _, d := range listed {
		parts = append(parts, fmt.Sprintf("DevNo=%d Pid=0x%04x Mode=%s", d.DevNo, d.ProductID, d.ModeLabel))
	}
	return strings.Join(parts, ", ")
}

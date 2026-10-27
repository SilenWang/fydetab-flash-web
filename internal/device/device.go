// Package device models the rockusb devices the tool can see, the CLI-style
// selection commands (`ld`, `cd`, `sd`) and the two-state workflow the M1 page
// has to make explicit: MASKROM can only be listed, described and bootstrapped
// with a Download Boot; LOADER is where read-only information and storage
// switching live.
package device

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
)

// RawInfo is what the page's main thread can read from a WebUSB device without
// opening it. Mode is derived here, not in JavaScript, so the bcdUSB bit-0
// convention has exactly one implementation.
type RawInfo struct {
	VendorID           uint16 `json:"vendorId"`
	ProductID          uint16 `json:"productId"`
	USBVersionMajor    uint8  `json:"usbVersionMajor"`
	USBVersionMinor    uint8  `json:"usbVersionMinor"`
	USBVersionSubminor uint8  `json:"usbVersionSubminor"`
	ProductName        string `json:"productName"`
	SerialNumber       string `json:"serialNumber"`
	InterfaceNumber    int    `json:"interfaceNumber"`
	InterfaceClass     uint16 `json:"interfaceClass"`
	BulkIn             uint8  `json:"bulkIn"`
	BulkOut            uint8  `json:"bulkOut"`
	// Handle is opaque to Go: the page's list() call puts an identifier here
	// and open() gets it back, so a re-ordered device list can never open the
	// wrong board.
	Handle string `json:"handle"`
}

// Info is a listed device with the DevNo the CLI shows.
type Info struct {
	DevNo           int               `json:"devNo"`
	VendorID        uint16            `json:"vendorId"`
	ProductID       uint16            `json:"productId"`
	BCDUSB          uint32            `json:"bcdUSB"`
	USBVersion      string            `json:"usbVersion"`
	ProductName     string            `json:"productName"`
	SerialNumber    string            `json:"serialNumber"`
	Mode            rockusb.MediaMode `json:"mode"`
	ModeLabel       string            `json:"modeLabel"`
	InterfaceNumber int               `json:"interfaceNumber"`
	InterfaceClass  uint16            `json:"interfaceClass"`
	BulkIn          uint8             `json:"bulkIn"`
	BulkOut         uint8             `json:"bulkOut"`
	Handle          string            `json:"handle,omitempty"`
}

// ModeLabel renders the mode the way upgrade_tool's `ld` prints it
// ("Maskrom" / "Loader"), not the upper-case protocol spelling.
func ModeLabel(m rockusb.MediaMode) string {
	switch m {
	case rockusb.ModeMaskrom:
		return "Maskrom"
	case rockusb.ModeLoader:
		return "Loader"
	default:
		return "Unknown"
	}
}

// Info converts raw WebUSB data into the listed form.
func (raw RawInfo) Info(devNo int) Info {
	bcdUSB := rockusb.PackBCDUSB(raw.USBVersionMajor, raw.USBVersionMinor, raw.USBVersionSubminor)
	mode := rockusb.ModeFromDescriptor(raw.VendorID, bcdUSB)
	return Info{
		DevNo:           devNo,
		VendorID:        raw.VendorID,
		ProductID:       raw.ProductID,
		BCDUSB:          bcdUSB,
		USBVersion:      fmt.Sprintf("%d.%d.%d", raw.USBVersionMajor, raw.USBVersionMinor, raw.USBVersionSubminor),
		ProductName:     raw.ProductName,
		SerialNumber:    raw.SerialNumber,
		Mode:            mode,
		ModeLabel:       ModeLabel(mode),
		InterfaceNumber: raw.InterfaceNumber,
		InterfaceClass:  raw.InterfaceClass,
		BulkIn:          raw.BulkIn,
		BulkOut:         raw.BulkOut,
		Handle:          raw.Handle,
	}
}

// Line is the exact `ld` line upgrade_tool prints, e.g.
//
//	DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom
func (i Info) Line() string {
	return fmt.Sprintf("DevNo=%d Vid=0x%04x,Pid=0x%04x,Mode=%s", i.DevNo, i.VendorID, i.ProductID, i.ModeLabel)
}

// Key identifies a device across re-enumeration. Maskrom and loader devices
// often carry no serial number, which is exactly why `sd` exists: after a
// Download Boot the same physical board comes back under the same VID:PID.
func (i Info) Key() string {
	if i.SerialNumber != "" {
		return fmt.Sprintf("%04x:%04x:sn:%s", i.VendorID, i.ProductID, i.SerialNumber)
	}
	return fmt.Sprintf("%04x:%04x", i.VendorID, i.ProductID)
}

// Registry maps DevNo to devices and keeps numbering stable when a device
// re-enumerates.
type Registry struct {
	devices []Info
}

// Replace updates the listing. A device that keeps its identity key keeps its
// DevNo; new devices take the lowest free numbers, so `cd 1` does not silently
// start pointing at a different board after a re-plug.
func (r *Registry) Replace(raw []RawInfo) []Info {
	previous := make(map[string]int, len(r.devices))
	used := make(map[int]bool, len(raw))
	for _, d := range r.devices {
		previous[d.Key()] = d.DevNo
	}

	next := make([]Info, 0, len(raw))
	pending := make([]RawInfo, 0, len(raw))
	previousByKey := make(map[string]Info, len(r.devices))
	for _, d := range r.devices {
		previousByKey[d.Key()] = d
	}
	for _, item := range raw {
		key := item.Info(0).Key()
		if devNo, ok := previous[key]; ok && !used[devNo] {
			info := item.Info(devNo)
			next = append(next, info)
			used[devNo] = true
			continue
		}
		pending = append(pending, item)
	}
	sort.Slice(next, func(i, j int) bool { return next[i].DevNo < next[j].DevNo })

	for _, item := range pending {
		devNo := 1
		for used[devNo] {
			devNo++
		}
		used[devNo] = true
		next = append(next, item.Info(devNo))
	}
	sort.Slice(next, func(i, j int) bool { return next[i].DevNo < next[j].DevNo })
	r.devices = next
	return r.List()
}

// List returns the current listing.
func (r *Registry) List() []Info {
	return append([]Info(nil), r.devices...)
}

// Len reports how many devices are listed.
func (r *Registry) Len() int { return len(r.devices) }

// Find looks a device up by DevNo.
func (r *Registry) Find(devNo int) (Info, bool) {
	for _, d := range r.devices {
		if d.DevNo == devNo {
			return d, true
		}
	}
	return Info{}, false
}

// Describe renders the descriptor summary `desc` prints. It is pure: no USB
// transfer happens, so it works in either mode.
func Describe(i Info) []string {
	return []string{
		fmt.Sprintf("DevNo=%d Vid=0x%04x,Pid=0x%04x", i.DevNo, i.VendorID, i.ProductID),
		fmt.Sprintf("Product: %s", orDash(i.ProductName)),
		fmt.Sprintf("Serial: %s", orDash(i.SerialNumber)),
		fmt.Sprintf("bcdUSB: 0x%04X (%s, bit0=%d) -> %s", i.BCDUSB, i.USBVersion, i.BCDUSB&1, i.ModeLabel),
		fmt.Sprintf("Interface: %d class=0x%02X bulkIn=0x%02X bulkOut=0x%02X", i.InterfaceNumber, i.InterfaceClass, 0x80|i.BulkIn, i.BulkOut),
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

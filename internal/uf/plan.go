package uf

import (
	"fmt"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// Plan is what the runner executes. It is a projection of the M2 dry-run
// report (cmd/uf's BuildReport), not a second planner: every decision — item
// order, target LBAs, IDB byte count, uncovered tails — is taken from that one
// report so `uf --dry-run` and `uf --mock` cannot disagree.
type Plan struct {
	Image string `json:"image"`
	// Chip is the image's chip name (e.g. "RK3588"), checked against the
	// device's READ_CHIP_INFO. Empty disables the check.
	Chip string `json:"chip,omitempty"`
	// Loader is the RKFW loader blob, when the image has one.
	Loader *LoaderPlan `json:"loader,omitempty"`
	// IDB is the parsed loader's IDB stage.
	IDB IDBPlan `json:"idb"`
	// Items are the payload writes, in the order they must run (the M2 plan
	// sorts them by target LBA).
	Items []Item `json:"items"`
	// Tails are partition tails the image does not fill. They are only
	// touched when Options.EraseTail says so.
	Tails []Tail `json:"tails,omitempty"`
	// DiskSectors/DiskKnown/DiskDerived mirror the M2 GPT plan's view of the
	// target flash, so the runner can compare it with READ_FLASH_INFO.
	DiskSectors uint64   `json:"diskSectors,omitempty"`
	DiskKnown   bool     `json:"diskKnown"`
	DiskDerived bool     `json:"diskDerived,omitempty"`
	NoReset     bool     `json:"noReset,omitempty"`
	TotalBytes  int64    `json:"totalBytes,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

// LoaderPlan is the Download Boot blob and its provenance.
//
// The default is the loader inside the image (Source "image", located by
// FileOffset/Length). A caller may pass a different file instead (Source
// "file", bytes in Bytes); the runner marks that in the report so a write made
// with a non-image loader is visible after the fact.
type LoaderPlan struct {
	Source     string `json:"source,omitempty"` // image | file
	Name       string `json:"name,omitempty"`
	FileOffset int64  `json:"fileOffset"`
	Length     int64  `json:"length"`
	SHA256     string `json:"sha256,omitempty"`
	// Chip is the loader's own supportChip model ("RK3588"), read from the
	// RKBOOT header. It is compared with Plan.Chip before Download Boot.
	Chip      string `json:"chip,omitempty"`
	Version   string `json:"version,omitempty"`
	BuildTime string `json:"buildTime,omitempty"`
	// Bytes holds the blob when it did not come from the image. It is never
	// serialised: a log must stay small.
	Bytes []byte `json:"-"`
	// NonImage marks a loader the user supplied instead of the image's own.
	NonImage bool `json:"nonImage,omitempty"`
}

// IDBPlan is the gpt stage: the IDB byte count the dry run projected and where
// it goes.
type IDBPlan struct {
	LBASector uint32 `json:"lbaSector"`
	Bytes     int64  `json:"bytes"`
	// Rewrite is the explicit --rewrite-idb decision. False means the runner
	// reads LBA 64 first and leaves an existing valid IDB untouched.
	Rewrite bool `json:"rewrite"`
}

// Item is one payload write (`wl`).
type Item struct {
	Name       string `json:"name"`
	Partition  string `json:"partition,omitempty"`
	FileOffset int64  `json:"fileOffset"`
	Size       int64  `json:"size"`
	PaddedSize int64  `json:"paddedSize"`
	TargetLBA  uint32 `json:"targetLba"`
}

// Tail is the part of a known-size partition the image does not fill.
type Tail struct {
	Partition string `json:"partition"`
	FirstLBA  uint64 `json:"firstLba"`
	Bytes     int64  `json:"bytes"`
}

// Validate checks the invariants the runner relies on, so a bad plan fails
// before a single USB transfer instead of half way through a flash.
func (p *Plan) Validate() error {
	if p == nil {
		return fmt.Errorf("uf: nil plan")
	}
	if len(p.Items) == 0 && p.IDB.Bytes == 0 {
		return fmt.Errorf("uf: plan has nothing to write (no IDB and no items)")
	}
	if p.IDB.Bytes > 0 {
		if p.Loader == nil {
			return fmt.Errorf("uf: plan has an IDB stage but no loader to build it from")
		}
		if p.IDB.Bytes%SectorSize != 0 {
			return fmt.Errorf("uf: IDB size %d is not a multiple of %d", p.IDB.Bytes, SectorSize)
		}
		if end := uint64(p.IDB.LBASector) + uint64(p.IDB.Bytes/SectorSize); end > uint64(MaxLBA)+1 {
			return fmt.Errorf("uf: IDB at LBA 0x%x runs past the 32-bit LBA space", p.IDB.LBASector)
		}
	}
	seen := map[string]bool{}
	for i, it := range p.Items {
		if it.Name == "" {
			return fmt.Errorf("uf: item %d has no name", i)
		}
		if seen[it.Name] {
			return fmt.Errorf("uf: item %q appears twice in the plan", it.Name)
		}
		seen[it.Name] = true
		if it.Size < 0 || it.PaddedSize < it.Size {
			return fmt.Errorf("uf: item %q has size %d / padded size %d", it.Name, it.Size, it.PaddedSize)
		}
		if it.FileOffset < 0 {
			return fmt.Errorf("uf: item %q has a negative file offset", it.Name)
		}
		if it.PaddedSize%SectorSize != 0 {
			return fmt.Errorf("uf: item %q padded size %d is not a multiple of %d", it.Name, it.PaddedSize, SectorSize)
		}
		if end := uint64(it.TargetLBA) + uint64(it.PaddedSize/SectorSize); end > uint64(MaxLBA)+1 {
			return fmt.Errorf("uf: item %q at LBA 0x%x runs past the 32-bit LBA space", it.Name, it.TargetLBA)
		}
	}
	for i, t := range p.Tails {
		if t.Bytes <= 0 || t.Bytes%SectorSize != 0 {
			return fmt.Errorf("uf: tail %d (%s) has an unsectoral length %d", i, t.Partition, t.Bytes)
		}
		if end := t.FirstLBA + uint64(t.Bytes/SectorSize); end > uint64(MaxLBA)+1 {
			return fmt.Errorf("uf: tail of %q at LBA 0x%x runs past the 32-bit LBA space", t.Partition, t.FirstLBA)
		}
	}
	return nil
}

// Item looks an item up by name.
func (p *Plan) Item(name string) (Item, bool) {
	for _, it := range p.Items {
		if it.Name == name {
			return it, true
		}
	}
	return Item{}, false
}

func chipMatches(want, got string) bool {
	w := chipDigits(want)
	g := chipDigits(got)
	if w == "" || g == "" {
		return true
	}
	if w == g {
		return true
	}
	// READ_CHIP_INFO returns Rockchip's 4-byte chip tag byte-reversed: an
	// RK3588 tag 0x33353838 reads as "8853". Accept that form explicitly
	// instead of reporting a false mismatch (SIL-515's first write attempt
	// aborted at Check Chip for exactly this reason).
	return len(w) == 4 && len(g) == 4 && reverseString(g) == w
}

// reverseString reverses the ASCII bytes of s. Chip tags are stored as four
// reversed ASCII digits, so this is the only transformation needed.
func reverseString(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// chipDigits normalises "RK3588", "rk3588s" and "3588" to "3588" so the image
// header and the device's 16-byte chip info string can be compared.
func chipDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			out = append(out, c)
		} else if len(out) > 0 {
			break
		}
	}
	return string(out)
}

// MaxFlashSectors is READ_FLASH_INFO's uiFlashSize field, i.e. flash sectors.
func flashSectors(info rockusb.FlashInfo) uint64 { return uint64(info.FlashSizeSectors) }

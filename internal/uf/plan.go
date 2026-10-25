package uf

import (
	"fmt"

	"github.com/SilenWang/rk3588-webflasher/internal/gpt"
	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
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
	// GPT is the partition table and the parameter block the gpt stage writes.
	// nil means the plan cannot write a partition table (an image without a
	// parameter entry).
	GPT *GPTPlan `json:"gpt,omitempty"`
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

// IDBPlan is the gpt stage's IDB byte count and where it goes.
type IDBPlan struct {
	LBASector uint32 `json:"lbaSector"`
	Bytes     int64  `json:"bytes"`
	// Rewrite is the explicit --rewrite-idb decision. False means the runner
	// reads LBA 64 first and leaves an existing valid IDB untouched.
	Rewrite bool `json:"rewrite"`
}

// GPTPlan is the partition-table half of the gpt stage: the parameter-derived
// table, written as a primary and a backup copy, plus the parameter block the
// device reads. The runner rebuilds the bytes against the flash size the device
// reports, because the backup table lives at the end of the disk.
type GPTPlan struct {
	// Partitions is the parameter table (start LBA, size, flags) as the dry-run
	// planned it.
	Partitions []parameter.Partition `json:"partitions,omitempty"`
	// Parameter is the parameter block write, replicated across its reserved
	// area.
	Parameter *ParameterPlan `json:"parameter,omitempty"`
}

// ParameterPlan is where and how the parameter block is written.
type ParameterPlan struct {
	// FileOffset/Size locate the PARM block in the image.
	FileOffset int64 `json:"fileOffset"`
	Size       int64 `json:"size"`
	// CopyBytes is the sector-aligned length of every copy (the block plus its
	// slot padding).
	CopyBytes int64 `json:"copyBytes"`
	// Copies, and the placement: copy i goes to BaseLBA + i*StrideLBA.
	Copies       int    `json:"copies"`
	First4Access bool   `json:"first4Access"`
	BaseLBA      uint64 `json:"baseLba"`
	StrideLBA    uint64 `json:"strideLba"`
	// CRCOK records whether the block's own PARM checksum verified in the plan.
	CRCOK bool `json:"crcOk"`
}

// Replicas expands the placement into the individual copy ranges.
func (p *ParameterPlan) Replicas() []parameter.Replica {
	if p == nil || p.Copies <= 0 {
		return nil
	}
	sectors := uint64(p.CopyBytes) / SectorSize
	out := make([]parameter.Replica, 0, p.Copies)
	for i := 0; i < p.Copies; i++ {
		out = append(out, parameter.Replica{Index: i, LBA: p.BaseLBA + uint64(i)*p.StrideLBA, Sectors: sectors})
	}
	return out
}

// Bytes is the total transfer size of every copy.
func (p *ParameterPlan) Bytes() int64 {
	if p == nil {
		return 0
	}
	return int64(p.Copies) * p.CopyBytes
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
	if len(p.Items) == 0 && p.IDB.Bytes == 0 && p.GPT == nil {
		return fmt.Errorf("uf: plan has nothing to write (no IDB, no partition table and no items)")
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
	if err := p.validateGPT(); err != nil {
		return err
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

// GPT stage geometry: the primary region covers LBA 0..33 and the backup region
// the last 33 sectors of the disk, whatever the partition table says.
const (
	// GPTRegionSectors is the primary plus the backup region.
	GPTRegionSectors = int64(gpt.PrimarySectors + gpt.BackupSectors)
	// GPTRegionBytes is the transfer size of both regions.
	GPTRegionBytes = GPTRegionSectors * SectorSize
)

// GPTBytes is the partition table's share of the gpt stage.
func (p *Plan) GPTBytes() int64 {
	if p.GPT == nil {
		return 0
	}
	return GPTRegionBytes
}

// ParameterBytes is the parameter block's share of the gpt stage.
func (p *Plan) ParameterBytes() int64 {
	if p.GPT == nil {
		return 0
	}
	return p.GPT.Parameter.Bytes()
}

// GPTStageBytes is everything the gpt stage transfers, ignoring whether the
// IDB turns out to be kept.
func (p *Plan) GPTStageBytes() int64 {
	return p.IDB.Bytes + p.GPTBytes() + p.ParameterBytes()
}

// validateGPT checks the partition-table half of the plan. Everything it can
// rule out statically it rules out here, so a run never starts writing a
// partition table it will have to abort half way through.
func (p *Plan) validateGPT() error {
	if p.GPT == nil {
		return nil
	}
	if len(p.GPT.Partitions) == 0 {
		return fmt.Errorf("uf: plan has a GPT stage but no partitions")
	}
	if len(p.GPT.Partitions) > gpt.EntryCount {
		return fmt.Errorf("uf: plan has %d partitions, more than the %d GPT slots", len(p.GPT.Partitions), gpt.EntryCount)
	}
	pp := p.GPT.Parameter
	if pp == nil {
		return fmt.Errorf("uf: plan has a partition table but no parameter block to write with it")
	}
	if pp.Size <= 0 {
		return fmt.Errorf("uf: parameter block size %d is empty", pp.Size)
	}
	if pp.CopyBytes < pp.Size || pp.CopyBytes%SectorSize != 0 {
		return fmt.Errorf("uf: parameter copy of %d bytes cannot hold the %d-byte block", pp.CopyBytes, pp.Size)
	}
	if pp.Copies <= 0 {
		return fmt.Errorf("uf: parameter copies %d is not positive", pp.Copies)
	}
	if pp.StrideLBA == 0 {
		return fmt.Errorf("uf: parameter copies have a zero stride")
	}
	if pp.BaseLBA <= gpt.PrimarySectors {
		return fmt.Errorf("uf: parameter copies start at LBA 0x%x, inside the primary GPT", pp.BaseLBA)
	}
	last := pp.BaseLBA + uint64(pp.Copies-1)*pp.StrideLBA + uint64(pp.CopyBytes)/SectorSize
	if last > uint64(MaxLBA)+1 {
		return fmt.Errorf("uf: parameter copies end at LBA 0x%x, past the 32-bit LBA space", last)
	}
	return nil
}

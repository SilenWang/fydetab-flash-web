// Package uf implements the write half of Rockchip's `uf` upgrade: the rockusb
// write commands (WRITE_LBA / ERASE_LBA / DEVICE_RESET), the IDB byte model and
// the state machine that turns the M2 dry-run plan into device transfers.
//
// The read-only command table lives in internal/rockusb and stays read-only on
// purpose (TestNoWritePath); the opcodes and CBW layouts that write flash are
// declared here instead, transcribed from Rockchip's GPL-2.0 rkdeveloptool
// (RKComm.cpp:520-800) and documented in docs/uf-write-path.md.
//
// Everything in this package talks to a rockusb.Transport, so the browser
// bridge, an in-memory simulator and a replayed trace are interchangeable.
package uf

import (
	"fmt"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

const (
	// MinChunkSize and MaxChunkSize bound Options.ChunkSize: one write chunk
	// is 1..4 MiB. The chunk is the progress and retry unit; below it a
	// WRITE_LBA exchange is at most MaxTransferSectors sectors because that
	// is what upstream's DEFAULT_RW_LBA does.
	MinChunkSize int64 = 1 << 20
	// MaxChunkSize is the largest single progress/retry chunk.
	MaxChunkSize int64 = 4 << 20
	// DefaultChunkSize is used when Options.ChunkSize is zero.
	DefaultChunkSize int64 = MaxChunkSize

	// DefaultTimeout bounds every single USB transfer. 5 s is plenty for a
	// 64 KiB bulk transfer; a longer stall is a dead pipe, not a slow device.
	DefaultTimeout = rockusb.DefaultTimeout

	// DefaultReenumTimeout is how long the runner waits for the device to
	// re-enumerate (after Download Boot). The browser often needs the user to
	// re-authorize the new USB device instance, so this is longer than a USB
	// timeout.
	DefaultReenumTimeout = 20 * time.Second
	// DefaultReenumPoll is the re-enumeration poll interval.
	DefaultReenumPoll = 500 * time.Millisecond

	// ErrCancelled is a local code: the caller cancelled the run. rkdeveloptool
	// has no ERR_* value for cancellation because upstream cannot cancel.
	ErrCancelled rockusb.ErrorCode = -14
	// ErrLoaderMismatch is a local code: the loader cannot be sent for this
	// image (its supportChip disagrees with the image's chip). Upstream has no
	// such error because download_boot() does not check at all.
	ErrLoaderMismatch rockusb.ErrorCode = -15
	// ErrSignature is a local code: an item or IDB did not match its
	// expected magic before a write.
	ErrSignature rockusb.ErrorCode = -16
	// ErrReadback is a local code: the post-write read-back differs from the
	// source, so the flash does not hold what the plan says.
	ErrReadback rockusb.ErrorCode = -17
)

// CodeName renders an rkdeveloptool error code, including the local codes this
// package adds.
func CodeName(code rockusb.ErrorCode) string {
	switch code {
	case ErrCancelled:
		return "ERR_CANCELLED"
	case ErrSignature:
		return "ERR_SIGNATURE"
	case ErrReadback:
		return "ERR_READBACK"
	case ErrLoaderMismatch:
		return "ERR_LOADER_MISMATCH"
	}
	return code.String()
}

// EraseMode is the explicit answer to "what happens to the part of a partition
// the image does not fill?".
//
// This is a real, unresolved question for the Fydetab Duo image: the STATE
// partition is 5 GiB but the image only carries ~1 GiB of payload, so 4 GiB are
// neither written nor described by the image. Whether the device's loader
// erases that tail is not known yet (it needs real hardware, SIL-511/SIL-515),
// so the choice is never silent: it is a flag, it is printed by --dry-run and
// it is logged before the first write.
type EraseMode string

const (
	// EraseNone leaves the uncovered tail untouched. This is the default: it
	// writes the fewest blocks and it is the only choice that cannot destroy
	// data the image does not mention.
	EraseNone EraseMode = "none"
	// EraseFill writes 0xFF over the uncovered tail with WRITE_LBA, which is
	// what rkdeveloptool's EraseEmmcByWriteLBA does.
	EraseFill EraseMode = "fill"
	// EraseLBA asks the device to erase the uncovered tail with ERASE_LBA,
	// which is what rkdeveloptool's erase_partition does.
	EraseLBA EraseMode = "erase-lba"
)

// ParseEraseMode accepts the --erase-tail values.
func ParseEraseMode(s string) (EraseMode, error) {
	switch EraseMode(s) {
	case "", EraseNone:
		return EraseNone, nil
	case EraseFill:
		return EraseFill, nil
	case EraseLBA:
		return EraseLBA, nil
	}
	return "", fmt.Errorf("unknown erase mode %q (want none, fill or erase-lba)", s)
}

// Describe returns the sentence that --dry-run prints and the runner logs, so
// the decision is visible in both places.
func (m EraseMode) Describe() string {
	switch m {
	case EraseFill:
		return "未覆盖的分区尾部用 WRITE_LBA 写 0xFF（擦除语义未知，待真机核对）"
	case EraseLBA:
		return "未覆盖的分区尾部发 ERASE_LBA 擦除（擦除语义未知，待真机核对）"
	default:
		return "未覆盖的分区尾部保持原内容（不写、不擦）；是否擦除待真机核对"
	}
}

// Options tunes one uf run.
type Options struct {
	// Timeout bounds every single USB transfer. Zero means DefaultTimeout.
	Timeout time.Duration
	// Retries is how many additional attempts a transfer may make after a
	// timeout. Only timeouts are retried: a CSW failure, a bad tag or a STALL
	// is a definitive device answer. A retried WRITE_LBA repeats the same
	// bytes at the same LBA, so it is idempotent.
	Retries int
	// ChunkSize is the progress/retry chunk in bytes. It is clamped to
	// [MinChunkSize, MaxChunkSize]; zero means DefaultChunkSize.
	ChunkSize int64
	// EraseTail selects what happens to uncovered partition tails. Empty
	// means EraseNone.
	EraseTail EraseMode
	// NoReset skips the final DEVICE_RESET (`rd`), mirroring uf --noreset.
	NoReset bool
	// StartItem resumes a previous run at this item name: every item before
	// it is skipped and the run reports itself as resumed. The item itself is
	// rewritten from its first sector, which is safe because the same bytes
	// land at the same LBAs.
	StartItem string
	// SkipChipCheck disables matching the image chip against READ_CHIP_INFO.
	SkipChipCheck bool
	// SkipSignatureCheck disables the pre-write magic gate. Mock/replay runs
	// set it; a real write must not.
	SkipSignatureCheck bool
	// SkipReadback disables the post-write first/last 4 KiB comparison.
	// Mock/replay runs set it; a real write must not.
	SkipReadback bool
	// IDB overrides the IDB byte model. nil means NewRK28RC4Model().
	IDB IDBModel
	// RewriteIDB forces a rebuild of the IDB at LBA 64 even when the device
	// already holds a valid one. The default (false) never overwrites an
	// existing IDB: the device's own block is what its BootROM expects, and
	// SIL-515 showed the two models are not interchangeable.
	RewriteIDB bool
	// IDBBackupPath, when set, receives the bytes read from LBA 64 before any
	// overwrite. The same bytes are always in the run log as hex.
	IDBBackupPath string
	// Trace receives every USB transfer, successful or not.
	Trace func(rockusb.Transfer)
	// Progress receives phase/chunk progress snapshots.
	Progress func(Progress)
	// Event receives human-readable log lines (the same lines the JSON log
	// carries as "event" records).
	Event func(Event)
	// ReenumTimeout and ReenumPoll tune the post-Download-Boot wait.
	ReenumTimeout time.Duration
	ReenumPoll    time.Duration
	// Now and Sleep are test hooks; nil means time.Now / time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// normalize fills defaults and clamps the knobs, returning the warnings it had
// to make visible.
func (o Options) normalize() (Options, []string) {
	var warnings []string
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Retries < 0 {
		o.Retries = 0
	}
	switch {
	case o.ChunkSize == 0:
		o.ChunkSize = DefaultChunkSize
	case o.ChunkSize < MinChunkSize:
		warnings = append(warnings, fmt.Sprintf("chunk size %d B is below the 1 MiB floor; using %d B", o.ChunkSize, MinChunkSize))
		o.ChunkSize = MinChunkSize
	case o.ChunkSize > MaxChunkSize:
		warnings = append(warnings, fmt.Sprintf("chunk size %d B is above the 4 MiB ceiling; using %d B", o.ChunkSize, MaxChunkSize))
		o.ChunkSize = MaxChunkSize
	}
	if o.ChunkSize%SectorSize != 0 {
		o.ChunkSize -= o.ChunkSize % SectorSize
	}
	if o.EraseTail == "" {
		o.EraseTail = EraseNone
	}
	if o.IDB == nil {
		o.IDB = NewRKBOOTModel()
	}
	if o.ReenumTimeout <= 0 {
		o.ReenumTimeout = DefaultReenumTimeout
	}
	if o.ReenumPoll <= 0 {
		o.ReenumPoll = DefaultReenumPoll
	}
	return o, warnings
}

// NormalizeOptions exposes normalize for callers that need the effective
// timeout/chunk/model (the CLI echoes them into its log header).
func NormalizeOptions(o Options) (Options, []string) { return o.normalize() }

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	if o.Sleep != nil {
		o.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (o Options) progress(p Progress) {
	if o.Progress != nil {
		o.Progress(p)
	}
}

func (o Options) event(ev Event) {
	if o.Event != nil {
		o.Event(ev)
	}
}

// Progress is one progress snapshot. Done/Total/Percent are for the whole run;
// ItemDone/ItemTotal are for the current item, when there is one.
type Progress struct {
	Phase     string  `json:"phase"`
	Item      string  `json:"item,omitempty"`
	Done      int64   `json:"doneBytes"`
	Total     int64   `json:"totalBytes"`
	Percent   float64 `json:"percent"`
	ItemDone  int64   `json:"itemDoneBytes,omitempty"`
	ItemTotal int64   `json:"itemTotalBytes,omitempty"`
	Note      string  `json:"note,omitempty"`
}

// Event is one human-readable log line.
type Event struct {
	Phase   string `json:"phase"`
	Level   string `json:"level"` // info | warn | error
	Item    string `json:"item,omitempty"`
	Message string `json:"message"`
}

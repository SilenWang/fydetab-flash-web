package uf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// Phase names. "ul", "gpt", "wl" and "rd" are the M2 dry-run stage names; "td",
// "chip" and "flash" are the pre-flight steps upgrade_tool runs before them,
// and "erase" is the explicit tail decision.
const (
	PhaseBoot  = "ul"
	PhaseTest  = "td"
	PhaseChip  = "chip"
	PhaseFlash = "flash"
	PhaseIDB   = "gpt"
	PhaseWrite = "wl"
	PhaseErase = "erase"
	PhaseReset = "rd"
)

// Connector is the device side of a uf run. In the browser it is the WebUSB
// bridge; in tests and `uf --mock` it is an in-memory device. Open is called
// once at the start and again after Download Boot re-enumerates the device.
type Connector interface {
	Open(ctx context.Context) (rockusb.Transport, rockusb.Link, error)
	Close() error
}

// Source supplies image payload bytes at absolute file offsets. An *os.File
// over update.img is the real implementation; tests use a bytes.Reader.
type Source interface {
	ReadAt(p []byte, off int64) (int, error)
}

// Report is the JSON-friendly outcome of one run. It is returned even when the
// run fails, so a caller can log how far it got and where to resume.
type Report struct {
	Schema     string        `json:"schema"`
	Image      string        `json:"image"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt time.Time     `json:"finishedAt"`
	DurationMs float64       `json:"durationMs"`
	OK         bool          `json:"ok"`
	Cancelled  bool          `json:"cancelled,omitempty"`
	Phases     []PhaseReport `json:"phases"`
	Items      []ItemReport  `json:"items"`
	IDB        *IDBReport    `json:"idb,omitempty"`
	Erase      *EraseReport  `json:"erase,omitempty"`
	Device     DeviceReport  `json:"device"`
	// Loader is the Download Boot blob's identity (source, sha256, build
	// date, chip check), recorded whether or not the device needed it.
	Loader *LoaderReport `json:"loader,omitempty"`
	// DB is set when Download Boot failed: the first-block/mid-transfer split
	// the operator needs, with the loader block index the BootROM rejected.
	DB           *DBFailReport `json:"dbFailure,omitempty"`
	Reset        bool          `json:"reset"`
	BytesWritten int64         `json:"bytesWritten"`
	BytesErased  int64         `json:"bytesErased"`
	TotalBytes   int64         `json:"totalBytes"`
	ChunkBytes   int64         `json:"chunkBytes"`
	TimeoutMs    int64         `json:"timeoutMs"`
	Retries      int           `json:"retries"`
	EraseMode    EraseMode     `json:"eraseMode"`
	NoReset      bool          `json:"noReset"`
	StartItem    string        `json:"startItem,omitempty"`
	Resumed      bool          `json:"resumed,omitempty"`
	Completed    []string      `json:"completedItems,omitempty"`
	Skipped      []string      `json:"skippedItems,omitempty"`
	NextItem     string        `json:"nextItem,omitempty"`
	Warnings     []string      `json:"warnings,omitempty"`
	Notes        []string      `json:"notes,omitempty"`
	Hint         string        `json:"hint,omitempty"`
	Error        *RunError     `json:"error,omitempty"`
}

// PhaseReport is one phase's outcome.
type PhaseReport struct {
	Phase      string  `json:"phase"`
	Name       string  `json:"name"`
	Bytes      int64   `json:"bytes"`
	DurationMs float64 `json:"durationMs"`
	Status     string  `json:"status"` // ok | skipped | failed | cancelled
	Note       string  `json:"note,omitempty"`
}

// ItemReport is one `wl` item's outcome.
type ItemReport struct {
	Name       string  `json:"name"`
	Partition  string  `json:"partition,omitempty"`
	TargetLBA  uint32  `json:"targetLba"`
	Bytes      int64   `json:"bytes"`
	Status     string  `json:"status"` // ok | skipped | failed | cancelled
	DurationMs float64 `json:"durationMs"`
}

// IDBReport records which byte model produced the gpt stage.
type IDBReport struct {
	LBA          uint32 `json:"lba"`
	Bytes        int64  `json:"bytes"`
	PlannedBytes int64  `json:"plannedBytes"`
	Model        string `json:"model"`
	Verified     bool   `json:"verified"`
}

// EraseReport records the explicit tail decision and what actually happened.
type EraseReport struct {
	Mode   EraseMode `json:"mode"`
	Tails  int       `json:"tails"`
	Bytes  int64     `json:"bytes"`
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
}

// DeviceReport is what the pre-flight phases learned from the device.
type DeviceReport struct {
	Mode           string            `json:"mode"`
	Chip           string            `json:"chip,omitempty"`
	ChipExpected   string            `json:"chipExpected,omitempty"`
	Flash          rockusb.FlashInfo `json:"flash"`
	FlashKnown     bool              `json:"flashKnown"`
	ReenumAttempts int               `json:"reenumAttempts,omitempty"`
}

// LoaderReport is the identity of the loader a Download Boot would send, so a
// failed attempt can be compared with a known-good file after the fact. The
// build date is the field that separates the official v23 loader (2024-09-06)
// from the SDK's v1.06.111 (2022-06-28).
type LoaderReport struct {
	Source       string `json:"source"` // image | file
	Name         string `json:"name,omitempty"`
	Offset       int64  `json:"offset,omitempty"`
	Length       int64  `json:"length"`
	SHA256       string `json:"sha256,omitempty"`
	Version      string `json:"version,omitempty"`
	BuildTime    string `json:"buildTime,omitempty"`
	Chip         string `json:"chip,omitempty"`
	ChipExpected string `json:"chipExpected,omitempty"`
	// ChipCheck is ok | mismatch | unknown, the same vocabulary imageplan and
	// the page use.
	ChipCheck string `json:"chipCheck"`
	// NonImage marks a loader the user picked instead of the image's own.
	NonImage bool   `json:"nonImage,omitempty"`
	Checked  string `json:"checked,omitempty"`
}

// DBFailReport is the Download Boot failure split the operator sees:
// "first-block" means the BootROM received the loader's first data block and
// refused it (loader/image mismatch or a dirty power-on cycle, in that order),
// "mid-transfer" means the first block was accepted and a later one failed.
type DBFailReport struct {
	Category  string `json:"category"`
	Index     int    `json:"index"`
	BlockName string `json:"blockName,omitempty"`
	Detail    string `json:"detail"`
}

// RunError is a failed run with the recovery information the UI needs: which
// item to resume from and what to tell the user.
type RunError struct {
	Code     rockusb.ErrorCode `json:"code"`
	CodeName string            `json:"codeName"`
	Phase    string            `json:"phase"`
	Item     string            `json:"item,omitempty"`
	// Category is the coarse classification the page switches on, e.g.
	// "first-block" / "mid-transfer" for a Download Boot failure or
	// "loader-chip-mismatch" when the loader was refused before transfer.
	Category  string   `json:"category,omitempty"`
	Op        int      `json:"op,omitempty"`
	LBA       uint32   `json:"lba,omitempty"`
	Sectors   int      `json:"sectors,omitempty"`
	Attempt   int      `json:"attempt,omitempty"`
	Detail    string   `json:"detail"`
	Hint      string   `json:"hint,omitempty"`
	Completed []string `json:"completedItems,omitempty"`
	NextItem  string   `json:"nextItem,omitempty"`
	Err       error    `json:"-"`
}

func (e *RunError) Error() string {
	where := e.Phase
	if e.Item != "" {
		where += "/" + e.Item
	}
	if where == "" {
		return fmt.Sprintf("%s: %s", e.CodeName, e.Detail)
	}
	return fmt.Sprintf("%s: %s: %s", e.CodeName, where, e.Detail)
}

func (e *RunError) Unwrap() error { return e.Err }

// device couples the read-only M1 session with the write path over one
// transport. Both share a tracer, so a log has one transfer sequence.
type device struct {
	link  rockusb.Link
	read  *rockusb.Session
	write *writer
}

type runner struct {
	plan   *Plan
	src    Source
	conn   Connector
	opts   Options
	rep    *Report
	tracer *tracer

	dev  *device
	link rockusb.Link

	loader []byte

	total        int64
	done         int64
	completed    []string
	skipped      map[string]bool
	currentItem  string
	phaseBytes   int64
	phaseStarted time.Time
	phaseName    string
	phaseCode    string
	phaseOpen    bool
	notes        []string
}

// Run executes a plan against a device. It returns the report even on failure;
// the error is a *RunError whenever the failure is attributable to a phase.
func Run(ctx context.Context, plan *Plan, src Source, conn Connector, opts Options) (*Report, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if src == nil {
		return nil, errors.New("uf: nil image source")
	}
	if conn == nil {
		return nil, errors.New("uf: nil connector")
	}
	opts, warnings := opts.normalize()
	if opts.StartItem != "" {
		if _, ok := plan.Item(opts.StartItem); !ok {
			return nil, fmt.Errorf("uf: --resume-from item %q is not in the plan", opts.StartItem)
		}
	}

	r := &runner{
		plan:    plan,
		src:     src,
		conn:    conn,
		opts:    opts,
		tracer:  &tracer{fn: opts.Trace},
		skipped: map[string]bool{},
	}
	resuming := false
	if opts.StartItem != "" {
		for _, it := range plan.Items {
			if it.Name == opts.StartItem {
				resuming = true
				break
			}
			r.skipped[it.Name] = true
		}
	}

	rep := &Report{
		Schema:     "uf-report/1",
		Image:      plan.Image,
		StartedAt:  opts.now(),
		ChunkBytes: opts.ChunkSize,
		TimeoutMs:  opts.Timeout.Milliseconds(),
		Retries:    opts.Retries,
		EraseMode:  opts.EraseTail,
		NoReset:    opts.NoReset,
		StartItem:  opts.StartItem,
		Resumed:    resuming,
		Warnings:   append([]string(nil), warnings...),
	}
	rep.Warnings = append(rep.Warnings, plan.Warnings...)
	r.rep = rep

	if resuming {
		for _, it := range plan.Items {
			if r.skipped[it.Name] {
				rep.Skipped = append(rep.Skipped, it.Name)
			}
		}
	}

	err := r.run(ctx)

	rep.FinishedAt = opts.now()
	rep.DurationMs = msSince(rep.StartedAt, rep.FinishedAt)
	rep.OK = err == nil
	rep.Completed = append([]string(nil), r.completed...)
	rep.NextItem = ""
	rep.Notes = append(rep.Notes, r.notes...)
	if err != nil {
		var rerr *RunError
		if errors.As(err, &rerr) {
			rep.Error = rerr
			rep.NextItem = rerr.NextItem
			rep.Hint = rerr.Hint
			rep.Cancelled = rerr.Code == ErrCancelled
			rep.Completed = append([]string(nil), rerr.Completed...)
		} else {
			rep.Hint = err.Error()
		}
	}
	r.closeDevice()
	return rep, err
}

func (r *runner) run(ctx context.Context) error {
	// The loader check runs before the device is opened: a wrong loader must
	// be refused without sending a single byte.
	if err := r.checkLoaderPlan(); err != nil {
		return err
	}
	if err := r.connect(ctx); err != nil {
		return err
	}
	if err := r.bootIfNeeded(ctx); err != nil {
		return err
	}
	if err := r.testDevice(ctx); err != nil {
		return err
	}
	if err := r.checkChip(ctx); err != nil {
		return err
	}
	if err := r.readFlashInfo(ctx); err != nil {
		return err
	}
	if err := r.downloadIDB(ctx); err != nil {
		return err
	}
	if err := r.writeItems(ctx); err != nil {
		return err
	}
	if err := r.eraseTails(ctx); err != nil {
		return err
	}
	return r.reset(ctx)
}

// --- device plumbing --------------------------------------------------------

func (r *runner) connect(ctx context.Context) error {
	if err := r.openDevice(ctx); err != nil {
		return err
	}
	r.rep.Device.Mode = r.link.Mode.String()
	r.event(PhaseTest, "info", "", fmt.Sprintf("设备已连接：%s（VID 0x%04X, PID 0x%04X）", r.link.Mode, r.link.VendorID, r.link.ProductID))

	var total int64
	if r.link.Mode == rockusb.ModeMaskrom && r.plan.Loader != nil {
		total += r.plan.Loader.Length
	}
	total += r.plan.IDB.Bytes
	for _, it := range r.plan.Items {
		if r.skipped[it.Name] {
			continue
		}
		total += it.PaddedSize
	}
	if r.opts.EraseTail != EraseNone {
		for _, t := range r.plan.Tails {
			total += t.Bytes
		}
	}
	r.total = total
	r.rep.TotalBytes = total
	r.progress(PhaseTest, "", 0, 0, fmt.Sprintf("计划传输 %d 字节", total))
	return nil
}

func (r *runner) openDevice(ctx context.Context) error {
	tr, link, err := r.conn.Open(ctx)
	if err != nil {
		if isCancelled(err) {
			return r.fail("", "", ErrCancelled, "打开设备前被取消", cancelHint(), err)
		}
		return r.fail("", "", rockusb.ErrDeviceOpenFailed, fmt.Sprintf("打开设备失败: %v", err),
			"确认设备已连接并在浏览器里授权（0x2207）；裸 maskrom 需要先 Download Boot。", err)
	}
	r.link = link
	r.dev = r.newDevice(tr, link)
	return nil
}

func (r *runner) newDevice(tr rockusb.Transport, link rockusb.Link) *device {
	readOpts := rockusb.Options{
		Timeout: r.opts.Timeout,
		Retries: r.opts.Retries,
		Trace:   r.tracer.record,
		Sleep:   r.opts.Sleep,
	}
	if r.opts.Now != nil {
		readOpts.Now = r.opts.Now
	}
	return &device{
		link:  link,
		read:  rockusb.NewSession(tr, link, readOpts),
		write: newWriter(tr, link, r.opts, r.tracer),
	}
}

func (r *runner) closeDevice() {
	if r.dev == nil {
		return
	}
	r.dev = nil
	_ = r.conn.Close()
}

// --- pre-flight -------------------------------------------------------------

// checkLoaderPlan loads the Download Boot blob and runs the check upstream's
// download_boot() does not have: the loader's supportChip must match the
// image's chip. It touches no device, so a mismatch is refused before the first
// control transfer. On real hardware a mismatched loader is transmitted in full
// and then rejected at index 0, which looks exactly like a cable/port fault —
// that confusion is what this check exists to remove.
func (r *runner) checkLoaderPlan() error {
	lp := r.plan.Loader
	if lp == nil {
		return nil
	}
	data := lp.Bytes
	if len(data) == 0 {
		var err error
		data, err = r.readAt(lp.FileOffset, lp.Length)
		if err != nil {
			rerr := r.fail(PhaseBoot, "", rockusb.ErrFailed,
				fmt.Sprintf("读取镜像自带 loader 失败（offset 0x%x, %d B）: %v", lp.FileOffset, lp.Length, err),
				"确认镜像文件完整可读；镜像被移动或截断时不能继续写入。", err)
			rerr.Category = "loader-invalid"
			return rerr
		}
	}
	r.loader = data

	parsed, err := rockusb.ParseLoaderImage(data)
	if err != nil {
		rerr := r.fail(PhaseBoot, "", rockusb.ErrDeviceNotSupport,
			fmt.Sprintf("loader 无法解析: %v", err),
			"确认镜像里的 RKFW loader 是完整的 RKBOOT；镜像损坏时不要继续写入。", err)
		rerr.Category = "loader-invalid"
		return rerr
	}

	source := lp.Source
	if source == "" {
		source = "image"
	}
	report := &LoaderReport{
		Source:       source,
		Name:         lp.Name,
		Offset:       lp.FileOffset,
		Length:       int64(len(data)),
		SHA256:       lp.SHA256,
		Version:      parsed.VersionString(),
		BuildTime:    parsed.BuildTime.String(),
		Chip:         parsed.ChipName(),
		ChipExpected: r.plan.Chip,
		ChipCheck:    "unknown",
		NonImage:     lp.NonImage || source != "image",
	}
	if report.SHA256 == "" {
		sum := sha256.Sum256(data)
		report.SHA256 = hex.EncodeToString(sum[:])
	}

	if r.plan.Chip != "" {
		if err := parsed.CheckChip(r.plan.Chip); err != nil {
			report.ChipCheck = "mismatch"
			report.Checked = "refused before Download Boot"
			r.rep.Loader = report
			hint := "loader 必须与镜像来自同一份 update.img：镜像自带的 loader 在 RKFW 头的 loader offset 处，用它就不会错配。"
			if report.NonImage {
				hint = "当前用的是用户指定的 loader（非镜像自带）。改回“使用镜像自带 loader”再试；错配的 loader 会在 index 0 被 BootROM 拒收。"
			}
			rerr := r.fail(PhaseBoot, "", ErrLoaderMismatch, err.Error(), hint, err)
			rerr.Category = "loader-chip-mismatch"
			return rerr
		}
		report.ChipCheck = "ok"
		report.Checked = fmt.Sprintf("supportChip %s matches image %s", report.Chip, r.plan.Chip)
	} else {
		report.Checked = "image has no chip field; only the RK3588 parse check applied"
	}
	r.rep.Loader = report
	r.event(PhaseBoot, "info", "", fmt.Sprintf("loader 自检：%s · %d B · sha256 %s · 构建 %s · chip %s（%s）",
		source, report.Length, shortSHA(report.SHA256), report.BuildTime, report.Chip, report.ChipCheck))
	return nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

func (r *runner) bootIfNeeded(ctx context.Context) error {
	if r.link.Mode != rockusb.ModeMaskrom {
		if r.plan.Loader != nil {
			r.event(PhaseBoot, "info", "", "设备已在 LOADER 态：跳过 Download Boot（ul 由 M1 实现，这里不重做）")
		}
		return nil
	}
	if r.plan.Loader == nil {
		return r.fail(PhaseBoot, "", rockusb.ErrDeviceNotSupport,
			"设备处于 MASKROM，但镜像没有 RKFW loader",
			"裸 RKAF 镜像无法从 maskrom 进入 LOADER：请用带 loader 的 update.img 整包。", nil)
	}

	r.beginPhase(PhaseBoot, "Download Boot")
	loader := r.loader
	if loader == nil {
		var err error
		loader, err = r.readAt(r.plan.Loader.FileOffset, r.plan.Loader.Length)
		if err != nil {
			return r.fail(PhaseBoot, "", rockusb.ErrFailed, fmt.Sprintf("读取 loader 失败: %v", err), "确认镜像文件完整可读。", err)
		}
		r.loader = loader
	}
	img, err := rockusb.ParseLoaderImage(loader)
	if err != nil {
		return r.fail(PhaseBoot, "", rockusb.ErrDeviceNotSupport, fmt.Sprintf("loader 无法解析: %v", err),
			"确认镜像里的 RKFW loader 是完整的 RKBOOT；工具不分发 loader。", err)
	}

	ulDone := int64(0)
	report, dbErr := r.dev.read.DownloadBoot(img, rockusb.DBOptions{
		Progress: func(ev rockusb.DBEvent) {
			switch ev.Kind {
			case "segment":
				r.event(PhaseBoot, "info", "", fmt.Sprintf("Download Boot 段 %s (0x%04X, %d B)", ev.Name, ev.Request, ev.Length))
			case "packet":
				ulDone += int64(ev.Length)
				if ulDone > r.plan.Loader.Length {
					ulDone = r.plan.Loader.Length
				}
				if r.done < ulDone {
					r.done = ulDone
				}
				r.progress(PhaseBoot, "", ulDone, r.plan.Loader.Length, "Download Boot")
			case "settle":
				r.event(PhaseBoot, "info", "", "等待设备重新枚举为 LOADER")
			}
		},
	})
	if dbErr != nil {
		code := rockusb.CodeOf(dbErr)
		hint := "Download Boot 失败：确认设备是裸 maskrom、loader 与芯片匹配，然后重新进入 maskrom。"
		if classified := rockusb.AsDBError(dbErr); classified != nil {
			if classified.Hint() != "" {
				hint = classified.Hint()
			}
			// Split the two hardware-observed failures: the BootROM refusing
			// the loader's first data block (index 0) is a loader/power-cycle
			// problem, while a later failure is a transport problem.
			r.rep.DB = &DBFailReport{
				Category:  classified.Category(),
				Index:     classified.Index,
				BlockName: classified.BlockName,
				Detail:    classified.Detail,
			}
			rerr := r.fail(PhaseBoot, "", code, dbErr.Error(), hint, dbErr)
			rerr.Category = classified.Category()
			return rerr
		}
		return r.fail(PhaseBoot, "", code, dbErr.Error(), hint, dbErr)
	}
	r.endPhase("ok", fmt.Sprintf("%d 段 / %d 包 / %d 字节", report.Segments, report.Packets, report.Bytes))

	// The maskrom transport is dead: the device re-enumerates as LOADER.
	r.closeDevice()
	return r.waitForLoader(ctx)
}

// waitForLoader polls the connector until the device comes back in LOADER mode.
func (r *runner) waitForLoader(ctx context.Context) error {
	deadline := r.opts.now().Add(r.opts.ReenumTimeout)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return r.fail(PhaseBoot, "", ErrCancelled, "等待设备重新枚举时被取消", cancelHint(), err)
		}
		tr, link, err := r.conn.Open(ctx)
		if err == nil {
			if link.Mode == rockusb.ModeLoader {
				r.link = link
				r.dev = r.newDevice(tr, link)
				r.rep.Device.Mode = link.Mode.String()
				r.rep.Device.ReenumAttempts = attempt
				r.event(PhaseBoot, "info", "", fmt.Sprintf("设备已重新枚举为 LOADER（第 %d 次尝试）", attempt))
				return nil
			}
			_ = r.conn.Close()
			if attempt == 1 {
				r.event(PhaseBoot, "warn", "", "设备仍报告 MASKROM：继续等待重新枚举")
			}
		}
		if !r.opts.now().Before(deadline) {
			return r.fail(PhaseBoot, "", rockusb.ErrDeviceNotSupport,
				fmt.Sprintf("Download Boot 之后 %s 内没有看到 LOADER 设备", r.opts.ReenumTimeout),
				"浏览器会撤销按实例的 USB 授权：点“授权设备”重新选择，然后重新运行；已写完的 item 不会被重写。", nil)
		}
		r.opts.sleep(r.opts.ReenumPoll)
	}
}

func (r *runner) testDevice(ctx context.Context) error {
	r.beginPhase(PhaseTest, "Test Device")
	cmd, ok := rockusb.CommandFor(rockusb.OpTestUnitReady)
	if !ok {
		return r.fail(PhaseTest, "", rockusb.ErrDeviceNotSupport, "TEST_UNIT_READY 不在只读命令表里", "", nil)
	}
	res := r.dev.read.Exec(cmd)
	if !res.OK {
		return r.fail(PhaseTest, "", res.Code, "TestDeviceReady 失败: "+res.Detail, readHint(res.Code), nil)
	}
	r.endPhase("ok", "TestDeviceReady")
	return nil
}

func (r *runner) checkChip(ctx context.Context) error {
	r.beginPhase(PhaseChip, "Check Chip")
	cmd, _ := rockusb.CommandFor(rockusb.OpReadChipInfo)
	res := r.dev.read.Exec(cmd)
	if !res.OK {
		return r.fail(PhaseChip, "", res.Code, "读取芯片型号失败: "+res.Detail, readHint(res.Code), nil)
	}
	info, ok := res.Decoded.(rockusb.ChipInfo)
	if !ok {
		return r.fail(PhaseChip, "", rockusb.ErrFailed, fmt.Sprintf("芯片型号数据无法解码: %s", res.DataHex), "", nil)
	}
	r.rep.Device.Chip = info.Text
	r.rep.Device.ChipExpected = r.plan.Chip
	if r.plan.Chip == "" || r.opts.SkipChipCheck {
		r.endPhase("ok", fmt.Sprintf("设备芯片 %q（未与镜像比对）", info.Text))
		return nil
	}
	if !chipMatches(r.plan.Chip, info.Text) {
		return r.fail(PhaseChip, "", rockusb.ErrDeviceNotSupport,
			fmt.Sprintf("镜像芯片 %q 与设备 %q 不匹配", r.plan.Chip, info.Text),
			"换与机型匹配的镜像；不要继续写入。", nil)
	}
	r.endPhase("ok", fmt.Sprintf("设备芯片 %q 匹配镜像 %q", info.Text, r.plan.Chip))
	return nil
}

func (r *runner) readFlashInfo(ctx context.Context) error {
	r.beginPhase(PhaseFlash, "Get FlashInfo")
	cmd, _ := rockusb.CommandFor(rockusb.OpReadFlashInfo)
	res := r.dev.read.Exec(cmd)
	if !res.OK {
		return r.fail(PhaseFlash, "", res.Code, "读取 flash 信息失败: "+res.Detail, readHint(res.Code), nil)
	}
	info, ok := res.Decoded.(rockusb.FlashInfo)
	if !ok {
		return r.fail(PhaseFlash, "", rockusb.ErrFailed, fmt.Sprintf("flash 信息无法解码: %s", res.DataHex), "", nil)
	}
	r.rep.Device.Flash = info
	r.rep.Device.FlashKnown = info.FlashSizeSectors > 0

	if info.FlashSizeSectors == 0 {
		r.rep.Warnings = append(r.rep.Warnings, "设备没有报告 flash 容量：跳过目标 LBA 的边界检查")
		r.endPhase("ok", "设备未报告容量，跳过边界检查")
		return nil
	}

	sectors := flashSectors(info)
	for _, it := range r.plan.Items {
		if r.skipped[it.Name] {
			continue
		}
		end := uint64(it.TargetLBA) + uint64(it.PaddedSize/SectorSize)
		if end > sectors {
			return r.fail(PhaseFlash, it.Name, rockusb.ErrDeviceNotSupport,
				fmt.Sprintf("item %q 写到 LBA 0x%x，超出 flash 容量 %d 扇区", it.Name, end, sectors),
				"检查 --flash-sectors 与镜像的分区表；不要写到容量之外。", nil)
		}
	}
	if r.plan.IDB.Bytes > 0 {
		end := uint64(r.plan.IDB.LBASector) + uint64(r.plan.IDB.Bytes/SectorSize)
		if end > sectors {
			return r.fail(PhaseFlash, "", rockusb.ErrDeviceNotSupport,
				fmt.Sprintf("IDB 写到 LBA 0x%x，超出 flash 容量 %d 扇区", end, sectors),
				"IDB 的 LBA 64 约定待真机核对；确认设备容量。", nil)
		}
	}
	if r.plan.DiskKnown && r.plan.DiskSectors > 0 && r.plan.DiskSectors != sectors {
		warning := fmt.Sprintf("计划的磁盘大小 %d 扇区与设备的 %d 扇区不一致%s",
			r.plan.DiskSectors, sectors, derivedNote(r.plan.DiskDerived))
		r.rep.Warnings = append(r.rep.Warnings, warning)
		r.event(PhaseFlash, "warn", "", warning)
	}
	r.endPhase("ok", fmt.Sprintf("flash %d 扇区（%d MiB），目标 LBA 在范围内", sectors, info.FlashSizeBytes>>20))
	return nil
}

// --- gpt / wl / erase / rd --------------------------------------------------

func (r *runner) downloadIDB(ctx context.Context) error {
	if r.plan.IDB.Bytes == 0 {
		r.rep.Phases = append(r.rep.Phases, PhaseReport{Phase: PhaseIDB, Name: "Prepare/Download IDB", Status: "skipped",
			Note: "镜像没有 loader，gpt 阶段不适用"})
		r.notes = append(r.notes, "镜像没有 RKFW loader：跳过 IDB（gpt）阶段。")
		return nil
	}
	if r.plan.Loader == nil {
		return r.fail(PhaseIDB, "", rockusb.ErrFailed, "计划有 IDB 阶段但缺少 loader 位置", "", nil)
	}
	if r.loader == nil {
		loader, err := r.readAt(r.plan.Loader.FileOffset, r.plan.Loader.Length)
		if err != nil {
			return r.fail(PhaseIDB, "", rockusb.ErrFailed, fmt.Sprintf("读取 loader 失败: %v", err), "确认镜像文件完整可读。", err)
		}
		r.loader = loader
	}

	r.beginPhase(PhaseIDB, "Prepare/Download IDB")
	model := r.opts.IDB
	in := LoaderInput{Container: r.loader}
	if parsed, err := rockusb.ParseLoaderImage(r.loader); err == nil {
		in.Parsed = parsed
	}
	idb, err := model.Build(in)
	if err != nil {
		return r.fail(PhaseIDB, "", rockusb.ErrFailed, fmt.Sprintf("准备 IDB 失败: %v", err), "", err)
	}
	lba := r.plan.IDB.LBASector
	if lba == 0 {
		lba = IDBDefaultLBA
	}
	r.event(PhaseIDB, "info", "", fmt.Sprintf("Prepare IDB: model=%s verified=%t, %d B -> LBA %d（字节模型待真机核对）",
		model.Name(), model.Verified(), len(idb), lba))
	if int64(len(idb)) != r.plan.IDB.Bytes {
		warning := fmt.Sprintf("IDB 实际 %d 字节，与 dry-run 计划的 %d 字节不一致（模型与计划可能脱节）", len(idb), r.plan.IDB.Bytes)
		r.rep.Warnings = append(r.rep.Warnings, warning)
		r.event(PhaseIDB, "warn", "", warning)
	}

	r.rep.IDB = &IDBReport{LBA: lba, Bytes: int64(len(idb)), PlannedBytes: r.plan.IDB.Bytes,
		Model: model.Name(), Verified: model.Verified()}

	itemDone := int64(0)
	if err := r.writeBuffer(ctx, PhaseIDB, "", lba, idb, &itemDone, int64(len(idb))); err != nil {
		return err
	}
	r.endPhase("ok", fmt.Sprintf("%d 字节写入 LBA %d（%s，待真机核对）", len(idb), lba, model.Name()))
	return nil
}

func (r *runner) writeItems(ctx context.Context) error {
	if len(r.plan.Items) == 0 {
		r.rep.Phases = append(r.rep.Phases, PhaseReport{Phase: PhaseWrite, Name: "Download Firmware", Status: "skipped",
			Note: "计划里没有可写 item"})
		return nil
	}
	for _, it := range r.plan.Items {
		if r.skipped[it.Name] {
			r.rep.Items = append(r.rep.Items, ItemReport{Name: it.Name, Partition: it.Partition,
				TargetLBA: it.TargetLBA, Status: "skipped"})
			continue
		}
		if err := ctx.Err(); err != nil {
			return r.fail(PhaseWrite, it.Name, ErrCancelled, fmt.Sprintf("写 item %q 前被取消", it.Name), cancelHint(), err)
		}

		r.currentItem = it.Name
		start := r.opts.now()
		r.beginPhase(PhaseWrite, it.Name)
		r.event(PhaseWrite, "info", it.Name, fmt.Sprintf("wl %s: %d 字节 -> LBA 0x%x (%s)",
			it.Name, it.PaddedSize, it.TargetLBA, partitionOrDash(it.Partition)))

		if err := r.writeItem(ctx, it); err != nil {
			r.rep.Items = append(r.rep.Items, ItemReport{Name: it.Name, Partition: it.Partition,
				TargetLBA: it.TargetLBA, Bytes: r.phaseBytes, Status: statusForErr(err),
				DurationMs: msSince(start, r.opts.now())})
			return err
		}
		r.completed = append(r.completed, it.Name)
		r.rep.Items = append(r.rep.Items, ItemReport{Name: it.Name, Partition: it.Partition,
			TargetLBA: it.TargetLBA, Bytes: it.PaddedSize, Status: "ok",
			DurationMs: msSince(start, r.opts.now())})
		r.endPhase("ok", fmt.Sprintf("%d 字节", it.PaddedSize))
		r.currentItem = ""
	}
	return nil
}

func (r *runner) writeItem(ctx context.Context, it Item) error {
	chunk := r.opts.ChunkSize
	itemDone := int64(0)
	for off := int64(0); off < it.PaddedSize; off += chunk {
		if err := ctx.Err(); err != nil {
			return r.fail(PhaseWrite, it.Name, ErrCancelled, fmt.Sprintf("写 item %q 时被取消（已写 %d/%d 字节）", it.Name, itemDone, it.PaddedSize), cancelHint(), err)
		}
		n := it.PaddedSize - off
		if n > chunk {
			n = chunk
		}
		buf, err := r.readAt(it.FileOffset+off, n)
		if err != nil {
			return r.fail(PhaseWrite, it.Name, rockusb.ErrFailed,
				fmt.Sprintf("读取 item %q 的 %d..%d 字节失败: %v", it.Name, off, off+n, err),
				"镜像文件可能被移动或截断。", err)
		}
		lba := it.TargetLBA + uint32(off/SectorSize)
		if err := r.dev.write.WriteLBAs(ctx, lba, buf); err != nil {
			return r.writeFailure(PhaseWrite, it.Name, err)
		}
		r.done += n
		r.phaseBytes += n
		r.rep.BytesWritten += n
		itemDone += n
		r.progress(PhaseWrite, it.Name, itemDone, it.PaddedSize, "")
	}
	return nil
}

func (r *runner) eraseTails(ctx context.Context) error {
	if len(r.plan.Tails) == 0 {
		return nil
	}
	mode := r.opts.EraseTail
	var total int64
	for _, t := range r.plan.Tails {
		total += t.Bytes
	}
	if mode == EraseNone {
		r.event(PhaseErase, "warn", "", "擦除语义："+EraseNone.Describe())
		for _, t := range r.plan.Tails {
			r.event(PhaseErase, "warn", "", fmt.Sprintf("%s: LBA 0x%x 起 %d 字节未写入/未擦除，保留原内容", t.Partition, t.FirstLBA, t.Bytes))
		}
		r.rep.Erase = &EraseReport{Mode: mode, Tails: len(r.plan.Tails), Status: "skipped",
			Detail: "未覆盖区间保持原内容；是否擦除待真机核对"}
		r.notes = append(r.notes, fmt.Sprintf("未覆盖的分区尾部共 %d 字节未写入（--erase-tail=none）；STATE 等分区的剩余内容保留。", total))
		return nil
	}

	r.beginPhase(PhaseErase, "Erase uncovered tails")
	r.event(PhaseErase, "warn", "", "擦除语义："+mode.Describe())
	for _, t := range r.plan.Tails {
		if err := ctx.Err(); err != nil {
			return r.fail(PhaseErase, t.Partition, ErrCancelled, "擦除未覆盖区间时被取消", cancelHint(), err)
		}
		lba := uint32(t.FirstLBA)
		if mode == EraseLBA {
			if err := r.dev.write.EraseLBAs(ctx, lba, uint64(t.Bytes/SectorSize)); err != nil {
				return r.writeFailure(PhaseErase, t.Partition, err)
			}
			r.done += t.Bytes
			r.phaseBytes += t.Bytes
			r.rep.BytesErased += t.Bytes
			r.progress(PhaseErase, t.Partition, t.Bytes, t.Bytes, "ERASE_LBA")
			continue
		}
		// EraseFill: write 0xFF, which is what EraseEmmcByWriteLBA does.
		buf := make([]byte, r.opts.ChunkSize)
		for i := range buf {
			buf[i] = 0xFF
		}
		for off := int64(0); off < t.Bytes; off += r.opts.ChunkSize {
			n := t.Bytes - off
			if n > r.opts.ChunkSize {
				n = r.opts.ChunkSize
			}
			if err := r.dev.write.WriteLBAs(ctx, lba+uint32(off/SectorSize), buf[:n]); err != nil {
				return r.writeFailure(PhaseErase, t.Partition, err)
			}
			r.done += n
			r.phaseBytes += n
			r.rep.BytesErased += n
			r.progress(PhaseErase, t.Partition, off+n, t.Bytes, "0xFF fill")
		}
	}
	r.rep.Erase = &EraseReport{Mode: mode, Tails: len(r.plan.Tails), Bytes: total, Status: "ok",
		Detail: mode.Describe()}
	r.endPhase("ok", fmt.Sprintf("%d 字节（%s）", total, mode))
	return nil
}

func (r *runner) reset(ctx context.Context) error {
	if r.opts.NoReset {
		r.rep.Phases = append(r.rep.Phases, PhaseReport{Phase: PhaseReset, Name: "Reset device", Status: "skipped",
			Note: "--noreset: 跳过 rd"})
		r.notes = append(r.notes, "已跳过 rd（--noreset）：设备仍在 LOADER 态，需要手动复位或断电重启。")
		return nil
	}
	r.beginPhase(PhaseReset, "Reset device")
	if err := r.dev.write.Reset(ctx); err != nil {
		return r.writeFailure(PhaseReset, "", err)
	}
	r.rep.Reset = true
	r.endPhase("ok", "DEVICE_RESET (rd) 已发送")
	r.event(PhaseReset, "info", "", "设备会重新枚举（bcdUSB bit0=1 -> LOADER 实例消失）：浏览器授权可能失效，需要重新选择设备。")
	return nil
}

// --- helpers ----------------------------------------------------------------

func (r *runner) readAt(off, length int64) ([]byte, error) {
	if length < 0 {
		return nil, fmt.Errorf("negative length %d", length)
	}
	buf := make([]byte, length)
	n, err := r.src.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(n) != length {
		return nil, fmt.Errorf("short read at offset 0x%x: %d of %d bytes", off, n, length)
	}
	return buf, nil
}

func (r *runner) writeBuffer(ctx context.Context, phase, item string, lba uint32, data []byte, itemDone *int64, itemTotal int64) error {
	for off := 0; off < len(data); off += int(r.opts.ChunkSize) {
		if err := ctx.Err(); err != nil {
			return r.fail(phase, item, ErrCancelled, "写入时被取消", cancelHint(), err)
		}
		n := len(data) - off
		if n > int(r.opts.ChunkSize) {
			n = int(r.opts.ChunkSize)
		}
		if err := r.dev.write.WriteLBAs(ctx, lba+uint32(off/SectorSize), data[off:off+n]); err != nil {
			return r.writeFailure(phase, item, err)
		}
		r.done += int64(n)
		r.phaseBytes += int64(n)
		r.rep.BytesWritten += int64(n)
		if itemDone != nil {
			*itemDone += int64(n)
		}
		r.progress(phase, item, *itemDone, itemTotal, "")
	}
	return nil
}

func (r *runner) writeFailure(phase, item string, err error) error {
	code := CodeOf(err)
	rerr := r.fail(phase, item, code, err.Error(), writeHint(code), err)
	var werr *Error
	if errors.As(err, &werr) {
		rerr.Op = int(werr.Op)
		rerr.LBA = werr.LBA
		rerr.Sectors = werr.Sectors
		rerr.Attempt = werr.Attempt
	}
	return rerr
}

func (r *runner) fail(phase, item string, code rockusb.ErrorCode, detail, hint string, err error) *RunError {
	if r.phaseOpen {
		status := "failed"
		if code == ErrCancelled {
			status = "cancelled"
		}
		r.endPhase(status, shortDetail(detail))
	}
	next := ""
	writing := len(r.completed) > 0 || phase == PhaseWrite || phase == PhaseErase
	if writing {
		next = r.nextItem(item)
	}
	if next != "" && hint != "" {
		hint += " " + fmt.Sprintf("重新连接并重新授权后，用 --resume-from %s 续跑；已完成的 item 不会重写，失败的那个会从头重写。", next)
	}
	if code == ErrCancelled {
		hint = cancelHint() + "（已完成 " + fmt.Sprintf("%d", len(r.completed)) + " 个 item）"
		if next != "" {
			hint += fmt.Sprintf("；用 --resume-from %s 续跑。", next)
		}
	}
	return &RunError{
		Code: code, CodeName: CodeName(code), Phase: phase, Item: item,
		Detail: detail, Hint: hint, Completed: append([]string(nil), r.completed...),
		NextItem: next, Err: err,
	}
}

// nextItem is the first item that still has to be written: the failed one when
// the failure happened mid-item, otherwise the first uncompleted, unskipped
// item.
func (r *runner) nextItem(failed string) string {
	if failed != "" {
		done := false
		for _, name := range r.completed {
			if name == failed {
				done = true
				break
			}
		}
		if !done {
			return failed
		}
	}
	for _, it := range r.plan.Items {
		if r.skipped[it.Name] {
			continue
		}
		done := false
		for _, name := range r.completed {
			if name == it.Name {
				done = true
				break
			}
		}
		if !done {
			return it.Name
		}
	}
	return ""
}

func (r *runner) beginPhase(phase, name string) {
	r.phaseStarted = r.opts.now()
	r.phaseName = name
	r.phaseCode = phase
	r.phaseBytes = 0
	r.phaseOpen = true
	r.event(phase, "info", "", "开始 "+name)
}

func (r *runner) endPhase(status, note string) {
	if !r.phaseOpen {
		return
	}
	r.phaseOpen = false
	r.rep.Phases = append(r.rep.Phases, PhaseReport{
		Phase:      r.phaseCode,
		Name:       r.phaseName,
		Bytes:      r.phaseBytes,
		DurationMs: msSince(r.phaseStarted, r.opts.now()),
		Status:     status,
		Note:       note,
	})
}

func shortDetail(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func (r *runner) progress(phase, item string, itemDone, itemTotal int64, note string) {
	p := Progress{
		Phase: phase, Item: item, Done: r.done, Total: r.total,
		ItemDone: itemDone, ItemTotal: itemTotal, Note: note,
	}
	if r.total > 0 {
		p.Percent = float64(r.done) / float64(r.total) * 100
		if p.Percent > 100 {
			p.Percent = 100
		}
	}
	r.opts.progress(p)
}

func (r *runner) event(phase, level, item, message string) {
	r.opts.event(Event{Phase: phase, Level: level, Item: item, Message: message})
}

func readHint(code rockusb.ErrorCode) string {
	switch code {
	case rockusb.ErrDeviceUnready:
		return "设备还没准备好：稍等后重试，或重新进入 LOADER。"
	case rockusb.ErrTimeout:
		return "设备没有响应（超时）：检查 USB 线与接口，必要时重新授权设备。"
	case rockusb.ErrDeviceNotSupport:
		return "设备不支持该命令：确认设备已经是 LOADER（先做 Download Boot）。"
	}
	return "设备可能已掉线：重新连接并重新授权后再试。"
}

func writeHint(code rockusb.ErrorCode) string {
	switch code {
	case rockusb.ErrTimeout:
		return "USB 传输超时（已按 --retries 重试）：检查线材/供电；设备可能已掉线。"
	case rockusb.ErrDeviceWriteFailed:
		return "bulk OUT 失败：设备可能拒绝了写入或已经掉线。"
	case rockusb.ErrDeviceReadFailed:
		return "读取 CSW 失败：设备没有回状态，通常表示已经掉线或复位。"
	case rockusb.ErrFailed:
		return "设备返回 CSW failed：该 LBA 不可写（超出分区/flash、只读或需要先擦除）。"
	case rockusb.ErrCmdNotMatch:
		return "CSW 与 CBW 不匹配：设备状态混乱，先复位/重新进入 LOADER 再试。"
	case ErrCancelled:
		return cancelHint()
	}
	return "写入失败：确认设备仍在 LOADER 且镜像与机型匹配。"
}

func cancelHint() string {
	return "运行已取消：设备未复位，仍停在 LOADER；已写入的 item 保持有效"
}

func statusForErr(err error) string {
	if isCancelled(err) {
		return "cancelled"
	}
	return "failed"
}

func partitionOrDash(name string) string {
	if name == "" {
		return "-"
	}
	return name
}

func derivedNote(derived bool) string {
	if derived {
		return "（计划用的是派生值：dry-run 未指定 --flash-sectors）"
	}
	return ""
}

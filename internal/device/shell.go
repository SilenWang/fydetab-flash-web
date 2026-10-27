package device

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
)

// Bridge is the page-side half of the wasm core: the only way the shell can
// touch USB. In the browser it is implemented on top of WebUSB transfers that
// run on the page's main thread; in the tests it is a fake.
type Bridge interface {
	// List returns the already-authorized Rockchip devices.
	List() ([]RawInfo, error)
	// Open opens, configures and claims one device and returns a transport
	// bound to it. handle is the opaque identifier the list() call supplied;
	// the link (endpoints, class, mode) is derived from the listed Info, so
	// the page never re-implements bcdUSB mode detection.
	Open(devNo int, handle string) (rockusb.Transport, error)
	// Close releases the interface and the device.
	Close(devNo int) error
}

// Config tunes the shell.
type Config struct {
	// Timeout bounds one USB transfer.
	Timeout time.Duration
	// ReadRetries is how many extra attempts a read-only command may make
	// after a timeout. Default 1: a read is safe to repeat, and a stuck pipe
	// is the one failure a retry can actually fix. STALLs are never retried.
	ReadRetries int
}

// Shell is a small upgrade_tool-flavoured command interpreter. It keeps the
// session state the page renders (device list, current device, mode, whether a
// Download Boot is waiting for re-enumeration) and returns everything the page
// needs as one JSON-friendly value.
type Shell struct {
	bridge Bridge
	cfg    Config

	reg      Registry
	current  int
	session  *rockusb.Session
	openOn   int
	loader   []byte
	loaderAt string

	awaitingReenum bool
	transfers      []rockusb.Transfer
	seq            int

	// Now is a test hook; nil means time.Now.
	Now func() time.Time
}

// New builds a shell around a bridge.
func New(bridge Bridge, cfg Config) *Shell {
	if cfg.ReadRetries < 0 {
		cfg.ReadRetries = 0
	}
	return &Shell{bridge: bridge, cfg: cfg, seq: 0}
}

// State is the page state the UI renders. It never includes device bytes.
type State struct {
	Mode                  string `json:"mode"` // MASKROM / LOADER / UNKNOWN / NONE
	ModeLabel             string `json:"modeLabel"`
	Current               int    `json:"current"`
	Devices               []Info `json:"devices"`
	SessionOpen           bool   `json:"sessionOpen"`
	AwaitingReenumeration bool   `json:"awaitingReenumeration"`
	LoaderLoaded          bool   `json:"loaderLoaded"`
	LoaderName            string `json:"loaderName"`
}

// InfoPayload is the read-only information gathered in loader mode.
type InfoPayload struct {
	Chip          *rockusb.ChipInfo   `json:"chip,omitempty"`
	Flash         *rockusb.FlashInfo  `json:"flash,omitempty"`
	Capability    *rockusb.Capability `json:"capability,omitempty"`
	Storage       *rockusb.Storage    `json:"storage,omitempty"`
	TestUnitReady bool                `json:"testUnitReady"`
	ReadOnly      bool                `json:"readOnly"`
}

// ErrorPayload is a classified failure plus the next step to try.
type ErrorPayload struct {
	Code     rockusb.ErrorCode    `json:"code"`
	CodeName string               `json:"codeName"`
	Detail   string               `json:"detail"`
	Hint     string               `json:"hint,omitempty"`
	DB       *rockusb.DBError     `json:"db,omitempty"`
	Loader   *rockusb.LoaderError `json:"loader,omitempty"`
}

// Result is one command's outcome.
type Result struct {
	OK        bool               `json:"ok"`
	Command   string             `json:"command"`
	Lines     []string           `json:"lines"`
	State     State              `json:"state"`
	Commands  []rockusb.Result   `json:"commands,omitempty"`
	Transfers []rockusb.Transfer `json:"transfers,omitempty"`
	Info      *InfoPayload       `json:"info,omitempty"`
	DB        *rockusb.DBReport  `json:"db,omitempty"`
	Loader    []string           `json:"loader,omitempty"`
	Error     *ErrorPayload      `json:"error,omitempty"`
}

// Commands is the complete command surface. There is deliberately no write,
// erase, reset or flash command at this stage; TestNoFlashWriteCommands keeps
// it that way.
var Commands = []struct {
	Name    string
	Usage   string
	Summary string
}{
	{"ld", "ld", "列出已授权设备（DevNo=... Vid=0x2207,Pid=...,Mode=...）"},
	{"desc", "desc", "打印当前设备描述符（无 USB 传输）"},
	{"cd", "cd <n>", "ChangeDevice：在当前枚举结果里按 DevNo 选择设备"},
	{"sd", "sd <n>", "SwitchDevice：重新绑定当前设备（db 重新枚举后用）"},
	{"db", "db", "Download Boot：把本地 loader 写进 maskrom RAM（只写 RAM）"},
	{"info", "info [chip|flash|cap|storage]", "读取只读信息（仅 LOADER 态）"},
	{"help", "help", "显示命令列表"},
	{"version", "version", "显示核心版本"},
}

const CoreVersion = "0.2.0-m1"

// Exec runs one command line. loaderBytes is the user-picked loader file for
// `db` (nil means "reuse the one loaded earlier"); loaderName is only used for
// display.
func (s *Shell) Exec(line string, loaderBytes []byte, loaderName string) Result {
	s.transfers = nil
	trimmed := strings.TrimSpace(line)
	res := Result{Command: strings.ToLower(firstField(trimmed))}
	if trimmed == "" {
		return s.fail(res, nil, rockusb.ErrFailed, "empty command", "输入 ld / cd <n> / sd <n> / db / info / help")
	}

	args := strings.Fields(trimmed)
	switch strings.ToLower(args[0]) {
	case "ld":
		return s.ld(res)
	case "desc":
		return s.desc(res)
	case "cd":
		return s.selectDevice(res, args, true)
	case "sd":
		return s.selectDevice(res, args, false)
	case "db":
		return s.downloadBoot(res, loaderBytes, loaderName)
	case "info":
		return s.info(res, args[1:])
	case "help":
		return s.help(res)
	case "version":
		res.OK = true
		res.Lines = []string{fmt.Sprintf("fydetab-flash-web core %s (rockusb CBW + Download Boot, no flash writes)", CoreVersion)}
		return s.finishCode(res, rockusb.ErrSuccess)
	default:
		return s.fail(res, nil, rockusb.ErrDeviceNotSupport,
			fmt.Sprintf("unknown command %q", args[0]),
			"可用命令："+strings.Join(commandNames(), ", "))
	}
}

func commandNames() []string {
	out := make([]string, 0, len(Commands))
	for _, c := range Commands {
		out = append(out, c.Name)
	}
	return out
}

func firstField(s string) string {
	if s == "" {
		return ""
	}
	return strings.Fields(s)[0]
}

// --- commands ---------------------------------------------------------------

func (s *Shell) ld(res Result) Result {
	raw, err := s.bridge.List()
	if err != nil {
		return s.fail(res, nil, rockusb.ErrDeviceOpenFailed, fmt.Sprintf("list devices: %v", err), "确认浏览器已授权设备（0x2207）")
	}
	list := s.reg.Replace(raw)

	if s.current != 0 {
		if _, ok := s.reg.Find(s.current); !ok {
			s.closeSession()
			s.current = 0
		}
	}
	if s.current == 0 && len(list) == 1 {
		s.current = list[0].DevNo
	}

	if len(list) == 0 {
		res.Lines = []string{"No device found."}
		return s.finishCode(res, rockusb.ErrSuccess)
	}
	for _, d := range list {
		res.Lines = append(res.Lines, d.Line())
	}
	if len(list) > 1 {
		res.Lines = append(res.Lines, fmt.Sprintf("Current: DevNo=%d", s.current))
	}
	// A device that reappears in the other mode is exactly what a Download
	// Boot produces; report it instead of leaving the page guessing.
	if s.awaitingReenum {
		cur, _ := s.reg.Find(s.current)
		if cur.Mode == rockusb.ModeLoader {
			s.awaitingReenum = false
			res.Lines = append(res.Lines,
				"Re-enumerated as LOADER after Download Boot: 现在可以 info / cd / sd。")
		}
	}
	return s.finishCode(res, rockusb.ErrSuccess)
}

// refreshAwaiting re-lists after a Download Boot so the next command sees the
// device that just came back in LOADER mode. It returns a note line when the
// transition is observed.
func (s *Shell) refreshAwaiting() []string {
	if !s.awaitingReenum {
		return nil
	}
	raw, err := s.bridge.List()
	if err != nil {
		return nil
	}
	s.reg.Replace(raw)
	if dev, ok := s.currentDevice(); ok && dev.Mode == rockusb.ModeLoader {
		s.awaitingReenum = false
		return []string{"Re-enumerated as LOADER after Download Boot: 现在可以 info / cd / sd。"}
	}
	return nil
}

func (s *Shell) desc(res Result) Result {
	dev, ok := s.currentDevice()
	if !ok {
		return s.fail(res, nil, rockusb.ErrDeviceOpenFailed, "no current device", "先运行 ld 选择设备")
	}
	res.Lines = Describe(dev)
	return s.finishCode(res, rockusb.ErrSuccess)
}

func (s *Shell) selectDevice(res Result, args []string, change bool) Result {
	notes := s.refreshAwaiting()
	verb := "SwitchDevice"
	if change {
		verb = "ChangeDevice"
	}
	if len(args) != 2 {
		return s.fail(res, nil, rockusb.ErrFailed, fmt.Sprintf("%s needs exactly one device number", strings.ToLower(args[0])),
			"用法："+verb+" <n>，n 来自 ld")
	}
	n, err := strconv.Atoi(args[1])
	if err != nil {
		return s.fail(res, nil, rockusb.ErrFailed, fmt.Sprintf("%q is not a device number", args[1]), "用法："+verb+" <n>")
	}
	dev, ok := s.reg.Find(n)
	if !ok {
		if s.reg.Len() == 0 {
			// Refresh once: the page may not have listed devices yet.
			if raw, lerr := s.bridge.List(); lerr == nil {
				s.reg.Replace(raw)
			}
			dev, ok = s.reg.Find(n)
		}
	}
	if !ok {
		return s.fail(res, nil, rockusb.ErrDeviceOpenFailed,
			fmt.Sprintf("no device with DevNo=%d (listed: %s)", n, listedDevNos(s.reg.List())),
			"先运行 ld 看当前设备编号")
	}

	s.closeSession()
	s.current = n
	res.Lines = append(notes, fmt.Sprintf("%s: DevNo=%d Vid=0x%04x,Pid=0x%04x,Mode=%s", verb, dev.DevNo, dev.VendorID, dev.ProductID, dev.ModeLabel))
	switch dev.Mode {
	case rockusb.ModeMaskrom:
		res.Lines = append(res.Lines, "Note: maskrom 态只能 ld / desc / db；只读信息要在 Download Boot 之后的 LOADER 态读取。")
	case rockusb.ModeLoader:
		res.Lines = append(res.Lines, "Mode=Loader：可以 info / cd / sd。")
	}
	return s.finishCode(res, rockusb.ErrSuccess)
}

func listedDevNos(list []Info) string {
	if len(list) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(list))
	for _, d := range list {
		parts = append(parts, strconv.Itoa(d.DevNo))
	}
	return strings.Join(parts, ",")
}

func (s *Shell) downloadBoot(res Result, loaderBytes []byte, loaderName string) Result {
	candidate := loaderBytes
	if candidate == nil {
		candidate = s.loader
	}
	if len(candidate) == 0 {
		return s.fail(res, nil, rockusb.ErrFailed,
			"no loader file supplied",
			"选择本地 loader（如 rk3588_spl_loader_v1.06.111.bin）；工具不分发 loader，文件只在本机内存里使用。")
	}
	img, err := rockusb.ParseLoaderImage(candidate)
	if err != nil {
		var lerr *rockusb.LoaderError
		payload := &ErrorPayload{Code: rockusb.ErrFailed, CodeName: rockusb.ErrFailed.String(), Detail: err.Error()}
		if errors.As(err, &lerr) {
			payload.Loader = lerr
			payload.Hint = loaderHint(lerr.Kind)
		}
		res.Loader = nil
		return s.failWith(res, payload)
	}
	// Only a file that parsed becomes the session's loader, so a mistyped
	// pick cannot linger as "loaded".
	s.loader = candidate
	s.loaderAt = loaderName
	res.Loader = img.Summary()

	dev, ok := s.currentDevice()
	if !ok {
		return s.fail(res, nil, rockusb.ErrDeviceOpenFailed, "no current device", "先运行 ld 选择设备")
	}
	if dev.Mode != rockusb.ModeMaskrom {
		return s.fail(res, nil, rockusb.ErrDeviceNotSupport,
			fmt.Sprintf("device DevNo=%d is in %s mode", dev.DevNo, dev.ModeLabel),
			"Download Boot 只对裸 maskrom 有意义；LOADER 态直接 info / cd / sd。")
	}

	sess, openErr := s.openSession(dev.DevNo)
	if openErr != nil {
		return s.fail(res, openErr, rockusb.ErrDeviceOpenFailed, openErr.Error(), "确认设备仍在 maskrom、且浏览器授权未失效")
	}

	// No progress callback: the worker blocks until db returns, and the page
	// shows the per-segment summary from the report afterwards.
	report, dbErr := sess.DownloadBoot(img, rockusb.DBOptions{})
	s.closeSession()

	if dbErr != nil {
		classified := rockusb.AsDBError(dbErr)
		payload := &ErrorPayload{Code: rockusb.CodeOf(dbErr), CodeName: rockusb.CodeOf(dbErr).String(), Detail: dbErr.Error()}
		if classified != nil {
			payload.Code = classified.Code
			payload.CodeName = classified.CodeName
			payload.Detail = classified.Detail
			payload.Hint = classified.Hint()
			payload.DB = classified
			res.Lines = append(res.Lines, "Downloading bootloader failed: "+classified.Kind.String())
		}
		return s.failWith(res, payload)
	}

	s.awaitingReenum = true
	res.DB = report
	res.Lines = append(res.Lines, "Downloading bootloader succeeded.")
	for _, ev := range report.Names {
		res.Lines = append(res.Lines, "  sent "+ev)
	}
	res.Lines = append(res.Lines,
		fmt.Sprintf("  %d segments, %d packets, %d bytes over EP0 vendor requests 0x0471/0x0472",
			report.Segments, report.Packets, report.Bytes),
		"设备应重新枚举为 LOADER（2207:...., bcdUSB bit0=1）。重新枚举后浏览器会撤销按实例的授权：点“授权设备”重新选择，再 ld / info。")
	return s.finishCode(res, rockusb.ErrSuccess)
}

func loaderHint(kind rockusb.LoaderErrorKind) string {
	switch kind {
	case rockusb.LoaderEmpty:
		return "文件是空的：确认选择的是 loader 二进制，不是占位文件。"
	case rockusb.LoaderTooShort:
		return "文件太短，不像 RKBOOT：确认 loader 下载完整（rk3588_spl_loader_*.bin 约 140 KiB）。"
	case rockusb.LoaderTooLarge:
		return "文件远大于 RK3588 loader 的合理大小：确认没有选成整包 update.img。"
	case rockusb.LoaderBadTag:
		return "文件头不是 BOOT/LDR：这不是 RKBOOT loader，可能是解压后的裸 bin。"
	case rockusb.LoaderBadCRC:
		return "RKBOOT CRC-32 不匹配：文件被截断或改写过。"
	case rockusb.LoaderChipMismatch:
		return "loader 是为其他芯片构建的：RK3588 需要 chip=0x33353838（\"3588\"）。"
	case rockusb.LoaderNoBootEntries:
		return "loader 没有 0x0471/0x0472 段：不是可用于 Download Boot 的 loader。"
	case rockusb.LoaderBadEntry:
		return "段表越界或损坏：文件可能只下载了一部分。"
	default:
		return ""
	}
}

func (s *Shell) info(res Result, args []string) Result {
	res.Lines = append(res.Lines, s.refreshAwaiting()...)
	dev, ok := s.currentDevice()
	if !ok {
		return s.fail(res, nil, rockusb.ErrDeviceOpenFailed, "no current device", "先运行 ld 选择设备")
	}
	if dev.Mode != rockusb.ModeLoader {
		return s.fail(res, nil, rockusb.ErrDeviceNotSupport,
			fmt.Sprintf("device DevNo=%d is in %s mode; read-only info needs LOADER", dev.DevNo, dev.ModeLabel),
			"maskrom 会 STALL 掉 31 字节 CBW（M0b 实测 5/5 条都失败），先 db 进入 LOADER 再读。")
	}

	want := map[string]bool{"chip": true, "flash": true, "cap": true, "storage": true}
	if len(args) > 0 {
		want = map[string]bool{}
		for _, a := range args {
			name := strings.ToLower(a)
			if name == "all" {
				want = map[string]bool{"chip": true, "flash": true, "cap": true, "storage": true}
				continue
			}
			if !isInfoSection(name) {
				return s.fail(res, nil, rockusb.ErrFailed, fmt.Sprintf("unknown info section %q", a),
					"可用：chip / flash / cap / storage / all")
			}
			want[name] = true
		}
	}

	sess, openErr := s.openSession(dev.DevNo)
	if openErr != nil {
		return s.fail(res, openErr, rockusb.ErrDeviceOpenFailed, openErr.Error(), "确认设备仍在 LOADER、且浏览器授权未失效")
	}
	defer s.closeSession()

	payload := &InfoPayload{ReadOnly: true}
	failures := 0

	ready, _ := rockusb.CommandFor(rockusb.OpTestUnitReady)
	readyRes := sess.Exec(ready)
	res.Commands = append(res.Commands, readyRes)
	payload.TestUnitReady = readyRes.OK
	if readyRes.OK {
		res.Lines = append(res.Lines, "TestDeviceReady: OK")
	} else {
		failures++
		res.Lines = append(res.Lines, fmt.Sprintf("TestDeviceReady: %s (%d)", readyRes.CodeName, readyRes.Code))
	}

	sections := []struct {
		name string
		op   rockusb.Op
	}{
		{"chip", rockusb.OpReadChipInfo},
		{"flash", rockusb.OpReadFlashInfo},
		{"cap", rockusb.OpReadCapability},
		{"storage", rockusb.OpReadStorage},
	}
	for _, section := range sections {
		if !want[section.name] {
			continue
		}
		cmd, _ := rockusb.CommandFor(section.op)
		r := sess.Exec(cmd)
		res.Commands = append(res.Commands, r)
		if !r.OK {
			failures++
			res.Lines = append(res.Lines, fmt.Sprintf("%s: %s (%d) %s", cmd.Name, r.CodeName, r.Code, shortDetail(r.Detail)))
			continue
		}
		switch decoded := r.Decoded.(type) {
		case rockusb.ChipInfo:
			info := decoded
			payload.Chip = &info
			res.Lines = append(res.Lines, fmt.Sprintf("ChipInfo: %s (%s)", info.Text, info.Raw))
		case rockusb.FlashInfo:
			info := decoded
			payload.Flash = &info
			res.Lines = append(res.Lines, flashLines(info)...)
		case rockusb.Capability:
			info := decoded
			payload.Capability = &info
			res.Lines = append(res.Lines, fmt.Sprintf("Capability: %s [%s]", info.Raw, strings.Join(info.Flags, ", ")))
		case rockusb.Storage:
			info := decoded
			payload.Storage = &info
			res.Lines = append(res.Lines, fmt.Sprintf("Storage: current=%s available=[%s] bitmap=0x%08X",
				info.CurrentName, strings.Join(info.Names, ", "), info.Bitmap))
		default:
			res.Lines = append(res.Lines, fmt.Sprintf("%s: %s", cmd.Name, r.DataHex))
		}
	}

	res.Info = payload
	if failures > 0 {
		result := s.finishCode(res, rockusb.ErrFailed)
		result.Error.Detail = fmt.Sprintf("%d read-only command(s) failed; see lines", failures)
		return result
	}
	return s.finishCode(res, rockusb.ErrSuccess)
}

func isInfoSection(name string) bool {
	switch name {
	case "chip", "flash", "cap", "storage", "all":
		return true
	}
	return false
}

func flashLines(info rockusb.FlashInfo) []string {
	return []string{
		fmt.Sprintf("FlashInfo: size=%s block=%s page=%s ecc=%dbit manufacturer=%s cs=%v raw=%s",
			bytesHuman(info.FlashSizeBytes), bytesHuman(uint64(info.BlockSizeBytes)),
			bytesHuman(uint64(info.PageSizeBytes)), info.ECCBits, info.Manufacturer, info.FlashCSList, info.Raw),
	}
}

func bytesHuman(n uint64) string {
	const (
		mib = 1024 * 1024
		kib = 1024
	)
	switch {
	case n >= mib && n%mib == 0:
		return fmt.Sprintf("%d MiB", n/mib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib && n%kib == 0:
		return fmt.Sprintf("%d KiB", n/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func shortDetail(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func (s *Shell) help(res Result) Result {
	res.OK = true
	res.Lines = []string{"fydetab-flash-web M1 — rockusb over WebUSB（无写 flash 路径）"}
	for _, c := range Commands {
		res.Lines = append(res.Lines, fmt.Sprintf("  %-24s %s", c.Usage, c.Summary))
	}
	return s.finishCode(res, rockusb.ErrSuccess)
}

// --- session plumbing -------------------------------------------------------

func (s *Shell) currentDevice() (Info, bool) {
	if s.current == 0 {
		return Info{}, false
	}
	return s.reg.Find(s.current)
}

func (s *Shell) openSession(devNo int) (*rockusb.Session, error) {
	if s.session != nil && s.openOn == devNo {
		return s.session, nil
	}
	s.closeSession()
	info, ok := s.reg.Find(devNo)
	if !ok {
		return nil, fmt.Errorf("open DevNo=%d: device is no longer listed", devNo)
	}
	transport, err := s.bridge.Open(devNo, info.Handle)
	if err != nil {
		return nil, fmt.Errorf("open DevNo=%d: %w", devNo, err)
	}
	link := rockusb.Link{
		InterfaceNumber: info.InterfaceNumber,
		InterfaceClass:  info.InterfaceClass,
		BulkIn:          info.BulkIn,
		BulkOut:         info.BulkOut,
		VendorID:        info.VendorID,
		ProductID:       info.ProductID,
		Mode:            info.Mode,
	}
	s.seq = 0
	s.session = rockusb.NewSession(transport, link, rockusb.Options{
		Timeout: s.cfg.Timeout,
		Retries: s.cfg.ReadRetries,
		Trace:   s.recordTransfer,
	})
	s.openOn = devNo
	return s.session, nil
}

func (s *Shell) closeSession() {
	if s.session == nil {
		return
	}
	devNo := s.openOn
	s.session = nil
	s.openOn = 0
	_ = s.bridge.Close(devNo)
}

// recordTransfer keeps one globally numbered trace across commands, so the
// page can append instead of resetting its table.
func (s *Shell) recordTransfer(t rockusb.Transfer) {
	s.seq++
	t.Seq = s.seq
	s.transfers = append(s.transfers, t)
}

// Close releases the device when the page unloads or the user disconnects.
func (s *Shell) Close() {
	s.closeSession()
}

// --- result helpers ---------------------------------------------------------

func (s *Shell) state() State {
	st := State{
		Mode:                  "NONE",
		ModeLabel:             "None",
		Current:               s.current,
		Devices:               s.reg.List(),
		SessionOpen:           s.session != nil,
		AwaitingReenumeration: s.awaitingReenum,
		LoaderLoaded:          len(s.loader) > 0,
		LoaderName:            s.loaderAt,
	}
	if dev, ok := s.currentDevice(); ok {
		st.Mode = modeCode(dev.Mode)
		st.ModeLabel = dev.ModeLabel
	}
	return st
}

func modeCode(m rockusb.MediaMode) string {
	switch m {
	case rockusb.ModeMaskrom:
		return "MASKROM"
	case rockusb.ModeLoader:
		return "LOADER"
	default:
		return "UNKNOWN"
	}
}

func (s *Shell) finishCode(res Result, code rockusb.ErrorCode) Result {
	res.OK = code == rockusb.ErrSuccess
	res.State = s.state()
	res.Transfers = append([]rockusb.Transfer(nil), s.transfers...)
	if !res.OK && res.Error == nil {
		res.Error = &ErrorPayload{Code: code, CodeName: code.String()}
	}
	return res
}

func (s *Shell) fail(res Result, err error, code rockusb.ErrorCode, detail, hint string) Result {
	// An explicit code wins; the error only fills in when the caller had none.
	if err != nil && code == rockusb.ErrSuccess {
		code = rockusb.CodeOf(err)
		if code == rockusb.ErrSuccess {
			code = rockusb.ErrFailed
		}
	}
	if code == rockusb.ErrSuccess {
		code = rockusb.ErrFailed
	}
	return s.failWith(res, &ErrorPayload{Code: code, CodeName: code.String(), Detail: detail, Hint: hint})
}

func (s *Shell) failWith(res Result, payload *ErrorPayload) Result {
	res.Error = payload
	res.Lines = append(res.Lines, fmt.Sprintf("%s (%d): %s", payload.CodeName, payload.Code, payload.Detail))
	if payload.Hint != "" {
		res.Lines = append(res.Lines, "hint: "+payload.Hint)
	}
	st := s.state()
	res.State = st
	res.OK = false
	res.Transfers = append([]rockusb.Transfer(nil), s.transfers...)
	return res
}

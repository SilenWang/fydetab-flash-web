//go:build js && wasm

package main

// The write half of the page: the same `internal/uf` state machine the CLI runs
// against a mock, bound to the browser's WebUSB transport.
//
// Division of labour mirrors the M1 shell:
//   - the page owns the image bytes and exposes them as {name, size,
//     read(offset, length) -> Promise<Uint8Array>}; jsImageSource turns that
//     into an io.ReaderAt, so rkfw/parameter/imageplan read a multi-GiB file
//     lazily instead of copying it into wasm memory;
//   - planning is imageplan.BuildReport, the one planner the CLI also uses;
//   - Download Boot + writes go through uf.BridgeConnector, which opens the
//     device.Bridge the worker bound (jsBridge → WebUSB on the main thread);
//   - mock mode uses uf.SimConnector, an in-wasm device, so the demo and the
//     node tests exercise the identical state machine without hardware.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"syscall/js"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/imageplan"
	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
	"github.com/SilenWang/rk3588-webflasher/internal/uf"
)

// --- image source -----------------------------------------------------------

// jsImageSource is an io.ReaderAt over the page's image object. Every read is
// one JS call that resolves a Promise; awaitPromise parks this goroutine and
// yields to the event loop, which is what lets a synchronous parser read an
// asynchronous Web File.
type jsImageSource struct {
	obj  js.Value
	name string
	size int64

	mu sync.Mutex
}

func (s *jsImageSource) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("uf: negative offset %d", off)
	}
	if off >= s.size {
		return 0, io.EOF
	}
	want := len(p)
	if avail := int(s.size - off); want > avail {
		want = avail
	}

	s.mu.Lock()
	value := s.obj.Call("read", off, want)
	res, err := awaitPromise(value)
	s.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("read image [0x%x,0x%x): %w", off, off+int64(want), err)
	}
	data := bytesFromJS(res)
	n := copy(p, data)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func imageSourceFromJS(v js.Value) (*jsImageSource, error) {
	if v.Type() != js.TypeObject {
		return nil, fmt.Errorf("image object is required")
	}
	size := v.Get("size")
	if size.Type() != js.TypeNumber {
		return nil, fmt.Errorf("image.size must be a number")
	}
	read := v.Get("read")
	if read.Type() != js.TypeFunction {
		return nil, fmt.Errorf("image.read(offset, length) must be a function")
	}
	name := ""
	if n := v.Get("name"); n.Type() == js.TypeString {
		name = n.String()
	}
	return &jsImageSource{obj: v, name: name, size: int64(size.Int())}, nil
}

// --- options ----------------------------------------------------------------

type ufOptions struct {
	FlashSectors  uint64
	EraseTail     string
	NoReset       bool
	Verify        bool
	LoaderBytes   []byte
	LoaderName    string
	Mock          bool
	DeviceHandle  string
	ChunkSize     int64
	Retries       int
	Timeout       time.Duration
	ReenumTimeout time.Duration
}

func ufOptionsFromJS(v js.Value) (ufOptions, error) {
	opts := ufOptions{EraseTail: string(uf.EraseNone)}
	if v.Type() != js.TypeObject {
		return opts, nil
	}
	if n := v.Get("flashSectors"); n.Type() == js.TypeNumber {
		opts.FlashSectors = uint64(n.Int())
	}
	if s := v.Get("eraseTail"); s.Type() == js.TypeString {
		opts.EraseTail = s.String()
	}
	opts.NoReset = v.Get("noReset").Truthy()
	opts.Verify = v.Get("verify").Truthy()
	opts.Mock = v.Get("mock").Truthy()
	if s := v.Get("deviceHandle"); s.Type() == js.TypeString {
		opts.DeviceHandle = s.String()
	}
	if n := v.Get("chunkSize"); n.Type() == js.TypeNumber {
		opts.ChunkSize = int64(n.Int())
	}
	if n := v.Get("retries"); n.Type() == js.TypeNumber {
		opts.Retries = n.Int()
	}
	if n := v.Get("timeoutMs"); n.Type() == js.TypeNumber {
		opts.Timeout = millis(int64(n.Int()))
	}
	if n := v.Get("reenumTimeoutMs"); n.Type() == js.TypeNumber {
		opts.ReenumTimeout = millis(int64(n.Int()))
	}
	if name := v.Get("loaderName"); name.Type() == js.TypeString {
		opts.LoaderName = name.String()
	}
	if data := v.Get("loaderBytes"); data.Type() == js.TypeObject {
		opts.LoaderBytes = bytesFromJS(data)
	}
	if _, err := uf.ParseEraseMode(opts.EraseTail); err != nil {
		return opts, err
	}
	return opts, nil
}

func ufArgs(args []js.Value) (*jsImageSource, ufOptions, error) {
	if len(args) == 0 {
		return nil, ufOptions{}, fmt.Errorf("uf: image object is required")
	}
	src, err := imageSourceFromJS(args[0])
	if err != nil {
		return nil, ufOptions{}, err
	}
	var raw js.Value
	if len(args) > 1 {
		raw = args[1]
	}
	opts, err := ufOptionsFromJS(raw)
	return src, opts, err
}

// --- planning ---------------------------------------------------------------

// ufImageInfo is the image identity the page shows next to the plan.
type ufImageInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ufFacts are the pre-flight values the page's checklist switches on. The page
// decides what is a pass; Go only reports measurements.
type ufFacts struct {
	Chip            string `json:"chip"`
	Partitions      int    `json:"partitions"`
	Items           int    `json:"items"`
	TotalBytes      int64  `json:"totalBytes"`
	UncoveredBytes  int64  `json:"uncoveredBytes"`
	UncoveredTails  int    `json:"uncoveredTails"`
	UncoveredParts  string `json:"uncoveredPartitions,omitempty"`
	EraseMode       string `json:"eraseMode"`
	HasLoader       bool   `json:"hasLoader"`
	LoaderSource    string `json:"loaderSource,omitempty"`
	LoaderChipCheck string `json:"loaderChipCheck,omitempty"`
	LoaderChip      string `json:"loaderChip,omitempty"`
	LoaderSHA256    string `json:"loaderSha256,omitempty"`
	LoaderLength    int64  `json:"loaderLength,omitempty"`
	LoaderBuildTime string `json:"loaderBuildTime,omitempty"`
	LoaderNonImage  bool   `json:"loaderNonImage,omitempty"`
}

type ufPlanResult struct {
	OK     bool                      `json:"ok"`
	Image  ufImageInfo               `json:"image"`
	Report *imageplan.Report         `json:"report,omitempty"`
	Loader *imageplan.EmbeddedLoader `json:"loader,omitempty"`
	Facts  ufFacts                   `json:"facts"`
	Kind   string                    `json:"kind,omitempty"`
	Error  string                    `json:"error,omitempty"`
}

type ufRunResult struct {
	OK     bool                      `json:"ok"`
	Image  ufImageInfo               `json:"image"`
	Loader *imageplan.EmbeddedLoader `json:"loader,omitempty"`
	Facts  ufFacts                   `json:"facts"`
	Run    *uf.Report                `json:"run,omitempty"`
	Kind   string                    `json:"kind,omitempty"`
	Error  string                    `json:"error,omitempty"`
}

// buildUfPlan parses the image, builds the one planner's report and projects it
// onto the writer's plan. It is exactly what `uf --dry-run` does, minus the
// streaming MD5 unless the caller asks for it.
func buildUfPlan(src *jsImageSource, opts ufOptions) (*imageplan.Report, *imageplan.EmbeddedLoader, *uf.Plan, error) {
	img, err := rkfw.OpenReaderAt(src, src.size, src.name)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("解析镜像: %w", err)
	}
	if img.Parameter == nil {
		return nil, nil, nil, fmt.Errorf("镜像没有 parameter 分区，无法建立写入计划")
	}
	raw, err := img.ReadItem(*img.Parameter, imageplan.MaxParameterBytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取 parameter: %w", err)
	}
	doc, err := parameter.Parse(raw)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("解析 parameter: %w", err)
	}

	ver := imageplan.VerificationInfo{}
	if doc.CRCAvailable {
		ver.Parameter = &rkfw.CRCResult{Stored: doc.CRCStored, Computed: doc.CRCComputed, OK: doc.CRCOK}
	}
	if opts.Verify {
		md5res, err := img.VerifyMD5()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("校验 MD5: %w", err)
		}
		ver.MD5 = &md5res
		crc, err := img.VerifyRKAFCRC()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("校验 RKAF CRC: %w", err)
		}
		ver.RKAFCRC = &crc
	} else {
		ver.Skipped = true
	}

	rep, err := imageplan.BuildReport(img, doc, ver, imageplan.Options{
		FlashSectors: opts.FlashSectors,
		NoReset:      opts.NoReset,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	chip := ""
	if img.HasRKFW {
		chip = img.RKFW.ChipName()
	}
	var loader *imageplan.EmbeddedLoader
	if len(opts.LoaderBytes) > 0 {
		loader = imageplan.FromFile(opts.LoaderName, opts.LoaderBytes, chip)
	} else {
		loader, err = imageplan.FromImage(img)
		if err != nil && err != imageplan.ErrNoImageLoader {
			return nil, nil, nil, err
		}
	}
	mode, _ := uf.ParseEraseMode(opts.EraseTail)
	imageplan.ApplyEraseDecision(rep, mode)
	rep.Loader = loader
	if loader != nil && loader.ChipCheck == "mismatch" {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"loader 与镜像 chip 不匹配（loader %s / 镜像 %s）：写入会在打开设备前被拒绝", loader.Chip, loader.ImageChip))
	}

	plan, err := imageplan.WriterPlan(rep, loader)
	if err != nil {
		return rep, loader, nil, err
	}
	return rep, loader, plan, nil
}

func factsOf(rep *imageplan.Report, loader *imageplan.EmbeddedLoader, opts ufOptions) ufFacts {
	facts := ufFacts{
		Chip:       rep.Container.Chip,
		Partitions: len(rep.Partitions),
		Items:      0,
		TotalBytes: rep.TotalBytes,
		EraseMode:  opts.EraseTail,
	}
	for _, it := range rep.Items {
		if it.Action == imageplan.ActionWrite && it.TargetKnown {
			facts.Items++
		}
	}
	var names []string
	for _, u := range rep.Uncovered {
		facts.UncoveredBytes += u.Bytes
		names = append(names, u.Partition)
	}
	facts.UncoveredTails = len(rep.Uncovered)
	facts.UncoveredParts = joinComma(names)
	if loader != nil {
		facts.HasLoader = true
		facts.LoaderSource = loader.Source
		facts.LoaderChipCheck = loader.ChipCheck
		facts.LoaderChip = loader.Chip
		facts.LoaderSHA256 = loader.SHA256
		facts.LoaderLength = loader.Length
		facts.LoaderBuildTime = loader.BuildTime
		facts.LoaderNonImage = loader.Source == imageplan.LoaderSourceFile
	}
	return facts
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

// --- bindings ---------------------------------------------------------------

// ufPlan(image, opts) resolves to the dry-run plan plus the loader identity.
func ufPlan(_ js.Value, args []js.Value) any {
	src, opts, err := ufArgs(args)
	if err != nil {
		return promiseReject(err)
	}
	return promise(func() ([]byte, error) {
		res := ufPlanResult{
			Image: ufImageInfo{Name: src.name, Size: src.size},
			Facts: ufFacts{EraseMode: opts.EraseTail},
		}
		rep, loader, plan, planErr := buildUfPlan(src, opts)
		if rep != nil {
			res.Report = rep
			res.Facts = factsOf(rep, loader, opts)
		}
		res.Loader = loader
		if planErr != nil {
			res.Kind = ufErrorKind(planErr)
			res.Error = planErr.Error()
		} else if plan != nil {
			res.OK = true
			if opts.LoaderBytes != nil {
				// Keep the report's loader block consistent with the source.
				res.Facts.LoaderNonImage = loader != nil && loader.Source == imageplan.LoaderSourceFile
			}
		}
		return json.Marshal(res)
	})
}

// ufRun(image, opts, onEvent) executes the state machine. opts.mock uses the
// in-wasm simulator; otherwise the bound device.Bridge is used (WebUSB).
func ufRun(_ js.Value, args []js.Value) any {
	src, opts, err := ufArgs(args)
	if err != nil {
		return promiseReject(err)
	}
	var onEvent js.Value
	if len(args) > 2 && args[2].Type() == js.TypeFunction {
		onEvent = args[2]
	}
	return promise(func() ([]byte, error) {
		res := ufRunResult{
			Image: ufImageInfo{Name: src.name, Size: src.size},
			Facts: ufFacts{EraseMode: opts.EraseTail},
		}
		rep, loader, plan, planErr := buildUfPlan(src, opts)
		if rep != nil {
			res.Facts = factsOf(rep, loader, opts)
		}
		res.Loader = loader
		if planErr != nil {
			res.Kind = ufErrorKind(planErr)
			res.Error = planErr.Error()
			return json.Marshal(res)
		}

		var emit func(kind string, payload any)
		var eventFn js.Func
		if onEvent.Type() == js.TypeFunction {
			eventFn = js.FuncOf(func(_ js.Value, cbArgs []js.Value) any {
				kind := ""
				if len(cbArgs) > 0 && cbArgs[0].Type() == js.TypeString {
					kind = cbArgs[0].String()
				}
				body := ""
				if len(cbArgs) > 1 && cbArgs[1].Type() == js.TypeString {
					body = cbArgs[1].String()
				}
				onEvent.Invoke(kind, body)
				return nil
			})
			defer eventFn.Release()
			emit = func(kind string, payload any) {
				data, err := json.Marshal(payload)
				if err != nil {
					return
				}
				eventFn.Invoke(kind, string(data))
			}
		}

		runOpts := uf.Options{
			Timeout:            opts.Timeout,
			SkipSignatureCheck: opts.Mock,
			SkipReadback:       opts.Mock,
			Retries:       opts.Retries,
			ChunkSize:     opts.ChunkSize,
			EraseTail:     mustEraseMode(opts.EraseTail),
			NoReset:       opts.NoReset,
			ReenumTimeout: opts.ReenumTimeout,
			Event: func(ev uf.Event) {
				if emit != nil {
					emit("event", ev)
				}
			},
			Progress: func(p uf.Progress) {
				if emit != nil {
					emit("progress", p)
				}
			},
		}

		var conn uf.Connector
		if opts.Mock {
			conn = uf.NewSimConnector(uf.SimConfig{
				Mode:         rockusb.ModeMaskrom,
				Chip:         plan.Chip,
				FlashSectors: simFlashSectors(plan),
			})
		} else {
			if bridgeJS.Type() == js.TypeUndefined {
				res.Kind = "device"
				res.Error = "USB 桥接还没有绑定：先点“授权设备”"
				return json.Marshal(res)
			}
			conn = &uf.BridgeConnector{
				Bridge:       &jsBridge{bridge: bridgeJS},
				Handle:       opts.DeviceHandle,
				MaskromFirst: true,
			}
		}

		ctx, cancel := newUfContext()
		defer cancel()
		out, runErr := uf.Run(ctx, plan, src, conn, runOpts)
		res.Run = out
		if out != nil && res.Loader != nil {
			// The report records what was actually sent; the page's loader
			// block keeps the plan's identity (same blob, richer fields).
			res.Facts.LoaderSHA256 = out.Loader.SHA256
			res.Facts.LoaderChipCheck = out.Loader.ChipCheck
			res.Facts.LoaderBuildTime = out.Loader.BuildTime
			res.Facts.LoaderSource = out.Loader.Source
			res.Facts.LoaderNonImage = out.Loader.NonImage
		}
		if runErr != nil {
			res.Kind = "run"
			res.Error = runErr.Error()
			return json.Marshal(res)
		}
		res.OK = true
		return json.Marshal(res)
	})
}

// imageLoader(image) resolves to the loader bytes the image carries, so the
// M1 `db` command can use the image's own loader instead of a file picker.
func imageLoader(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return promiseReject(fmt.Errorf("imageLoader: image object is required"))
	}
	src, err := imageSourceFromJS(args[0])
	if err != nil {
		return promiseReject(err)
	}
	return promise(func() ([]byte, error) {
		img, err := rkfw.OpenReaderAt(src, src.size, src.name)
		if err != nil {
			return nil, fmt.Errorf("解析镜像: %w", err)
		}
		loader, err := imageplan.FromImage(img)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(map[string]any{
			"source":    loader.Source,
			"name":      loader.Name,
			"sha256":    loader.SHA256,
			"length":    loader.Length,
			"version":   loader.Version,
			"buildTime": loader.BuildTime,
			"chip":      loader.Chip,
			"chipCheck": loader.ChipCheck,
			"imageChip": loader.ImageChip,
			"summary":   loader.Summary(),
			"bytes":     bytesToJSBase64(loader.Bytes),
		})
		if err != nil {
			return nil, err
		}
		return data, nil
	})
}

// demoImage() resolves to the synthetic RKFW image the page's demo mode uses.
// It is built in Go so the demo exercises the real parser and planner.
func demoImage(_ js.Value, _ []js.Value) any {
	return promise(func() ([]byte, error) {
		return json.Marshal(map[string]any{
			"name":  imageplan.DemoImageName,
			"bytes": bytesToJSBase64(imageplan.DemoImage()),
		})
	})
}

// ufCancel stops the run in flight; the state machine reports ERR_CANCELLED.
func ufCancel(_ js.Value, _ []js.Value) any {
	ok := cancelUf()
	return promise(func() ([]byte, error) {
		return json.Marshal(map[string]bool{"ok": ok})
	})
}

// --- helpers ----------------------------------------------------------------

var (
	ufMu       sync.Mutex
	ufCancelFn context.CancelFunc
)

func newUfContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ufMu.Lock()
	ufCancelFn = cancel
	ufMu.Unlock()
	return ctx, func() {
		ufMu.Lock()
		if ufCancelFn != nil {
			ufCancelFn = nil
		}
		ufMu.Unlock()
		cancel()
	}
}

func cancelUf() bool {
	ufMu.Lock()
	defer ufMu.Unlock()
	if ufCancelFn == nil {
		return false
	}
	ufCancelFn()
	ufCancelFn = nil
	return true
}

func mustEraseMode(s string) uf.EraseMode {
	mode, err := uf.ParseEraseMode(s)
	if err != nil {
		return uf.EraseNone
	}
	return mode
}

// ufErrorKind labels a planning failure for the page.
func ufErrorKind(err error) string {
	if err == nil {
		return ""
	}
	return "plan"
}

// simFlashSectors sizes the in-wasm simulation so a plan fits, mirroring
// cmd/uf's mock sizing.
func simFlashSectors(plan *uf.Plan) uint32 {
	if plan.DiskKnown && plan.DiskSectors > 0 && plan.DiskSectors <= 1<<32-1 {
		return uint32(plan.DiskSectors)
	}
	var end uint64
	if plan.IDB.Bytes > 0 {
		end = uint64(plan.IDB.LBASector) + uint64(plan.IDB.Bytes/uf.SectorSize)
	}
	for _, it := range plan.Items {
		if e := uint64(it.TargetLBA) + uint64(it.PaddedSize/uf.SectorSize); e > end {
			end = e
		}
	}
	for _, t := range plan.Tails {
		if e := t.FirstLBA + uint64(t.Bytes/uf.SectorSize); e > end {
			end = e
		}
	}
	end += 4096
	if end > 1<<32-1 {
		end = 1<<32 - 1
	}
	return uint32(end)
}

// bytesToJSBase64 encodes a blob for the JSON envelope. The loader is ~400 KiB
// and the image source is read lazily, so this is the only place a whole blob
// crosses as text; it keeps the API one JSON object per call.
func bytesToJSBase64(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

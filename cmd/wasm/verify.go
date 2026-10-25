//go:build js && wasm

package main

// Image verification for the page's "校验" step.
//
// The rules are not re-implemented here: rkfw parses the container, parameter
// parses the partition table, imageplan builds the same report the CLI's
// dry-run prints, and rkfw.VerifyRKAFCRC / rkfw.VerifyMD5 do the two streaming
// checksums. This file only adds what a browser needs and a CLI does not:
//
//   - progress. A full MD5 scan reads several GiB; the page has to show
//     "已校验 X / 总 Y" and an ETA, so every read the checksum makes through the
//     image source is counted and throttled into a progress event.
//   - a structure report the page can render without knowing the format:
//     container, item table, partitions, uncovered tails, payload audit.
//
// The scan is deliberately not done twice: the same ReaderAt is wrapped once,
// and the phase label is switched before each stage, so both the RKAF CRC scan
// and the MD5 scan report their own bytes.

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"syscall/js"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/imageplan"
	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
)

// verifyProgressQuiet is the minimum interval between two progress events.
// 250 ms keeps the UI at four updates a second without flooding postMessage
// while a 4 MiB read loop runs at ~200 reads/s.
const verifyProgressQuiet = 250 * time.Millisecond

// progressSource counts every byte the core reads and reports it for the
// current phase. Reads are sequential during a checksum scan, so the counter
// is the number of image bytes processed so far.
type progressSource struct {
	src   *jsImageSource
	emit  func(phase string, done, total int64)
	mu    sync.Mutex
	phase string
	total int64
	done  int64
	last  time.Time
}

func (p *progressSource) ReadAt(buf []byte, off int64) (int, error) {
	n, err := p.src.ReadAt(buf, off)

	p.mu.Lock()
	p.done += int64(n)
	phase, done, total := p.phase, p.done, p.total
	due := time.Since(p.last) >= verifyProgressQuiet
	if due {
		p.last = time.Now()
	}
	p.mu.Unlock()

	if due && p.emit != nil {
		p.emit(phase, done, total)
	}
	return n, err
}

// begin starts a phase with a known total (0 means "unknown").
func (p *progressSource) begin(phase string, total int64) {
	p.mu.Lock()
	p.phase = phase
	p.total = total
	p.done = 0
	p.last = time.Time{}
	p.mu.Unlock()
	if p.emit != nil {
		p.emit(phase, 0, total)
	}
}

func (p *progressSource) finish() {
	p.mu.Lock()
	phase, done, total := p.phase, p.done, p.total
	p.mu.Unlock()
	if p.emit != nil {
		p.emit(phase, done, total)
	}
}

// --- result shape -----------------------------------------------------------

type verifyPhase struct {
	Phase string  `json:"phase"`
	Bytes int64   `json:"bytes"`
	Ms    float64 `json:"ms"`
}

type verifySample struct {
	Bytes     int64   `json:"bytes"`
	Ms        float64 `json:"ms"`
	MiBPerSec float64 `json:"mibPerSec"`
}

type verifyResult struct {
	// OK means the image parsed and a report could be built. The checksum
	// outcome is reported separately: the page decides what a mismatch means,
	// exactly like the CLI does.
	OK        bool                      `json:"ok"`
	Image     ufImageInfo               `json:"image"`
	Full      bool                      `json:"full"`
	Report    *imageplan.Report         `json:"report,omitempty"`
	Parameter *rkfw.CRCResult           `json:"parameterCrc,omitempty"`
	RKAFCRC   *rkfw.CRCResult           `json:"rkafCrc,omitempty"`
	MD5       *rkfw.MD5Result           `json:"md5,omitempty"`
	Sample    *verifySample             `json:"sample,omitempty"`
	Phases    []verifyPhase             `json:"phases"`
	Duration  float64                   `json:"durationMs"`
	Error     string                    `json:"error,omitempty"`
	Kind      string                    `json:"kind,omitempty"`
	Loader    *imageplan.EmbeddedLoader `json:"loader,omitempty"`
}

// ufVerify(image, opts, onEvent) resolves to the structure report plus the
// checksum measurements. opts.full=false stops after the parameter CRC, which
// is the "quick" verdict: seconds, no full-file scan.
func ufVerify(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return promiseReject(fmt.Errorf("ufVerify: image object is required"))
	}
	src, err := imageSourceFromJS(args[0])
	if err != nil {
		return promiseReject(err)
	}
	full := false
	sampleMiB := 0
	if len(args) > 1 && args[1].Type() == js.TypeObject {
		opts := args[1]
		full = opts.Get("full").Truthy()
		if v := opts.Get("sampleMiB"); v.Type() == js.TypeNumber {
			sampleMiB = v.Int()
		}
	}
	var onEvent js.Value
	if len(args) > 2 && args[2].Type() == js.TypeFunction {
		onEvent = args[2]
	}

	return promise(func() ([]byte, error) {
		res := verifyResult{
			Image: ufImageInfo{Name: src.name, Size: src.size},
			Full:  full,
			Kind:  "verify",
		}
		started := time.Now()

		var eventFn js.Func
		emit := func(phase string, done, total int64) {}
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
			emit = func(phase string, done, total int64) {
				payload := map[string]any{
					"phase":      phase,
					"doneBytes":  done,
					"totalBytes": total,
				}
				if total > 0 {
					payload["percent"] = float64(done) / float64(total) * 100
				}
				data, err := json.Marshal(payload)
				if err != nil {
					return
				}
				eventFn.Invoke("verify-progress", string(data))
			}
		}

		prog := &progressSource{src: src, emit: emit}

		prog.begin("structure", 0)
		img, err := rkfw.OpenReaderAt(prog, src.size, src.name)
		if err != nil {
			res.Error = fmt.Sprintf("解析镜像: %v", err)
			return json.Marshal(res)
		}
		if img.Parameter == nil {
			res.Error = "镜像没有 parameter 分区，无法建立写入计划"
			return json.Marshal(res)
		}
		raw, err := img.ReadItem(*img.Parameter, imageplan.MaxParameterBytes)
		if err != nil {
			res.Error = fmt.Sprintf("读取 parameter: %v", err)
			return json.Marshal(res)
		}
		doc, err := parameter.Parse(raw)
		if err != nil {
			res.Error = fmt.Sprintf("解析 parameter: %v", err)
			return json.Marshal(res)
		}
		rep, err := imageplan.BuildReport(img, doc, imageplan.VerificationInfo{}, imageplan.Options{})
		if err != nil {
			res.Error = fmt.Sprintf("构建写入计划: %v", err)
			return json.Marshal(res)
		}
		prog.finish()
		res.OK = true
		res.Report = rep
		res.Phases = append(res.Phases, verifyPhase{Phase: "structure", Bytes: prog.snapshot(), Ms: sinceMs(started)})

		// PARM CRC: the parameter partition is a few hundred bytes, so it is
		// part of both the quick and the full verdict.
		parmStart := time.Now()
		prog.begin("parameter", 0)
		if doc.CRCAvailable {
			res.Parameter = &rkfw.CRCResult{Stored: doc.CRCStored, Computed: doc.CRCComputed, OK: doc.CRCOK}
		}
		prog.finish()
		res.Phases = append(res.Phases, verifyPhase{Phase: "parameter", Bytes: 0, Ms: sinceMs(parmStart)})

		// Throughput sample: hash the first few MiB so the page can decide
		// between the full scan and the quick verdict from a measurement of
		// this machine and this image, not from a hard-coded guess.
		if sampleMiB > 0 {
			want := int64(sampleMiB) << 20
			if want > img.Size {
				want = img.Size
			}
			sampleStart := time.Now()
			prog.begin("sample", want)
			h := md5.New()
			buf := make([]byte, 4<<20)
			n, err := io.CopyBuffer(h, io.NewSectionReader(prog, 0, want), buf)
			prog.finish()
			if err != nil {
				res.Error = fmt.Sprintf("校验测速样本: %v", err)
				res.Kind = "verify"
				return json.Marshal(res)
			}
			ms := sinceMs(sampleStart)
			rate := 0.0
			if ms > 0 {
				rate = float64(n) / (1 << 20) / (ms / 1000)
			}
			res.Sample = &verifySample{Bytes: n, Ms: ms, MiBPerSec: rate}
			res.Phases = append(res.Phases, verifyPhase{Phase: "sample", Bytes: n, Ms: ms})
		}

		if full {
			// RKAF CRC: the archive body, ~4 GiB, streamed.
			crcStart := time.Now()
			prog.begin("rkaf-crc", img.RKAFLength)
			crc, err := img.VerifyRKAFCRC()
			prog.finish()
			if err != nil {
				res.Error = fmt.Sprintf("校验 RKAF CRC: %v", err)
				res.Kind = "verify"
				return json.Marshal(res)
			}
			res.RKAFCRC = &crc
			res.Phases = append(res.Phases, verifyPhase{Phase: "rkaf-crc", Bytes: img.RKAFLength, Ms: sinceMs(crcStart)})

			// Trailing MD5: every byte up to the digest, plus the tail read.
			md5Start := time.Now()
			prog.begin("md5", img.Size)
			sum, err := img.VerifyMD5()
			prog.finish()
			if err != nil {
				res.Error = fmt.Sprintf("校验整镜像 MD5: %v", err)
				res.Kind = "verify"
				return json.Marshal(res)
			}
			res.MD5 = &sum
			res.Phases = append(res.Phases, verifyPhase{Phase: "md5", Bytes: img.Size, Ms: sinceMs(md5Start)})
		}

		// Loader identity is cheap to add here and keeps block 4's summary
		// honest: the write uses this blob, not a file picked later.
		if loader, err := imageplan.FromImage(img); err == nil {
			res.Loader = loader
		}
		res.Duration = sinceMs(started)
		return json.Marshal(res)
	})
}

func sinceMs(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

// snapshot returns the bytes read so far (0 is fine: the page only needs the
// final number for the structure phase).
func (p *progressSource) snapshot() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

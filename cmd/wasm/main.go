//go:build js && wasm

// Command wasm is the Go core of the M1 page: the rockusb protocol, the
// Download Boot path and the CLI-style shell, compiled to WebAssembly and
// driven from a Web Worker.
//
// The Go side never touches navigator.usb directly — WebUSB is Window-only and
// requestDevice() needs a user gesture, so the page's main thread owns every
// device transfer. Go calls into a small JavaScript bridge (bound from the
// worker) and every call returns a Promise; awaiting it parks the calling
// goroutine and gives the event loop back to JavaScript.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall/js"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/device"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

var (
	shell    *device.Shell
	bridgeJS js.Value
	keep     []js.Func // js.Func values must outlive their registration
)

func main() {
	api := js.Global().Get("Object").New()
	set := func(name string, fn js.Func) {
		keep = append(keep, fn)
		api.Set(name, fn)
	}
	set("bindBridge", js.FuncOf(bindBridge))
	set("exec", js.FuncOf(exec))
	set("inspectLoader", js.FuncOf(inspectLoader))
	// M3b: the write path (internal/uf) bound to the same WebUSB bridge.
	set("ufPlan", js.FuncOf(ufPlan))
	set("ufRun", js.FuncOf(ufRun))
	set("ufCancel", js.FuncOf(ufCancel))
	set("imageLoader", js.FuncOf(imageLoader))
	set("demoImage", js.FuncOf(demoImage))
	set("close", js.FuncOf(closeDevice))
	api.Set("version", device.CoreVersion)
	js.Global().Set("rk3588", api)

	// Keep the runtime alive for the lifetime of the worker.
	select {}
}

// bindBridge(bridge, options) installs the USB bridge. bridge must expose
// list(), open(devNo), close(devNo), controlOut(...), bulkOut(...), bulkIn(...)
// and reset(...); each returns a Promise.
func bindBridge(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return promiseReject(fmt.Errorf("bindBridge: bridge object is required"))
	}
	bridgeJS = args[0]

	cfg := device.Config{Timeout: rockusb.DefaultTimeout, ReadRetries: 1}
	if len(args) > 1 && args[1].Type() == js.TypeObject {
		opts := args[1]
		if v := opts.Get("timeoutMs"); v.Type() == js.TypeNumber {
			cfg.Timeout = millis(int64(v.Int()))
		}
		if v := opts.Get("readRetries"); v.Type() == js.TypeNumber {
			cfg.ReadRetries = v.Int()
		}
	}
	shell = device.New(&jsBridge{bridge: bridgeJS}, cfg)
	return js.Undefined()
}

func millis(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

// exec(line, loaderBytes?, loaderName?) resolves to the shell's Result as JSON.
func exec(_ js.Value, args []js.Value) any {
	if shell == nil {
		return promiseReject(fmt.Errorf("exec: bridge is not bound yet"))
	}
	if len(args) == 0 || args[0].Type() != js.TypeString {
		return promiseReject(fmt.Errorf("exec: command line must be a string"))
	}
	line := args[0].String()
	var loader []byte
	if len(args) > 1 && args[1].Type() != js.TypeUndefined && args[1].Type() != js.TypeNull {
		loader = bytesFromJS(args[1])
	}
	name := ""
	if len(args) > 2 && args[2].Type() == js.TypeString {
		name = args[2].String()
	}

	return promise(func() ([]byte, error) {
		res := shell.Exec(line, loader, name)
		return json.Marshal(res)
	})
}

// inspectLoader(bytes) validates a loader file without touching the device, so
// the page can show the segment table right after the file picker closes.
func inspectLoader(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return promiseReject(fmt.Errorf("inspectLoader: loader bytes are required"))
	}
	data := bytesFromJS(args[0])
	return promise(func() ([]byte, error) {
		img, lines, err := rockusb.VerifyLoaderFile(data)
		payload := map[string]any{"ok": err == nil, "lines": lines}
		if err != nil {
			payload["error"] = err.Error()
			var lerr *rockusb.LoaderError
			if errors.As(err, &lerr) {
				payload["kind"] = lerr.Kind.String()
			}
		}
		if img != nil {
			payload["segments"] = img.Plan()
			payload["size"] = img.Size
			payload["chipType"] = img.ChipType
		}
		return json.Marshal(payload)
	})
}

// closeDevice releases any open interface (page unload / device disconnect).
func closeDevice(_ js.Value, _ []js.Value) any {
	if shell == nil {
		return js.Undefined()
	}
	return promise(func() ([]byte, error) {
		shell.Close()
		return []byte(`{"ok":true}`), nil
	})
}

// --- promise plumbing -------------------------------------------------------

// promise runs fn on its own goroutine and resolves a JS Promise with its JSON
// result. Running off the callback's stack is what lets fn park on a bridge
// promise: the executor returns immediately, Go's scheduler yields to the
// event loop, and the resolution arrives as a fresh JS callback.
func promise(fn func() ([]byte, error)) js.Value {
	ctor := js.Global().Get("Promise")
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve := args[0]
		reject := args[1]
		go func() {
			out, err := fn()
			if err != nil {
				reject.Invoke(err.Error())
				return
			}
			resolve.Invoke(string(out))
		}()
		return nil
	})
	keep = append(keep, executor)
	return ctor.New(executor)
}

func promiseReject(err error) js.Value {
	ctor := js.Global().Get("Promise")
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		args[1].Invoke(err.Error())
		return nil
	})
	keep = append(keep, executor)
	return ctor.New(executor)
}

// --- byte marshalling -------------------------------------------------------

func bytesToJS(b []byte) js.Value {
	u8 := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(u8, b)
	return u8
}

func bytesFromJS(v js.Value) []byte {
	if v.Type() != js.TypeObject {
		return nil
	}
	byteLength := v.Get("byteLength")
	if byteLength.Type() != js.TypeNumber {
		return nil
	}
	n := byteLength.Int()
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	js.CopyBytesToGo(out, v)
	return out
}

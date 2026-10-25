//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/SilenWang/rk3588-webflasher/internal/device"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// jsBridge implements device.Bridge on top of the worker-side JavaScript
// bridge. Structured results travel as JSON strings and bytes as Uint8Array;
// the page's layout of the RPC messages is documented in web/app/worker.js.
type jsBridge struct {
	bridge js.Value
}

type listedDevice struct {
	Devices []device.RawInfo `json:"devices"`
}

func (b *jsBridge) List() ([]device.RawInfo, error) {
	value, err := awaitPromise(b.call("list"))
	if err != nil {
		return nil, err
	}
	text, err := jsonArg(value)
	if err != nil {
		return nil, err
	}
	var payload listedDevice
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	return payload.Devices, nil
}

func (b *jsBridge) Open(devNo int, handle string) (rockusb.Transport, error) {
	if _, err := awaitPromise(b.call("open", devNo, handle)); err != nil {
		return nil, err
	}
	return &jsTransport{bridge: b.bridge, devNo: devNo}, nil
}

func (b *jsBridge) Close(devNo int) error {
	_, err := awaitPromise(b.call("close", devNo))
	return err
}

func (b *jsBridge) call(method string, args ...any) js.Value {
	return b.bridge.Call(method, args...)
}

// jsTransport forwards rockusb.Transport calls to the page's main thread.
type jsTransport struct {
	bridge js.Value
	devNo  int
}

func (t *jsTransport) call(method string, args ...any) js.Value {
	values := make([]any, 0, len(args)+1)
	values = append(values, t.devNo)
	values = append(values, args...)
	return t.bridge.Call(method, values...)
}

func (t *jsTransport) ControlOut(requestType, request, value, index uint16, data []byte) (int, error) {
	res, err := awaitPromise(t.call("controlOut", int(requestType), int(request), int(value), int(index), bytesToJS(data)))
	if err != nil {
		return 0, err
	}
	return bytesWritten(res)
}

func (t *jsTransport) BulkOut(endpoint uint8, data []byte) (int, error) {
	res, err := awaitPromise(t.call("bulkOut", int(endpoint), bytesToJS(data)))
	if err != nil {
		return 0, err
	}
	return bytesWritten(res)
}

func (t *jsTransport) BulkIn(endpoint uint8, length int) ([]byte, error) {
	res, err := awaitPromise(t.call("bulkIn", int(endpoint), length))
	if err != nil {
		return nil, err
	}
	obj, err := asObject(res)
	if err != nil {
		return nil, err
	}
	if msg := obj.Get("error"); msg.Type() == js.TypeString && msg.String() != "" {
		return nil, fmt.Errorf("%s", msg.String())
	}
	return bytesFromJS(obj.Get("data")), nil
}

func (t *jsTransport) Reset() error {
	_, err := awaitPromise(t.call("reset"))
	return err
}

func bytesWritten(v js.Value) (int, error) {
	obj, err := asObject(v)
	if err != nil {
		return 0, err
	}
	if msg := obj.Get("error"); msg.Type() == js.TypeString && msg.String() != "" {
		return 0, fmt.Errorf("%s", msg.String())
	}
	n := obj.Get("bytesWritten")
	if n.Type() != js.TypeNumber {
		return 0, fmt.Errorf("bridge returned no bytesWritten")
	}
	return n.Int(), nil
}

func asObject(v js.Value) (js.Value, error) {
	if v.Type() != js.TypeObject {
		return js.Value{}, fmt.Errorf("bridge returned %s, want an object", v.Type().String())
	}
	return v, nil
}

func jsonArg(v js.Value) (string, error) {
	if v.Type() != js.TypeString {
		return "", fmt.Errorf("bridge returned %s, want a JSON string", v.Type().String())
	}
	return v.String(), nil
}

// awaitPromise parks the calling goroutine until the JavaScript promise
// settles. Go's wasm scheduler hands control back to the event loop while the
// goroutine is blocked, which is what makes synchronous-looking protocol code
// possible on top of an async transport.
func awaitPromise(p js.Value) (js.Value, error) {
	type outcome struct {
		value js.Value
		err   error
	}
	done := make(chan outcome, 1)

	var onValue, onError js.Func
	onValue = js.FuncOf(func(_ js.Value, args []js.Value) any {
		var v js.Value
		if len(args) > 0 {
			v = args[0]
		}
		done <- outcome{value: v}
		return nil
	})
	onError = js.FuncOf(func(_ js.Value, args []js.Value) any {
		msg := "bridge call failed"
		if len(args) > 0 {
			if e := args[0]; e.Type() == js.TypeObject {
				if m := e.Get("message"); m.Type() == js.TypeString {
					msg = m.String()
				}
			} else if e.Type() == js.TypeString {
				msg = e.String()
			}
		}
		done <- outcome{err: fmt.Errorf("%s", msg)}
		return nil
	})

	p.Call("then", onValue, onError)
	res := <-done
	onValue.Release()
	onError.Release()
	return res.value, res.err
}

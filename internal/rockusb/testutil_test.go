package rockusb

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
)

// mockTransport is the shared in-memory Transport for the protocol tests.
// Every hook may be replaced per test; the zero value accepts everything.
type mockTransport struct {
	mu sync.Mutex

	controlFn  func(requestType, request, value, index uint16, data []byte) (int, error)
	bulkOutFn  func(endpoint uint8, data []byte) (int, error)
	bulkInFn   func(endpoint uint8, length int) ([]byte, error)
	resetCount int

	// calls records what actually reached the "device".
	bulkOuts []bulkOut
	controls []controlOut
}

type bulkOut struct {
	endpoint uint8
	data     []byte
}

type controlOut struct {
	requestType, request, value, index uint16
	length                             int
}

func (m *mockTransport) ControlOut(requestType, request, value, index uint16, data []byte) (int, error) {
	m.mu.Lock()
	m.controls = append(m.controls, controlOut{requestType, request, value, index, len(data)})
	fn := m.controlFn
	m.mu.Unlock()
	if fn != nil {
		return fn(requestType, request, value, index, data)
	}
	return len(data), nil
}

func (m *mockTransport) BulkOut(endpoint uint8, data []byte) (int, error) {
	m.mu.Lock()
	cp := append([]byte(nil), data...)
	m.bulkOuts = append(m.bulkOuts, bulkOut{endpoint, cp})
	fn := m.bulkOutFn
	m.mu.Unlock()
	if fn != nil {
		return fn(endpoint, data)
	}
	return len(data), nil
}

func (m *mockTransport) BulkIn(endpoint uint8, length int) ([]byte, error) {
	m.mu.Lock()
	fn := m.bulkInFn
	m.mu.Unlock()
	if fn != nil {
		return fn(endpoint, length)
	}
	return make([]byte, length), nil
}

func (m *mockTransport) Reset() error {
	m.mu.Lock()
	m.resetCount++
	m.mu.Unlock()
	return nil
}

func (m *mockTransport) resets() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resetCount
}

func (m *mockTransport) outbound() []bulkOut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bulkOut(nil), m.bulkOuts...)
}

func (m *mockTransport) controlOuts() []controlOut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]controlOut(nil), m.controls...)
}

// scriptedDevice is a fake LOADER-mode rockusb device: it answers a CBW with
// the canned data for that opcode and a matching CSW.
type scriptedDevice struct {
	mockTransport

	data map[Op][]byte
	csw  func(tag uint32, op Op) []byte

	pendingOp  Op
	pendingTag uint32
	pendingLen uint32
	sentData   bool
}

func newScriptedDevice(data map[Op][]byte) *scriptedDevice {
	d := &scriptedDevice{data: data}
	d.bulkOutFn = func(_ uint8, buf []byte) (int, error) {
		if len(buf) != CBWLength {
			return 0, fmt.Errorf("unexpected bulk OUT of %d bytes", len(buf))
		}
		d.pendingOp = Op(buf[15])
		d.pendingTag = binary.LittleEndian.Uint32(buf[4:8])
		d.pendingLen = binary.LittleEndian.Uint32(buf[8:12])
		d.sentData = false
		return len(buf), nil
	}
	d.bulkInFn = func(_ uint8, length int) ([]byte, error) {
		if d.pendingLen > 0 && !d.sentData {
			d.sentData = true
			out := make([]byte, length)
			copy(out, d.data[d.pendingOp])
			return out, nil
		}
		return d.cswFor(d.pendingTag, d.pendingOp), nil
	}
	return d
}

func (d *scriptedDevice) cswFor(tag uint32, op Op) []byte {
	if d.csw != nil {
		return d.csw(tag, op)
	}
	b := make([]byte, CSWLength)
	binary.LittleEndian.PutUint32(b[0:4], CSWSignature)
	binary.LittleEndian.PutUint32(b[4:8], tag)
	b[12] = StatusPassed
	return b
}

// testSegment is one loader entry in the RKBOOT fixture.
type testSegment struct {
	request uint16
	name    string
	data    []byte
	delay   uint32
}

// buildLoader assembles an RKBOOT container the way Rockchip's boot_merger
// writes one: packed header, packed entry tables, segment payloads, trailing
// CRC-32 of everything before it.
func buildLoader(t *testing.T, chipType uint32, tag uint32, segments []testSegment) []byte {
	t.Helper()

	var count471, count472 int
	for _, seg := range segments {
		switch seg.request {
		case RequestIndex471:
			count471++
		case RequestIndex472:
			count472++
		default:
			t.Fatalf("unsupported segment request 0x%04X in fixture", seg.request)
		}
	}

	offset471 := uint32(LoaderHeaderSize)
	offset472 := offset471 + uint32(count471*LoaderEntrySize)
	dataOffset := offset472 + uint32(count472*LoaderEntrySize)

	total := int(dataOffset)
	for _, seg := range segments {
		total += len(seg.data)
	}
	buf := make([]byte, total)

	binary.LittleEndian.PutUint32(buf[0:4], tag)
	binary.LittleEndian.PutUint16(buf[4:6], LoaderHeaderSize)
	binary.LittleEndian.PutUint32(buf[6:10], 0x00010006)
	binary.LittleEndian.PutUint32(buf[10:14], 0x01030000)
	binary.LittleEndian.PutUint32(buf[21:25], chipType)
	buf[25] = byte(count471)
	binary.LittleEndian.PutUint32(buf[26:30], offset471)
	buf[30] = LoaderEntrySize
	buf[31] = byte(count472)
	binary.LittleEndian.PutUint32(buf[32:36], offset472)
	buf[36] = LoaderEntrySize
	buf[43] = 'N'

	seen471, seen472 := 0, 0
	dataPos := int(dataOffset)
	for _, seg := range segments {
		var slot int
		var entryType uint32
		if seg.request == RequestIndex471 {
			slot = int(offset471) + seen471*LoaderEntrySize
			seen471++
			entryType = DBEntry471
		} else {
			slot = int(offset472) + seen472*LoaderEntrySize
			seen472++
			entryType = DBEntry472
		}
		buf[slot] = LoaderEntrySize
		binary.LittleEndian.PutUint32(buf[slot+1:slot+5], entryType)
		for i, r := range seg.name {
			if i >= 20 {
				break
			}
			binary.LittleEndian.PutUint16(buf[slot+5+i*2:], uint16(r))
		}
		binary.LittleEndian.PutUint32(buf[slot+45:slot+49], uint32(dataPos))
		binary.LittleEndian.PutUint32(buf[slot+49:slot+53], uint32(len(seg.data)))
		binary.LittleEndian.PutUint32(buf[slot+53:slot+57], seg.delay)
		copy(buf[dataPos:], seg.data)
		dataPos += len(seg.data)
	}

	binary.LittleEndian.PutUint32(buf[len(buf)-4:], CRC32(buf[:len(buf)-4]))
	return buf
}

// rk3588Loader mirrors the segment sizes stage 2 measured on the real
// rk3588_spl_loader_v1.06.111.bin: UsbHead 2048 B plus DDR 51200 B on 0x0471,
// usbplug 88064 B on 0x0472.
func rk3588Loader(t *testing.T) []byte {
	t.Helper()
	head := make([]byte, 2048)
	ddr := make([]byte, 51200)
	usbplug := make([]byte, 88064)
	for i := range head {
		head[i] = byte(i)
	}
	for i := range ddr {
		ddr[i] = byte(i * 3)
	}
	for i := range usbplug {
		usbplug[i] = byte(i * 7)
	}
	return buildLoader(t, RK3588ChipTag, LoaderTagBoot, []testSegment{
		{request: RequestIndex471, name: "UsbHead", data: head},
		{request: RequestIndex471, name: "rk3588_ddr_lp4_2112M", data: ddr},
		{request: RequestIndex472, name: "rk3588_usbplug_v1", data: usbplug},
	})
}

package uf

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// The fixtures in this file are synthetic: no Fydetab Duo byte is hard-coded,
// so a wrong assumption here shows up as a failing test rather than as a
// plausible-looking number.

// memSource answers ReadAt from a byte slice and can be marked truncated.
type memSource struct {
	data []byte
}

func (m *memSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(m.data)) {
		return 0, fmt.Errorf("read at 0x%x past the %d-byte source", off, len(m.data))
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, fmt.Errorf("short source")
	}
	return n, nil
}

// loaderLen is the RKBOOT container size of the fixture loader.
func rkbootLoader(t *testing.T) []byte {
	t.Helper()
	head := make([]byte, 2048)
	plug := make([]byte, 1024)
	for i := range head {
		head[i] = byte(i * 3)
	}
	for i := range plug {
		plug[i] = byte(i * 7)
	}

	const (
		header  = 102
		entry   = 57
		entryN  = 2
		dataOff = header + entry*entryN
	)
	total := dataOff + len(head) + len(plug)
	buf := make([]byte, total)

	binary.LittleEndian.PutUint32(buf[0:4], rockusb.LoaderTagBoot)
	binary.LittleEndian.PutUint16(buf[4:6], header)
	binary.LittleEndian.PutUint32(buf[6:10], 0x00010006)
	binary.LittleEndian.PutUint32(buf[10:14], 0x01030000)
	binary.LittleEndian.PutUint32(buf[21:25], rockusb.RK3588ChipTag)
	buf[25] = 1 // 471 entries
	binary.LittleEndian.PutUint32(buf[26:30], header)
	buf[30] = entry
	buf[31] = 1 // 472 entries
	binary.LittleEndian.PutUint32(buf[32:36], header+entry)
	buf[36] = entry
	buf[43] = 'N'

	writeEntry := func(slot int, typ uint32, name string, off, size int) {
		buf[slot] = entry
		binary.LittleEndian.PutUint32(buf[slot+1:slot+5], typ)
		for i, r := range name {
			if i >= 20 {
				break
			}
			binary.LittleEndian.PutUint16(buf[slot+5+i*2:], uint16(r))
		}
		binary.LittleEndian.PutUint32(buf[slot+45:slot+49], uint32(off))
		binary.LittleEndian.PutUint32(buf[slot+49:slot+53], uint32(size))
	}
	writeEntry(header, rockusb.DBEntry471, "UsbHead", dataOff, len(head))
	writeEntry(header+entry, rockusb.DBEntry472, "rk3588_usbplug", dataOff+len(head), len(plug))
	copy(buf[dataOff:], head)
	copy(buf[dataOff+len(head):], plug)
	binary.LittleEndian.PutUint32(buf[total-4:], rockusb.CRC32(buf[:total-4]))
	return buf
}

// testLayout describes the synthetic image the runner tests read.
type testLayout struct {
	loader       []byte
	loaderOff    int64
	bootOff      int64
	bootBytes    int64
	stateOff     int64
	stateBytes   int64
	source       []byte
	sourceLength int64
}

func newTestLayout(t *testing.T) *testLayout {
	t.Helper()
	loader := rkbootLoader(t)
	l := &testLayout{
		loader:     loader,
		loaderOff:  0,
		bootOff:    0x1000,
		bootBytes:  4096,
		stateOff:   0x2000,
		stateBytes: 8192,
	}
	total := int(l.stateOff + l.stateBytes)
	l.source = make([]byte, total)
	copy(l.source[l.loaderOff:], loader)
	for i := int(l.bootOff); i < int(l.bootOff+l.bootBytes); i++ {
		l.source[i] = byte(i * 7)
	}
	for i := int(l.stateOff); i < int(l.stateOff+l.stateBytes); i++ {
		l.source[i] = byte(i * 11)
	}
	l.sourceLength = int64(total)
	return l
}

// plan builds the write plan the tests execute.
func (l *testLayout) plan(t *testing.T) *Plan {
	t.Helper()
	idbBytes := alignUp(int64(len(l.loader)), IDBBlockSize)
	p := &Plan{
		Image:  "synthetic-update.img",
		Chip:   "RK3588",
		Loader: &LoaderPlan{FileOffset: l.loaderOff, Length: int64(len(l.loader))},
		IDB:    IDBPlan{LBASector: IDBDefaultLBA, Bytes: idbBytes},
		Items: []Item{
			{Name: "boot", Partition: "boot", FileOffset: l.bootOff, Size: l.bootBytes, PaddedSize: l.bootBytes, TargetLBA: 0x1000},
			{Name: "state", Partition: "state", FileOffset: l.stateOff, Size: l.stateBytes, PaddedSize: l.stateBytes, TargetLBA: 0x2000},
		},
		Tails: []Tail{
			{Partition: "state", FirstLBA: 0x2000 + uint64(l.stateBytes)/SectorSize, Bytes: 4096},
		},
		DiskSectors: 0x10000,
		DiskKnown:   true,
	}
	return p
}

func testOptions() Options {
	return Options{
		Timeout:   time.Second,
		Retries:   0,
		ChunkSize: MinChunkSize,
		Sleep:     func(time.Duration) {},
	}
}

func testSimConfig(plan *Plan) SimConfig {
	return SimConfig{
		Chip:         plan.Chip,
		FlashSectors: uint32(plan.DiskSectors),
	}
}

// progressLog captures the progress stream.
type progressLog struct {
	events []Progress
}

func (p *progressLog) record(pr Progress) { p.events = append(p.events, pr) }

func (p *progressLog) last() Progress {
	if len(p.events) == 0 {
		return Progress{}
	}
	return p.events[len(p.events)-1]
}

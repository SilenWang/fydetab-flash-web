package uf

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SilenWang/fydetab-flash-web/internal/parameter"
	"github.com/SilenWang/fydetab-flash-web/internal/rkcrc"
	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
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
	data := make([]byte, 1024)
	boot := make([]byte, 512)
	for i := range head {
		head[i] = byte(i * 3)
	}
	copy(head, "RKNS") // the real FlashHead starts with the New IDB magic
	for i := range plug {
		plug[i] = byte(i * 7)
	}
	for i := range data {
		data[i] = byte(i * 11)
	}
	for i := range boot {
		boot[i] = byte(i * 13)
	}

	const (
		header  = 102
		entry   = 57
		entryN  = 5 // 0x0471, 0x0472, then three ENTRYLOADER segments
		dataOff = header + entry*entryN
	)
	total := dataOff + len(head) + len(plug) + len(data) + len(boot)
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
	// ENTRYLOADER: FlashHead / FlashData / FlashBoot.
	buf[37] = 3
	binary.LittleEndian.PutUint32(buf[38:42], header+entry*2)
	buf[42] = entry
	buf[43] = 'N'
	buf[44] = 0 // fixture stores plaintext FlashHead; the real loader sets this to 1

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
	loadOff := dataOff + len(head) + len(plug)
	writeEntry(header+entry*2, rockusb.DBEntryLoader, "FlashHead", dataOff, len(head))
	writeEntry(header+entry*3, rockusb.DBEntryLoader, "FlashData", loadOff, len(data))
	writeEntry(header+entry*4, rockusb.DBEntryLoader, "FlashBoot", loadOff+len(data), len(boot))
	copy(buf[dataOff:], head)
	copy(buf[dataOff+len(head):], plug)
	copy(buf[loadOff:], data)
	copy(buf[loadOff+len(data):], boot)
	binary.LittleEndian.PutUint32(buf[total-4:], rockusb.CRC32(buf[:total-4]))
	return buf
}

// fixturePartitions is the partition table the fixture's parameter block and
// its GPT plan describe.
func fixturePartitions() []parameter.Partition {
	return []parameter.Partition{
		{Name: "boot", First: 0x1000, Sectors: 0x10},
		{Name: "state", First: 0x8000, Sectors: 0x60},
	}
}

// parameterBlock builds the PARM-wrapped table the fixture's parameter entry
// carries. It describes exactly the partitions the plan's GPT describes, which
// is what the runner checks before it writes either of them.
func parameterBlock(t *testing.T, parts []parameter.Partition) []byte {
	t.Helper()
	var list []string
	for _, p := range parts {
		list = append(list, fmt.Sprintf("0x%08x@0x%08x(%s)", p.Sectors, p.First, p.Name))
	}
	text := "FIRMWARE_VER: 9.9\nTYPE: GPT\n" +
		"CMDLINE: mtdparts=rk29xxnand:" + strings.Join(list, ",") + "\n"
	out := make([]byte, 8+len(text)+4)
	copy(out, "PARM")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(text)))
	copy(out[8:], text)
	binary.LittleEndian.PutUint32(out[8+len(text):], rkcrc.Checksum([]byte(text)))
	return out
}

// setPartitions rewrites the fixture's parameter block so it still describes
// exactly the partitions the plan writes, which is the invariant the runner
// enforces.
func (l *testLayout) setPartitions(t *testing.T, plan *Plan, parts []parameter.Partition) {
	t.Helper()
	block := parameterBlock(t, parts)
	need := int(l.paramOff) + len(block)
	if need > len(l.source) {
		grown := make([]byte, need)
		copy(grown, l.source)
		l.source = grown
		l.sourceLength = int64(need)
	}
	copy(l.source[l.paramOff:], block)
	l.paramBlock = block
	plan.GPT.Partitions = parts
	plan.GPT.Parameter.Size = int64(len(block))
}

// testLayout describes the synthetic image the runner tests read.
type testLayout struct {
	loader       []byte
	loaderOff    int64
	bootOff      int64
	bootBytes    int64
	stateOff     int64
	stateBytes   int64
	paramOff     int64
	paramBlock   []byte
	source       []byte
	sourceLength int64
}

func newTestLayout(t *testing.T) *testLayout {
	t.Helper()
	loader := rkbootLoader(t)
	block := parameterBlock(t, fixturePartitions())
	l := &testLayout{
		loader:     loader,
		loaderOff:  0,
		bootOff:    0x1800,
		bootBytes:  4096,
		stateOff:   0x3000,
		stateBytes: 8192,
		paramOff:   0x5000,
		paramBlock: block,
	}
	total := int(l.paramOff) + len(block)
	l.source = make([]byte, total)
	copy(l.source[l.loaderOff:], loader)
	for i := int(l.bootOff); i < int(l.bootOff+l.bootBytes); i++ {
		l.source[i] = byte(i * 7)
	}
	for i := int(l.stateOff); i < int(l.stateOff+l.stateBytes); i++ {
		l.source[i] = byte(i * 11)
	}
	copy(l.source[l.paramOff:], block)
	l.sourceLength = int64(total)
	return l
}

// plan builds the write plan the tests execute.
func (l *testLayout) plan(t *testing.T) *Plan {
	t.Helper()
	idb, err := NewRKBOOTModel().Build(LoaderInput{Container: l.loader})
	if err != nil {
		t.Fatalf("build fixture IDB: %v", err)
	}
	idbBytes := int64(len(idb))
	parts := fixturePartitions()
	replicas, err := parameter.ReplicaLayout(0, 0x4000, true, 2048)
	if err != nil {
		t.Fatalf("fixture replica layout: %v", err)
	}
	p := &Plan{
		Image:  "synthetic-update.img",
		Chip:   "RK3588",
		Loader: &LoaderPlan{FileOffset: l.loaderOff, Length: int64(len(l.loader))},
		IDB:    IDBPlan{LBASector: IDBDefaultLBA, Bytes: idbBytes},
		GPT: &GPTPlan{
			Partitions: parts,
			Parameter: &ParameterPlan{
				FileOffset:   l.paramOff,
				Size:         int64(len(l.paramBlock)),
				CopyBytes:    2048,
				Copies:       len(replicas),
				First4Access: true,
				BaseLBA:      replicas[0].LBA,
				StrideLBA:    replicas[1].LBA - replicas[0].LBA,
				CRCOK:        true,
			},
		},
		Items: []Item{
			{Name: "boot", Partition: "boot", FileOffset: l.bootOff, Size: l.bootBytes, PaddedSize: l.bootBytes, TargetLBA: 0x1000},
			{Name: "state", Partition: "state", FileOffset: l.stateOff, Size: l.stateBytes, PaddedSize: l.stateBytes, TargetLBA: 0x8000},
		},
		Tails: []Tail{
			{Partition: "state", FirstLBA: 0x8000 + uint64(l.stateBytes)/SectorSize, Bytes: 4096},
		},
		DiskSectors: 0x10000,
		DiskKnown:   true,
	}
	return p
}

func testOptions() Options {
	return Options{
		Timeout:            time.Second,
		Retries:            0,
		ChunkSize:          MinChunkSize,
		Sleep:              func(time.Duration) {},
		SkipSignatureCheck: true,
		SkipReadback:       true,
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

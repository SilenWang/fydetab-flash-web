package device

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// --- fakes ------------------------------------------------------------------

type fakeTransport struct {
	mode       rockusb.MediaMode
	data       map[rockusb.Op][]byte
	controls   []fakeControl
	cbws       [][]byte
	bulkOutErr error
	controlFn  func(index uint16, length int) (int, error)
	closed     bool

	pendingOp  rockusb.Op
	pendingTag uint32
	pendingLen uint32
	sentData   bool
}

type fakeControl struct {
	requestType, request, value, index uint16
	length                             int
}

func (f *fakeTransport) ControlOut(requestType, request, value, index uint16, data []byte) (int, error) {
	f.controls = append(f.controls, fakeControl{requestType, request, value, index, len(data)})
	if f.controlFn != nil {
		return f.controlFn(index, len(data))
	}
	return len(data), nil
}

func (f *fakeTransport) BulkOut(_ uint8, data []byte) (int, error) {
	if f.bulkOutErr != nil {
		return 0, f.bulkOutErr
	}
	f.cbws = append(f.cbws, append([]byte(nil), data...))
	if len(data) != rockusb.CBWLength {
		return 0, fmt.Errorf("bad CBW length %d", len(data))
	}
	f.pendingOp = rockusb.Op(data[15])
	f.pendingTag = binary.LittleEndian.Uint32(data[4:8])
	f.pendingLen = binary.LittleEndian.Uint32(data[8:12])
	f.sentData = false
	return len(data), nil
}

func (f *fakeTransport) BulkIn(_ uint8, length int) ([]byte, error) {
	if f.pendingLen > 0 && !f.sentData {
		f.sentData = true
		out := make([]byte, length)
		copy(out, f.data[f.pendingOp])
		return out, nil
	}
	csw := make([]byte, rockusb.CSWLength)
	binary.LittleEndian.PutUint32(csw[0:4], rockusb.CSWSignature)
	binary.LittleEndian.PutUint32(csw[4:8], f.pendingTag)
	return csw, nil
}

func (f *fakeTransport) Reset() error { return nil }

type fakeBridge struct {
	raw        []RawInfo
	reg        Registry
	listErr    error
	openErr    error
	transports map[int]*fakeTransport
	mode       map[int]rockusb.MediaMode
	opened     []int
	closed     []int
}

func newFakeBridge(raw ...RawInfo) *fakeBridge {
	return &fakeBridge{raw: raw, transports: map[int]*fakeTransport{}, mode: map[int]rockusb.MediaMode{}}
}

func (b *fakeBridge) List() ([]RawInfo, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	b.reg.Replace(b.raw)
	return append([]RawInfo(nil), b.raw...), nil
}

func (b *fakeBridge) lookup(devNo int) (Info, bool) {
	if info, ok := b.reg.Find(devNo); ok {
		return info, true
	}
	return b.reg.Find(devNo)
}

func (b *fakeBridge) Open(devNo int, _ string) (rockusb.Transport, error) {
	if b.openErr != nil {
		return nil, b.openErr
	}
	transport, ok := b.transports[devNo]
	if !ok {
		transport = &fakeTransport{mode: b.mode[devNo], data: map[rockusb.Op][]byte{}}
		b.transports[devNo] = transport
	}
	b.opened = append(b.opened, devNo)
	if _, ok := b.lookup(devNo); !ok {
		return nil, fmt.Errorf("no device with DevNo=%d", devNo)
	}
	return transport, nil
}

func (b *fakeBridge) Close(devNo int) error {
	b.closed = append(b.closed, devNo)
	if t, ok := b.transports[devNo]; ok {
		t.closed = true
	}
	return nil
}

// --- fixtures ---------------------------------------------------------------

func rawMaskrom(pid uint16, serial string) RawInfo {
	return RawInfo{
		VendorID: 0x2207, ProductID: pid, USBVersionMajor: 2,
		SerialNumber: serial, InterfaceNumber: 0, InterfaceClass: 0xff, BulkIn: 1, BulkOut: 2,
	}
}

func rawLoader(pid uint16, serial string) RawInfo {
	raw := rawMaskrom(pid, serial)
	raw.USBVersionSubminor = 1 // bit0 of bcdUSB set => LOADER
	return raw
}

func newMaskromShell() (*Shell, *fakeBridge) {
	bridge := newFakeBridge(rawMaskrom(0x350b, ""))
	return New(bridge, Config{}), bridge
}

func newLoaderShell() (*Shell, *fakeBridge) {
	bridge := newFakeBridge(rawLoader(0x350b, ""))
	return New(bridge, Config{}), bridge
}

// buildLoader writes a minimal but valid RKBOOT container.
func buildLoader(t *testing.T, segments [][2]any) []byte {
	t.Helper()
	var count471, count472 int
	for _, seg := range segments {
		if seg[0].(uint16) == rockusb.RequestIndex471 {
			count471++
		} else {
			count472++
		}
	}
	offset471 := rockusb.LoaderHeaderSize
	offset472 := offset471 + count471*rockusb.LoaderEntrySize
	dataOffset := offset472 + count472*rockusb.LoaderEntrySize
	total := dataOffset
	for _, seg := range segments {
		total += len(seg[1].([]byte))
	}
	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf[0:4], rockusb.LoaderTagBoot)
	binary.LittleEndian.PutUint16(buf[4:6], rockusb.LoaderHeaderSize)
	binary.LittleEndian.PutUint32(buf[21:25], rockusb.RK3588ChipTag)
	buf[25] = byte(count471)
	binary.LittleEndian.PutUint32(buf[26:30], uint32(offset471))
	buf[30] = rockusb.LoaderEntrySize
	buf[31] = byte(count472)
	binary.LittleEndian.PutUint32(buf[32:36], uint32(offset472))
	buf[36] = rockusb.LoaderEntrySize
	buf[43] = 'N'

	seen := map[uint16]int{}
	pos := dataOffset
	for i, seg := range segments {
		request := seg[0].(uint16)
		payload := seg[1].([]byte)
		slot := offset471 + seen[request]*rockusb.LoaderEntrySize
		if request == rockusb.RequestIndex472 {
			slot = offset472 + seen[request]*rockusb.LoaderEntrySize
		}
		seen[request]++
		buf[slot] = rockusb.LoaderEntrySize
		entryType := uint32(rockusb.DBEntry471)
		if request == rockusb.RequestIndex472 {
			entryType = rockusb.DBEntry472
		}
		binary.LittleEndian.PutUint32(buf[slot+1:slot+5], entryType)
		name := fmt.Sprintf("seg%d", i)
		for j, r := range name {
			if j >= 20 {
				break
			}
			binary.LittleEndian.PutUint16(buf[slot+5+j*2:], uint16(r))
		}
		binary.LittleEndian.PutUint32(buf[slot+45:slot+49], uint32(pos))
		binary.LittleEndian.PutUint32(buf[slot+49:slot+53], uint32(len(payload)))
		copy(buf[pos:], payload)
		pos += len(payload)
	}
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], rockusb.CRC32(buf[:len(buf)-4]))
	return buf
}

func minimalLoader(t *testing.T) []byte {
	t.Helper()
	return buildLoader(t, [][2]any{
		{rockusb.RequestIndex471, make([]byte, 2048)},
	})
}

// --- tests ------------------------------------------------------------------

func TestListPrintsUpgradeToolLines(t *testing.T) {
	shell, _ := newMaskromShell()
	res := shell.Exec("ld", nil, "")
	if !res.OK {
		t.Fatalf("ld failed: %+v", res.Error)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("lines = %v", res.Lines)
	}
	if want := "DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom"; res.Lines[0] != want {
		t.Fatalf("line = %q, want %q", res.Lines[0], want)
	}
	if res.State.Mode != "MASKROM" || res.State.Current != 1 {
		t.Fatalf("state = %+v", res.State)
	}
}

func TestListWithNoDevices(t *testing.T) {
	bridge := newFakeBridge()
	shell := New(bridge, Config{})
	res := shell.Exec("ld", nil, "")
	if !res.OK || len(res.Lines) != 1 || res.Lines[0] != "No device found." {
		t.Fatalf("res = %+v", res)
	}
	if res.State.Current != 0 || res.State.Mode != "NONE" {
		t.Fatalf("state = %+v", res.State)
	}
}

func TestRegistryKeepsDevNoAcrossReenumeration(t *testing.T) {
	var reg Registry
	first := reg.Replace([]RawInfo{rawLoader(0x350b, ""), rawLoader(0x110a, "SN2")})
	if len(first) != 2 || first[0].DevNo != 1 || first[1].DevNo != 2 {
		t.Fatalf("first listing = %+v", first)
	}
	// The first board re-enumerates from maskrom to loader: same VID:PID,
	// but the list order flipped. DevNo must follow the board, not the index.
	second := reg.Replace([]RawInfo{rawLoader(0x110a, "SN2"), rawMaskrom(0x350b, "")})
	if second[0].DevNo != 1 || second[0].ProductID != 0x350b {
		t.Fatalf("maskrom board lost DevNo=1: %+v", second[0])
	}
	if second[1].DevNo != 2 {
		t.Fatalf("serial device lost DevNo=2: %+v", second[1])
	}
	// A brand new device takes the lowest free number.
	third := reg.Replace([]RawInfo{rawLoader(0x110a, "SN2"), rawMaskrom(0x350b, ""), rawLoader(0x330c, "")})
	if third[2].DevNo != 3 {
		t.Fatalf("new device DevNo = %d, want 3", third[2].DevNo)
	}
}

func TestCdAndSdSelectByDevNo(t *testing.T) {
	bridge := newFakeBridge(rawLoader(0x350b, "a"), rawLoader(0x110a, "b"))
	shell := New(bridge, Config{})
	if res := shell.Exec("ld", nil, ""); !res.OK {
		t.Fatalf("ld: %+v", res.Error)
	}
	res := shell.Exec("cd 2", nil, "")
	if !res.OK || res.State.Current != 2 {
		t.Fatalf("cd 2: %+v", res)
	}
	if !strings.Contains(res.Lines[0], "ChangeDevice: DevNo=2") {
		t.Errorf("line = %q", res.Lines[0])
	}
	res = shell.Exec("sd 1", nil, "")
	if !res.OK || res.State.Current != 1 {
		t.Fatalf("sd 1: %+v", res)
	}
	if !strings.Contains(res.Lines[0], "SwitchDevice: DevNo=1") {
		t.Errorf("line = %q", res.Lines[0])
	}

	for _, bad := range []string{"cd", "cd 9", "cd x", "sd 0", "sd -1"} {
		if res := shell.Exec(bad, nil, ""); res.OK {
			t.Errorf("%q should fail", bad)
		}
	}
	if res := shell.Exec("cd 9", nil, ""); !strings.Contains(strings.Join(res.Lines, "\n"), "DevNo=9") {
		t.Errorf("out-of-range message should name the number: %v", res.Lines)
	}
}

func TestSelectInMaskromExplainsTheMode(t *testing.T) {
	shell, _ := newMaskromShell()
	shell.Exec("ld", nil, "")
	res := shell.Exec("cd 1", nil, "")
	if !res.OK {
		t.Fatalf("cd 1 failed: %+v", res.Error)
	}
	joined := strings.Join(res.Lines, "\n")
	if !strings.Contains(joined, "maskrom 态只能") {
		t.Fatalf("maskrom selection should explain what is possible: %v", res.Lines)
	}
}

func TestInfoIsRefusedInMaskrom(t *testing.T) {
	shell, bridge := newMaskromShell()
	shell.Exec("ld", nil, "")
	res := shell.Exec("info", nil, "")
	if res.OK {
		t.Fatal("info must not run in maskrom")
	}
	if res.Error.Code != rockusb.ErrDeviceNotSupport {
		t.Fatalf("code = %v, want %v", res.Error.Code, rockusb.ErrDeviceNotSupport)
	}
	if !strings.Contains(res.Error.Hint, "db") {
		t.Errorf("hint should point at db: %q", res.Error.Hint)
	}
	if len(bridge.opened) != 0 {
		t.Errorf("info must not even open the device in maskrom, opened=%v", bridge.opened)
	}
}

func loadLoaderDevice(bridge *fakeBridge, devNo int) *fakeTransport {
	chip := make([]byte, 16)
	copy(chip, "3588")
	flash := make([]byte, 11)
	binary.LittleEndian.PutUint32(flash[0:4], 0x00700000)
	binary.LittleEndian.PutUint16(flash[4:6], 0x0400)
	flash[6] = 0x08
	flash[9] = 0x01
	flash[10] = 0x03
	capability := []byte{0x0F, 0x01, 0, 0, 0, 0, 0, 0}
	storage := []byte{0x02, 0x02, 0, 0} // eMMC (bit 1) + SPI NOR (bit 9)
	transport := &fakeTransport{
		mode: rockusb.ModeLoader,
		data: map[rockusb.Op][]byte{
			rockusb.OpReadChipInfo:   chip,
			rockusb.OpReadFlashInfo:  flash,
			rockusb.OpReadCapability: capability,
			rockusb.OpReadStorage:    storage,
		},
	}
	bridge.transports[devNo] = transport
	bridge.mode[devNo] = rockusb.ModeLoader
	return transport
}

func TestInfoReadsEverythingInLoaderMode(t *testing.T) {
	shell, bridge := newLoaderShell()
	shell.Exec("ld", nil, "")
	transport := loadLoaderDevice(bridge, 1)

	res := shell.Exec("info", nil, "")
	if !res.OK {
		t.Fatalf("info failed: %+v", res.Error)
	}
	if len(res.Commands) != 5 {
		t.Fatalf("commands = %d, want test-unit-ready + 4 reads", len(res.Commands))
	}
	if res.Info == nil || res.Info.Chip == nil || res.Info.Chip.Text != "3588" {
		t.Fatalf("chip info = %+v", res.Info)
	}
	if res.Info.Storage == nil || res.Info.Storage.CurrentName != "eMMC" {
		t.Fatalf("storage = %+v", res.Info.Storage)
	}
	if res.Info.Capability == nil || len(res.Info.Capability.Flags) != 5 {
		t.Fatalf("capability = %+v", res.Info.Capability)
	}
	if !res.Info.TestUnitReady {
		t.Error("test unit ready should be reported as OK")
	}
	joined := strings.Join(res.Lines, "\n")
	for _, want := range []string{"ChipInfo: 3588", "FlashInfo:", "Capability:", "Storage: current=eMMC", "TestDeviceReady: OK"} {
		if !strings.Contains(joined, want) {
			t.Errorf("info lines are missing %q:\n%s", want, joined)
		}
	}
	// info is read-only: only 31-byte CBWs may leave the host.
	for i, cbw := range transport.cbws {
		if len(cbw) != rockusb.CBWLength {
			t.Errorf("CBW %d is %d bytes", i, len(cbw))
		}
		if !rockusb.IsReadOnly(rockusb.Op(cbw[15])) {
			t.Errorf("CBW %d has non read-only opcode 0x%02X", i, cbw[15])
		}
	}
	if len(transport.controls) != 0 {
		t.Error("info must not issue control transfers")
	}
	if len(res.Transfers) == 0 {
		t.Fatal("info should return its transfers for the page trace")
	}
	for i, tr := range res.Transfers {
		if tr.Seq != i+1 {
			t.Errorf("transfer %d seq = %d, want %d", i, tr.Seq, i+1)
		}
	}
	if len(bridge.closed) != 1 {
		t.Errorf("close calls = %v, want one after the read batch", bridge.closed)
	}
}

func TestInfoSectionFilter(t *testing.T) {
	shell, bridge := newLoaderShell()
	shell.Exec("ld", nil, "")
	loadLoaderDevice(bridge, 1)
	res := shell.Exec("info chip", nil, "")
	if !res.OK {
		t.Fatalf("info chip failed: %+v", res.Error)
	}
	if len(res.Commands) != 2 {
		t.Fatalf("commands = %d, want test-unit-ready + chip", len(res.Commands))
	}
	if res.Info.Flash != nil || res.Info.Storage != nil {
		t.Errorf("filtered sections must stay empty: %+v", res.Info)
	}
	if res := shell.Exec("info nonsense", nil, ""); res.OK {
		t.Error("unknown info section must fail")
	}
}

func TestDownloadBootNeedsLoaderFile(t *testing.T) {
	shell, _ := newMaskromShell()
	shell.Exec("ld", nil, "")
	res := shell.Exec("db", nil, "")
	if res.OK {
		t.Fatal("db without a loader must fail")
	}
	if !strings.Contains(res.Error.Detail, "no loader") {
		t.Errorf("detail = %q", res.Error.Detail)
	}
	if !strings.Contains(res.Error.Hint, "rk3588_spl_loader") {
		t.Errorf("hint should name a loader: %q", res.Error.Hint)
	}
}

func TestDownloadBootRejectsBrokenLoader(t *testing.T) {
	shell, _ := newMaskromShell()
	shell.Exec("ld", nil, "")
	broken := append([]byte("this is not an RKBOOT"), make([]byte, 200)...)
	res := shell.Exec("db", broken, "broken.bin")
	if res.OK {
		t.Fatal("db with a broken loader must fail")
	}
	if res.Error.Loader == nil || res.Error.Loader.Kind != rockusb.LoaderBadTag {
		t.Fatalf("loader error = %+v", res.Error.Loader)
	}
	if res.Error.Hint == "" {
		t.Error("a loader error must carry a hint")
	}
	if res.State.LoaderLoaded {
		t.Error("a rejected file must not become the loaded loader")
	}
}

func TestDownloadBootRejectsLoaderMode(t *testing.T) {
	shell, bridge := newLoaderShell()
	shell.Exec("ld", nil, "")
	loadLoaderDevice(bridge, 1)
	res := shell.Exec("db", minimalLoader(t), "loader.bin")
	if res.OK {
		t.Fatal("db in loader mode must fail")
	}
	if res.Error.Code != rockusb.ErrDeviceNotSupport {
		t.Fatalf("code = %v", res.Error.Code)
	}
}

func TestDownloadBootSuccessAndReenumeration(t *testing.T) {
	shell, bridge := newMaskromShell()
	shell.Exec("ld", nil, "")
	transport := &fakeTransport{mode: rockusb.ModeMaskrom, data: map[rockusb.Op][]byte{}}
	bridge.transports[1] = transport
	bridge.mode[1] = rockusb.ModeMaskrom

	res := shell.Exec("db", minimalLoader(t), "rk3588_spl_loader_v1.06.111.bin")
	if !res.OK {
		t.Fatalf("db failed: %+v", res.Error)
	}
	if res.DB == nil || res.DB.Segments != 1 || res.DB.Packets != 1 {
		t.Fatalf("report = %+v", res.DB)
	}
	if res.DB.Bytes != 2050 {
		t.Errorf("first packet = 2050 bytes (2048 + 2 CRC), got %d total bytes", res.DB.Bytes)
	}
	joined := strings.Join(res.Lines, "\n")
	if !strings.Contains(joined, "Downloading bootloader succeeded.") {
		t.Errorf("lines = %v", res.Lines)
	}
	if !res.State.AwaitingReenumeration {
		t.Error("state should wait for re-enumeration after db")
	}
	if !res.State.LoaderLoaded || !strings.Contains(res.State.LoaderName, "rk3588_spl_loader") {
		t.Errorf("state loader = %+v", res.State)
	}
	if len(transport.controls) != 1 || transport.controls[0].index != rockusb.RequestIndex471 {
		t.Fatalf("control transfers = %+v", transport.controls)
	}

	// The device comes back as LOADER; the next ld must notice and clear the
	// waiting flag instead of leaving the page in "waiting" forever.
	bridge.raw = []RawInfo{rawLoader(0x350b, "")}
	ld := shell.Exec("ld", nil, "")
	if ld.State.AwaitingReenumeration {
		t.Error("re-enumeration should clear the waiting flag")
	}
	if !strings.Contains(strings.Join(ld.Lines, "\n"), "Re-enumerated as LOADER") {
		t.Errorf("ld should report the mode change: %v", ld.Lines)
	}
}

// TestInfoRefreshesAfterDownloadBoot: after db the board re-enumerates as
// LOADER, and the next command must notice without a manual ld in between.
func TestInfoRefreshesAfterDownloadBoot(t *testing.T) {
	shell, bridge := newMaskromShell()
	shell.Exec("ld", nil, "")
	bridge.transports[1] = &fakeTransport{mode: rockusb.ModeMaskrom, data: map[rockusb.Op][]byte{}}
	bridge.mode[1] = rockusb.ModeMaskrom

	if res := shell.Exec("db", minimalLoader(t), "loader.bin"); !res.OK {
		t.Fatalf("db failed: %+v", res.Error)
	}

	// Same board, now in LOADER, with read-only data available.
	bridge.raw = []RawInfo{rawLoader(0x350b, "")}
	loadLoaderDevice(bridge, 1)

	res := shell.Exec("info", nil, "")
	if !res.OK {
		t.Fatalf("info after db failed: %+v", res.Error)
	}
	if res.State.Mode != "LOADER" {
		t.Errorf("mode = %s, want LOADER", res.State.Mode)
	}
	if res.State.AwaitingReenumeration {
		t.Error("the re-enumeration should have been noticed")
	}
	if !strings.Contains(strings.Join(res.Lines, "\n"), "Re-enumerated as LOADER") {
		t.Errorf("info should report the mode change: %v", res.Lines)
	}
}

func TestDownloadBootReportsBootromRejection(t *testing.T) {
	shell, bridge := newMaskromShell()
	shell.Exec("ld", nil, "")
	transport := &fakeTransport{mode: rockusb.ModeMaskrom, data: map[rockusb.Op][]byte{}}
	// The stage-2 signature: BootROM refuses the very first 0x0471 packet.
	transport.controlFn = func(index uint16, _ int) (int, error) {
		if index == rockusb.RequestIndex471 {
			return -1, fmt.Errorf("A transfer error has occurred.")
		}
		return 0, nil
	}
	bridge.transports[1] = transport
	bridge.mode[1] = rockusb.ModeMaskrom

	res := shell.Exec("db", minimalLoader(t), "loader.bin")
	if res.OK {
		t.Fatal("db should fail")
	}
	if res.Error.DB == nil || res.Error.DB.Kind != rockusb.DBBootromRejected {
		t.Fatalf("db error = %+v", res.Error)
	}
	if !strings.Contains(res.Error.Hint, "maskrom") {
		t.Errorf("hint = %q", res.Error.Hint)
	}
	if len(transport.controls) != 1 {
		t.Errorf("controls = %d, want to stop after the first rejected packet", len(transport.controls))
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	shell, _ := newMaskromShell()
	res := shell.Exec("uf rk3588-update.img", nil, "")
	if res.OK {
		t.Fatal("flash write commands must not exist")
	}
	if res.Error.Code != rockusb.ErrDeviceNotSupport {
		t.Fatalf("code = %v", res.Error.Code)
	}
	if !strings.Contains(res.Error.Detail, "unknown command") {
		t.Errorf("detail = %q", res.Error.Detail)
	}
}

// TestNoFlashWriteCommands is the M1 scope guard: no command may write flash.
func TestNoFlashWriteCommands(t *testing.T) {
	forbidden := map[string]bool{
		"uf": true, "wl": true, "el": true, "rd": true, "ppt": true, "gpt": true,
		"cs": true, "rci": true, "td": true, "ef": true, "wipe": true,
	}
	for _, c := range Commands {
		if forbidden[c.Name] {
			t.Errorf("%s is a flash-writing or storage-switching command and must not exist in M1", c.Name)
		}
	}
	allowed := map[string]bool{"ld": true, "desc": true, "cd": true, "sd": true, "db": true, "info": true, "help": true, "version": true}
	if len(Commands) != len(allowed) {
		t.Fatalf("command table has %d entries, want %d", len(Commands), len(allowed))
	}
	for _, c := range Commands {
		if !allowed[c.Name] {
			t.Errorf("unexpected command %q", c.Name)
		}
	}
	// The help text is what users read first: it must not advertise writes.
	shell, _ := newMaskromShell()
	help := strings.Join(shell.Exec("help", nil, "").Lines, "\n")
	for _, word := range []string{"uf", "write flash", "擦除"} {
		if strings.Contains(help, word) {
			t.Errorf("help mentions %q: %s", word, help)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	shell, _ := newMaskromShell()
	help := shell.Exec("help", nil, "")
	if !help.OK || len(help.Lines) < len(Commands)+1 {
		t.Fatalf("help = %+v", help)
	}
	ver := shell.Exec("version", nil, "")
	if !ver.OK || !strings.Contains(ver.Lines[0], CoreVersion) {
		t.Fatalf("version = %+v", ver)
	}
	if res := shell.Exec("", nil, ""); res.OK {
		t.Fatal("an empty command must fail")
	}
}

func TestDescribe(t *testing.T) {
	shell, _ := newMaskromShell()
	shell.Exec("ld", nil, "")
	res := shell.Exec("desc", nil, "")
	if !res.OK {
		t.Fatalf("desc failed: %+v", res.Error)
	}
	joined := strings.Join(res.Lines, "\n")
	for _, want := range []string{"Vid=0x2207", "bcdUSB: 0x0200", "bit0=0", "Maskrom", "bulkIn=0x81"} {
		if !strings.Contains(joined, want) {
			t.Errorf("desc is missing %q:\n%s", want, joined)
		}
	}
}

func TestListErrorIsReported(t *testing.T) {
	bridge := newFakeBridge()
	bridge.listErr = fmt.Errorf("permission revoked")
	shell := New(bridge, Config{})
	res := shell.Exec("ld", nil, "")
	if res.OK || res.Error.Code != rockusb.ErrDeviceOpenFailed {
		t.Fatalf("res = %+v", res)
	}
}

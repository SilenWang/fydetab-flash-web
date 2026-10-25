package rockusb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseLoaderImageFromRealisticLayout(t *testing.T) {
	raw := rk3588Loader(t)
	img, err := ParseLoaderImage(raw)
	if err != nil {
		t.Fatalf("ParseLoaderImage: %v", err)
	}
	if img.TagName != "BOOT" {
		t.Errorf("tag name = %q", img.TagName)
	}
	if img.ChipType != RK3588ChipTag {
		t.Errorf("chip type = 0x%08X", img.ChipType)
	}
	if img.CRCStored != img.CRCCalculated {
		t.Errorf("crc stored != calculated")
	}
	if len(img.Entries471) != 2 || len(img.Entries472) != 1 {
		t.Fatalf("entries = %d/472:%d, want 2/1", len(img.Entries471), len(img.Entries472))
	}
	if img.Entries471[0].Name != "UsbHead" || img.Entries471[0].DataSize != 2048 {
		t.Errorf("first 471 entry = %+v", img.Entries471[0])
	}
	if img.Entries471[1].Name != "rk3588_ddr_lp4_2112M" || img.Entries471[1].DataSize != 51200 {
		t.Errorf("second 471 entry = %+v", img.Entries471[1])
	}
	if img.Entries472[0].DataSize != 88064 {
		t.Errorf("472 entry = %+v", img.Entries472[0])
	}

	plan := img.Plan()
	if len(plan) != 3 || plan[0].Request != RequestIndex471 || plan[1].Request != RequestIndex471 || plan[2].Request != RequestIndex472 {
		t.Fatalf("plan order = %+v, want 471,471,472", plan)
	}
	if seg := img.Segment(plan[0].Entry); len(seg) != 2048 || seg[0] != 0 || seg[1] != 1 {
		t.Fatalf("segment data does not point at the entry payload")
	}

	summary := strings.Join(img.Summary(), "\n")
	for _, want := range []string{"UsbHead", "rk3588_ddr_lp4_2112M", "rk3588_usbplug_v1", "CRC-32", "0x0471", "0x0472"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary)
		}
	}
}

func TestParseLoaderImageRejectsBadFiles(t *testing.T) {
	good := rk3588Loader(t)

	clone := func() []byte { return append([]byte(nil), good...) }

	t.Run("empty", func(t *testing.T) {
		if _, err := ParseLoaderImage(nil); loaderKind(err) != LoaderEmpty {
			t.Fatalf("err = %v, want LOADER_EMPTY", err)
		}
	})
	t.Run("too short", func(t *testing.T) {
		if _, err := ParseLoaderImage(good[:LoaderHeaderSize+10]); loaderKind(err) != LoaderTooShort {
			t.Fatalf("err = %v, want LOADER_TOO_SHORT", err)
		}
	})
	t.Run("bad tag", func(t *testing.T) {
		b := clone()
		binary.LittleEndian.PutUint32(b[0:4], 0xDEADBEEF)
		fixLoaderCRC(b)
		if _, err := ParseLoaderImage(b); loaderKind(err) != LoaderBadTag {
			t.Fatalf("err = %v, want LOADER_BAD_TAG", err)
		}
	})
	t.Run("bad crc", func(t *testing.T) {
		b := clone()
		b[len(b)-10] ^= 0xFF // flip payload, leave the tag intact
		if _, err := ParseLoaderImage(b); loaderKind(err) != LoaderBadCRC {
			t.Fatalf("err = %v, want LOADER_BAD_CRC", err)
		}
	})
	t.Run("foreign chip", func(t *testing.T) {
		b := clone()
		binary.LittleEndian.PutUint32(b[21:25], 0x33353638) // "3568"
		fixLoaderCRC(b)
		if _, err := ParseLoaderImage(b); loaderKind(err) != LoaderChipMismatch {
			t.Fatalf("err = %v, want LOADER_CHIP_MISMATCH", err)
		}
	})
	t.Run("zero chip type is tolerated", func(t *testing.T) {
		b := clone()
		binary.LittleEndian.PutUint32(b[21:25], 0)
		fixLoaderCRC(b)
		if _, err := ParseLoaderImage(b); err != nil {
			t.Fatalf("chip type 0 should be tolerated: %v", err)
		}
	})
	t.Run("segment outside the file", func(t *testing.T) {
		b := clone()
		// First 471 entry: dataOffset at +45, dataSize at +49.
		entry := LoaderHeaderSize
		binary.LittleEndian.PutUint32(b[entry+49:entry+53], uint32(len(b)))
		fixLoaderCRC(b)
		if _, err := ParseLoaderImage(b); loaderKind(err) != LoaderBadEntry {
			t.Fatalf("err = %v, want LOADER_BAD_ENTRY", err)
		}
	})
	t.Run("no entries", func(t *testing.T) {
		b := clone()
		b[25] = 0
		b[31] = 0
		fixLoaderCRC(b)
		if _, err := ParseLoaderImage(b); loaderKind(err) != LoaderNoBootEntries {
			t.Fatalf("err = %v, want LOADER_NO_BOOT_ENTRIES", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		big := make([]byte, MaxLoaderSize+1)
		if _, err := ParseLoaderImage(big); loaderKind(err) != LoaderTooLarge {
			t.Fatalf("err = %v, want LOADER_TOO_LARGE", err)
		}
	})
}

func loaderKind(err error) LoaderErrorKind {
	var lerr *LoaderError
	if errors.As(err, &lerr) {
		return lerr.Kind
	}
	return 0
}

func fixLoaderCRC(b []byte) {
	binary.LittleEndian.PutUint32(b[len(b)-4:], CRC32(b[:len(b)-4]))
}

func TestPlanVendorRequestChunking(t *testing.T) {
	cases := []struct {
		size        int
		wantSizes   []int
		wantPending bool
	}{
		// Stage 2 measured the first packet as 2048 + 2 CRC bytes.
		{2048, []int{2050}, false},
		{51200, []int{4096, 4096, 4096, 4096, 4096, 4096, 4096, 4096, 4096, 4096, 4096, 4096, 2048 + 2}, false},
		// Exact multiples: one extra short packet carries the CRC.
		{4096, []int{4096, 2}, false},
		// 4095 mod 4096: upstream pads one byte so the CRC starts a new
		// packet.
		{4095, []int{4096, 2}, false},
		// 4094 mod 4096: the CRC completes the packet and upstream then
		// writes a trailing one-byte packet.
		{4094, []int{4096}, true},
	}
	for _, c := range cases {
		payload := make([]byte, c.size)
		for i := range payload {
			payload[i] = byte(i)
		}
		plan, err := PlanVendorRequest(RequestIndex471, payload)
		if err != nil {
			t.Fatalf("size %d: %v", c.size, err)
		}
		if len(plan.Packets) != len(c.wantSizes) {
			t.Fatalf("size %d: %d packets, want %d (%v)", c.size, len(plan.Packets), len(c.wantSizes), plan.PacketSizes)
		}
		for i, want := range c.wantSizes {
			if len(plan.Packets[i]) != want {
				t.Errorf("size %d: packet %d = %d bytes, want %d", c.size, i, len(plan.Packets[i]), want)
			}
		}
		if plan.PendingZeroWrite != c.wantPending {
			t.Errorf("size %d: pendingZeroWrite = %v, want %v", c.size, plan.PendingZeroWrite, c.wantPending)
		}

		// The last two bytes of the payload stream are the CRC, high byte
		// first, over the (possibly padded) payload.
		flat := bytes.Join(plan.Packets, nil)
		dataLen := plan.DataSize
		if len(flat) != dataLen {
			t.Fatalf("size %d: packets hold %d bytes, dataSize says %d", c.size, len(flat), dataLen)
		}
		body := flat[:dataLen-2]
		want := CRCCCITT(body)
		if got := binary.BigEndian.Uint16(flat[dataLen-2:]); got != want {
			t.Errorf("size %d: trailing CRC 0x%04X, want 0x%04X", c.size, got, want)
		}
		if plan.CRCCCITT != want {
			t.Errorf("size %d: plan CRC 0x%04X, want 0x%04X", c.size, plan.CRCCCITT, want)
		}
	}
}

func TestPlanVendorRequestRejectsOtherIndices(t *testing.T) {
	if _, err := PlanVendorRequest(0x0470, []byte{1}); err == nil {
		t.Fatal("only 0x0471 and 0x0472 are Download Boot indices")
	}
	if _, err := PlanVendorRequest(RequestIndex471, nil); err == nil {
		t.Fatal("empty payload must be rejected")
	}
}

func TestLoaderHeaderExposesVersionBuildTimeAndChip(t *testing.T) {
	img, err := ParseLoaderImage(rk3588Loader(t))
	if err != nil {
		t.Fatalf("ParseLoaderImage: %v", err)
	}
	if got, want := img.VersionString(), "0.1.6"; got != want {
		t.Errorf("VersionString() = %q, want %q", got, want)
	}
	if got, want := img.MergeVersionString(), "1.3.0"; got != want {
		t.Errorf("MergeVersionString() = %q, want %q", got, want)
	}
	// The build time is the field that tells the two real loaders apart
	// (v23's 2024-09-06 vs the SDK's 2022-06-28); the fixture pins the parse.
	if got, want := img.BuildTime.String(), "2019-07-01 02:03:04 UTC"; got != want {
		t.Errorf("BuildTime = %q, want %q", got, want)
	}
	if !img.BuildTime.Valid() {
		t.Error("BuildTime should be valid")
	}
	if got, want := img.ChipName(), "RK3588"; got != want {
		t.Errorf("ChipName() = %q, want %q", got, want)
	}
	var zero LoaderTime
	if zero.Valid() || zero.String() != "unknown" {
		t.Errorf("zero LoaderTime = valid:%v %q, want invalid/unknown", zero.Valid(), zero.String())
	}
	var feb LoaderTime = LoaderTime{Year: 2024, Month: 2, Day: 31}
	if feb.Valid() {
		t.Error("2024-02-31 is not a calendar date")
	}
}

func TestCheckChipRejectsLoaderForAnotherChip(t *testing.T) {
	img, err := ParseLoaderImage(rk3588Loader(t))
	if err != nil {
		t.Fatalf("ParseLoaderImage: %v", err)
	}
	for _, want := range []string{"RK3588", "rk3588s", "3588", ""} {
		if err := img.CheckChip(want); err != nil {
			t.Errorf("CheckChip(%q) = %v, want nil", want, err)
		}
	}
	for _, want := range []string{"RK3566", "RK3399"} {
		err := img.CheckChip(want)
		if err == nil {
			t.Fatalf("CheckChip(%q) must fail", want)
		}
		var lerr *LoaderError
		if !errors.As(err, &lerr) || lerr.Kind != LoaderChipMismatch {
			t.Fatalf("CheckChip(%q) = %v, want LOADER_CHIP_MISMATCH", want, err)
		}
		if !strings.Contains(err.Error(), "RK3588") {
			t.Errorf("the message should name the loader's own chip: %v", err)
		}
	}
	if img.SupportsChip("RK3588") != true || img.SupportsChip("RK3566") != false {
		t.Error("SupportsChip disagrees with CheckChip")
	}
}

func TestDownloadBootSendsSegmentsInOrder(t *testing.T) {
	raw := rk3588Loader(t)
	img, err := ParseLoaderImage(raw)
	if err != nil {
		t.Fatalf("ParseLoaderImage: %v", err)
	}

	dev := newScriptedDevice(nil)
	var sleeps []time.Duration
	var events []DBEvent
	s := NewSession(dev, maskromLink(), Options{
		Sleep: func(d time.Duration) { sleeps = append(sleeps, d) },
	})

	report, err := s.DownloadBoot(img, DBOptions{Progress: func(e DBEvent) { events = append(events, e) }})
	if err != nil {
		t.Fatalf("DownloadBoot: %v", err)
	}

	// 2050 + 51200+2 + 88064+2 bytes, in chunks of at most 4096.
	wantPackets := 1 + (51202/4096 + 1) + (88066/4096 + 1)
	if report.Packets != wantPackets {
		t.Errorf("packets = %d, want %d", report.Packets, wantPackets)
	}
	if report.Bytes != 2050+51202+88066 {
		t.Errorf("bytes = %d, want %d", report.Bytes, 2050+51202+88066)
	}
	if report.Segments471 != 2 || report.Segments472 != 1 {
		t.Errorf("segments = %d/472:%d", report.Segments471, report.Segments472)
	}
	if len(report.CRCs) != 3 {
		t.Errorf("crcs = %v", report.CRCs)
	}

	controls := dev.controlOuts()
	if len(controls) != report.Packets {
		t.Fatalf("%d control transfers, report says %d", len(controls), report.Packets)
	}
	var indices []uint16
	for i, c := range controls {
		if c.requestType != RequestTypeVendorOut || c.request != RequestDeviceRequest || c.value != 0 {
			t.Fatalf("control %d = %+v, want bmRequestType=0x40 bRequest=0x0C value=0", i, c)
		}
		if c.length == 0 || c.length > VendorChunkSize {
			t.Fatalf("control %d carries %d bytes", i, c.length)
		}
		if i == 0 || indices[len(indices)-1] != c.index {
			indices = append(indices, c.index)
		}
	}
	if len(indices) != 2 || indices[0] != RequestIndex471 || indices[1] != RequestIndex472 {
		t.Fatalf("request index order = %v, want [0x0471 0x0472]", indices)
	}

	// One settle sleep of one second; the fixture sets no per-entry delays.
	if len(sleeps) != 1 || sleeps[0] != DBSettle {
		t.Errorf("sleeps = %v, want one %s settle", sleeps, DBSettle)
	}

	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if got, want := len(kinds), 3+report.Packets+2; got != want {
		t.Fatalf("%d events, want %d (3 segments + %d packets + settle + done)", got, want, report.Packets)
	}
	segments := 0
	packets := 0
	for _, e := range events {
		switch e.Kind {
		case "segment":
			segments++
		case "packet":
			packets++
		}
	}
	if segments != 3 {
		t.Errorf("%d segment events, want 3", segments)
	}
	if packets != report.Packets {
		t.Errorf("%d packet events, want %d", packets, report.Packets)
	}
	if kinds[len(kinds)-2] != "settle" || kinds[len(kinds)-1] != "done" {
		t.Errorf("tail events = %v, want settle,done", kinds[len(kinds)-2:])
	}
	for _, e := range events {
		if e.Kind == "segment" && e.Name == "" {
			t.Errorf("segment event without a name: %+v", e)
		}
	}
}

func TestDownloadBootRequiresMaskrom(t *testing.T) {
	raw := rk3588Loader(t)
	img, _ := ParseLoaderImage(raw)
	s := NewSession(newScriptedDevice(nil), loaderLink(), Options{})

	_, err := s.DownloadBoot(img, DBOptions{})
	dbErr := dbErrOf(err)
	if dbErr == nil {
		t.Fatalf("expected a classified error in loader mode, got %v", err)
	}
	if dbErr.Kind != DBNotMaskrom || dbErr.Code != ErrDeviceNotSupport {
		t.Fatalf("kind = %v code = %v, want DBNotMaskrom/ERR_DEVICE_NOT_SUPPORT", dbErr.Kind, dbErr.Code)
	}
	if dbErr.Hint() == "" {
		t.Error("a DB error must carry a next-step hint")
	}
}

func TestDownloadBootClassifiesFirstPacketRejection(t *testing.T) {
	// The stage-2 hardware signature: BootROM rejects the very first 0x0471
	// packet ("Boot_VendorRequest471 failed,index(0), err=-1").
	raw := rk3588Loader(t)
	img, _ := ParseLoaderImage(raw)
	dev := newScriptedDevice(nil)
	dev.controlFn = func(_, _, _, index uint16, data []byte) (int, error) {
		if index == RequestIndex471 {
			return -1, errors.New("Failed to execute 'controlOut': A transfer error has occurred")
		}
		return len(data), nil
	}
	s := NewSession(dev, maskromLink(), Options{})

	_, err := s.DownloadBoot(img, DBOptions{})
	dbErr := dbErrOf(err)
	if dbErr == nil || dbErr.Kind != DBBootromRejected {
		t.Fatalf("err = %v, want DBBootromRejected", err)
	}
	if dbErr.Code != ErrRequestFail {
		t.Errorf("code = %v, want %v", dbErr.Code, ErrRequestFail)
	}
	// The corrected reading of the hardware log: index(0) is the first data
	// block of the loader, not "the first USB request".
	if dbErr.Index != 0 || !dbErr.FirstBlock || dbErr.Category() != "first-block" {
		t.Errorf("index/block/category = %d/%v/%q, want 0/true/first-block",
			dbErr.Index, dbErr.FirstBlock, dbErr.Category())
	}
	if !strings.Contains(dbErr.Detail, "index 0") {
		t.Errorf("detail should name the rejected block as index 0: %q", dbErr.Detail)
	}
	if !strings.Contains(dbErr.Hint(), "loader") || !strings.Contains(dbErr.Hint(), "干净的上电周期") {
		t.Errorf("hint should blame the loader and the power-on cycle, not the cable first: %q", dbErr.Hint())
	}
	if got := len(dev.controlOuts()); got != 1 {
		t.Errorf("control transfers = %d, want to stop after the first rejected packet", got)
	}
}

func TestDownloadBootClassifiesFailureAfterFirstPacket(t *testing.T) {
	raw := rk3588Loader(t)
	img, _ := ParseLoaderImage(raw)
	dev := newScriptedDevice(nil)
	calls := 0
	dev.controlFn = func(_, _, _, _ uint16, data []byte) (int, error) {
		calls++
		// The first data block is one packet; anything after it is a later
		// block, which is the "not index 0" case.
		if calls == 15 {
			return 0, errors.New("device disappeared mid-download")
		}
		return len(data), nil
	}
	s := NewSession(dev, maskromLink(), Options{})

	_, err := s.DownloadBoot(img, DBOptions{})
	dbErr := dbErrOf(err)
	if dbErr == nil || dbErr.Kind != DBTransferFailed {
		t.Fatalf("err = %v, want DBTransferFailed", err)
	}
	if dbErr.Kind == DBBootromRejected {
		t.Fatal("a mid-download failure must not be reported as a BootROM rejection")
	}
	if dbErr.Index == 0 || dbErr.FirstBlock || dbErr.Category() != "mid-transfer" {
		t.Errorf("index/block/category = %d/%v/%q, want a non-zero block and mid-transfer",
			dbErr.Index, dbErr.FirstBlock, dbErr.Category())
	}
	if !strings.Contains(dbErr.Hint(), "首块已经被接受") {
		t.Errorf("hint should say the first block already worked: %q", dbErr.Hint())
	}
}

func TestDownloadBootShortWriteIsTransferFailure(t *testing.T) {
	raw := rk3588Loader(t)
	img, _ := ParseLoaderImage(raw)
	dev := newScriptedDevice(nil)
	dev.controlFn = func(_, _, _, _ uint16, data []byte) (int, error) {
		return len(data) - 1, nil
	}
	s := NewSession(dev, maskromLink(), Options{})
	_, err := s.DownloadBoot(img, DBOptions{})
	dbErr := dbErrOf(err)
	if dbErr == nil || dbErr.Kind != DBTransferFailed {
		t.Fatalf("err = %v, want DBTransferFailed", err)
	}
}

func TestDownloadBootSendsPendingZeroPacket(t *testing.T) {
	// 4094-byte segment exercises the trailing one-byte packet. It is not
	// reachable with stock RK3588 entry sizes, so the fixture builds one.
	payload := make([]byte, 4094)
	raw := buildLoader(t, RK3588ChipTag, LoaderTagBoot, []testSegment{
		{request: RequestIndex471, name: "odd", data: payload},
	})
	img, err := ParseLoaderImage(raw)
	if err != nil {
		t.Fatalf("ParseLoaderImage: %v", err)
	}
	dev := newScriptedDevice(nil)
	var pendingWrites int
	dev.controlFn = func(_, _, _, _ uint16, data []byte) (int, error) {
		if len(data) == 1 {
			pendingWrites++
			return 0, nil // upstream expects zero bytes written
		}
		return len(data), nil
	}
	s := NewSession(dev, maskromLink(), Options{})
	if _, err := s.DownloadBoot(img, DBOptions{}); err != nil {
		t.Fatalf("DownloadBoot: %v", err)
	}
	if pendingWrites != 1 {
		t.Fatalf("pending packets = %d, want 1", pendingWrites)
	}

	// If the device instead accepts the byte, upstream fails; so do we.
	dev2 := newScriptedDevice(nil)
	dev2.controlFn = func(_, _, _, _ uint16, data []byte) (int, error) { return len(data), nil }
	s2 := NewSession(dev2, maskromLink(), Options{})
	_, err = s2.DownloadBoot(img, DBOptions{})
	dbErr := dbErrOf(err)
	if dbErr == nil || dbErr.Kind != DBTransferFailed {
		t.Fatalf("err = %v, want DBTransferFailed when the pending packet writes a byte", err)
	}
}

func TestDownloadBootHonoursEntryDelay(t *testing.T) {
	raw := buildLoader(t, RK3588ChipTag, LoaderTagBoot, []testSegment{
		{request: RequestIndex471, name: "head", data: make([]byte, 128), delay: 25},
	})
	img, _ := ParseLoaderImage(raw)
	var sleeps []time.Duration
	s := NewSession(newScriptedDevice(nil), maskromLink(), Options{Sleep: func(d time.Duration) { sleeps = append(sleeps, d) }})
	if _, err := s.DownloadBoot(img, DBOptions{}); err != nil {
		t.Fatalf("DownloadBoot: %v", err)
	}
	if len(sleeps) != 2 || sleeps[0] != 25*time.Millisecond || sleeps[1] != DBSettle {
		t.Fatalf("sleeps = %v, want [25ms 1s]", sleeps)
	}
}

func TestDownloadBootTraceRecordsVendorTransfers(t *testing.T) {
	raw := buildLoader(t, RK3588ChipTag, LoaderTagBoot, []testSegment{
		{request: RequestIndex471, name: "head", data: make([]byte, 2048)},
	})
	img, _ := ParseLoaderImage(raw)
	var trace []Transfer
	s := NewSession(newScriptedDevice(nil), maskromLink(), Options{Trace: func(tr Transfer) { trace = append(trace, tr) }})
	if _, err := s.DownloadBoot(img, DBOptions{}); err != nil {
		t.Fatalf("DownloadBoot: %v", err)
	}
	if len(trace) != 1 {
		t.Fatalf("trace = %d entries, want 1", len(trace))
	}
	tr := trace[0]
	if tr.Kind != "control-out" || tr.RequestType != int(RequestTypeVendorOut) ||
		tr.Request != int(RequestDeviceRequest) || tr.Index != int(RequestIndex471) ||
		tr.Length != 2050 || tr.Status != "ok" {
		t.Fatalf("trace entry = %+v", tr)
	}
}

func TestVerifyLoaderFile(t *testing.T) {
	_, lines, err := VerifyLoaderFile(rk3588Loader(t))
	if err != nil {
		t.Fatalf("VerifyLoaderFile: %v", err)
	}
	if len(lines) < 4 {
		t.Fatalf("summary lines = %v", lines)
	}
	if _, _, err := VerifyLoaderFile([]byte("not a loader")); err == nil {
		t.Fatal("a random file must not verify")
	}
}

// dbErrOf narrows a DownloadBoot error to its classified form.
func dbErrOf(err error) *DBError {
	var d *DBError
	if errors.As(err, &d) {
		return d
	}
	return nil
}

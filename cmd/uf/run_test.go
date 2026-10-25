package main

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
	"github.com/SilenWang/rk3588-webflasher/internal/uf"
)

func intPtr(v int) *int       { return &v }
func u32Ptr(v uint32) *uint32 { return &v }

// bareReport builds the M2 report for the bare RKAF CLI fixture.
func bareReport(t *testing.T) *Report {
	t.Helper()
	path := writeCLIImage(t, buildCLIImage(t))
	img, err := rkfw.Open(path)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	paramBytes, err := img.ReadItem(*img.Parameter, maxParameterBytes)
	if err != nil {
		t.Fatalf("read parameter: %v", err)
	}
	parsed, err := parameter.Parse(paramBytes)
	if err != nil {
		t.Fatalf("parse parameter: %v", err)
	}
	rep, err := BuildReport(img, parsed, VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	return rep
}

// buildLoaderBlob builds a minimal but valid RKBOOT container for the RKFW
// fixture: one 0x0471 segment and one 0x0472 segment, plus the trailing CRC-32.
func buildLoaderBlob(t *testing.T) []byte {
	t.Helper()
	const (
		header  = 102
		entry   = 57
		dataOff = header + 2*entry
	)
	head := make([]byte, 2048)
	plug := make([]byte, 1024)
	for i := range head {
		head[i] = byte(i)
	}
	for i := range plug {
		plug[i] = byte(i * 5)
	}
	buf := make([]byte, dataOff+len(head)+len(plug))
	le := binary.LittleEndian
	copy(buf[0:4], "BOOT")
	le.PutUint16(buf[4:6], header)
	le.PutUint32(buf[6:10], 0x00010006)
	le.PutUint32(buf[10:14], 0x01030000)
	le.PutUint32(buf[21:25], rockusb.RK3588ChipTag)
	buf[25] = 1
	le.PutUint32(buf[26:30], header)
	buf[30] = entry
	buf[31] = 1
	le.PutUint32(buf[32:36], header+entry)
	buf[36] = entry
	buf[43] = 'N'
	writeEntry := func(slot int, typ uint32, name string, off, size int) {
		buf[slot] = entry
		le.PutUint32(buf[slot+1:slot+5], typ)
		for i, r := range name {
			if i >= 20 {
				break
			}
			le.PutUint16(buf[slot+5+i*2:], uint16(r))
		}
		le.PutUint32(buf[slot+45:slot+49], uint32(off))
		le.PutUint32(buf[slot+49:slot+53], uint32(size))
	}
	writeEntry(header, rockusb.DBEntry471, "UsbHead", dataOff, len(head))
	writeEntry(header+entry, rockusb.DBEntry472, "rk3588_usbplug", dataOff+len(head), len(plug))
	copy(buf[dataOff:], head)
	copy(buf[dataOff+len(head):], plug)
	le.PutUint32(buf[len(buf)-4:], rockusb.CRC32(buf[:len(buf)-4]))
	return buf
}

// buildRKFWImage wraps the bare RKAF fixture behind an RKFW header with a
// loader and the trailing ASCII MD5, so the CLI can exercise ul/gpt too.
func buildRKFWImage(t *testing.T) []byte {
	t.Helper()
	loader := buildLoaderBlob(t)
	rkaf := buildCLIImage(t)
	le := binary.LittleEndian

	head := make([]byte, 102)
	copy(head[0:4], "RKFW")
	le.PutUint16(head[4:6], 102)
	le.PutUint32(head[6:10], 0x00010006)
	le.PutUint32(head[10:14], 0x01030000)
	le.PutUint16(head[14:16], 2024)
	head[16] = 1              // month
	head[17] = 2              // day
	copy(head[21:25], "8853") // reversed "3588"
	le.PutUint32(head[25:29], 102)
	le.PutUint32(head[29:33], uint32(len(loader)))
	le.PutUint32(head[33:37], uint32(102+len(loader)))
	le.PutUint32(head[37:41], uint32(len(rkaf)))

	body := append([]byte(nil), head...)
	body = append(body, loader...)
	body = append(body, rkaf...)
	sum := md5.Sum(body)
	return append(body, []byte(hex.EncodeToString(sum[:]))...)
}

func TestPlanFromReportUsesTheM2Plan(t *testing.T) {
	rep := bareReport(t)
	plan, err := planFromReport(rep)
	if err != nil {
		t.Fatalf("planFromReport: %v", err)
	}
	if plan.Loader != nil || plan.IDB.Bytes != 0 {
		t.Fatalf("bare RKAF plan has a loader/IDB: %+v", plan)
	}
	var want []string
	for _, it := range rep.Items {
		if it.Action == actionWrite {
			want = append(want, it.Name)
		}
	}
	if len(plan.Items) != len(want) {
		t.Fatalf("plan items = %d, want %d (%v)", len(plan.Items), len(want), want)
	}
	for i, name := range want {
		if plan.Items[i].Name != name {
			t.Fatalf("plan item %d = %s, want %s", i, plan.Items[i].Name, name)
		}
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("projected plan does not validate: %v", err)
	}
}

func TestPlanFromReportCarriesLoaderAndIDB(t *testing.T) {
	path := writeCLIImage(t, buildRKFWImage(t))
	img, err := rkfw.Open(path)
	if err != nil {
		t.Fatalf("open RKFW image: %v", err)
	}
	paramBytes, err := img.ReadItem(*img.Parameter, maxParameterBytes)
	if err != nil {
		t.Fatalf("read parameter: %v", err)
	}
	parsed, err := parameter.Parse(paramBytes)
	if err != nil {
		t.Fatalf("parse parameter: %v", err)
	}
	rep, err := BuildReport(img, parsed, VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	plan, err := planFromReport(rep)
	if err != nil {
		t.Fatalf("planFromReport: %v", err)
	}
	if plan.Loader == nil || plan.Loader.Length != img.LoaderLength {
		t.Fatalf("plan loader = %+v, want length %d", plan.Loader, img.LoaderLength)
	}
	var gptBytes int64
	for _, st := range rep.Stages {
		if st.Phase == "gpt" {
			gptBytes = st.Bytes
		}
	}
	if plan.IDB.Bytes != gptBytes || plan.IDB.LBASector != 64 {
		t.Fatalf("plan IDB = %+v, want %d bytes at LBA 64", plan.IDB, gptBytes)
	}
	if plan.Chip != "RK3588" {
		t.Fatalf("plan chip = %q, want RK3588", plan.Chip)
	}
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunMockBareImageSucceedsAndLogs(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	logPath := filepath.Join(t.TempDir(), "uf-log.ndjson")
	code, stdout, stderr := runCLI(t, "--mock", "--json", "--log", logPath, path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var rep uf.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout)
	}
	if !rep.OK || !rep.Reset || rep.IDB != nil {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(rep.Items))
	}
	if rep.Device.Mode != "LOADER" {
		t.Fatalf("device mode = %q, want LOADER", rep.Device.Mode)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 4 {
		t.Fatalf("log has %d lines", len(lines))
	}
	var first, last map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if first["kind"] != "header" || last["kind"] != "result" {
		t.Fatalf("log envelope = %v ... %v", first["kind"], last["kind"])
	}
}

func TestRunMockWithLoaderBootsBuildsIDBAndResets(t *testing.T) {
	path := writeCLIImage(t, buildRKFWImage(t))
	code, stdout, stderr := runCLI(t, "--mock", "--json", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var rep uf.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout)
	}
	if !rep.OK || !rep.Reset {
		t.Fatalf("report = %+v", rep)
	}
	if rep.IDB == nil || rep.IDB.LBA != 64 || rep.IDB.Model != uf.ModelRK28RC4 || rep.IDB.Verified {
		t.Fatalf("IDB report = %+v", rep.IDB)
	}
	if rep.Device.ReenumAttempts != 1 {
		t.Fatalf("re-enumeration attempts = %d, want 1", rep.Device.ReenumAttempts)
	}
	if !strings.Contains(stderr, "Download Boot") {
		t.Fatalf("stderr does not show the boot progress:\n%s", stderr)
	}
}

func TestRunMockFailureReportsWhereToResume(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	code, stdout, stderr := runCLI(t, "--mock", "--mock-fault", "failed:uboot", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "next item: uboot") {
		t.Fatalf("stdout does not name the resume item:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--resume-from uboot") {
		t.Fatalf("stdout does not print the resume command:\n%s", stdout)
	}
	if !strings.Contains(stdout, "result: FAILED") {
		t.Fatalf("stdout does not report the failure:\n%s", stdout)
	}
}

func TestRunMockTimeoutIsRetried(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	code, stdout, stderr := runCLI(t, "--mock", "--mock-fault", "timeout:boot", "--retries", "1", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "result: ok") {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestRunMockDisconnectAborts(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	code, stdout, stderr := runCLI(t, "--mock", "--mock-fault", "disconnect:boot", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "next item: boot") {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestRunMockResumeFromSkipsItems(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	code, stdout, stderr := runCLI(t, "--mock", "--json", "--resume-from", "boot", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var rep uf.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !rep.Resumed || len(rep.Skipped) != 2 {
		t.Fatalf("resume state = %t skipped %v, want 2 skipped items", rep.Resumed, rep.Skipped)
	}
	if strings.Join(rep.Completed, ",") != "boot" {
		t.Fatalf("completed = %v, want [boot]", rep.Completed)
	}
}

func TestRunRefusesToWriteAVerificationFailure(t *testing.T) {
	data := buildCLIImage(t)
	data[len(data)-8] ^= 0xFF
	path := writeCLIImage(t, data)
	code, _, stderr := runCLI(t, "--mock", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "refusing") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunDryRunPrintsTheEraseDecision(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))

	code, stdout, stderr := runCLI(t, "--dry-run", "--erase-tail", "fill", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"erase semantics (--erase-tail=fill)", "待真机核对", "uncovered ranges"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-run output is missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runCLI(t, "--dry-run", "--json", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var rep Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Erase == nil || rep.Erase.Mode != "none" || rep.Erase.Tails == 0 {
		t.Fatalf("erase decision = %+v", rep.Erase)
	}
}

func TestRunReplayBareImage(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	rep := bareReport(t)
	plan, err := planFromReport(rep)
	if err != nil {
		t.Fatalf("planFromReport: %v", err)
	}
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(tracePath, mustJSON(t, bareTrace(plan)), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}

	code, stdout, stderr := runCLI(t, "--replay", tracePath, "--json", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var got uf.Report
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || !got.Reset {
		t.Fatalf("report = %+v", got)
	}
}

func TestParseMockFaultRejectsBadSpecs(t *testing.T) {
	plan, err := planFromReport(bareReport(t))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, spec := range []string{"", "nope", "timeout", "timeout:missing", "weird:boot", "failed:idb"} {
		if _, err := parseMockFault(spec, plan); err != nil && spec == "" {
			t.Fatalf("empty spec should disable faults: %v", err)
		}
		if spec != "" && spec != "timeout:idb" {
			if _, err := parseMockFault(spec, plan); err == nil && spec != "failed:boot" {
				// "failed:idb" must fail because a bare plan has no IDB stage;
				// "timeout"/"timeout:missing"/"weird:boot" must fail as well.
				t.Fatalf("spec %q was accepted", spec)
			}
		}
	}
	if _, err := parseMockFault("failed:idb", plan); err == nil {
		t.Fatal("failed:idb must be rejected when the plan has no IDB")
	}
	if _, err := parseMockFault("failed:boot", plan); err != nil {
		t.Fatalf("failed:boot should be accepted: %v", err)
	}
}

func TestMockConnectorMode(t *testing.T) {
	plan, err := planFromReport(bareReport(t))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if conn := mockConnector(plan, nil); conn == nil {
		t.Fatal("mockConnector returned nil")
	}
	sectors := mockFlashSectors(plan)
	if sectors == 0 {
		t.Fatal("mock flash has no sectors")
	}
	for _, it := range plan.Items {
		if end := uint64(it.TargetLBA) + uint64(it.PaddedSize/uf.SectorSize); end > uint64(sectors) {
			t.Fatalf("mock flash %d sectors does not cover item %s (end 0x%x)", sectors, it.Name, end)
		}
	}
}

// bareTrace records a full LOADER session for a plan without a loader.
func bareTrace(plan *uf.Plan) uf.Trace {
	var frames []uf.Frame
	add := func(f ...uf.Frame) { frames = append(frames, f...) }

	add(uf.Frame{Kind: uf.FrameCBW, Op: intPtr(int(rockusb.OpTestUnitReady))}, uf.Frame{Kind: uf.FrameCSW})

	chip := make([]byte, 16)
	copy(chip, "3588")
	add(
		uf.Frame{Kind: uf.FrameCBW, Op: intPtr(int(rockusb.OpReadChipInfo))},
		uf.Frame{Kind: uf.FrameReadData, Length: intPtr(16), RespondHex: hex.EncodeToString(chip)},
		uf.Frame{Kind: uf.FrameCSW},
	)
	flash := make([]byte, 11)
	binary.LittleEndian.PutUint32(flash[0:4], uint32(plan.DiskSectors))
	binary.LittleEndian.PutUint16(flash[4:6], 128)
	flash[6] = 8
	flash[7] = 4
	flash[10] = 1
	add(
		uf.Frame{Kind: uf.FrameCBW, Op: intPtr(int(rockusb.OpReadFlashInfo))},
		uf.Frame{Kind: uf.FrameReadData, Length: intPtr(11), RespondHex: hex.EncodeToString(flash)},
		uf.Frame{Kind: uf.FrameCSW},
	)
	for _, it := range plan.Items {
		binary.LittleEndian.PutUint32(flash[0:4], uint32(plan.DiskSectors))
		for off := int64(0); off < it.PaddedSize; off += 128 * 512 {
			n := it.PaddedSize - off
			if n > 128*512 {
				n = 128 * 512
			}
			add(
				uf.Frame{Kind: uf.FrameCBW, Op: intPtr(int(uf.OpWriteLBA)),
					LBA: u32Ptr(it.TargetLBA + uint32(off/512)), Sectors: intPtr(int(n / 512))},
				uf.Frame{Kind: uf.FrameWriteData, Length: intPtr(int(n))},
				uf.Frame{Kind: uf.FrameCSW},
			)
		}
	}
	add(uf.Frame{Kind: uf.FrameCBW, Op: intPtr(int(uf.OpDeviceReset))}, uf.Frame{Kind: uf.FrameCSW})

	return uf.Trace{Schema: uf.TraceSchema, Frames: frames}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

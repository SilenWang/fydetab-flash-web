package imageplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rkcrc"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
)

// --- fixtures ---------------------------------------------------------------

// testLoader builds a valid RKBOOT ("BOOT") container with the three segment
// sizes stage 2 measured, a fixed build time and the chip tag the caller asks
// for. It is a fixture, not a vendor blob.
func testLoader(t *testing.T, chipTag uint32) []byte {
	t.Helper()
	const headerSize, entrySize = 102, 57
	segments := []struct {
		request uint16
		name    string
		size    int
	}{
		{0x0471, "UsbHead", 2048},
		{0x0471, "rk3588_ddr_lp4_2112M", 51200},
		{0x0472, "rk3588_usbplug_v1", 88064},
	}

	offset471 := uint32(headerSize)
	offset472 := offset471 + 2*entrySize
	dataOffset := offset472 + entrySize
	total := int(dataOffset)
	for _, s := range segments {
		total += s.size
	}
	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf[0:4], 0x544F4F42) // "BOOT"
	binary.LittleEndian.PutUint16(buf[4:6], headerSize)
	binary.LittleEndian.PutUint32(buf[6:10], 0x00010006)
	binary.LittleEndian.PutUint32(buf[10:14], 0x01030000)
	binary.LittleEndian.PutUint16(buf[14:16], 2024)
	buf[16] = 9 // September
	buf[17] = 6
	buf[18] = 1
	buf[19] = 2
	buf[20] = 3
	binary.LittleEndian.PutUint32(buf[21:25], chipTag)
	buf[25] = 2
	binary.LittleEndian.PutUint32(buf[26:30], offset471)
	buf[30] = entrySize
	buf[31] = 1
	binary.LittleEndian.PutUint32(buf[32:36], offset472)
	buf[36] = entrySize
	buf[43] = 'N'

	seen471, seen472, dataPos := 0, 0, int(dataOffset)
	for _, s := range segments {
		var slot int
		var typ uint32
		if s.request == 0x0471 {
			slot = int(offset471) + seen471*entrySize
			seen471++
			typ = 1
		} else {
			slot = int(offset472) + seen472*entrySize
			seen472++
			typ = 2
		}
		buf[slot] = entrySize
		binary.LittleEndian.PutUint32(buf[slot+1:slot+5], typ)
		for i, r := range s.name {
			if i >= 20 {
				break
			}
			binary.LittleEndian.PutUint16(buf[slot+5+i*2:], uint16(r))
		}
		binary.LittleEndian.PutUint32(buf[slot+45:slot+49], uint32(dataPos))
		binary.LittleEndian.PutUint32(buf[slot+49:slot+53], uint32(s.size))
		dataPos += s.size
	}
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], crc32Rockchip(buf[:len(buf)-4]))
	return buf
}

// crc32Rockchip reproduces rkdeveloptool's CRC (poly 0x04C10DB7). Duplicated
// here only so the fixture builder stays self-contained.
func crc32Rockchip(b []byte) uint32 {
	var crc uint32
	for _, v := range b {
		crc ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C10DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

const testCmdline = `CMDLINE: mtdparts=rk29xxnand:` +
	`0x00000004@0x00000040(parameter),` +
	`0x0000000c@0x00000044(uboot),` +
	`0x00a00000@0x00000050(state)`

// testImageBytes assembles a sparse-free RKFW image in memory: header, loader,
// RKAF (parameter + uboot + a 1 GiB state payload) and the trailing MD5.
// chipTag is the RKFW header chip field, so a caller can build an image whose
// chip disagrees with the loader it carries.
func testImageBytes(t *testing.T, chipTag uint32) []byte {
	t.Helper()
	loader := testLoader(t, 0x33353838) // "3588"
	param := make([]byte, 2048)
	copy(param, testCmdline)
	uboot := bytes.Repeat([]byte{0x11}, 6144)
	state := bytes.Repeat([]byte{0x22}, 512*64)

	items := []struct {
		name    string
		lba     uint32
		payload []byte
	}{
		{"parameter", 0x40, param},
		{"uboot", 0x44, uboot},
		{"state", 0x50, state},
	}
	rkafHeader := make([]byte, rkfw.RKAFHeaderSize)
	copy(rkafHeader[0:4], rkfw.MagicRKAF[:])
	copy(rkafHeader[8:42], "Synthetic Duo")
	copy(rkafHeader[42:72], "rockchip")
	copy(rkafHeader[72:128], "Rockchip")
	binary.LittleEndian.PutUint32(rkafHeader[132:136], 0x01000000)
	binary.LittleEndian.PutUint32(rkafHeader[136:140], uint32(len(items)))

	body := append([]byte(nil), rkafHeader...)
	pos := uint32(rkfw.RKAFHeaderSize)
	for i, it := range items {
		slot := body[140+i*rkfw.RKAFItemSize : 140+(i+1)*rkfw.RKAFItemSize]
		copy(slot[0:32], it.name)
		copy(slot[32:92], "Image/"+it.name)
		padded := (len(it.payload) + 511) / 512 * 512
		binary.LittleEndian.PutUint32(slot[92:96], rkfw.NoAddress)
		binary.LittleEndian.PutUint32(slot[96:100], pos)
		binary.LittleEndian.PutUint32(slot[100:104], it.lba)
		binary.LittleEndian.PutUint32(slot[104:108], uint32((padded+2047)/2048))
		binary.LittleEndian.PutUint32(slot[108:112], uint32(len(it.payload)))
		buf := make([]byte, padded)
		copy(buf, it.payload)
		body = append(body, buf...)
		pos += uint32(padded)
	}
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(body)))
	rkaf := append(body, make([]byte, 4)...)
	binary.LittleEndian.PutUint32(rkaf[len(rkaf)-4:], rkcrc.Checksum(body))

	head := make([]byte, rkfw.HeaderSize)
	copy(head[0:4], rkfw.MagicRKFW[:])
	binary.LittleEndian.PutUint16(head[4:6], rkfw.HeaderSize)
	binary.LittleEndian.PutUint32(head[6:10], 0x17000001)
	binary.LittleEndian.PutUint16(head[14:16], 2025)
	head[16] = 8
	head[17] = 1
	binary.LittleEndian.PutUint32(head[21:25], chipTag)
	binary.LittleEndian.PutUint32(head[25:29], rkfw.HeaderSize)
	binary.LittleEndian.PutUint32(head[29:33], uint32(len(loader)))
	binary.LittleEndian.PutUint32(head[33:37], uint32(rkfw.HeaderSize+len(loader)))
	binary.LittleEndian.PutUint32(head[37:41], uint32(len(rkaf)))

	img := append(append(append([]byte(nil), head...), loader...), rkaf...)
	// A correctly shaped 32-hex tail: the planner never verifies the RKFW MD5,
	// and rkfw only needs the tail to be present to compute its size.
	return append(img, []byte(md5Hex(img))...)
}

func md5Hex(b []byte) string {
	// rkfw's VerifyMD5 only reads the tail in its own tests; the planner never
	// verifies it, so a correctly shaped 32-hex tail is enough here.
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

func openTestImage(t *testing.T, chipTag uint32) *rkfw.Image {
	t.Helper()
	data := testImageBytes(t, chipTag)
	img, err := rkfw.OpenReaderAt(bytes.NewReader(data), int64(len(data)), "synthetic-update.img")
	if err != nil {
		t.Fatalf("OpenReaderAt: %v", err)
	}
	return img
}

// --- tests ------------------------------------------------------------------

func TestFromImageExtractsTheImagesOwnLoader(t *testing.T) {
	img := openTestImage(t, chipTagRK3588)
	loader, err := FromImage(img)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	if loader.Source != LoaderSourceImage {
		t.Errorf("source = %q, want %q", loader.Source, LoaderSourceImage)
	}
	if loader.FileOffset != img.LoaderOffset || loader.Length != img.LoaderLength {
		t.Errorf("loader located at %d/%d, want %d/%d", loader.FileOffset, loader.Length, img.LoaderOffset, img.LoaderLength)
	}
	if !bytes.Equal(loader.Bytes, testLoader(t, chipTagRK3588)) {
		t.Error("embedded loader bytes differ from the image's RKFW loader slot")
	}
	sum := sha256.Sum256(loader.Bytes)
	if loader.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want the digest of the extracted bytes", loader.SHA256)
	}
	if loader.Chip != "RK3588" || loader.ChipCheck != "ok" || !loader.Matches() {
		t.Errorf("chip = %q check = %q, want RK3588/ok", loader.Chip, loader.ChipCheck)
	}
	if loader.BuildTime != "2024-09-06 01:02:03 UTC" {
		t.Errorf("build time = %q", loader.BuildTime)
	}
	if loader.Version != "0.1.6" || loader.Segments != 3 {
		t.Errorf("version = %q segments = %d", loader.Version, loader.Segments)
	}
	if err := loader.Check(); err != nil {
		t.Errorf("Check() = %v, want nil", err)
	}
	joined := strings.Join(loader.Summary(), "\n")
	for _, want := range []string{"镜像自带", loader.SHA256, "2024-09-06", "RK3588"} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary missing %q:\n%s", want, joined)
		}
	}
}

func TestFromImageRefusesLoaderBuiltForAnotherChip(t *testing.T) {
	// The image is for RK3566 but carries a loader for RK3588 — the shape of
	// the real mismatch (loader from one package, image from another).
	img := openTestImage(t, chipTagRK3566)
	if got := img.RKFW.ChipName(); got != "RK3566" {
		t.Fatalf("image chip = %q, want RK3566", got)
	}
	loader, err := FromImage(img)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	if loader.ChipCheck != "mismatch" || loader.Matches() {
		t.Fatalf("chipCheck = %q, want mismatch", loader.ChipCheck)
	}
	err = loader.Check()
	if err == nil || !strings.Contains(err.Error(), "拒绝 Download Boot") {
		t.Fatalf("Check() = %v, want a refusal naming the mismatch", err)
	}
	if joined := strings.Join(loader.Summary(), "\n"); !strings.Contains(joined, "mismatch") {
		t.Errorf("summary should show the mismatch:\n%s", joined)
	}
}

func TestFromFileIsMarkedAsNotFromTheImage(t *testing.T) {
	data := testLoader(t, chipTagRK3588)
	loader := FromFile("rk3588_spl_loader_v1.06.111.bin", data, "RK3588")
	if loader.Source != LoaderSourceFile {
		t.Errorf("source = %q, want %q", loader.Source, LoaderSourceFile)
	}
	if loader.FileOffset != 0 || loader.Name != "rk3588_spl_loader_v1.06.111.bin" {
		t.Errorf("file loader = offset %d name %q", loader.FileOffset, loader.Name)
	}
	if !strings.Contains(strings.Join(loader.Summary(), "\n"), "非镜像自带") {
		t.Error("summary must mark a user-supplied loader as not from the image")
	}

	bad := FromBytes([]byte("not a loader"), "RK3588")
	if bad.ParseError == "" {
		t.Fatal("garbage bytes must carry a parse error")
	}
	if bad.Check() == nil {
		t.Fatal("Check() must refuse an unparsable loader")
	}
}

func TestFromImageWithoutRKFW(t *testing.T) {
	// A bare RKAF image has no loader: maskrom cannot be bootstrapped from it.
	data := testImageBytes(t, chipTagRK3588)
	full := openTestImage(t, chipTagRK3588)
	bareData := data[full.RKAFOffset:]
	bare, err := rkfw.OpenReaderAt(bytes.NewReader(bareData), int64(len(bareData)), "bare.img")
	if err != nil {
		t.Fatalf("open bare RKAF: %v", err)
	}
	if bare.HasRKFW {
		t.Fatal("bare image should have no RKFW wrapper")
	}
	if _, err := FromImage(bare); err != ErrNoImageLoader {
		t.Fatalf("FromImage(bare) = %v, want ErrNoImageLoader", err)
	}
}

func TestWriterPlanCarriesLoaderIdentity(t *testing.T) {
	img := openTestImage(t, chipTagRK3588)
	raw, err := img.ReadItem(*img.Parameter, MaxParameterBytes)
	if err != nil {
		t.Fatalf("ReadItem(parameter): %v", err)
	}
	doc, err := parameter.Parse(raw)
	if err != nil {
		t.Fatalf("parameter.Parse: %v", err)
	}
	rep, err := BuildReport(img, doc, VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	loader, err := FromImage(img)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	plan, err := WriterPlan(rep, loader)
	if err != nil {
		t.Fatalf("WriterPlan: %v", err)
	}
	if plan.Loader == nil {
		t.Fatal("plan has no loader")
	}
	if plan.Loader.Source != LoaderSourceImage || plan.Loader.NonImage {
		t.Errorf("loader source = %q nonImage = %v", plan.Loader.Source, plan.Loader.NonImage)
	}
	if plan.Loader.SHA256 != loader.SHA256 || plan.Loader.BuildTime == "" || plan.Loader.Chip != "RK3588" {
		t.Errorf("loader identity not carried: %+v", plan.Loader)
	}
	if len(plan.Loader.Bytes) == 0 {
		t.Error("the plan should carry the blob so the runner does not depend on a file offset")
	}
	if plan.Chip != "RK3588" {
		t.Errorf("plan chip = %q", plan.Chip)
	}

	// The same report with a user file is explicitly non-image.
	override, err := WriterPlan(rep, FromFile("other.bin", testLoader(t, chipTagRK3588), "RK3588"))
	if err != nil {
		t.Fatalf("WriterPlan(override): %v", err)
	}
	if !override.Loader.NonImage {
		t.Error("a user-supplied loader must be marked NonImage")
	}
}

const (
	chipTagRK3588 uint32 = 0x33353838 // "3588"
	// "6653" is what an RK3566 image stores; ChipName reverses it to "3566".
	chipTagRK3566 uint32 = 0x33353636
)

// DemoImage is what the page's demo mode plans against: it has to be a real
// image (magic, CRC, MD5, parameter table) or the demo would not exercise the
// same code as a user's file.
func TestDemoImageIsAPlanableImage(t *testing.T) {
	data := DemoImage()
	img, err := rkfw.OpenReaderAt(bytes.NewReader(data), int64(len(data)), DemoImageName)
	if err != nil {
		t.Fatalf("OpenReaderAt(DemoImage): %v", err)
	}
	if !img.HasRKFW || img.RKFW.ChipName() != "RK3588" {
		t.Fatalf("demo image chip = %q (rkfw=%v)", img.RKFW.ChipName(), img.HasRKFW)
	}
	raw, err := img.ReadItem(*img.Parameter, MaxParameterBytes)
	if err != nil {
		t.Fatalf("ReadItem(parameter): %v", err)
	}
	doc, err := parameter.Parse(raw)
	if err != nil {
		t.Fatalf("parameter.Parse: %v", err)
	}
	rep, err := BuildReport(img, doc, VerificationInfo{}, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(rep.Uncovered) == 0 {
		t.Fatal("the demo image must leave a partition tail uncovered so the checklist has something to decide")
	}
	loader, err := FromImage(img)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	if loader.ChipCheck != "ok" || loader.BuildTime != "2024-09-06 01:00:00 UTC" {
		t.Errorf("demo loader = %+v", loader)
	}
	if md5res, err := img.VerifyMD5(); err != nil || !md5res.OK {
		t.Errorf("demo image MD5 = %+v, %v", md5res, err)
	}
	if crc, err := img.VerifyRKAFCRC(); err != nil || !crc.OK {
		t.Errorf("demo image RKAF CRC = %+v, %v", crc, err)
	}
}

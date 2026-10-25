package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/rkcrc"
)

// CLI-level fixture: a bare RKAF image (no RKFW, no MD5) with three
// partitions, all payloads small enough to keep the test instant.
const cliParameter = `FIRMWARE_VER: 23.0.0
TYPE: GPT
CMDLINE: mtdparts=rk29xxnand:0x00001000@0x00000040(parameter),0x00002000@0x00001040(uboot),0x00008000@0x00003040(boot)
`

type cliPart struct {
	name string
	data []byte
	lba  uint32
}

func wrapParam(text string) []byte {
	content := []byte(text)
	out := make([]byte, 8+len(content)+4)
	copy(out, "PARM")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(content)))
	copy(out[8:], content)
	binary.LittleEndian.PutUint32(out[8+len(content):], rkcrc.Checksum(content))
	return out
}

// buildCLIImage writes a 2048-byte RKAF header, the payloads and the CRC
// trailer. It returns the bytes so tests can tamper with them.
func buildCLIImage(t *testing.T) []byte {
	t.Helper()
	parts := []cliPart{
		{name: "parameter", data: wrapParam(cliParameter), lba: 0x40},
		{name: "uboot", data: bytes.Repeat([]byte{0x11}, 1024), lba: 0x1040},
		{name: "boot", data: bytes.Repeat([]byte{0x22}, 2048), lba: 0x3040},
	}

	le := binary.LittleEndian
	header := make([]byte, 2048)
	copy(header[0:4], "RKAF")
	copy(header[8:42], "Synthetic Duo")
	copy(header[42:72], "rockchip")
	copy(header[72:128], "Rockchip")
	le.PutUint32(header[132:136], 0x01000000)
	le.PutUint32(header[136:140], uint32(len(parts)))

	body := append([]byte(nil), header...)
	pos := uint32(2048)
	for i, p := range parts {
		off := 140 + i*112
		slot := body[off : off+112]
		copy(slot[0:32], p.name)
		copy(slot[32:92], "Image/"+p.name)
		size := uint32(len(p.data))
		padded := (size + 511) / 512 * 512
		le.PutUint32(slot[92:96], 0xFFFFFFFF)
		le.PutUint32(slot[96:100], pos)
		le.PutUint32(slot[100:104], p.lba)
		le.PutUint32(slot[104:108], padded)
		le.PutUint32(slot[108:112], size)
		payload := make([]byte, padded)
		copy(payload, p.data)
		body = append(body, payload...)
		pos += padded
	}
	le.PutUint32(body[4:8], uint32(len(body)))
	trailer := make([]byte, 4)
	le.PutUint32(trailer, rkcrc.Checksum(body))
	return append(body, trailer...)
}

func writeCLIImage(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "update.img")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	return path
}

func TestRunDryRunJSON(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dry-run", "--json", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	var rep Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if rep.Container.HasRKFW {
		t.Fatal("bare RKAF image reported HasRKFW")
	}
	if len(rep.Partitions) != 3 {
		t.Fatalf("partitions = %d, want 3", len(rep.Partitions))
	}
	if rep.GPT == nil || !rep.GPT.DiskKnown || !rep.GPT.DiskDerived {
		t.Fatalf("GPT = %+v", rep.GPT)
	}
	if len(rep.Stages) != 4 { // 3 wl + rd (no loader => no ul/gpt)
		t.Fatalf("stages = %+v", rep.Stages)
	}
	for _, st := range rep.Stages[:3] {
		if st.Phase != "wl" {
			t.Fatalf("stage %+v, want wl", st)
		}
	}
	if rep.Stages[3].Phase != "rd" || rep.Stages[3].Progress != 100 {
		t.Fatalf("last stage = %+v", rep.Stages[3])
	}
	if rep.Verification.RKAFCRC == nil || !rep.Verification.RKAFCRC.OK {
		t.Fatalf("verification = %+v", rep.Verification)
	}
	if rep.Verification.MD5 == nil || rep.Verification.MD5.Present {
		t.Fatalf("bare RKAF must report MD5 as not present: %+v", rep.Verification.MD5)
	}
	wantTotal := int64(512 + 1024 + 2048)
	if rep.TotalBytes != wantTotal {
		t.Fatalf("total bytes = %d, want %d", rep.TotalBytes, wantTotal)
	}
	for _, p := range rep.Partitions {
		if p.GPTOnly {
			t.Fatalf("unexpected GPT-only partition: %+v", p)
		}
	}
	// The parameter partition is much larger than its payload, so the tail is
	// reported as uncovered instead of being silently ignored.
	if len(rep.Uncovered) == 0 {
		t.Fatal("expected uncovered ranges for small payloads in larger partitions")
	}
}

func TestRunDryRunText(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dry-run", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"uf --dry-run:",
		"projected stages",
		"partition plan",
		"wl",
		"rd",
		"dry run: nothing was written and no device was opened.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MISMATCH") {
		t.Fatalf("untampered image reported a mismatch:\n%s", out)
	}
}

func TestRunWrongChecksumExitsNonZero(t *testing.T) {
	data := buildCLIImage(t)
	// Corrupt the last payload byte; the RKAF CRC trailer then mismatches.
	data[len(data)-8] ^= 0xFF
	path := writeCLIImage(t, data)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dry-run", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1 (verification failure); stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "RKAF CRC MISMATCH") {
		t.Fatalf("output does not report the CRC mismatch:\n%s", stdout.String())
	}
}

func TestRunRequiresDryRun(t *testing.T) {
	path := writeCLIImage(t, buildCLIImage(t))
	var stdout, stderr bytes.Buffer
	if code := run([]string{path}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--dry-run") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunMissingParameterItem(t *testing.T) {
	data := buildCLIImage(t)
	// Rename the parameter item in the RKAF header so the plan cannot start.
	copy(data[140:140+32], strings.Repeat("x", 32))
	path := writeCLIImage(t, data)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dry-run", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "parameter") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), versionString) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHumanHelpers(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1 << 20, "1.00 MiB"},
		{1 << 30, "1.00 GiB"},
		{5 << 30, "5.00 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := groupDigits(4294967296); got != "4,294,967,296" {
		t.Fatalf("groupDigits = %q", got)
	}
	if got := groupDigits(999); got != "999" {
		t.Fatalf("groupDigits = %q", got)
	}
}

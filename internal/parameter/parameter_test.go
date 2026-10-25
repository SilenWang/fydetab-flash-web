package parameter

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/rkcrc"
)

// syntheticParameter is deliberately not a copy of any real vendor file: the
// numbers are invented so the parser is tested, not the Fydetab layout.
const syntheticParameter = `FIRMWARE_VER: 23.0.0
MACHINE_MODEL: Synthetic Duo
MACHINE_ID: 007
MANUFACTURER: Rockchip
MAGIC: 0x5041524B
ATAG: 0x00200800
MACHINE: 0xffffffff
CHECK_MASK: 0x80
TYPE: GPT
CMDLINE: mtdparts=rk29xxnand:0x00002000@0x00004000(uboot),0x00001000@0x00006000(misc:bootable),0x00020000@0x00007000(boot),-@0x00027000(rootfs:grow)
uuid:rootfs=614e0000-0000-4b53-8000-1d28000054a9
`

func wrapParameter(t *testing.T, text string) []byte {
	t.Helper()
	content := []byte(text)
	out := make([]byte, WrapperHeader+len(content)+WrapperTrailer)
	copy(out, ParMagic)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(content)))
	copy(out[WrapperHeader:], content)
	binary.LittleEndian.PutUint32(out[WrapperHeader+len(content):], rkcrc.Checksum(content))
	return out
}

func TestParseWrapped(t *testing.T) {
	doc, err := Parse(wrapParameter(t, syntheticParameter))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !doc.Wrapped || !doc.CRCAvailable || !doc.CRCOK {
		t.Fatalf("wrapper flags = wrapped:%v available:%v ok:%v", doc.Wrapped, doc.CRCAvailable, doc.CRCOK)
	}
	if doc.DeclaredLength != uint32(len(syntheticParameter)) {
		t.Fatalf("declared length = %d, want %d", doc.DeclaredLength, len(syntheticParameter))
	}
	if got := doc.Fields["MACHINE_MODEL"]; got != "Synthetic Duo" {
		t.Fatalf("MACHINE_MODEL = %q", got)
	}
	if got := doc.UUIDs["rootfs"]; got != "614e0000-0000-4b53-8000-1d28000054a9" {
		t.Fatalf("uuid rootfs = %q", got)
	}
	if doc.PartitionCount != 4 {
		t.Fatalf("partition count = %d, want 4: %+v", doc.PartitionCount, doc.Partitions)
	}

	uboot := doc.Partitions[0]
	if uboot.Name != "uboot" || uboot.First != 0x4000 || uboot.Sectors != 0x2000 || uboot.Grow {
		t.Fatalf("uboot = %+v", uboot)
	}
	if b, ok := uboot.Bytes(); !ok || b != 0x2000*SectorSize {
		t.Fatalf("uboot bytes = %d, ok=%v", b, ok)
	}
	misc := doc.Partitions[1]
	if len(misc.Flags) != 1 || misc.Flags[0] != "bootable" {
		t.Fatalf("misc flags = %v", misc.Flags)
	}
	rootfs := doc.Partitions[3]
	if !rootfs.Grow {
		t.Fatalf("rootfs should grow: %+v", rootfs)
	}
	if _, ok := rootfs.End(); ok {
		t.Fatal("grow partition reported a known end")
	}
	if len(doc.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", doc.Warnings)
	}
}

func TestParseRawText(t *testing.T) {
	doc, err := Parse([]byte(syntheticParameter))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Wrapped || doc.CRCAvailable || doc.CRCOK {
		t.Fatalf("raw text must not report a PARM wrapper: %+v", doc)
	}
	if doc.PartitionCount != 4 {
		t.Fatalf("partition count = %d", doc.PartitionCount)
	}
}

func TestParseCRCMismatch(t *testing.T) {
	data := wrapParameter(t, syntheticParameter)
	data[len(data)-1] ^= 0xFF
	doc, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse must not fail on a CRC mismatch: %v", err)
	}
	if !doc.CRCAvailable || doc.CRCOK {
		t.Fatalf("CRC flags = available:%v ok:%v", doc.CRCAvailable, doc.CRCOK)
	}
	if doc.CRCStored == doc.CRCComputed {
		t.Fatalf("stored and computed CRC should differ: 0x%08X", doc.CRCStored)
	}
}

func TestParseTruncatedWrapper(t *testing.T) {
	data := wrapParameter(t, syntheticParameter)
	// Claim a content length far beyond the item.
	binary.LittleEndian.PutUint32(data[4:8], 1<<20)
	if _, err := Parse(data); err == nil {
		t.Fatal("expected an error for a PARM length overrun")
	} else if !strings.Contains(err.Error(), "overruns") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := Parse([]byte("PARM\x01\x02")); err == nil {
		t.Fatal("expected an error for a 6-byte PARM header")
	}
}

func TestParseNULPadding(t *testing.T) {
	data := wrapParameter(t, syntheticParameter)
	data = append(data, make([]byte, 512)...)
	doc, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.PartitionCount != 4 {
		t.Fatalf("partition count = %d", doc.PartitionCount)
	}
}

func TestParseGrowForms(t *testing.T) {
	text := `CMDLINE: mtdparts=rk29xxnand:0x1000@0x40(one),-@0x1040(two),0x2000@0x1040(three:grow)`
	doc, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Partitions) != 3 {
		t.Fatalf("got %d partitions", len(doc.Partitions))
	}
	if !doc.Partitions[1].Grow {
		t.Fatal("'-' size should mean grow")
	}
	if !doc.Partitions[2].Grow {
		t.Fatal("':grow' flag should mean grow")
	}
	if len(doc.Warnings) == 0 {
		t.Fatal("grow partitions that are not last should produce a warning")
	}
}

func TestParseDecimalSectors(t *testing.T) {
	text := `CMDLINE: mtdparts=rk29xxnand:8192@64(uboot)`
	doc, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := doc.Partitions[0]
	if p.First != 64 || p.Sectors != 8192 {
		t.Fatalf("partition = %+v", p)
	}
}

func TestParseContinuationLine(t *testing.T) {
	text := "CMDLINE: mtdparts=rk29xxnand:0x1000@0x40(one),\\\n0x2000@0x1040(two)\n"
	doc, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Partitions) != 2 || doc.Partitions[1].Name != "two" {
		t.Fatalf("partitions = %+v", doc.Partitions)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"missing offset", "CMDLINE: mtdparts=rk29xxnand:0x1000(one)", "missing '@offset'"},
		{"missing name", "CMDLINE: mtdparts=rk29xxnand:0x1000@0x40", "missing a (name)"},
		{"unterminated name", "CMDLINE: mtdparts=rk29xxnand:0x1000@0x40(one", "unterminated"},
		{"duplicate name", "CMDLINE: mtdparts=rk29xxnand:0x1000@0x40(one),0x1000@0x1040(one)", "duplicate"},
		{"zero size", "CMDLINE: mtdparts=rk29xxnand:0x0@0x40(one)", "zero size"},
		{"bad number", "CMDLINE: mtdparts=rk29xxnand:zz@0x40(one)", "size"},
		{"empty offset", "CMDLINE: mtdparts=rk29xxnand:0x1000@(one)", "empty offset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.text))
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestParseNoCmdline(t *testing.T) {
	doc, err := Parse([]byte("FIRMWARE_VER: 1.0.0\nMACHINE_MODEL: X\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.PartitionCount != 0 {
		t.Fatalf("partition count = %d", doc.PartitionCount)
	}
	if len(doc.Warnings) == 0 {
		t.Fatal("missing CMDLINE should warn")
	}
}

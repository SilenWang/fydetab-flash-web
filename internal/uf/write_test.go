package uf

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
)

func intPtr(v int) *int       { return &v }
func u32Ptr(v uint32) *uint32 { return &v }

func TestEncodeCommandCBWWriteLBA(t *testing.T) {
	cbw, err := EncodeCommandCBW(0x52534B01, OpWriteLBA, 0x1234, 128, 128*SectorSize, methodImage)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(cbw) != rockusb.CBWLength {
		t.Fatalf("CBW is %d bytes, want %d", len(cbw), rockusb.CBWLength)
	}
	le := binary.LittleEndian
	if got := le.Uint32(cbw[0:4]); got != rockusb.CBWSignature {
		t.Fatalf("signature = 0x%08X", got)
	}
	if got := le.Uint32(cbw[4:8]); got != 0x52534B01 {
		t.Fatalf("tag = 0x%08X", got)
	}
	if got := le.Uint32(cbw[8:12]); got != 128*SectorSize {
		t.Fatalf("transfer length = %d", got)
	}
	if cbw[12] != 0x00 { // DIRECTION_OUT
		t.Fatalf("flags = 0x%02X, want 0x00 (host to device)", cbw[12])
	}
	if cbw[13] != 0 {
		t.Fatalf("LUN = %d", cbw[13])
	}
	if cbw[14] != 0x0a { // WRITE_LBA is a 10-byte CDB
		t.Fatalf("cbwcb length = 0x%02X, want 0x0a", cbw[14])
	}
	if cbw[15] != byte(OpWriteLBA) {
		t.Fatalf("opcode = 0x%02X", cbw[15])
	}
	if cbw[16] != methodImage {
		t.Fatalf("subcode = 0x%02X, want RWMETHOD_IMAGE", cbw[16])
	}
	if got := binary.BigEndian.Uint32(cbw[17:21]); got != 0x1234 {
		t.Fatalf("address = 0x%x", got)
	}
	if got := binary.BigEndian.Uint16(cbw[22:24]); got != 128 {
		t.Fatalf("usLength = %d", got)
	}
}

func TestEncodeCommandCBWEraseAndReset(t *testing.T) {
	le := binary.LittleEndian

	erase, err := EncodeCommandCBW(7, OpEraseLBA, 0x2000, 16, 0, methodImage)
	if err != nil {
		t.Fatalf("encode erase: %v", err)
	}
	if le.Uint32(erase[8:12]) != 0 {
		t.Fatalf("ERASE_LBA transfer length = %d, want 0 (no data stage)", le.Uint32(erase[8:12]))
	}
	if erase[14] != 0x0a || erase[15] != byte(OpEraseLBA) {
		t.Fatalf("erase cbwcb length/opcode = 0x%02X/0x%02X", erase[14], erase[15])
	}
	if binary.BigEndian.Uint32(erase[17:21]) != 0x2000 || binary.BigEndian.Uint16(erase[22:24]) != 16 {
		t.Fatalf("erase address/length = 0x%x/%d", binary.BigEndian.Uint32(erase[17:21]), binary.BigEndian.Uint16(erase[22:24]))
	}

	reset, err := EncodeCommandCBW(8, OpDeviceReset, 0, 0, 0, ResetSubcodeNone)
	if err != nil {
		t.Fatalf("encode reset: %v", err)
	}
	if reset[14] != 0x06 || reset[15] != byte(OpDeviceReset) || reset[16] != ResetSubcodeNone {
		t.Fatalf("reset cbwcb length/opcode/subcode = 0x%02X/0x%02X/0x%02X", reset[14], reset[15], reset[16])
	}
}

func TestEncodeCommandCBWRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name string
		op   Op
		lba  uint32
		sec  uint16
		len  uint32
	}{
		{"write with no sectors", OpWriteLBA, 0, 0, 0},
		{"write length mismatch", OpWriteLBA, 0, 2, 1000},
		{"write above the 128-sector transfer limit", OpWriteLBA, 0, MaxTransferSectors + 1, (MaxTransferSectors + 1) * SectorSize},
		{"erase with no sectors", OpEraseLBA, 0, 0, 0},
		{"erase above the 32768-sector limit", OpEraseLBA, 0, MaxEraseSectors + 1, 0},
		{"reset with an address", OpDeviceReset, 1, 0, 0},
		{"unknown write-path opcode", Op(0x05), 0, 1, 512},
	}
	for _, c := range cases {
		if _, err := EncodeCommandCBW(1, c.op, c.lba, c.sec, c.len, 0); err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
	}
}

func TestWriteLBAsSplitsIntoUpstreamTransfers(t *testing.T) {
	data := bytes.Repeat([]byte{0xA5}, (MaxTransferSectors+1)*SectorSize)
	lba := uint32(0x40)

	frames := []Frame{
		{Kind: FrameCBW, Op: intPtr(int(OpWriteLBA)), LBA: u32Ptr(lba), Sectors: intPtr(MaxTransferSectors)},
		{Kind: FrameWriteData, Length: intPtr(MaxTransferSectors * SectorSize)},
		{Kind: FrameCSW},
		{Kind: FrameCBW, Op: intPtr(int(OpWriteLBA)), LBA: u32Ptr(lba + MaxTransferSectors), Sectors: intPtr(1)},
		{Kind: FrameWriteData, Length: intPtr(SectorSize)},
		{Kind: FrameCSW},
	}
	rt := NewReplayTransport(frames)
	w := newWriter(rt, simLink(rockusb.ModeLoader), testOptions(), &tracer{})
	if err := w.WriteLBAs(context.Background(), lba, data); err != nil {
		t.Fatalf("WriteLBAs: %v", err)
	}
	if !rt.Done() {
		t.Fatalf("%d recorded frames were not consumed", rt.Remaining())
	}
	if err := rt.Err(); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestEraseLBAsSplitsLikeErasePartition(t *testing.T) {
	frames := []Frame{
		{Kind: FrameCBW, Op: intPtr(int(OpEraseLBA)), LBA: u32Ptr(0), Sectors: intPtr(MaxEraseSectors)},
		{Kind: FrameCSW},
		{Kind: FrameCBW, Op: intPtr(int(OpEraseLBA)), LBA: u32Ptr(MaxEraseSectors), Sectors: intPtr(1000)},
		{Kind: FrameCSW},
	}
	rt := NewReplayTransport(frames)
	w := newWriter(rt, simLink(rockusb.ModeLoader), testOptions(), &tracer{})
	if err := w.EraseLBAs(context.Background(), 0, MaxEraseSectors+1000); err != nil {
		t.Fatalf("EraseLBAs: %v", err)
	}
	if !rt.Done() {
		t.Fatalf("%d recorded frames were not consumed", rt.Remaining())
	}
}

func TestWriteRejectsUnalignedAndOutOfRange(t *testing.T) {
	w := newWriter(NewReplayTransport(nil), simLink(rockusb.ModeLoader), testOptions(), &tracer{})
	if err := w.WriteLBAs(context.Background(), 0, make([]byte, 100)); err == nil {
		t.Fatal("expected an error for a 100-byte write")
	}
	if err := w.WriteLBAs(context.Background(), MaxLBA, make([]byte, 2*SectorSize)); err == nil {
		t.Fatal("expected an error for a write past the 32-bit LBA space")
	}
	if err := w.EraseLBAs(context.Background(), MaxLBA, 2); err == nil {
		t.Fatal("expected an error for an erase past the 32-bit LBA space")
	}
}

func TestWriteRetriesTimeoutsOnly(t *testing.T) {
	lba := uint32(0x40)
	sim := newSimulator(SimConfig{
		FlashSectors: 0x1000,
		Faults:       []SimFault{{Kind: SimTimeout, LBA: &lba, Times: 1}},
	}, rockusb.ModeLoader, nil)
	opts := testOptions()
	opts.Retries = 1
	var attempts []int
	opts.Trace = func(tr rockusb.Transfer) {
		if tr.Stage == "data" {
			attempts = append(attempts, tr.Attempt)
		}
	}
	w := newWriter(sim, sim.link, opts, &tracer{fn: opts.Trace})
	if err := w.WriteLBAs(context.Background(), lba, bytes.Repeat([]byte{0x11}, SectorSize)); err != nil {
		t.Fatalf("WriteLBAs after a retried timeout: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("data transfer attempts = %v, want [1 2]", attempts)
	}
	if len(sim.Writes()) != 1 {
		t.Fatalf("writes applied = %d, want 1 (the timed-out attempt must not apply)", len(sim.Writes()))
	}
}

func TestWriteDoesNotRetryCSWFailure(t *testing.T) {
	lba := uint32(0x80)
	sim := newSimulator(SimConfig{
		FlashSectors: 0x1000,
		Faults:       []SimFault{{Kind: SimFailed, LBA: &lba, Times: -1}},
	}, rockusb.ModeLoader, nil)
	opts := testOptions()
	opts.Retries = 3
	dataAttempts := 0
	opts.Trace = func(tr rockusb.Transfer) {
		if tr.Stage == "data" {
			dataAttempts++
		}
	}
	w := newWriter(sim, sim.link, opts, &tracer{fn: opts.Trace})
	err := w.WriteLBAs(context.Background(), lba, bytes.Repeat([]byte{0x22}, SectorSize))
	if err == nil {
		t.Fatal("a CSW failure must not be reported as success")
	}
	if code := CodeOf(err); code != rockusb.ErrFailed {
		t.Fatalf("error code = %s, want ERR_FAILED", CodeName(code))
	}
	if dataAttempts != 1 {
		t.Fatalf("data attempts = %d, want 1: a failed CSW is a definitive answer", dataAttempts)
	}
	var werr *Error
	if !asError(err, &werr) {
		t.Fatalf("error is %T, want *uf.Error", err)
	}
	if werr.LBA != lba || werr.Stage != "csw" {
		t.Fatalf("error context = %+v", werr)
	}
}

func TestWriteCancellationStopsBeforeSending(t *testing.T) {
	w := newWriter(NewReplayTransport(nil), simLink(rockusb.ModeLoader), testOptions(), &tracer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.WriteLBAs(ctx, 0x40, make([]byte, SectorSize))
	if code := CodeOf(err); code != ErrCancelled {
		t.Fatalf("error code = %s, want ERR_CANCELLED (%v)", CodeName(code), err)
	}
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// SIL-515: READ_LBA on the write path uses the same big-endian address/length
// and 10-byte CBWCB as WRITE_LBA; otherwise the loader returns phase error and
// zero data, which made the IDB probe look empty and the readback fail.
func TestEncodeCommandCBWReadLBAWireFormat(t *testing.T) {
	cbw, err := EncodeCommandCBW(0x0A0B0C0D, OpReadLBA, 64, 8, 8*SectorSize, methodImage)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if cbw[12] != 0x80 {
		t.Fatalf("flags = 0x%02X, want 0x80 (device to host)", cbw[12])
	}
	if cbw[14] != 0x0a || cbw[15] != byte(OpReadLBA) {
		t.Fatalf("cbwcb length/opcode = 0x%02X/0x%02X", cbw[14], cbw[15])
	}
	if got := binary.BigEndian.Uint32(cbw[17:21]); got != 64 {
		t.Fatalf("dwAddress = 0x%x, want 0x40", got)
	}
	if got := binary.BigEndian.Uint16(cbw[22:24]); got != 8 {
		t.Fatalf("usLength = %d, want 8", got)
	}
	if got := binary.LittleEndian.Uint32(cbw[8:12]); got != 8*SectorSize {
		t.Fatalf("transfer length = %d, want %d", got, 8*SectorSize)
	}
}

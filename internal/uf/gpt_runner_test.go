package uf

// Behaviour of the partition-table half of the gpt stage against a simulated
// device: the table is written first and last, the parameter block is
// replicated, and both are read back before a single payload sector goes out.

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/SilenWang/rk3588-webflasher/internal/gpt"
	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// corruptReads flips the first byte of the data stage of READ_LBA for one LBA,
// which is how a device that does not hold what was written looks to the
// read-back comparison.
type corruptReads struct {
	inner  rockusb.Transport
	lba    uint32
	armed  bool
	length int
}

func (c *corruptReads) ControlOut(requestType, request, value, index uint16, data []byte) (int, error) {
	return c.inner.ControlOut(requestType, request, value, index, data)
}

func (c *corruptReads) BulkOut(endpoint uint8, data []byte) (int, error) {
	if len(data) == rockusb.CBWLength && binary.LittleEndian.Uint32(data[0:4]) == rockusb.CBWSignature {
		op := Op(data[15])
		lba := binary.LittleEndian.Uint32(data[17:21])
		// Only the read back of the target LBA is corrupted: the IDB probe must
		// still work, otherwise the run stops earlier for another reason.
		c.armed = op == OpReadLBA && lba == c.lba && lba != IDBDefaultLBA
	}
	return c.inner.BulkOut(endpoint, data)
}

func (c *corruptReads) BulkIn(endpoint uint8, length int) ([]byte, error) {
	b, err := c.inner.BulkIn(endpoint, length)
	if err == nil && c.armed && length != rockusb.CSWLength && len(b) > 0 {
		b = append([]byte(nil), b...)
		b[0] ^= 0xFF
		c.armed = false
	}
	return b, err
}

func (c *corruptReads) Reset() error { return c.inner.Reset() }

type corruptConnector struct {
	inner Connector
	lba   uint32
}

func (c corruptConnector) Open(ctx context.Context) (rockusb.Transport, rockusb.Link, error) {
	tr, link, err := c.inner.Open(ctx)
	if err != nil {
		return tr, link, err
	}
	return &corruptReads{inner: tr, lba: c.lba}, link, nil
}

func (c corruptConnector) Close() error { return c.inner.Close() }

func gptOptions() Options {
	opts := testOptions()
	opts.SkipReadback = false
	return opts
}

// The whole stage against a simulator: primary GPT at LBA 0, backup GPT at the
// last sectors, eight parameter copies with the block's own checksum intact,
// and the payload items afterwards.
func TestRunWritesGPTAndParameterBeforeThePayload(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	conn := NewSimConnector(testSimConfig(plan))

	rep, err := runPlan(t, plan, layout, conn, gptOptions())
	if err != nil {
		t.Fatalf("run: %v (report %+v)", err, rep)
	}

	// 1. Both GPT regions hold exactly the bytes the encoder produced for the
	//    device's flash size.
	lay, err := gpt.Build(plan.GPT.Partitions, plan.DiskSectors)
	if err != nil {
		t.Fatalf("build the expected layout: %v", err)
	}
	want, err := gpt.Encode(lay, gpt.EncodeOptions{Seed: gptSeed(plan.GPT.Partitions)})
	if err != nil {
		t.Fatalf("encode the expected table: %v", err)
	}
	gotPrimary, err := conn.FlashAt(0, len(want.Primary))
	if err != nil {
		t.Fatalf("read primary GPT: %v", err)
	}
	if !bytes.Equal(gotPrimary, want.Primary) {
		t.Fatal("flash does not hold the primary GPT")
	}
	gotBackup, err := conn.FlashAt(uint32(want.BackupLBA), len(want.Backup))
	if err != nil {
		t.Fatalf("read backup GPT: %v", err)
	}
	if !bytes.Equal(gotBackup, want.Backup) {
		t.Fatal("flash does not hold the backup GPT")
	}
	parsed, err := gpt.Parse(gotPrimary, gotBackup)
	if err != nil {
		t.Fatalf("the written table does not parse: %v", err)
	}
	if len(parsed.Entries) != len(plan.GPT.Partitions) {
		t.Fatalf("written table has %d entries, want %d", len(parsed.Entries), len(plan.GPT.Partitions))
	}
	if rep.GPT == nil || !rep.GPT.Verified || rep.GPT.PrimaryLBA != 0 || rep.GPT.BackupLBA != want.BackupLBA {
		t.Fatalf("GPT report = %+v", rep.GPT)
	}

	// 2. Every parameter copy sits where the plan says and carries a PARM block
	//    whose checksum still verifies.
	block := layout.paramBlock
	for _, replica := range plan.GPT.Parameter.Replicas() {
		got, err := conn.FlashAt(uint32(replica.LBA), int(plan.GPT.Parameter.CopyBytes))
		if err != nil {
			t.Fatalf("read parameter copy %d: %v", replica.Index, err)
		}
		if !bytes.Equal(got[:len(block)], block) {
			t.Fatalf("parameter copy %d does not hold the block", replica.Index)
		}
		for _, b := range got[len(block):] {
			if b != 0 {
				t.Fatalf("parameter copy %d has non-zero padding", replica.Index)
			}
		}
		doc, err := parameter.Parse(got)
		if err != nil {
			t.Fatalf("copy %d: parse: %v", replica.Index, err)
		}
		if !doc.CRCOK || doc.PartitionCount != len(plan.GPT.Partitions) {
			t.Fatalf("copy %d: CRC ok=%t partitions=%d", replica.Index, doc.CRCOK, doc.PartitionCount)
		}
	}
	if rep.Parameter == nil || rep.Parameter.Written != plan.GPT.Parameter.Copies || !rep.Parameter.Verified {
		t.Fatalf("parameter report = %+v", rep.Parameter)
	}

	// 3. Ordering: the table and the parameter block are written before the
	//    first payload sector.
	firstItemLBA := uint32(1 << 30)
	for _, it := range plan.Items {
		if it.TargetLBA < firstItemLBA {
			firstItemLBA = it.TargetLBA
		}
	}
	lastTableLBA := uint32(want.BackupLBA)
	seenItem := false
	for _, w := range conn.Last().Writes() {
		inItem := w.LBA >= firstItemLBA
		if inItem {
			seenItem = true
			continue
		}
		if seenItem && w.LBA <= lastTableLBA {
			t.Fatalf("write at LBA 0x%x came after the payload started", w.LBA)
		}
	}
}

// A device that does not hold the table it accepted must stop the run: writing
// the payload on top of an unknown partition table is how a device ends up
// unbootable.
func TestRunAbortsWhenTheGPTReadBackDiffers(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	conn := corruptConnector{inner: NewSimConnector(testSimConfig(plan)), lba: 0}

	rep, err := runPlan(t, plan, layout, conn, gptOptions())
	if err == nil {
		t.Fatalf("run succeeded with a corrupted GPT read-back (report %+v)", rep)
	}
	if rep.Error == nil || rep.Error.Code != ErrReadback {
		t.Fatalf("error = %+v, want code %s", rep.Error, CodeName(ErrReadback))
	}
	if rep.Error == nil || rep.Error.Phase != PhaseIDB {
		t.Fatalf("error = %+v, want the gpt phase", rep.Error)
	}
	if len(rep.Items) != 0 {
		t.Fatalf("items were written after a failed read-back: %+v", rep.Items)
	}
}

// The same rule for the parameter copies: any copy that does not read back must
// stop the run before the payload.
func TestRunAbortsWhenAParameterCopyReadBackDiffers(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	lba := uint32(plan.GPT.Parameter.BaseLBA + 2*plan.GPT.Parameter.StrideLBA)
	conn := corruptConnector{inner: NewSimConnector(testSimConfig(plan)), lba: lba}

	rep, err := runPlan(t, plan, layout, conn, gptOptions())
	if err == nil {
		t.Fatalf("run succeeded with a corrupted parameter read-back (report %+v)", rep)
	}
	if rep.Error == nil || rep.Error.Code != ErrReadback {
		t.Fatalf("error = %+v, want code %s", rep.Error, CodeName(ErrReadback))
	}
	if len(rep.Items) != 0 {
		t.Fatalf("items were written after a failed read-back: %+v", rep.Items)
	}
}

// A copy that would land inside a partition this run does not rewrite is left
// out, and the rest are still written: the device keeps a partition table
// without losing data the run was told to keep.
func TestRunSkipsParameterCopiesOverDataItWillNotWrite(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	// Add a partition with no payload item: its sectors are not rewritten, so
	// a parameter copy must not land there. The parameter block has to describe
	// the same table, which setPartitions takes care of.
	parts := append(fixturePartitions(), parameter.Partition{Name: "keep", First: 0x3000, Sectors: 0x100})
	layout.setPartitions(t, plan, parts)
	conn := NewSimConnector(testSimConfig(plan))

	rep, err := runPlan(t, plan, layout, conn, gptOptions())
	if err != nil {
		t.Fatalf("run: %v (report %+v)", err, rep)
	}
	if rep.Parameter == nil || rep.Parameter.Skipped == 0 {
		t.Fatalf("parameter report = %+v, want skipped copies", rep.Parameter)
	}
	if rep.Parameter.Written != plan.GPT.Parameter.Copies-rep.Parameter.Skipped {
		t.Fatalf("written %d + skipped %d != %d copies", rep.Parameter.Written, rep.Parameter.Skipped, plan.GPT.Parameter.Copies)
	}
	for _, reason := range rep.Parameter.SkippedCopies {
		if !strings.Contains(reason, "keep") {
			t.Fatalf("skipped copy reason = %q, want it to name the partition", reason)
		}
	}
	// The skipped copies must still hold what was there before.
	empty, err := conn.FlashAt(0x3000, 16)
	if err != nil {
		t.Fatalf("read the kept partition: %v", err)
	}
	if !bytes.Equal(empty, make([]byte, 16)) {
		t.Fatal("a parameter copy was written into a partition the run keeps")
	}
}

// A parameter entry without a reserved area cannot be placed; the plan refuses
// before any device is opened.
func TestPlanWithoutParameterGeometryIsRejected(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.GPT.Parameter.StrideLBA = 0
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "zero stride") {
		t.Fatalf("Validate = %v, want a zero-stride error", err)
	}
	plan = layout.plan(t)
	plan.GPT.Parameter = nil
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "no parameter block") {
		t.Fatalf("Validate = %v, want a missing-parameter error", err)
	}
	plan = layout.plan(t)
	plan.GPT.Partitions = nil
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "no partitions") {
		t.Fatalf("Validate = %v, want a missing-partitions error", err)
	}
}

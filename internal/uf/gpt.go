package uf

// The partition-table half of the gpt stage: the GUID partition table derived
// from the parameter table, and the parameter block the device reads.
//
// Why this cannot be left to the device: the table is not part of the image.
// The host writes it, and once a device's flash has been written with anything
// else, "the table that is already there" is no longer a safe assumption. Both
// pieces are written before the first payload item, and both are read back
// before the run continues: a table the device does not hold is worse than no
// table at all, because the device would find its partitions somewhere else.

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/SilenWang/rk3588-webflasher/internal/gpt"
	"github.com/SilenWang/rk3588-webflasher/internal/parameter"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// GPTReport records the partition table write.
type GPTReport struct {
	// PrimaryLBA/PrimaryBytes and BackupLBA/BackupBytes are the two transfers.
	PrimaryLBA   uint32 `json:"primaryLba"`
	PrimaryBytes int64  `json:"primaryBytes"`
	BackupLBA    uint64 `json:"backupLba"`
	BackupBytes  int64  `json:"backupBytes"`
	// DiskSectors is the flash size the table was built for.
	DiskSectors uint64 `json:"diskSectors"`
	// Entries is how many partitions the table describes.
	Entries int `json:"entries"`
	// Verified reports that the primary region was read back and matched.
	Verified bool `json:"verified"`
	// BackupVerified reports the backup region read back and matched.
	BackupVerified bool `json:"backupVerified"`
	// BackupUnverified is set when the backup region lies outside the loader's
	// reliable READ_LBA window (or still reads erased), so its write cannot be
	// checked. SIL-515: reads at or above 32 MiB return 0xCC while writes there
	// work (a v23 install written above 32 MiB boots), so an unverifiable
	// backup is a warning, not an abort. The primary table is verified and is
	// what U-Boot reads.
	BackupUnverified bool `json:"backupUnverified,omitempty"`
	// Rebuilt marks a table built with the device's flash size where the plan
	// used a different (derived) one.
	Rebuilt bool `json:"rebuilt,omitempty"`
}

// ParameterReport records the parameter block write.
type ParameterReport struct {
	BaseLBA   uint64 `json:"baseLba"`
	StrideLBA uint64 `json:"strideLba"`
	Copies    int    `json:"copies"`
	// Written/Skipped count the copies, Bytes the transferred bytes.
	Written int   `json:"written"`
	Skipped int   `json:"skipped"`
	Bytes   int64 `json:"bytes"`
	// CopyBytes is the size of one copy.
	CopyBytes int64 `json:"copyBytes"`
	// Verified reports that every written copy was read back and matched.
	Verified bool `json:"verified"`
	// SkippedCopies names the copies that were left out and why.
	SkippedCopies []string `json:"skippedCopies,omitempty"`
}

// writeGPT builds the partition table for the device's real flash size, writes
// the primary and the backup region, and reads both back.
func (r *runner) writeGPT(ctx context.Context) (int64, error) {
	gp := r.plan.GPT
	if gp == nil {
		return 0, nil
	}
	disk := uint64(r.rep.Device.Flash.FlashSizeSectors)
	if disk > uint64(MaxLBA)+1 {
		return 0, r.fail(PhaseIDB, "", rockusb.ErrDeviceNotSupport,
			fmt.Sprintf("设备报告的容量 %d 扇区超出 32 位 LBA 空间", disk),
			"分区表的 LBA 是 32 位的；确认 Get FlashInfo 的返回值。", nil)
	}
	if disk == 0 {
		return 0, r.fail(PhaseIDB, "", rockusb.ErrDeviceNotSupport,
			"设备没有报告 flash 容量，无法确定备份分区表的位置",
			"分区表必须写在磁盘末尾：先确认 Get FlashInfo 能读到容量，再重跑。", nil)
	}

	lay, err := gpt.Build(gp.Partitions, disk)
	if err != nil {
		return 0, r.fail(PhaseIDB, "", ErrSignature, fmt.Sprintf("按设备容量 %d 扇区生成分区表失败: %v", disk, err),
			"parameter 的分区表和设备容量对不上；不要继续写入。", err)
	}
	rebuilt := false
	if r.plan.DiskKnown && r.plan.DiskSectors > 0 && r.plan.DiskSectors != disk {
		rebuilt = true
		warning := fmt.Sprintf("计划按 %d 扇区生成分区表，设备报告 %d 扇区：已按设备容量重建%s",
			r.plan.DiskSectors, disk, derivedNote(r.plan.DiskDerived))
		r.rep.Warnings = append(r.rep.Warnings, warning)
		r.event(PhaseIDB, "warn", "", warning)
	}
	// The table must describe the same partitions the plan approved: a rebuilt
	// table with different bounds would silently move the data writes.
	if err := samePartitions(gp.Partitions, lay.Entries); err != nil {
		return 0, r.fail(PhaseIDB, "", ErrSignature, err.Error(),
			"分区表与计划不一致；不要继续写入。", err)
	}

	img, err := gpt.Encode(lay, gpt.EncodeOptions{Seed: gptSeed(gp.Partitions)})
	if err != nil {
		return 0, r.fail(PhaseIDB, "", ErrSignature, fmt.Sprintf("序列化分区表失败: %v", err),
			"这是本工具的内部错误，不会写出可信的分区表；停止。", err)
	}
	// Self-check: the bytes must parse back as a GPT before they reach flash.
	parsed, err := gpt.Parse(img.Primary, img.Backup)
	if err != nil {
		return 0, r.fail(PhaseIDB, "", ErrSignature, fmt.Sprintf("生成的 GPT 自检失败: %v", err),
			"这是本工具的内部错误，不会写出可信的分区表；停止。", err)
	}
	if len(parsed.Entries) != len(lay.Entries) {
		return 0, r.fail(PhaseIDB, "", ErrSignature,
			fmt.Sprintf("GPT 自检：写入了 %d 个分区项，期望 %d 个", len(parsed.Entries), len(lay.Entries)),
			"这是本工具的内部错误；停止。", nil)
	}

	rep := &GPTReport{
		PrimaryLBA:   uint32(img.PrimaryLBA),
		PrimaryBytes: int64(len(img.Primary)),
		BackupLBA:    img.BackupLBA,
		BackupBytes:  int64(len(img.Backup)),
		DiskSectors:  disk,
		Entries:      len(lay.Entries),
		Rebuilt:      rebuilt,
	}
	r.rep.GPT = rep
	r.event(PhaseIDB, "info", "", fmt.Sprintf(
		"分区表：%d 项，主 GPT %d 字节 @ LBA 0，备份 GPT %d 字节 @ LBA 0x%x（设备 %d 扇区）",
		rep.Entries, rep.PrimaryBytes, rep.BackupBytes, img.BackupLBA, disk))

	total := int64(len(img.Primary)) + int64(len(img.Backup))
	done := int64(0)
	if err := r.writeBuffer(ctx, PhaseIDB, "", uint32(img.PrimaryLBA), img.Primary, &done, total); err != nil {
		return 0, err
	}
	if err := r.writeBuffer(ctx, PhaseIDB, "", uint32(img.BackupLBA), img.Backup, &done, total); err != nil {
		return 0, err
	}

	verified := true
	if err := r.readBackCompare(PhaseIDB, "", img.PrimaryLBA, img.Primary, "主 GPT（LBA 0）"); err != nil {
		return 0, err
	}
	backupVerified, err := r.verifyBackupGPT(img.BackupLBA, img.Backup, fmt.Sprintf("备份 GPT（LBA 0x%x）", img.BackupLBA))
	if err != nil {
		return 0, err
	}
	rep.BackupVerified = backupVerified
	rep.BackupUnverified = !backupVerified && !r.opts.SkipReadback
	if r.opts.SkipReadback {
		verified = false
	}
	rep.Verified = verified
	return int64(len(img.Primary)) + int64(len(img.Backup)), nil
}

// writeParameter writes the parameter block, replicated across its reserved
// area, and reads every copy back.
func (r *runner) writeParameter(ctx context.Context) (int64, error) {
	if r.plan.GPT == nil || r.plan.GPT.Parameter == nil {
		return 0, nil
	}
	pp := r.plan.GPT.Parameter
	block, err := r.readAt(pp.FileOffset, pp.Size)
	if err != nil {
		return 0, r.fail(PhaseIDB, "", rockusb.ErrFailed,
			fmt.Sprintf("读取 parameter 块失败（offset 0x%x, %d B）: %v", pp.FileOffset, pp.Size, err),
			"镜像可能被移动或截断；不要继续写入。", err)
	}
	// The block is checked whether or not the item-signature gate is on: this
	// is local data and it decides the device's partition table, so a block
	// whose checksum does not verify must never reach flash.
	doc, err := checkParameterBlock(block)
	if err != nil {
		return 0, r.fail(PhaseIDB, "", ErrSignature, err.Error(),
			"parameter 块不带有效的 PARM 校验；写进去会把分区表写坏，拒绝写入。", err)
	}
	// The block the device will read must describe the same partitions as the
	// table this run writes. If they disagree, one of them is wrong and the run
	// would leave the device with two conflicting layouts.
	if err := comparePartitions(r.plan.GPT.Partitions, doc.Partitions); err != nil {
		return 0, r.fail(PhaseIDB, "", ErrSignature, err.Error(),
			"parameter 与分区表互相矛盾：先确认镜像的解析结果，不要写入。", err)
	}

	buf := make([]byte, pp.CopyBytes)
	copy(buf, block)

	rep := &ParameterReport{
		BaseLBA:   pp.BaseLBA,
		StrideLBA: pp.StrideLBA,
		Copies:    pp.Copies,
		CopyBytes: pp.CopyBytes,
	}
	r.rep.Parameter = rep
	r.event(PhaseIDB, "info", "", fmt.Sprintf(
		"parameter：%d 字节，%d 份副本 x %d 字节，起始 LBA 0x%x，间隔 0x%x",
		pp.Size, pp.Copies, pp.CopyBytes, pp.BaseLBA, pp.StrideLBA))

	for _, replica := range pp.Replicas() {
		if reason := r.replicaConflict(replica); reason != "" {
			rep.Skipped++
			rep.SkippedCopies = append(rep.SkippedCopies, fmt.Sprintf("LBA 0x%x: %s", replica.LBA, reason))
			r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf(
				"parameter 副本 %d（LBA 0x%x）未写入：%s", replica.Index, replica.LBA, reason))
			r.event(PhaseIDB, "warn", "", fmt.Sprintf(
				"parameter 副本 %d（LBA 0x%x）跳过：%s", replica.Index, replica.LBA, reason))
			continue
		}
		done := int64(0)
		if err := r.writeBuffer(ctx, PhaseIDB, "", uint32(replica.LBA), buf, &done, pp.Bytes()); err != nil {
			return 0, err
		}
		if err := r.readBackCompare(PhaseIDB, "", replica.LBA, buf,
			fmt.Sprintf("parameter 副本 %d（LBA 0x%x）", replica.Index, replica.LBA)); err != nil {
			return 0, err
		}
		rep.Written++
		rep.Bytes += int64(len(buf))
	}
	if rep.Written == 0 {
		return 0, r.fail(PhaseIDB, "", rockusb.ErrDeviceNotSupport,
			"parameter 的每一份副本都落在本次不会重写的数据上，一份也没写",
			"分区表没有写下去：先检查 parameter 的保留区（flash offset / 分区大小）是否与设备布局一致。", nil)
	}
	rep.Verified = !r.opts.SkipReadback
	return rep.Bytes, nil
}

// replicaConflict says why a parameter copy must not be written, or "" when it
// is safe. Copies are allowed to land inside partitions this run rewrites (the
// parameter is written first, the payload overwrites them), but never inside
// the partition table, the IDB, or data the run leaves alone.
func (r *runner) replicaConflict(rep parameter.Replica) string {
	if rep.LBA < uint64(gpt.PrimarySectors) {
		return "落在主 GPT 上"
	}
	if disk := uint64(r.rep.Device.Flash.FlashSizeSectors); disk > 0 && rep.End() > disk {
		return "超出设备容量"
	} else if disk > 0 && rep.End()+uint64(gpt.BackupSectors) > disk {
		return "落在备份 GPT 上"
	}
	if r.plan.IDB.Bytes > 0 {
		start := uint64(r.plan.IDB.LBASector)
		end := start + uint64(r.plan.IDB.Bytes/SectorSize)
		if overlaps(rep.LBA, rep.End(), start, end) {
			return "落在 IDB 上"
		}
	}
	for _, p := range r.plan.GPT.Partitions {
		if p.Grow {
			continue
		}
		end := p.First + p.Sectors
		if !overlaps(rep.LBA, rep.End(), p.First, end) {
			continue
		}
		if !r.partitionRewritten(p.Name) {
			return fmt.Sprintf("落在本次不重写的分区 %s 上", p.Name)
		}
	}
	return ""
}

// partitionRewritten reports whether some item in this run writes the partition.
func (r *runner) partitionRewritten(name string) bool {
	for _, it := range r.plan.Items {
		if it.Partition == "" || !strings.EqualFold(it.Partition, name) {
			continue
		}
		if !r.skipped[it.Name] {
			return true
		}
	}
	return false
}

func overlaps(aStart, aEnd, bStart, bEnd uint64) bool {
	return aStart < bEnd && bStart < aEnd
}

// readBackCompare reads the written range and compares it byte for byte. A
// device that answers with different bytes will not have the layout the plan
// describes, so the run stops before the payload writes.
func (r *runner) readBackCompare(phase, item string, lba uint64, want []byte, label string) error {
	if r.opts.SkipReadback {
		return nil
	}
	n, clipped := r.readableSectors(lba, len(want)/SectorSize)
	if n == 0 {
		warning := fmt.Sprintf(
			"%s（LBA 0x%x）在设备 READ_LBA 窗口（前 32 MiB）之外：写入已执行，跳过读回校验",
			label, lba)
		r.rep.Warnings = append(r.rep.Warnings, warning)
		r.event(phase, "warn", item, warning)
		return nil
	}
	if clipped {
		warning := fmt.Sprintf(
			"%s（LBA 0x%x）跨过设备 READ_LBA 窗口（前 32 MiB）：只校验窗口内 %d 字节",
			label, lba, n*SectorSize)
		r.rep.Warnings = append(r.rep.Warnings, warning)
		r.event(phase, "warn", item, warning)
		want = want[:n*SectorSize]
	}
	got, err := r.dev.read.ReadLBAs(uint32(lba), n)
	if err != nil {
		if CodeOf(err) == rockusb.ErrDeviceNotSupport {
			r.event(phase, "warn", item, "设备不支持 READ_LBA，跳过写后读回校验")
			return nil
		}
		return r.fail(phase, item, CodeOf(err), fmt.Sprintf("读回 %s 失败: %v", label, err),
			"读回失败时不要继续写。", err)
	}
	if !bytes.Equal(got, want) {
		return r.fail(phase, item, ErrReadback, fmt.Sprintf(
			"读回校验失败 %s（LBA 0x%x，首处不一致 offset %d）", label, lba, firstDiff(got, want)),
			"停止写入；设备内容与计划不一致，先保留日志再排查。", nil)
	}
	r.event(phase, "info", item, "读回校验 ok："+label)
	return nil
}

// verifyBackupGPT reads the backup table back. A mismatch is fatal unless the
// region still reads as uniformly erased or lies outside the loader's readable
// window: some loaders accept a write to the last sectors, report success and
// leave the region erased, and return the erase fill for reads there. The
// primary table has already been verified and U-Boot boots from it, so an
// absent or unverifiable backup table is reported as a warning instead of
// failing the whole run.
func (r *runner) verifyBackupGPT(lba uint64, want []byte, label string) (bool, error) {
	if r.opts.SkipReadback {
		return false, nil
	}
	if n, _ := r.readableSectors(lba, len(want)/SectorSize); n == 0 {
		warning := fmt.Sprintf(
			"%s 在设备 READ_LBA 窗口（前 32 MiB）之外：写入已执行，无法读回校验；主 GPT 已校验，启动读主表",
			label)
		r.rep.Warnings = append(r.rep.Warnings, warning)
		r.event(PhaseIDB, "warn", "", warning)
		return false, nil
	}
	got, err := r.dev.read.ReadLBAs(uint32(lba), len(want)/SectorSize)
	if err != nil {
		if CodeOf(err) == rockusb.ErrDeviceNotSupport {
			r.event(PhaseIDB, "warn", "", "设备不支持 READ_LBA，跳过写后读回校验")
			return false, nil
		}
		return false, r.fail(PhaseIDB, "", CodeOf(err), fmt.Sprintf("读回 %s 失败: %v", label, err),
			"读回失败时不要继续写。", err)
	}
	if bytes.Equal(got, want) {
		r.event(PhaseIDB, "info", "", "读回校验 ok："+label)
		return true, nil
	}
	if pattern, ok := erasedPattern(got); ok {
		warning := fmt.Sprintf(
			"%s 读回仍是擦除态（%s）：该区域无法读回校验；主 GPT 已校验，启动读主表",
			label, pattern)
		r.rep.Warnings = append(r.rep.Warnings, warning)
		r.event(PhaseIDB, "warn", "", warning)
		return false, nil
	}
	return false, r.fail(PhaseIDB, "", ErrReadback, fmt.Sprintf(
		"读回校验失败 %s（LBA 0x%x，首处不一致 offset %d）", label, lba, firstDiff(got, want)),
		"停止写入；设备内容与计划不一致，先保留日志再排查。", nil)
}

// readableSectors limits a readback span to the loader's reliable READ_LBA
// window. It returns the number of sectors that can be compared and whether the
// span had to be clipped; zero means the whole span is outside the window.
func (r *runner) readableSectors(lba uint64, sectors int) (int, bool) {
	limit := r.opts.ReadLimitSectors
	if limit == 0 {
		limit = DefaultReadLimitSectors
	}
	if lba >= limit {
		return 0, false
	}
	if lba+uint64(sectors) > limit {
		return int(limit - lba), true
	}
	return sectors, false
}

// erasedPattern reports whether every byte is the same erase fill value
// (0xCC or 0xFF, the two patterns Rockchip loaders leave) and names it. A
// uniform zero buffer is deliberately not accepted: that is what a broken read
// returns, not what an erased flash sector contains.
func erasedPattern(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	switch b[0] {
	case 0xCC, 0xFF:
	default:
		return "", false
	}
	for _, v := range b {
		if v != b[0] {
			return "", false
		}
	}
	return fmt.Sprintf("0x%02X", b[0]), true
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// samePartitions checks that a rebuilt table still starts and sizes the same
// partitions the plan was approved for. A grow partition's end legitimately
// depends on the disk, so only start LBAs and fixed sizes are compared.
func samePartitions(planned []parameter.Partition, entries []gpt.Entry) error {
	return comparePartitions(planned, partitionsOfEntries(entries))
}

// partitionsOfEntries converts planned GPT entries into the parameter form, so
// the two tables can be compared field by field.
func partitionsOfEntries(entries []gpt.Entry) []parameter.Partition {
	out := make([]parameter.Partition, 0, len(entries))
	for _, e := range entries {
		out = append(out, parameter.Partition{Name: e.Name, Flags: e.Flags, First: e.FirstLBA, Sectors: e.Sectors, Grow: e.Grow})
	}
	return out
}

// comparePartitions reports the first difference between the table the plan
// approved and the table that is about to be written.
func comparePartitions(planned, got []parameter.Partition) error {
	if len(planned) != len(got) {
		return fmt.Errorf("分区表项数 %d 与计划 %d 不一致", len(got), len(planned))
	}
	for i := range planned {
		want := planned[i]
		have := got[i]
		if !strings.EqualFold(want.Name, have.Name) {
			return fmt.Errorf("分区 %d 名称 %q 与计划 %q 不一致", i, have.Name, want.Name)
		}
		if want.First != have.First {
			return fmt.Errorf("分区 %q 起始 LBA 0x%x 与计划 0x%x 不一致", have.Name, have.First, want.First)
		}
		if !want.Grow && !have.Grow && want.Sectors != have.Sectors {
			return fmt.Errorf("分区 %q 大小 %d 扇区与计划 %d 不一致", have.Name, have.Sectors, want.Sectors)
		}
	}
	return nil
}

// gptSeed makes the GUIDs a function of the partition table, so a retried write
// produces the same bytes as the first attempt.
func gptSeed(parts []parameter.Partition) string {
	var b strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&b, "%s:%d:%d:%t:%s;", p.Name, p.First, p.Sectors, p.Grow, strings.Join(p.Flags, ","))
	}
	return b.String()
}

// checkParameterBlock verifies the PARM wrapper and its checksum before the
// block is replicated across the disk.
func checkParameterBlock(block []byte) (*parameter.Document, error) {
	doc, err := parameter.Parse(block)
	if err != nil {
		return nil, fmt.Errorf("parameter 块无法解析: %v", err)
	}
	if !doc.Wrapped {
		return nil, fmt.Errorf("parameter 块没有 PARM 包装，无法校验")
	}
	if !doc.CRCOK {
		return nil, fmt.Errorf("parameter 块 CRC 不符：存储 0x%08x，计算 0x%08x", doc.CRCStored, doc.CRCComputed)
	}
	if len(doc.Partitions) == 0 {
		return nil, fmt.Errorf("parameter 块没有 mtdparts 分区表")
	}
	return doc, nil
}

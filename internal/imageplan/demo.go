package imageplan

// DemoImage builds a small, fully valid RKFW/RKAF image for the page's demo
// mode and for tests that need a real image without shipping a vendor blob.
//
// It is synthetic: the loader is a correctly shaped RKBOOT filled with a
// pattern (not Rockchip code), the parameter table is invented, and every
// payload is tiny. What it does reproduce is the *shape* that matters for the
// pre-flight checklist: an RK3588 header, a loader in the image's own loader
// slot, and partitions whose payload is smaller than the partition (the STATE
// tail this project has to decide about explicitly).

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"

	"github.com/SilenWang/rk3588-webflasher/internal/rkcrc"
	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// DemoImageName is what the page shows for DemoImage.
const DemoImageName = "demo-update.img (synthetic)"

const demoCmdline = `CMDLINE: mtdparts=rk29xxnand:` +
	`0x00000004@0x00000040(parameter),` +
	`0x00000008@0x00000044(uboot),` +
	`0x00000040@0x0000004c(boot),` +
	`0x00002000@0x0000008c(state)`

// DemoLoader builds the RKBOOT blob inside DemoImage: one 0x0471 segment and
// one 0x0472 segment, a fixed build time and the RK3588 chip tag.
func DemoLoader() []byte {
	const header, entry = 102, 57
	head := make([]byte, 2048)
	plug := make([]byte, 8192)
	for i := range head {
		head[i] = byte(i * 3)
	}
	for i := range plug {
		plug[i] = byte(i * 7)
	}
	dataOff := header + 2*entry
	buf := make([]byte, dataOff+len(head)+len(plug))

	binary.LittleEndian.PutUint32(buf[0:4], rockusb.LoaderTagBoot)
	binary.LittleEndian.PutUint16(buf[4:6], header)
	binary.LittleEndian.PutUint32(buf[6:10], 0x00010006)
	binary.LittleEndian.PutUint32(buf[10:14], 0x01030000)
	binary.LittleEndian.PutUint16(buf[14:16], 2024)
	buf[16] = 9 // 2024-09-06, the official v23 image's loader build date
	buf[17] = 6
	buf[18] = 1
	binary.LittleEndian.PutUint32(buf[21:25], rockusb.RK3588ChipTag)
	buf[25] = 1
	binary.LittleEndian.PutUint32(buf[26:30], header)
	buf[30] = entry
	buf[31] = 1
	binary.LittleEndian.PutUint32(buf[32:36], header+entry)
	buf[36] = entry
	buf[43] = 'N'

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
	writeEntry(header+entry, rockusb.DBEntry472, "demo_usbplug", dataOff+len(head), len(plug))
	copy(buf[dataOff:], head)
	copy(buf[dataOff+len(head):], plug)
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], rockusb.CRC32(buf[:len(buf)-4]))
	return buf
}

// DemoImage assembles the RKFW image: header, loader, RKAF archive and the
// trailing ASCII MD5, so `uf --dry-run`-equivalent checks behave exactly as
// they do on a real image.
func DemoImage() []byte {
	loader := DemoLoader()

	type item struct {
		name string
		lba  uint32
		data []byte
	}
	param := make([]byte, 2048)
	copy(param, demoCmdline)
	uboot := make([]byte, 2048)
	for i := range uboot {
		uboot[i] = byte(i * 5)
	}
	boot := make([]byte, 16*1024)
	for i := range boot {
		boot[i] = byte(i * 11)
	}
	state := make([]byte, 512*1024)
	for i := range state {
		state[i] = byte(i * 13)
	}
	items := []item{
		{"parameter", 0x40, param},
		{"uboot", 0x44, uboot},
		{"boot", 0x4c, boot},
		{"state", 0x8c, state},
	}

	rkaf := make([]byte, rkfw.RKAFHeaderSize)
	copy(rkaf[0:4], rkfw.MagicRKAF[:])
	copy(rkaf[8:42], "Demo Duo")
	copy(rkaf[42:72], "rockchip")
	copy(rkaf[72:128], "Synthetic")
	binary.LittleEndian.PutUint32(rkaf[132:136], 0x01000000)
	binary.LittleEndian.PutUint32(rkaf[136:140], uint32(len(items)))

	pos := uint32(rkfw.RKAFHeaderSize)
	for i, it := range items {
		slot := rkaf[140+i*rkfw.RKAFItemSize : 140+(i+1)*rkfw.RKAFItemSize]
		copy(slot[0:32], it.name)
		copy(slot[32:92], "Image/"+it.name)
		padded := (len(it.data) + 511) / 512 * 512
		binary.LittleEndian.PutUint32(slot[92:96], uint32(padded/512)) // partition sectors
		binary.LittleEndian.PutUint32(slot[96:100], pos)
		binary.LittleEndian.PutUint32(slot[100:104], it.lba)
		binary.LittleEndian.PutUint32(slot[104:108], uint32((padded+2047)/2048))
		binary.LittleEndian.PutUint32(slot[108:112], uint32(len(it.data)))
		buf := make([]byte, padded)
		copy(buf, it.data)
		rkaf = append(rkaf, buf...)
		pos += uint32(padded)
	}
	binary.LittleEndian.PutUint32(rkaf[4:8], uint32(len(rkaf)))
	rkaf = append(rkaf, make([]byte, 4)...)
	binary.LittleEndian.PutUint32(rkaf[len(rkaf)-4:], rkcrc.Checksum(rkaf[:len(rkaf)-4]))

	head := make([]byte, rkfw.HeaderSize)
	copy(head[0:4], rkfw.MagicRKFW[:])
	binary.LittleEndian.PutUint16(head[4:6], rkfw.HeaderSize)
	binary.LittleEndian.PutUint32(head[6:10], 0x17000001)
	binary.LittleEndian.PutUint32(head[10:14], 0x01030000)
	binary.LittleEndian.PutUint16(head[14:16], 2025)
	head[16] = 1
	head[17] = 2
	binary.LittleEndian.PutUint32(head[21:25], binary.LittleEndian.Uint32([]byte{'8', '8', '5', '3'}))
	binary.LittleEndian.PutUint32(head[25:29], rkfw.HeaderSize)
	binary.LittleEndian.PutUint32(head[29:33], uint32(len(loader)))
	binary.LittleEndian.PutUint32(head[33:37], uint32(rkfw.HeaderSize+len(loader)))
	binary.LittleEndian.PutUint32(head[37:41], uint32(len(rkaf)))

	out := append(append(append([]byte(nil), head...), loader...), rkaf...)
	sum := md5.Sum(out)
	return append(out, []byte(hex.EncodeToString(sum[:]))...)
}

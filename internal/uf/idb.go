package uf

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
)

// The IDB ("IDBlock") is what makes a bare SoC able to boot from flash: the
// loader bytes, encrypted the way the BootROM expects, written at a fixed
// sector. It is the one part of `uf` whose byte model has not been checked
// against a real device yet, so it sits behind this interface instead of being
// hard-coded in the writer.
//
// Two byte layouts are supported behind IDBModel:
//
//   - "new IDB" (default): the loader's ENTRYLOADER FlashHead/FlashData/
//     FlashBoot segments, each 2 KiB aligned and concatenated, with an
//     optional per-512-byte RC4 pass controlled by the loader header;
//   - "legacy whole-loader": the RKFW loader blob padded to 512 bytes and
//     RC4'd per sector.
//
// The device tells us which one it expects by the New IDB capability and by
// the header at LBA 64 ("RKNS" for the new layout). Keep both behind the
// interface; a hardware run records which model produced the bytes.
const (
	// IDBDefaultLBA is RK_IDB_SECTOR: the sector SD_Firmware_Tool and
	// rk_image_tool write the IDB copy at.
	IDBDefaultLBA uint32 = 64
	// IDBBlockSize is the RC4 unit (RK_IDB_BLOCK_SIZE).
	IDBBlockSize = 512
)

// Model names are part of the log schema: a hardware run records which model
// produced the bytes, so the trace can later confirm or refute it.
const (
	// ModelRKBOOT is the New IDB layout used by RK3588 devices: FlashHead,
	// FlashData and FlashBoot from the loader's ENTRYLOADER table, each padded
	// to 2 KiB and concatenated, with an optional per-512-byte RC4 pass when
	// the loader header asks for it. This is the default.
	ModelRKBOOT    = "rkboot-new-idb"
	ModelRK28RC4   = "rk28-rc4"
	ModelRawLoader = "raw-loader"
)

// IDBType is what the bytes at IDBDefaultLBA look like on a real device.
//
// SIL-515 read LBA 64 back on the Fydetab Duo and found an "RKNS" header, i.e.
// a New IDB — while this tool wrote an "rk28-rc4" block there. Writing the
// wrong IDB model makes the BootROM unable to read uboot, which is the most
// likely reason the device did not boot at all. The type is therefore read
// from the device, never assumed.
type IDBType string

const (
	// IDBTypeNew is the current block format ("RKNS" magic).
	IDBTypeNew IDBType = "RKNS"
	// IDBTypeRK28 is the legacy block format ("RK28" magic, the rk28-rc4
	// model this package can build).
	IDBTypeRK28 IDBType = "RK28"
	// IDBTypeEmpty means the sectors read as all 0x00 (erased or never
	// written).
	IDBTypeEmpty IDBType = "empty"
	// IDBTypeUnknown means the sectors hold bytes that carry neither magic.
	IDBTypeUnknown IDBType = "unknown"
)

// IDBInfo is the identification of the IDB currently on the device.
type IDBInfo struct {
	Type IDBType `json:"type"`
	// Magic is the first four bytes rendered as text (or hex when not
	// printable), so a log reader can see what was actually there.
	Magic string `json:"magic"`
	// HeadHex is the first 32 bytes, the fingerprint recorded before any
	// overwrite.
	HeadHex string `json:"headHex"`
	// Verified reports whether Type is a format this package recognises.
	Verified bool `json:"verified"`
}

// IDBProbeSectors is how much of LBA 64 is read to identify the block. A new
// IDB header is well inside the first sector; 8 sectors leaves room for the
// header fields and keeps the read one command.
const IDBProbeSectors = 8

// IdentifyIDB classifies the bytes read from IDBDefaultLBA.
func IdentifyIDB(b []byte) IDBInfo {
	info := IDBInfo{Type: IDBTypeUnknown}
	if len(b) < 4 {
		info.Type = IDBTypeEmpty
		return info
	}
	head := b
	if len(head) > 32 {
		head = head[:32]
	}
	info.HeadHex = hexBytes(head)
	info.Magic = printableMagic(b[:4])

	switch {
	case allZero(b):
		info.Type = IDBTypeEmpty
	case string(b[0:4]) == "RKNS":
		info.Type = IDBTypeNew
		info.Verified = true
	case string(b[0:4]) == "RK28":
		info.Type = IDBTypeRK28
		info.Verified = true
	}
	return info
}

// ModelForType returns the byte model that produces an IDB of the given type,
// or an error when this package cannot build that format.
func ModelForType(t IDBType) (IDBModel, error) {
	switch t {
	case IDBTypeRK28:
		return NewRK28RC4Model(), nil
	case IDBTypeNew:
		return NewRKBOOTModel(), nil
	case IDBTypeEmpty, IDBTypeUnknown:
		return nil, fmt.Errorf("uf: no existing IDB type to rebuild (%q)", t)
	}
	return nil, fmt.Errorf("uf: unknown IDB type %q", t)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0x00 {
			return false
		}
	}
	return true
}

func printableMagic(b []byte) string {
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return hexBytes(b)
		}
	}
	return string(b)
}

func hexBytes(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, digits[v>>4], digits[v&0x0F])
	}
	return string(out)
}

// rk28RC4Key is RK_RC4_KEY from rk_image_tool/src/common/rc4.c.
var rk28RC4Key = []byte{
	0x7C, 0x4E, 0x03, 0x04, 0x55, 0x05, 0x09, 0x07,
	0x2D, 0x2C, 0x7B, 0x38, 0x17, 0x0D, 0x17, 0x11,
}

// LoaderInput is everything an IDB model may use. Container is the RKFW loader
// blob exactly as stored in the image; Parsed is its RKBOOT form when it parsed,
// which is what a future "select FlashData/FlashBoot entries" model needs.
type LoaderInput struct {
	Container []byte
	Parsed    *rockusb.LoaderImage
}

// IDBModel turns the image's loader into the bytes written at IDBDefaultLBA.
type IDBModel interface {
	// Name is stable and appears in the JSON log.
	Name() string
	// Verified reports whether this byte model has been confirmed by a real
	// flash-and-boot run. Honest false is the point: the runner logs it
	// instead of implying the bytes are known-good.
	Verified() bool
	// Build returns the IDB bytes; their length is the gpt stage's byte count.
	Build(in LoaderInput) ([]byte, error)
}

// NewRKBOOTModel returns the New IDB model used by RK3588 devices.
func NewRKBOOTModel() IDBModel { return rkbootIDB{} }

// NewRK28RC4Model returns the legacy whole-loader model described above. It is
// kept only so an old log/plan can be reproduced; it is not what RK3588
// firmware expects for a "New IDB" device.
func NewRK28RC4Model() IDBModel { return rk28rc4{} }

// NewRawLoaderModel returns the loader padded to 512 bytes with no
// encryption. It exists to A/B the encryption step on hardware (if the device
// boots with plain bytes, the RC4 layer is wrong) and to prove the abstraction
// is replaceable.
func NewRawLoaderModel() IDBModel { return rawLoader{} }

// IDBModelByName resolves the --idb-model values.
func IDBModelByName(name string) (IDBModel, error) {
	switch name {
	case "", ModelRKBOOT:
		return NewRKBOOTModel(), nil
	case ModelRK28RC4:
		return NewRK28RC4Model(), nil
	case ModelRawLoader:
		return NewRawLoaderModel(), nil
	}
	return nil, fmt.Errorf("unknown IDB model %q (want %s or %s)", name, ModelRK28RC4, ModelRawLoader)
}

// IDBModelNames lists the selectable models for --help and docs.
func IDBModelNames() []string { return []string{ModelRKBOOT, ModelRK28RC4, ModelRawLoader} }

type rkbootIDB struct{}

func (rkbootIDB) Name() string   { return ModelRKBOOT }
func (rkbootIDB) Verified() bool { return false }

func (rkbootIDB) Build(in LoaderInput) ([]byte, error) {
	img := in.Parsed
	if img == nil {
		var err error
		img, err = rockusb.ParseLoaderImage(in.Container)
		if err != nil {
			return nil, fmt.Errorf("new IDB: parse loader: %w", err)
		}
	}
	if len(img.EntriesLoader) == 0 {
		return nil, fmt.Errorf("new IDB: loader has no ENTRYLOADER table (FlashHead/FlashData/FlashBoot)")
	}
	segment := func(want string) ([]byte, error) {
		for _, e := range img.EntriesLoader {
			if strings.EqualFold(e.Name, want) {
				b := img.Segment(e)
				if int64(len(b)) != int64(e.DataSize) {
					return nil, fmt.Errorf("new IDB: %s is %d bytes, want %d", want, len(b), e.DataSize)
				}
				out := make([]byte, len(b))
				copy(out, b)
				return out, nil
			}
		}
		return nil, fmt.Errorf("new IDB: loader has no %s entry", want)
	}
	var idb []byte
	for _, name := range []string{"FlashHead", "FlashData", "FlashBoot"} {
		seg, err := segment(name)
		if err != nil {
			return nil, err
		}
		// Each segment sits on a 2 KiB boundary in the New IDB.
		if rem := len(seg) % 2048; rem != 0 {
			seg = append(seg, make([]byte, 2048-rem)...)
		}
		idb = append(idb, seg...)
	}
	// When the loader header requests RC4, the transform is applied one
	// 512-byte sector at a time with the same fixed key as the legacy model.
	if img.RC4Flag != 0 {
		for off := 0; off < len(idb); off += IDBBlockSize {
			rc4(rk28RC4Key, idb[off:off+IDBBlockSize])
		}
	}
	return idb, nil
}

type rk28rc4 struct{}

func (rk28rc4) Name() string   { return ModelRK28RC4 }
func (rk28rc4) Verified() bool { return false }
func (rk28rc4) Build(in LoaderInput) ([]byte, error) {
	out, err := paddedLoader(in)
	if err != nil {
		return nil, err
	}
	for off := 0; off < len(out); off += IDBBlockSize {
		rc4(rk28RC4Key, out[off:off+IDBBlockSize])
	}
	return out, nil
}

type rawLoader struct{}

func (rawLoader) Name() string                         { return ModelRawLoader }
func (rawLoader) Verified() bool                       { return false }
func (rawLoader) Build(in LoaderInput) ([]byte, error) { return paddedLoader(in) }

func paddedLoader(in LoaderInput) ([]byte, error) {
	if len(in.Container) == 0 {
		return nil, errors.New("uf: the image carries no loader, so there is no IDB to build")
	}
	n := alignUp(int64(len(in.Container)), IDBBlockSize)
	out := make([]byte, n)
	copy(out, in.Container)
	return out, nil
}

// rc4 is the classic RC4 stream cipher (KSA + PRGA), applied in place. It is
// symmetric, which is why the same routine both produces and checks an IDB.
func rc4(key, buf []byte) {
	if len(key) == 0 {
		return
	}
	var s [256]byte
	for i := range s {
		s[i] = byte(i)
	}
	var j byte
	for i := 0; i < 256; i++ {
		j += s[i] + key[i%len(key)]
		s[i], s[j] = s[j], s[i]
	}
	var a, b byte
	for n := range buf {
		a++
		b += s[a]
		s[a], s[b] = s[b], s[a]
		buf[n] ^= s[s[a]+s[b]]
	}
}

func alignUp(v, align int64) int64 {
	if align <= 1 {
		return v
	}
	return (v + align - 1) / align * align
}

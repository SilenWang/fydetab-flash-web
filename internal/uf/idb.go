package uf

import (
	"errors"
	"fmt"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// The IDB ("IDBlock") is what makes a bare SoC able to boot from flash: the
// loader bytes, encrypted the way the BootROM expects, written at a fixed
// sector. It is the one part of `uf` whose byte model has not been checked
// against a real device yet, so it sits behind this interface instead of being
// hard-coded in the writer.
//
// What is implemented here (ModelRK28RC4) comes from the public
// reimplementation rk_image_tool (src/common/idblock.c, rc4.c):
//
//  1. round the loader up to a multiple of 512 bytes, zero padded;
//  2. RC4 every 512-byte sector independently with the fixed 16-byte key,
//     re-initialising the cipher state for each sector;
//  3. write the result at LBA 64 (RK_IDB_SECTOR).
//
// **待真机核对**: the model has never been diffed against an official
// `upgrade_tool uf` log or against real flash. In particular it is not settled
// whether `uf` writes the RKFW loader blob verbatim (this model), or the
// RKBOOT FlashData/FlashBoot entries concatenated and 2048-byte aligned (the
// path rkdeveloptool's `ul` takes for new IDBs). Both stay behind the interface;
// a device trace can settle it without touching the state machine.
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
		return nil, fmt.Errorf("uf: the device holds a New IDB (%q); this build has no verified RKNS writer, so it will not rebuild it", t)
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
	// Verified reports whether this byte model has been confirmed against a
	// real `upgrade_tool uf` run. Honest false is the point: the runner logs
	// it instead of implying the bytes are known-good.
	Verified() bool
	// Build returns the IDB bytes; their length is the gpt stage's byte count.
	Build(in LoaderInput) ([]byte, error)
}

// NewRK28RC4Model returns the default, unverified model described above.
func NewRK28RC4Model() IDBModel { return rk28rc4{} }

// NewRawLoaderModel returns the loader padded to 512 bytes with no
// encryption. It exists to A/B the encryption step on hardware (if the device
// boots with plain bytes, the RC4 layer is wrong) and to prove the abstraction
// is replaceable.
func NewRawLoaderModel() IDBModel { return rawLoader{} }

// IDBModelByName resolves the --idb-model values.
func IDBModelByName(name string) (IDBModel, error) {
	switch name {
	case "", ModelRK28RC4:
		return NewRK28RC4Model(), nil
	case ModelRawLoader:
		return NewRawLoaderModel(), nil
	}
	return nil, fmt.Errorf("unknown IDB model %q (want %s or %s)", name, ModelRK28RC4, ModelRawLoader)
}

// IDBModelNames lists the selectable models for --help and docs.
func IDBModelNames() []string { return []string{ModelRK28RC4, ModelRawLoader} }

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

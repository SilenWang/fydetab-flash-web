package imageplan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/SilenWang/rk3588-webflasher/internal/rkfw"
	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// Where a Download Boot loader came from. The distinction is shown on the page
// and written into the run report: a loader that is not the image's own is the
// one thing real hardware rejected at index 0, so it must never be implicit.
const (
	LoaderSourceImage = "image"
	LoaderSourceFile  = "file"
)

// ErrNoImageLoader means the image has no RKFW wrapper or an empty loader
// slot: a bare RKAF image cannot bootstrap a maskrom device at all.
var ErrNoImageLoader = errors.New("image carries no RKFW loader")

// EmbeddedLoader is the loader blob a Download Boot would send, with the
// identity fields a user can compare against a known-good file: length,
// sha256, the RKBOOT version and, most usefully, the LDR build date.
type EmbeddedLoader struct {
	Source     string `json:"source"` // image | file
	Name       string `json:"name,omitempty"`
	FileOffset int64  `json:"fileOffset"`
	Length     int64  `json:"length"`
	SHA256     string `json:"sha256"`

	// Header fields, parsed from the RKBOOT ("BOOT"/"LDR ") container.
	Version      string `json:"version,omitempty"`
	MergeVersion string `json:"mergeVersion,omitempty"`
	BuildTime    string `json:"buildTime,omitempty"`
	// Chip is the loader's own supportChip model, e.g. "RK3588".
	Chip     string `json:"chip,omitempty"`
	ChipType uint32 `json:"chipType,omitempty"`
	Segments int    `json:"segments,omitempty"`

	// ChipCheck is "ok" when the loader matches the image's chip, "mismatch"
	// when it does not and "unknown" when either side has no chip field.
	ChipCheck string `json:"chipCheck"`
	// ImageChip is the RKFW header's chip name, the value ChipCheck used.
	ImageChip string `json:"imageChip,omitempty"`
	// ParseError is set when the bytes are not a usable RKBOOT; the plan is
	// still returned so the page can show what the file is.
	ParseError string `json:"parseError,omitempty"`

	// Bytes is the blob itself. Download Boot reads it from here, so an
	// in-memory image (the browser) and a file both work the same way.
	Bytes []byte `json:"-"`
	// Parsed is the validated container when ParseError is empty.
	Parsed *rockusb.LoaderImage `json:"-"`
}

// ParseErrorAsLoaderError returns the classified loader error, if any.
func (l *EmbeddedLoader) LoaderError() *rockusb.LoaderError {
	if l == nil || l.Parsed != nil || l.Bytes == nil {
		return nil
	}
	_, err := rockusb.ParseLoaderImage(l.Bytes)
	var lerr *rockusb.LoaderError
	if errors.As(err, &lerr) {
		return lerr
	}
	return nil
}

// Matches reports whether the loader may be sent for this image's chip.
func (l *EmbeddedLoader) Matches() bool { return l != nil && l.ChipCheck == "ok" }

// Check refuses a loader built for another chip. The runner calls it before the
// first control transfer, which is the check upstream's download_boot() lacks.
func (l *EmbeddedLoader) Check() error {
	if l == nil {
		return ErrNoImageLoader
	}
	if l.ParseError != "" {
		return fmt.Errorf("loader 无法解析：%s", l.ParseError)
	}
	if l.Parsed == nil {
		return fmt.Errorf("loader 无法解析：没有可用的 RKBOOT 数据")
	}
	if l.ChipCheck == "mismatch" {
		return fmt.Errorf("loader 是为 %s 构建的，但镜像是 %s 的：拒绝 Download Boot（上游不会替你校验）",
			l.Chip, l.ImageChip)
	}
	return l.Parsed.CheckChip(l.ImageChip)
}

// Summary renders the identity lines the page and the CLI show.
func (l *EmbeddedLoader) Summary() []string {
	if l == nil {
		return []string{"(no loader)"}
	}
	source := "镜像自带"
	if l.Source == LoaderSourceFile {
		source = "用户指定（非镜像自带）"
	}
	lines := []string{
		fmt.Sprintf("loader: %s · %s · offset 0x%x · %d B", source, orDash(l.Name), l.FileOffset, l.Length),
		fmt.Sprintf("sha256: %s", l.SHA256),
		fmt.Sprintf("LDR: version %s · merge %s · built %s · chip %s (0x%08X)",
			orDash(l.Version), orDash(l.MergeVersion), l.BuildTime, orDash(l.Chip), l.ChipType),
		fmt.Sprintf("chip check: %s (image %s)", l.ChipCheck, orDash(l.ImageChip)),
	}
	if l.ParseError != "" {
		lines = append(lines, "✗ "+l.ParseError)
	}
	return lines
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// FromImage extracts the loader the image carries in its RKFW header. This is
// the default source: the vendor's own download_boot() only ever used the
// loader that came with the image, and mixing a loader from another package
// (the SDK's v1.06.111 with the official v23 image) is what real hardware
// rejected at index 0.
func FromImage(img *rkfw.Image) (*EmbeddedLoader, error) {
	if img == nil {
		return nil, errors.New("imageplan: nil image")
	}
	if !img.HasRKFW || img.LoaderLength <= 0 {
		return nil, ErrNoImageLoader
	}
	data, err := img.ReadRange(img.LoaderOffset, img.LoaderLength)
	if err != nil {
		return nil, fmt.Errorf("读取镜像自带 loader: %w", err)
	}
	loader := FromBytes(data, img.RKFW.ChipName())
	loader.Source = LoaderSourceImage
	loader.Name = fmt.Sprintf("%s @0x%x", orDash(baseName(img.Path)), img.LoaderOffset)
	loader.FileOffset = img.LoaderOffset
	return loader, nil
}

// FromFile wraps a user-supplied loader file. Source is "file" so every later
// step can say it is not the image's own.
func FromFile(name string, data []byte, imageChip string) *EmbeddedLoader {
	loader := FromBytes(data, imageChip)
	loader.Source = LoaderSourceFile
	loader.Name = name
	return loader
}

// FromBytes parses a loader and compares its supportChip with the image chip.
func FromBytes(data []byte, imageChip string) *EmbeddedLoader {
	sum := sha256.Sum256(data)
	loader := &EmbeddedLoader{
		Source:    LoaderSourceFile,
		Length:    int64(len(data)),
		SHA256:    hex.EncodeToString(sum[:]),
		Bytes:     data,
		ImageChip: imageChip,
		ChipCheck: "unknown",
	}
	parsed, err := rockusb.ParseLoaderImage(data)
	if err != nil {
		loader.ParseError = err.Error()
		return loader
	}
	loader.Parsed = parsed
	loader.Version = parsed.VersionString()
	loader.MergeVersion = parsed.MergeVersionString()
	loader.BuildTime = parsed.BuildTime.String()
	loader.Chip = parsed.ChipName()
	loader.ChipType = parsed.ChipType
	loader.Segments = len(parsed.Entries471) + len(parsed.Entries472)
	switch {
	case imageChip == "" || parsed.ChipType == 0:
		loader.ChipCheck = "unknown"
	case parsed.SupportsChip(imageChip):
		loader.ChipCheck = "ok"
	default:
		loader.ChipCheck = "mismatch"
	}
	return loader
}

func baseName(path string) string {
	if path == "" {
		return ""
	}
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}

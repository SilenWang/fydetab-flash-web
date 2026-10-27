package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/imageplan"
	"github.com/SilenWang/fydetab-flash-web/internal/rkfw"
	"github.com/SilenWang/fydetab-flash-web/internal/uf"
)

// buildRKFWImageWithChip rebuilds the RKFW fixture with a different chip field
// (four ASCII digits as stored, e.g. "6653" for RK3566). The MD5 tail is then
// stale, so callers pass --no-verify: the point is the loader check, not the
// checksum.
func buildRKFWImageWithChip(t *testing.T, stored string) []byte {
	t.Helper()
	data := buildRKFWImage(t)
	if len(stored) != 4 {
		t.Fatalf("chip field must be 4 digits, got %q", stored)
	}
	copy(data[21:25], stored)
	return data
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestSelectLoaderDefaultsToTheImagesOwnBlob(t *testing.T) {
	img, err := rkfw.Open(writeCLIImage(t, buildRKFWImage(t)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	loader, err := selectLoader(img, "")
	if err != nil {
		t.Fatalf("selectLoader: %v", err)
	}
	if loader == nil {
		t.Fatal("the image carries a loader, selectLoader returned nil")
	}
	if loader.Source != imageplan.LoaderSourceImage {
		t.Errorf("source = %q, want image", loader.Source)
	}
	if loader.Length != img.LoaderLength || loader.FileOffset != img.LoaderOffset {
		t.Errorf("located at %d/%d, want %d/%d", loader.FileOffset, loader.Length, img.LoaderOffset, img.LoaderLength)
	}
	if loader.SHA256 == "" || loader.ChipCheck != "ok" || loader.Chip != "RK3588" {
		t.Errorf("loader identity = %+v", loader)
	}

	// An explicit file is used instead, and only then is it non-image.
	path := writeFile(t, "sdk-loader.bin", buildLoaderBlob(t))
	override, err := selectLoader(img, path)
	if err != nil {
		t.Fatalf("selectLoader(--loader): %v", err)
	}
	if override.Source != imageplan.LoaderSourceFile {
		t.Errorf("override source = %q, want file", override.Source)
	}
	if override.SHA256 != loader.SHA256 {
		// Same bytes here on purpose: identity follows the bytes, not the path.
		t.Errorf("sha256 differs for identical bytes: %q vs %q", override.SHA256, loader.SHA256)
	}
}

func TestDryRunPrintsTheLoaderIdentity(t *testing.T) {
	path := writeCLIImage(t, buildRKFWImage(t))
	code, stdout, stderr := runCLI(t, "--dry-run", "--no-verify", path)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"loader for Download Boot",
		"镜像自带",
		"sha256:",
		"LDR: version",
		"chip check: ok",
		"dry run: nothing was written and no device was opened.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q\n%s", want, stdout)
		}
	}
}

func TestDryRunWarnsWhenLoaderDisagreesWithTheImageChip(t *testing.T) {
	path := writeCLIImage(t, buildRKFWImageWithChip(t, "6653")) // RK3566 image
	code, stdout, stderr := runCLI(t, "--dry-run", "--no-verify", path)
	if code != 0 {
		t.Fatalf("dry-run exit = %d (a plan is still produced); stderr = %s", code, stderr)
	}
	// Both the warning list and the loader block must name the mismatch, and
	// the report must say what would happen to a write.
	for _, want := range []string{
		"chip check: mismatch",
		"拒绝发送 db",
		"loader 与镜像 chip 不匹配",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q\n%s", want, stdout)
		}
	}
}

func TestLoaderOverrideIsMarkedInTheJSONReport(t *testing.T) {
	path := writeCLIImage(t, buildRKFWImage(t))
	loaderPath := writeFile(t, "sdk-loader.bin", buildLoaderBlob(t))
	code, stdout, stderr := runCLI(t, "--dry-run", "--no-verify", "--json", "--loader", loaderPath, path)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr = %s", code, stderr)
	}
	var rep Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if rep.Loader == nil {
		t.Fatal("JSON report has no loader")
	}
	if rep.Loader.Source != imageplan.LoaderSourceFile || rep.Loader.Length != int64(len(buildLoaderBlob(t))) {
		t.Errorf("loader = %+v, want a file-source loader", rep.Loader)
	}
	if rep.Loader.ChipCheck != "ok" {
		t.Errorf("chipCheck = %q, want ok", rep.Loader.ChipCheck)
	}
}

func TestMockRunRefusesAMismatchedLoaderBeforeWriting(t *testing.T) {
	path := writeCLIImage(t, buildRKFWImageWithChip(t, "6653")) // RK3566 image
	code, stdout, stderr := runCLI(t, "--mock", "--no-verify", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refused); stderr = %s", code, stderr)
	}
	// The human summary carries the classified code and the reason.
	for _, want := range []string{"ERR_LOADER_MISMATCH", "[loader-chip-mismatch]", "result: FAILED", "LOADER_CHIP_MISMATCH"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary missing %q:\n%s", want, stdout)
		}
	}
	// Nothing was written: the refusal happens before the device is opened.
	if strings.Contains(stdout, "wl    ") {
		t.Errorf("an item was written despite the refusal:\n%s", stdout)
	}
}

// TestMockRunUsesTheImageLoaderByDefault pins the contract the page relies on:
// a mock run without --loader boots with the image's own blob and finishes.
func TestMockRunUsesTheImageLoaderByDefault(t *testing.T) {
	path := writeCLIImage(t, buildRKFWImage(t))
	code, stdout, stderr := runCLI(t, "--mock", "--no-verify", "--erase-tail", "none", path)
	if code != 0 {
		t.Fatalf("mock run exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "loader 自检") {
		t.Errorf("the run should log the loader self-check: %s", stderr)
	}
	if !strings.Contains(stdout, "result: ok") {
		t.Errorf("mock run did not report ok:\n%s", stdout)
	}
}

// TestDryRunNeverBuildsAConnector is the "--dry-run touches no device" contract:
// not a single transfer, not even a connector is constructed.
func TestDryRunNeverBuildsAConnector(t *testing.T) {
	original := newConnector
	t.Cleanup(func() { newConnector = original })
	called := false
	newConnector = func(mode, replay string, plan *uf.Plan, fault string) (uf.Connector, error) {
		called = true
		return nil, nil
	}

	path := writeCLIImage(t, buildRKFWImage(t))
	code, stdout, stderr := runCLI(t, "--dry-run", "--no-verify", path)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr = %s", code, stderr)
	}
	if called {
		t.Fatal("--dry-run asked for a device connector")
	}
	if !strings.Contains(stdout, "nothing was written and no device was opened") {
		t.Errorf("dry-run summary missing its no-device line:\n%s", stdout)
	}
}

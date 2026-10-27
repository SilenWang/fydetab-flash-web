package uf

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/SilenWang/fydetab-flash-web/internal/rockusb"
)

func TestRecorderWritesHeaderTransfersProgressAndResult(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	var buf bytes.Buffer
	opts := testOptions()
	rec, err := NewRecorder(&buf, plan, opts)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	opts.Trace = rec.Transfer
	opts.Progress = rec.Progress
	opts.Event = rec.Event

	conn := NewSimConnector(testSimConfig(plan))
	rep, runErr := runPlan(t, plan, layout, conn, opts)
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if err := rec.Result(rep); err != nil {
		t.Fatalf("Result: %v", err)
	}

	dec := json.NewDecoder(&buf)
	var kinds []string
	var header map[string]any
	var transferSummaries []string
	progressEvents := 0
	for {
		var line map[string]any
		if err := dec.Decode(&line); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode log line: %v", err)
		}
		kind, _ := line["kind"].(string)
		kinds = append(kinds, kind)
		switch kind {
		case "header":
			header = line
		case "transfer":
			summary, _ := line["summary"].(string)
			if summary == "" {
				t.Fatal("a transfer record has no summary")
			}
			transferSummaries = append(transferSummaries, summary)
		case "progress":
			progressEvents++
		case "event":
		case "result":
			report, _ := line["report"].(map[string]any)
			if report == nil || report["ok"] != true {
				t.Fatalf("result record = %v", line["report"])
			}
		default:
			t.Fatalf("unexpected log record kind %q", kind)
		}
	}
	if len(kinds) < 3 || kinds[0] != "header" || kinds[len(kinds)-1] != "result" {
		t.Fatalf("record kinds = %v", kinds)
	}
	if header == nil {
		t.Fatal("no header record")
	}
	optsMap, _ := header["options"].(map[string]any)
	if optsMap["idbModel"] != ModelRKBOOT || optsMap["idbVerified"] != false {
		t.Fatalf("log options = %v", optsMap)
	}
	if optsMap["eraseTail"] != string(EraseNone) {
		t.Fatalf("log eraseTail = %v", optsMap["eraseTail"])
	}
	planMap, _ := header["plan"].(map[string]any)
	items, _ := planMap["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("header plan items = %d, want 2", len(items))
	}
	if len(transferSummaries) == 0 {
		t.Fatal("no transfer records were written")
	}
	if progressEvents < 3 {
		t.Fatalf("progress records = %d, want at least one per phase", progressEvents)
	}
	// The IDB and both items must be visible in the transfer summaries.
	joined := strings.Join(transferSummaries, "\n")
	if !strings.Contains(joined, "RKU_WriteLBA") || !strings.Contains(joined, "RKU_DeviceReset") {
		t.Fatalf("transfer summaries do not cover the write path:\n%s", joined)
	}
}

func TestRecorderRemembersWriteErrorsAndKeepsGoing(t *testing.T) {
	plan := newTestLayout(t).plan(t)
	w := &limitedWriter{remaining: 2}
	rec, err := NewRecorder(w, plan, testOptions())
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec.Transfer(rockusb.Transfer{OpName: "RKU_WriteLBA", Stage: "cbw", Length: 31, Status: "ok"})
	rec.Transfer(rockusb.Transfer{OpName: "RKU_WriteLBA", Stage: "data", Length: 512, Status: "ok"})
	rec.Progress(Progress{Phase: PhaseWrite})
	if err := rec.Result(&Report{OK: true}); err == nil {
		t.Fatal("a failed log write must be reported")
	}
	if rec.Records() != 2 {
		t.Fatalf("records written = %d, want 2", rec.Records())
	}
}

func TestNewRecorderRejectsNilWriter(t *testing.T) {
	if _, err := NewRecorder(nil, &Plan{}, Options{}); err == nil {
		t.Fatal("expected an error for a nil writer")
	}
}

func TestTransferSummary(t *testing.T) {
	got := TransferSummary(rockusb.Transfer{
		OpName: "RKU_WriteLBA", Stage: "data", Note: "lba=0x40 sectors=128",
		Length: 65536, Status: "ok", DurationMs: 1.5, Kind: "bulk-out",
	})
	for _, want := range []string{"RKU_WriteLBA", "lba=0x40", "65536B", "ok", "bulk-out"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q is missing %q", got, want)
		}
	}
	fallback := TransferSummary(rockusb.Transfer{Op: 0x15, Stage: "cbw"})
	if !strings.Contains(fallback, "op 0x15") {
		t.Fatalf("summary without an op name = %q", fallback)
	}
}

// limitedWriter succeeds for a fixed number of records, then fails.
type limitedWriter struct {
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errors.New("log device full")
	}
	w.remaining--
	return len(p), nil
}

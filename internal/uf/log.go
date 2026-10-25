package uf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/SilenWang/rk3588-webflasher/internal/rockusb"
)

// LogSchema tags every line of a uf log.
const LogSchema = "uf-log/1"

// Recorder writes the JSON log: one record per line (NDJSON), so a multi-GiB
// flash does not have to hold ~10^5 transfer records in memory before the log
// can be exported. The first record is the header (plan + options), followed by
// transfer summaries, progress snapshots and events; Result closes the log.
//
// A recorder never blocks a run: the first write error is remembered and later
// records are dropped, so a full disk cannot turn a successful flash into a
// failure.
type Recorder struct {
	mu        sync.Mutex
	w         io.Writer
	enc       *json.Encoder
	startedAt time.Time
	err       error
	records   int64
}

// LogOptions echoes the knobs a run used, so a log can be reproduced.
type LogOptions struct {
	TimeoutMs   int64     `json:"timeoutMs"`
	Retries     int       `json:"retries"`
	ChunkBytes  int64     `json:"chunkBytes"`
	EraseTail   EraseMode `json:"eraseTail"`
	NoReset     bool      `json:"noReset"`
	StartItem   string    `json:"startItem,omitempty"`
	IDBModel    string    `json:"idbModel"`
	IDBVerified bool      `json:"idbVerified"`
}

type headerRecord struct {
	Schema  string     `json:"schema"`
	Kind    string     `json:"kind"`
	At      time.Time  `json:"at"`
	Plan    *Plan      `json:"plan"`
	Options LogOptions `json:"options"`
}

type transferRecord struct {
	Schema  string `json:"schema"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	rockusb.Transfer
}

type progressRecord struct {
	Schema   string   `json:"schema"`
	Kind     string   `json:"kind"`
	Progress Progress `json:"progress"`
}

type eventRecord struct {
	Schema string `json:"schema"`
	Kind   string `json:"kind"`
	Event  Event  `json:"event"`
}

type resultRecord struct {
	Schema string  `json:"schema"`
	Kind   string  `json:"kind"`
	Report *Report `json:"report"`
}

// NewRecorder writes the header record and returns the recorder. The caller
// feeds it from Options.Trace/Progress/Event and closes it with Result.
func NewRecorder(w io.Writer, plan *Plan, opts Options) (*Recorder, error) {
	if w == nil {
		return nil, errors.New("uf: nil log writer")
	}
	normalized, _ := opts.normalize()
	rec := &Recorder{w: w, enc: json.NewEncoder(w), startedAt: normalized.now()}
	rec.write(headerRecord{
		Schema: LogSchema,
		Kind:   "header",
		At:     rec.startedAt,
		Plan:   plan,
		Options: LogOptions{
			TimeoutMs: normalized.Timeout.Milliseconds(), Retries: normalized.Retries,
			ChunkBytes: normalized.ChunkSize, EraseTail: normalized.EraseTail, NoReset: normalized.NoReset,
			StartItem: normalized.StartItem, IDBModel: normalized.IDB.Name(), IDBVerified: normalized.IDB.Verified(),
		},
	})
	return rec, rec.err
}

// Transfer records one USB transfer with a one-line human summary.
func (r *Recorder) Transfer(t rockusb.Transfer) {
	r.write(transferRecord{Schema: LogSchema, Kind: "transfer", Summary: TransferSummary(t), Transfer: t})
}

// Progress records one progress snapshot.
func (r *Recorder) Progress(p Progress) {
	r.write(progressRecord{Schema: LogSchema, Kind: "progress", Progress: p})
}

// Event records one human-readable log line.
func (r *Recorder) Event(ev Event) {
	r.write(eventRecord{Schema: LogSchema, Kind: "event", Event: ev})
}

// Result writes the final report and returns the first write error seen.
func (r *Recorder) Result(rep *Report) error {
	r.write(resultRecord{Schema: LogSchema, Kind: "result", Report: rep})
	return r.Err()
}

// Err is the first write error; nil means the log is intact.
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Records is how many lines were written before the first error.
func (r *Recorder) Records() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records
}

func (r *Recorder) write(v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return
	}
	if err := r.enc.Encode(v); err != nil {
		r.err = err
		return
	}
	r.records++
}

// TransferSummary renders one transfer as a single line, which is what a
// reader scans first in the JSON log.
func TransferSummary(t rockusb.Transfer) string {
	name := t.OpName
	if name == "" {
		name = fmt.Sprintf("op 0x%02X", t.Op)
	}
	dir := t.Kind
	status := t.Status
	if status == "" {
		status = "ok"
	}
	if t.Stage == "data" && t.Note != "" {
		return fmt.Sprintf("%s %s %s %dB %s %.1fms (%s)", name, t.Stage, t.Note, t.Length, status, t.DurationMs, dir)
	}
	return fmt.Sprintf("%s %s %dB %s %.1fms (%s)", name, t.Stage, t.Length, status, t.DurationMs, dir)
}

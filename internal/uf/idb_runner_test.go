package uf

// Behaviour of the gpt (IDB) phase against a device that already holds an IDB.
// The rule these tests pin: read LBA 64 first, keep a valid block unless
// --rewrite-idb says otherwise, and always back the old bytes up before an
// overwrite.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// seededIDB builds a plausible block of the given type.
func seededIDB(kind IDBType) []byte {
	b := make([]byte, IDBProbeSectors*SectorSize)
	switch kind {
	case IDBTypeNew:
		copy(b, "RKNS")
		for i := 4; i < len(b); i++ {
			b[i] = byte(i * 3)
		}
	case IDBTypeRK28:
		copy(b, "RK28")
		for i := 4; i < len(b); i++ {
			b[i] = byte(i * 5)
		}
	}
	return b
}

func TestRunKeepsExistingNewIDB(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	existing := seededIDB(IDBTypeNew)
	cfg := testSimConfig(plan)
	cfg.IDB = existing
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err != nil {
		t.Fatalf("run: %v (report %+v)", err, rep)
	}
	if rep.IDB == nil || rep.IDB.Existing != string(IDBTypeNew) {
		t.Fatalf("IDB report = %+v, want an identified RKNS block", rep.IDB)
	}
	if rep.IDB.Overwritten {
		t.Error("report says the IDB was overwritten")
	}
	got, err := conn.FlashAt(IDBDefaultLBA, len(existing))
	if err != nil {
		t.Fatalf("read flash: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Fatal("the device's RKNS IDB was modified; it must be kept by default")
	}
	for _, ph := range rep.Phases {
		if ph.Phase == PhaseIDB && ph.Status != "skipped" {
			t.Fatalf("gpt phase status = %s, want skipped", ph.Status)
		}
	}
	// The skipped IDB must not be counted as transferred bytes.
	if want := layout.bootBytes + layout.stateBytes; rep.BytesWritten != want {
		t.Fatalf("bytes written = %d, want %d (IDB skipped)", rep.BytesWritten, want)
	}
	if rep.TotalBytes != rep.BytesWritten {
		t.Fatalf("planned total = %d, want the actual %d", rep.TotalBytes, rep.BytesWritten)
	}
}

func TestRunRewriteLegacyIDBUsesItsOwnModelAndBacksUp(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.IDB.Rewrite = true
	existing := seededIDB(IDBTypeRK28)
	cfg := testSimConfig(plan)
	cfg.IDB = existing
	conn := NewSimConnector(cfg)

	dir := t.TempDir()
	backup := filepath.Join(dir, "idb-backup.bin")
	opts := testOptions()
	opts.RewriteIDB = true
	opts.IDBBackupPath = backup

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err != nil {
		t.Fatalf("run: %v (report %+v)", err, rep)
	}
	if rep.IDB == nil || !rep.IDB.Overwritten || rep.IDB.Model != ModelRK28RC4 {
		t.Fatalf("IDB report = %+v", rep.IDB)
	}
	if rep.IDB.BackupPath != backup {
		t.Errorf("backup path = %q, want %q", rep.IDB.BackupPath, backup)
	}
	saved, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(saved, existing) {
		t.Fatal("backup does not hold the original IDB bytes")
	}
	want, err := NewRK28RC4Model().Build(LoaderInput{Container: layout.loader})
	if err != nil {
		t.Fatalf("build idb: %v", err)
	}
	got, err := conn.FlashAt(IDBDefaultLBA, len(want))
	if err != nil {
		t.Fatalf("read flash: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("flash does not hold the rebuilt rk28-rc4 block")
	}
}

// A New IDB is rebuilt with the New IDB model, in the format the device uses,
// never with the legacy whole-loader block.
func TestRunRebuildsNewIDBWithNewModel(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	plan.IDB.Rewrite = true
	existing := seededIDB(IDBTypeNew)
	cfg := testSimConfig(plan)
	cfg.IDB = existing
	conn := NewSimConnector(cfg)

	opts := testOptions()
	opts.RewriteIDB = true

	rep, err := runPlan(t, plan, layout, conn, opts)
	if err != nil {
		t.Fatalf("run failed: %v (report %+v)", err, rep)
	}
	want, err := NewRKBOOTModel().Build(LoaderInput{Container: layout.loader})
	if err != nil {
		t.Fatalf("build idb: %v", err)
	}
	got, err := conn.FlashAt(IDBDefaultLBA, len(want))
	if err != nil {
		t.Fatalf("read flash: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("flash does not hold the rebuilt New IDB")
	}
	if rep.IDB == nil || !rep.IDB.Overwritten || rep.IDB.Model != ModelRKBOOT {
		t.Fatalf("IDB report = %+v", rep.IDB)
	}
}

// Bytes at LBA 64 that are neither format are not silently destroyed.
func TestRunRefusesUnknownIDBWithoutRewrite(t *testing.T) {
	layout := newTestLayout(t)
	plan := layout.plan(t)
	unknown := seededIDB(IDBTypeEmpty)
	copy(unknown, "\xde\xad\xbe\xef")
	cfg := testSimConfig(plan)
	cfg.IDB = unknown
	conn := NewSimConnector(cfg)

	rep, err := runPlan(t, plan, layout, conn, testOptions())
	if err == nil {
		t.Fatalf("run succeeded; unknown bytes at LBA 64 must stop it (report %+v)", rep)
	}
	got, _ := conn.FlashAt(IDBDefaultLBA, len(unknown))
	if !bytes.Equal(got, unknown) {
		t.Fatal("unknown bytes at LBA 64 were modified")
	}
}

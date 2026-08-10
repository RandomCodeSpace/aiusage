package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The store runs in WAL mode: a committed write lands in <db>-wal and leaves the
// main file untouched until a checkpoint, which the daemon may not reach for a
// long time while it holds the database open. Statting only the main file
// therefore reports a busy collector as silent — the freshness chip ages, and
// the stall banner accuses a daemon that is inserting every cycle.
//
// Observed live before the fix: usage.db at 04:33 against usage.db-wal at 04:53,
// with the daemon logging a successful insert at 04:53.
func TestFileMTimeSeesWALWrites(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "usage.db")
	wal := db + "-wal"

	if err := os.WriteFile(db, []byte("main"), 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	if err := os.WriteFile(wal, []byte("wal"), 0o600); err != nil {
		t.Fatalf("write wal: %v", err)
	}

	// Main file stale, WAL current — the shape of a live WAL-mode daemon.
	old := time.Now().Add(-20 * time.Minute)
	fresh := time.Now().Add(-5 * time.Second)
	if err := os.Chtimes(db, old, old); err != nil {
		t.Fatalf("chtimes db: %v", err)
	}
	if err := os.Chtimes(wal, fresh, fresh); err != nil {
		t.Fatalf("chtimes wal: %v", err)
	}

	got := fileMTime(db)
	if got.Before(fresh.Add(-time.Second)) {
		t.Errorf("fileMTime = %v, want the WAL write at ~%v; a live collector reads as stalled",
			got, fresh)
	}
}

// A checkpointed database has no WAL beside it, and the main file's own mtime is
// then the write time. The probe must not regress to the zero time just because
// the sidecar is missing.
func TestFileMTimeWithoutWAL(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "usage.db")
	if err := os.WriteFile(db, []byte("main"), 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	when := time.Now().Add(-time.Minute)
	if err := os.Chtimes(db, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := fileMTime(db); got.IsZero() {
		t.Error("fileMTime returned the zero time for a checkpointed database with no WAL")
	}
}

// A database that does not exist at all still reports the zero time, which the
// caller reads as "cannot tell" rather than as a write.
func TestFileMTimeMissingDatabase(t *testing.T) {
	if got := fileMTime(filepath.Join(t.TempDir(), "absent.db")); !got.IsZero() {
		t.Errorf("fileMTime = %v for a missing database, want the zero time", got)
	}
}

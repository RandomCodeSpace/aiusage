package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// newLoadedModel builds a width-sized model over src pointing at dbPath and
// drives the first async load to completion, returning a model in the live
// (loaded) state with lastMTime seeded from dbPath.
func newLoadedModel(t *testing.T, src DataSource, dbPath string) Model {
	t.Helper()
	m := NewModel(src, Options{DBPath: dbPath})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return loadOnce(tm.(Model))
}

// touchDB writes/creates the file at path and sets its mtime to t, returning the
// path. Hermetic: everything lives under the test's temp dir.
func touchDB(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// TestLoadingStateBeforeFirstData verifies View() renders the branded loading
// state (spinner + "loading usage…" + db path) before any dataLoadedMsg, and
// that the dashboard chrome is NOT yet present.
func TestLoadingStateBeforeFirstData(t *testing.T) {
	m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(Model)

	if m.loaded {
		t.Fatal("model marked loaded before any dataLoadedMsg")
	}
	out := m.View()
	if !strings.Contains(out, "loading usage…") {
		t.Fatalf("loading state missing 'loading usage…' text:\n%s", out)
	}
	if !strings.Contains(out, "aiusage") {
		t.Fatal("loading state missing wordmark")
	}
	if !strings.Contains(out, "/tmp/usage.db") {
		t.Fatal("loading state missing db path")
	}
	// The dashboard footer/help chrome must not be on screen yet.
	if strings.Contains(out, "● live") {
		t.Fatal("live indicator shown before first data load")
	}

	// After the load lands, the dashboard renders instead.
	m = loadOnce(m)
	if !m.loaded {
		t.Fatal("model not loaded after dataLoadedMsg")
	}
	if strings.Contains(m.View(), "loading usage…") {
		t.Fatal("still showing loading state after first data load")
	}
}

// TestRefreshTickUnchangedMtimeNoQuery verifies a refresh tick whose db mtime is
// unchanged since the last load triggers NO new Summarize call: idle cost stays
// at the single os.Stat.
func TestRefreshTickUnchangedMtimeNoQuery(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "usage.db")
	touchDB(t, db, time.Now().Add(-time.Hour))

	f := &fakeData{}
	m := newLoadedModel(t, f, db)

	before := f.summarizeCalls
	tm, cmd := m.Update(refreshTickMsg{})
	m = tm.(Model)
	// Tick must re-arm (cmd != nil) but must NOT kick a load.
	if cmd == nil {
		t.Fatal("refresh tick did not re-arm the tick")
	}
	if m.loading {
		t.Fatal("refresh tick entered loading on unchanged mtime")
	}
	// Draining whatever the tick returned must not re-query: the re-armed tick is
	// a pure timer, no dataLoadedMsg, no Summarize.
	if f.summarizeCalls != before {
		t.Fatalf("unchanged mtime triggered %d new Summarize calls", f.summarizeCalls-before)
	}
}

// TestRefreshTickChangedMtimeReloads verifies a refresh tick whose db mtime
// advanced since the last load dispatches a reload (cache invalidated + load cmd
// that re-queries the source).
func TestRefreshTickChangedMtimeReloads(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "usage.db")
	touchDB(t, db, time.Now().Add(-time.Hour))

	f := &fakeData{}
	m := newLoadedModel(t, f, db)

	// Daemon wrote new events: bump the db mtime forward.
	touchDB(t, db, time.Now().Add(time.Hour))

	before := f.summarizeCalls
	tm, cmd := m.Update(refreshTickMsg{})
	m = tm.(Model)
	if !m.loading {
		t.Fatal("changed mtime did not enter the loading state")
	}
	if cmd == nil {
		t.Fatal("changed mtime produced no command")
	}
	m = runPending(t, m, cmd)
	if f.summarizeCalls <= before {
		t.Fatal("changed mtime did not re-query the data source")
	}
	if m.loading {
		t.Fatal("still loading after the reload landed")
	}
}

// TestManualRefreshForcesReload verifies the `r` key forces a reload regardless
// of mtime: it invalidates the cache and re-queries off the UI thread.
func TestManualRefreshForcesReload(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "usage.db")
	touchDB(t, db, time.Now())

	f := &fakeData{}
	m := newLoadedModel(t, f, db)

	before := f.summarizeCalls
	tm, cmd := m.Update(keyMsg("r"))
	m = tm.(Model)
	if !m.loading {
		t.Fatal("manual refresh did not enter the loading state")
	}
	if cmd == nil {
		t.Fatal("manual refresh produced no command")
	}
	m = runPending(t, m, cmd)
	if f.summarizeCalls <= before {
		t.Fatal("manual refresh did not re-query the data source")
	}
}

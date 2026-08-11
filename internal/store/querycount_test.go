package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// This file provides a driver-level query counter: a wrapper around the
// modernc sqlite driver that counts every SQL statement database/sql executes.
// Because the wrapper hides the optional fast-path interfaces (QueryerContext,
// ExecerContext, ...), database/sql routes everything through Prepare + Stmt,
// where Exec/Query are counted exactly once per statement execution. It pins
// the store's query-count contracts (single-pass Summarize totals, batched
// SourceStats) so an N+1 regression fails a test instead of shipping.

// stmtCount counts statement executions across all counting connections. Tests
// in this package run sequentially, so before/after diffs are race-free.
var stmtCount atomic.Int64

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: c}, nil
}

type countingConn struct{ driver.Conn }

// preparedSQL records the text of every statement prepared through the counting
// driver. Statement counts pin how MANY queries run; this pins WHAT they ask
// for, which is how the event projection can be checked to leave the raw
// column in the database rather than merely to discard it after transfer.
var preparedSQL struct {
	mu sync.Mutex
	q  []string
}

func (c countingConn) Prepare(q string) (driver.Stmt, error) {
	preparedSQL.mu.Lock()
	preparedSQL.q = append(preparedSQL.q, q)
	preparedSQL.mu.Unlock()
	s, err := c.Conn.Prepare(q)
	if err != nil {
		return nil, err
	}
	return countingStmt{Stmt: s}, nil
}

// queriesDuring returns the SQL prepared while fn executed.
func queriesDuring(fn func()) []string {
	preparedSQL.mu.Lock()
	before := len(preparedSQL.q)
	preparedSQL.mu.Unlock()

	fn()

	preparedSQL.mu.Lock()
	defer preparedSQL.mu.Unlock()
	return append([]string{}, preparedSQL.q[before:]...)
}

// countingStmt counts executions via the context interfaces, which
// database/sql prefers over the embedded legacy Exec/Query whenever they are
// present — so every statement execution passes through exactly one counter.
type countingStmt struct{ driver.Stmt }

func (s countingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	stmtCount.Add(1)
	ec, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("querycount_test: inner driver stmt lacks StmtExecContext")
	}
	return ec.ExecContext(ctx, args)
}

func (s countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	stmtCount.Add(1)
	qc, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("querycount_test: inner driver stmt lacks StmtQueryContext")
	}
	return qc.QueryContext(ctx, args)
}

func init() {
	// sql.Open never connects, so this only resolves the registered driver.
	probe, err := sql.Open("sqlite", "probe")
	if err != nil {
		panic(fmt.Sprintf("resolve sqlite driver: %v", err))
	}
	inner := probe.Driver()
	probe.Close()
	sql.Register("sqlite-counting", countingDriver{inner: inner})
}

// openCounting opens a fresh store whose statements are counted via the
// wrapped driver, mirroring Open's DSN pragmas and schema setup.
func openCounting(t *testing.T) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite-counting", dsn)
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	if err := ensureSchema(context.Background(), db, path); err != nil {
		db.Close()
		t.Fatalf("ensure schema: %v", err)
	}
	st := &SQLite{db: db, path: path}
	t.Cleanup(func() { st.Close() })
	return st
}

// statementsDuring returns how many SQL statements ran while fn executed.
func statementsDuring(fn func()) int64 {
	before := stmtCount.Load()
	fn()
	return stmtCount.Load() - before
}

// seedTools inserts events across three tools with models and sessions so the
// read paths under test have multiple groups to aggregate.
func seedTools(t *testing.T, st *SQLite) {
	t.Helper()
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var evs []model.UsageEvent
	for i, tool := range []string{"tool-a", "tool-b", "tool-c"} {
		for j := 0; j < 3; j++ {
			e := ev(fmt.Sprintf("%s|%d", tool, j), tool, at.Add(time.Duration(i*3+j)*time.Minute), 100)
			e.Model = fmt.Sprintf("model-%d", j%2)
			e.SessionID = fmt.Sprintf("%s-s%d", tool, j)
			evs = append(evs, e)
		}
	}
	if _, err := st.InsertEvents(context.Background(), evs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestSummarizeQueryCount pins the single-pass Summarize contract at the SQL
// level: an ungrouped summarize is exactly ONE statement (the result row IS the
// grand total — re-running the aggregate for totals would be a second), and a
// grouped summarize is exactly TWO (the single grouped pass + the narrow
// distinct-session count, which cannot be summed across buckets).
func TestSummarizeQueryCount(t *testing.T) {
	st := openCounting(t)
	seedTools(t, st)
	ctx := context.Background()

	n := statementsDuring(func() {
		s, err := st.Summarize(ctx, Filter{})
		if err != nil {
			t.Fatalf("Summarize ungrouped: %v", err)
		}
		if s.Totals.Events != 9 {
			t.Fatalf("ungrouped totals events = %d, want 9", s.Totals.Events)
		}
	})
	if n != 1 {
		t.Errorf("ungrouped Summarize ran %d statements, want exactly 1 (single pass)", n)
	}

	n = statementsDuring(func() {
		s, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
		if err != nil {
			t.Fatalf("Summarize grouped: %v", err)
		}
		if len(s.Buckets) != 3 || s.Totals.Events != 9 {
			t.Fatalf("grouped buckets=%d totals=%d, want 3/9", len(s.Buckets), s.Totals.Events)
		}
	})
	if n != 2 {
		t.Errorf("grouped Summarize ran %d statements, want exactly 2 (grouped pass + distinct sessions)", n)
	}
}

// TestSourceStatsQueryCount pins the batched SourceStats contract: exactly TWO
// statements regardless of tool count (per-tool aggregate + one distinct-models
// query), never the old 1+N per-tool round trips.
func TestSourceStatsQueryCount(t *testing.T) {
	st := openCounting(t)
	seedTools(t, st)

	n := statementsDuring(func() {
		stats, err := st.SourceStats(context.Background())
		if err != nil {
			t.Fatalf("SourceStats: %v", err)
		}
		if len(stats) != 3 {
			t.Fatalf("stats rows = %d, want 3", len(stats))
		}
		for _, s := range stats {
			if len(s.Models) != 2 {
				t.Errorf("tool %s models = %v, want 2 entries", s.Tool, s.Models)
			}
		}
	})
	if n != 2 {
		t.Errorf("SourceStats ran %d statements for 3 tools, want exactly 2 (batched, no N+1)", n)
	}
}

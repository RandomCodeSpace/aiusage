package tui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// cyclebench_test.go measures the `t` (range cycle) key against a REAL on-disk
// ledger at the scale the owner's machine carries. The fakeData benchmarks in
// bench_test.go deliberately cost nothing per query, which hides the whole
// problem: a range toggle's cost is the store aggregation the flight runs, not
// the view-model rebuild.
//
// Two shapes are measured, because they answer different questions:
//
//   - BenchmarkRangeCycleBurst — eight presses dispatched before any flight
//     lands, on a cold cache. This is the reported repro: the CPU a rapid
//     toggle burst actually burns.
//   - BenchmarkRangeCycleRevisit — the same eight presses on a warm cache
//     (two full cycles through four ranges, so presses 5-8 revisit ranges
//     1-4). This answers whether revisits re-query.
//
//   - BenchmarkRangeCyclePaced — the same burst at human keypress speed, which
//     is where early cancellation has to actually earn its keep: a doomed
//     flight gets real work done before the next press supersedes it.
//
// All of them report queries/op (the number of store.Summarize calls served)
// and query-ms/op (the wall time summed ACROSS the flight goroutines inside
// those calls). Both are the honest cost measure here: the profile attributes
// essentially the whole burst to SQLite's VDBE, so summed query time is the
// CPU burned, while the benchmark's own ns/op understates a burst whose
// flights run concurrently and overstates a paced one that mostly sleeps.

// benchLedgerEvents is the synthetic ledger's row count, chosen to sit at the
// scale of the owner's real ledger (~361k events and growing).
const benchLedgerEvents = 360_000

// benchLedgerDays is how many local calendar days the synthetic events span, so
// today / 7d / 30d / all each select a materially different slice.
const benchLedgerDays = 120

// benchNow is the pinned load-generation clock. Events are seeded backwards
// from it so the range windows land on real data whatever day the benchmark
// runs.
var benchNow = time.Date(2026, 8, 9, 15, 0, 0, 0, time.Local)

var (
	benchLedgerOnce sync.Once
	benchLedgerDir  string
	benchLedgerPath string
	benchLedgerErr  error
)

// TestMain removes the seeded benchmark ledger (tens of MB) after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if benchLedgerDir != "" {
		os.RemoveAll(benchLedgerDir)
	}
	os.Exit(code)
}

// seedBenchLedger builds the synthetic ledger once per test binary. Batched
// inserts keep the seed to a handful of transactions; the shape (6 tools, 8
// models, 12 projects, rotating sessions) gives every grouped query a realistic
// number of buckets to aggregate into.
func seedBenchLedger() (string, error) {
	dir, err := os.MkdirTemp("", "aiusage-tui-bench-")
	if err != nil {
		return "", err
	}
	benchLedgerDir = dir
	path := filepath.Join(dir, "usage.db")
	st, err := store.Open(path)
	if err != nil {
		return "", err
	}
	defer st.Close()

	tools := []string{"claude-code", "codex", "copilot", "opencode", "hermes", "gemini"}
	models := []string{
		"claude-opus-4", "claude-sonnet-4", "claude-haiku-4",
		"gpt-5", "gpt-5-mini", "o4", "gemini-2.5-pro", "qwen3-coder",
	}
	projects := make([]string, 12)
	for i := range projects {
		projects[i] = "/work/project-" + strconv.Itoa(i)
	}

	start := benchNow.AddDate(0, 0, -benchLedgerDays)
	step := benchNow.Sub(start) / time.Duration(benchLedgerEvents)

	const batch = 20_000
	evs := make([]model.UsageEvent, 0, batch)
	for i := 0; i < benchLedgerEvents; i++ {
		at := start.Add(time.Duration(i) * step)
		evs = append(evs, model.UsageEvent{
			Tool:                tools[i%len(tools)],
			Model:               models[i%len(models)],
			SessionID:           "sess-" + strconv.Itoa(i/40),
			Project:             projects[i%len(projects)],
			EventTime:           at,
			ObservedTime:        at,
			InputTokens:         int64(100 + i%900),
			OutputTokens:        int64(50 + i%400),
			CacheCreationTokens: int64(i % 200),
			CacheReadTokens:     int64(i % 5000),
			ReasoningTokens:     int64(i % 60),
			TotalTokens:         int64(150 + i%1300),
			DedupKey:            "bench|" + strconv.Itoa(i),
			Kind:                model.KindUsage,
		})
		if len(evs) == batch {
			if _, err := st.InsertEvents(context.Background(), evs); err != nil {
				return "", err
			}
			evs = evs[:0]
		}
	}
	if len(evs) > 0 {
		if _, err := st.InsertEvents(context.Background(), evs); err != nil {
			return "", err
		}
	}
	return path, nil
}

// benchLedgerStore opens the shared synthetic ledger read-only, seeding it on
// first use.
func benchLedgerStore(b *testing.B) *store.SQLite {
	b.Helper()
	benchLedgerOnce.Do(func() { benchLedgerPath, benchLedgerErr = seedBenchLedger() })
	if benchLedgerErr != nil {
		b.Fatalf("seed bench ledger: %v", benchLedgerErr)
	}
	st, err := store.OpenReadOnly(benchLedgerPath)
	if err != nil {
		b.Fatalf("open bench ledger: %v", err)
	}
	b.Cleanup(func() { st.Close() })
	return st
}

// countingSource counts the Summarize calls that reach the store and the wall
// time spent inside them, summed across every flight goroutine. A cancelled
// query still bills the time it burned before it aborted, which is exactly what
// the fix has to shrink.
type countingSource struct {
	src   DataSource
	calls atomic.Int64
	nanos atomic.Int64
}

func (c *countingSource) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	c.calls.Add(1)
	t0 := time.Now()
	s, err := c.src.Summarize(ctx, f)
	c.nanos.Add(int64(time.Since(t0)))
	return s, err
}

func (c *countingSource) SummarizeActivity(ctx context.Context, f store.ActivityFilter) (*store.ActivitySummary, error) {
	c.calls.Add(1)
	t0 := time.Now()
	s, err := c.src.SummarizeActivity(ctx, f)
	c.nanos.Add(int64(time.Since(t0)))
	return s, err
}

func (c *countingSource) TopActivity(ctx context.Context, f store.ActivityFilter, by store.ActivityOrder, limit int) ([]store.ActivityBucket, error) {
	c.calls.Add(1)
	t0 := time.Now()
	rows, err := c.src.TopActivity(ctx, f, by, limit)
	c.nanos.Add(int64(time.Since(t0)))
	return rows, err
}

func (c *countingSource) SummarizeTurnContext(ctx context.Context, dim model.TurnDimension, f store.ActivityFilter) (*store.TurnContextSummary, error) {
	c.calls.Add(1)
	t0 := time.Now()
	s, err := c.src.SummarizeTurnContext(ctx, dim, f)
	c.nanos.Add(int64(time.Since(t0)))
	return s, err
}

func (c *countingSource) TopTurnContext(ctx context.Context, dim model.TurnDimension, f store.ActivityFilter, by store.ActivityOrder, limit int) ([]store.TurnContextBucket, error) {
	c.calls.Add(1)
	t0 := time.Now()
	rows, err := c.src.TopTurnContext(ctx, dim, f, by, limit)
	c.nanos.Add(int64(time.Since(t0)))
	return rows, err
}

// sample snapshots the counters so a benchmark loop can bill one iteration.
func (c *countingSource) sample() (calls, nanos int64) {
	return c.calls.Load(), c.nanos.Load()
}

// reportQueryCost turns accumulated counters into the two metrics every cycle
// benchmark reports.
func reportQueryCost(b *testing.B, calls, nanos int64) {
	b.ReportMetric(float64(calls)/float64(b.N), "queries/op")
	b.ReportMetric(float64(nanos)/float64(b.N)/1e6, "query-ms/op")
}

// benchLedgerModel returns a loaded Overview model over the synthetic ledger
// with its clock pinned to benchNow, plus the counting source wrapped around
// the store.
func benchLedgerModel(b *testing.B) (Model, *countingSource) {
	b.Helper()
	src := &countingSource{src: benchLedgerStore(b)}
	m := NewModel(src, Options{DBPath: benchLedgerPath})
	m.data.now = func() time.Time { return benchNow }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(Model)
	m.loadNow = benchNow
	return loadOnce(m), src
}

// cycleBurst presses `t` n times without waiting for any flight, runs every
// dispatched flight concurrently (what the Bubble Tea runtime does), then feeds
// the results back in arrival order. Only the last generation applies; the rest
// are the superseded flights whose CPU this benchmark exists to measure. gap is
// the pause between presses — zero for the tightest burst, a keypress interval
// for the paced one.
func cycleBurst(m Model, n int, gap time.Duration) Model {
	msgs := make([]tea.Msg, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if i > 0 && gap > 0 {
			time.Sleep(gap)
		}
		tm, cmd := m.Update(keyMsg("t"))
		m = tm.(Model)
		if cmd == nil {
			continue
		}
		wg.Add(1)
		go func(i int, c tea.Cmd) {
			defer wg.Done()
			msgs[i] = c()
		}(i, cmd)
	}
	wg.Wait()
	for _, msg := range msgs {
		if msg != nil {
			m = send(m, msg)
		}
	}
	return m
}

// cycleSequential presses `t` n times, each flight awaited and applied before
// the next press: every window the burst passes through is actually loaded.
func cycleSequential(m Model, n int) Model {
	for i := 0; i < n; i++ {
		tm, cmd := m.Update(keyMsg("t"))
		m = tm.(Model)
		if cmd != nil {
			m = send(m, cmd())
		}
	}
	return m
}

// benchCyclePresses is the burst length: two full cycles through the four
// ranges, which is what a couple of seconds of impatient `t` produces. Being a
// whole number of cycles it also lands back on the range it started from (7d),
// so the surviving flight — the only one that should cost anything — is the
// same window in every variant.
const benchCyclePresses = 8

// benchKeyGap is a fast human keypress interval — roughly eight presses a
// second, which is what "rapidly toggling" means at a keyboard.
const benchKeyGap = 120 * time.Millisecond

// benchColdCycle runs one cold-cache burst per iteration and reports the query
// cost. Shared by the tight and paced variants, which differ only in gap.
func benchColdCycle(b *testing.B, gap time.Duration) {
	m, src := benchLedgerModel(b)
	var queries, nanos int64
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		m.data.Invalidate()
		c0, n0 := src.sample()
		b.StartTimer()

		m = cycleBurst(m, benchCyclePresses, gap)

		b.StopTimer()
		c1, n1 := src.sample()
		queries += c1 - c0
		nanos += n1 - n0
		b.StartTimer()
	}
	reportQueryCost(b, queries, nanos)
}

// BenchmarkRangeCycleBurst is the reported repro at its tightest: a burst of
// range toggles on a COLD cache (the state after any daemon write invalidates
// it), every press dispatching a full query set against the real ledger before
// the previous one has landed.
func BenchmarkRangeCycleBurst(b *testing.B) { benchColdCycle(b, 0) }

// BenchmarkRangeCyclePaced is the same burst at keyboard speed. It is the
// number that matters for the fix: a superseded flight here has ~120ms of head
// start, so what early cancellation saves is measured rather than assumed.
func BenchmarkRangeCyclePaced(b *testing.B) { benchColdCycle(b, benchKeyGap) }

// BenchmarkRangeCycleRevisit measures the same burst on a WARM cache: two full
// cycles, so presses 5-8 revisit windows presses 1-4 already loaded. It answers
// whether a revisit re-queries — queries/op of zero means the summary cache
// already memoizes across toggles.
//
// The warm-up is SEQUENTIAL, not a burst. A burst no longer loads the windows
// it passes through (that is the fix), so warming with one would leave the
// cache half-cold and this benchmark would measure the warm-up's gaps instead
// of the revisit it is named for.
func BenchmarkRangeCycleRevisit(b *testing.B) {
	m, src := benchLedgerModel(b)
	m = cycleSequential(m, benchCyclePresses) // load every window once
	var queries, nanos int64
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c0, n0 := src.sample()
		m = cycleBurst(m, benchCyclePresses, 0)
		c1, n1 := src.sample()
		queries += c1 - c0
		nanos += n1 - n0
	}
	reportQueryCost(b, queries, nanos)
}

// BenchmarkRangeCycleSequential is the control: the same presses, each flight
// awaited before the next press, on a cold cache. It isolates the unavoidable
// work (one full query set per distinct window) from the waste the burst adds.
func BenchmarkRangeCycleSequential(b *testing.B) {
	m, src := benchLedgerModel(b)
	var queries, nanos int64
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		m.data.Invalidate()
		c0, n0 := src.sample()
		b.StartTimer()

		m = cycleSequential(m, benchCyclePresses)

		b.StopTimer()
		c1, n1 := src.sample()
		queries += c1 - c0
		nanos += n1 - n0
		b.StartTimer()
	}
	reportQueryCost(b, queries, nanos)
}

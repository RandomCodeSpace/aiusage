package tui

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/store"
)

var ansiWindow = regexp.MustCompile("\x1b\\[[0-9;]*m")

// windowClock is the pinned generation clock every test here loads against.
var windowClock = time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)

// TestSpanWindowStepsWholeCalendarSpans pins the issue #19 arithmetic: one press
// of [ moves the window by exactly ONE whole calendar span, measured off the
// same local-midnight origin the live window uses (issue #4, 1a). Consecutive
// windows are half-open and abut exactly — no overlap, no gap — so stepping can
// never double-count or skip a bucket.
func TestSpanWindowStepsWholeCalendarSpans(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 8, 9, 15, 4, 5, 0, loc)
	day := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}

	cases := []struct {
		r            Range
		step         int
		since, until time.Time
	}{
		{RangeToday, -1, day(2026, 8, 8), day(2026, 8, 9)},
		{RangeToday, -2, day(2026, 8, 7), day(2026, 8, 8)},
		{Range7d, -1, day(2026, 7, 27), day(2026, 8, 3)},
		{Range7d, -2, day(2026, 7, 20), day(2026, 7, 27)},
		{Range30d, -1, day(2026, 6, 11), day(2026, 7, 11)},
	}
	for _, c := range cases {
		since, until := Span{R: c.r, Step: c.step}.Window(now)
		if !since.Equal(c.since) {
			t.Errorf("%s step %d: since = %v, want %v", c.r.Label(), c.step, since, c.since)
		}
		if !until.Equal(c.until) {
			t.Errorf("%s step %d: until = %v, want %v", c.r.Label(), c.step, until, c.until)
		}
	}

	// Step 0 is the live window: unchanged, and still open at the top.
	for _, r := range []Range{RangeToday, Range7d, Range30d, RangeAll} {
		liveSince, liveUntil := r.Window(now)
		since, until := Span{R: r}.Window(now)
		if !since.Equal(liveSince) || !until.Equal(liveUntil) {
			t.Errorf("%s: live span window = [%v, %v), want the range window [%v, %v)",
				r.Label(), since, until, liveSince, liveUntil)
		}
	}

	// The open-ended range has no span to step: it stays fully open whatever the
	// step says.
	if since, until := (Span{R: RangeAll, Step: -3}).Window(now); !since.IsZero() || !until.IsZero() {
		t.Errorf("all step -3: window = [%v, %v), want fully open", since, until)
	}
	if RangeAll.Steppable() {
		t.Error("RangeAll reported steppable")
	}
}

// TestSpanLabelNamesSteppedWindow: a stepped window must never read as "now" —
// its label names the span width and the window's first local day.
func TestSpanLabelNamesSteppedWindow(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 8, 9, 15, 4, 5, 0, loc)

	cases := []struct {
		sp   Span
		want string
	}{
		{Span{R: Range7d}, "7d"},
		{Span{R: RangeToday}, "today"},
		{Span{R: RangeAll, Step: -1}, "all"},
		{Span{R: RangeToday, Step: -1}, "1d @ 2026-08-08"},
		{Span{R: Range7d, Step: -2}, "7d @ 2026-07-20"},
		{Span{R: Range30d, Step: -1}, "30d @ 2026-06-11"},
	}
	for _, c := range cases {
		if got := c.sp.Label(now); got != c.want {
			t.Errorf("Span{%s, %d}.Label = %q, want %q", c.sp.R.Label(), c.sp.Step, got, c.want)
		}
	}
}

// TestSpanLabelFormsShrinkWithoutLying: the header may fall back to a shorter
// window name on a narrow terminal, so EVERY form it can fall back to has to
// name the same window on its own. The forms are widest first, the widest is
// what Label returns, and the month-day rung is offered only while the window
// starts in the current year: a cross-year window drops straight to the step
// offset instead of printing an ambiguous "11-13".
func TestSpanLabelFormsShrinkWithoutLying(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 8, 9, 15, 4, 5, 0, loc)

	cases := []struct {
		sp   Span
		want []string
	}{
		{Span{R: Range7d}, []string{"7d"}},
		{Span{R: RangeAll, Step: -3}, []string{"all"}},
		{Span{R: RangeToday, Step: -1}, []string{"1d @ 2026-08-08", "1d @ 08-08", "1d-1"}},
		{Span{R: Range7d, Step: -2}, []string{"7d @ 2026-07-20", "7d @ 07-20", "7d-2"}},
		// Eight 30d windows back lands in 2025: the year is load-bearing, so the
		// month-day rung is not offered at all.
		{Span{R: Range30d, Step: -8}, []string{"30d @ 2025-11-13", "30d-8"}},
	}
	for _, c := range cases {
		got := c.sp.labelForms(now)
		if len(got) != len(c.want) {
			t.Errorf("Span{%s, %d}.labelForms = %q, want %q", c.sp.R.Label(), c.sp.Step, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Span{%s, %d}.labelForms[%d] = %q, want %q",
					c.sp.R.Label(), c.sp.Step, i, got[i], c.want[i])
			}
			if i > 0 && len(got[i]) >= len(got[i-1]) {
				t.Errorf("Span{%s, %d}.labelForms are not narrowing: %q then %q",
					c.sp.R.Label(), c.sp.Step, got[i-1], got[i])
			}
		}
		if lbl := c.sp.Label(now); lbl != got[0] {
			t.Errorf("Span{%s, %d}.Label = %q, want the widest form %q",
				c.sp.R.Label(), c.sp.Step, lbl, got[0])
		}
		// Whatever the form, it must resolve back to the window it names.
		wantSince, wantUntil := c.sp.Window(now)
		if c.sp.Step >= 0 || !c.sp.R.Steppable() {
			continue
		}
		for _, form := range got {
			since, until, ok := parseWindowChip(form, now)
			if !ok {
				t.Errorf("form %q does not parse back to a window", form)
				continue
			}
			if !since.Equal(wantSince) || !until.Equal(wantUntil) {
				t.Errorf("form %q names [%v, %v), want [%v, %v)",
					form, since, until, wantSince, wantUntil)
			}
		}
	}
}

// rangeChipRe captures the text between the range chip's guillemets. It is
// line-bounded on purpose: a chip the frame truncated has lost its closing
// guillemet, and must NOT be completed by a chevron further down the frame.
var rangeChipRe = regexp.MustCompile(`‹\s*([^›\n]*?)\s*›`)

// rangeChipLabel returns the window name the rendered frame is showing.
func rangeChipLabel(frame string) (string, bool) {
	mm := rangeChipRe.FindStringSubmatch(frame)
	if mm == nil {
		return "", false
	}
	return mm[1], true
}

// parseWindowChip resolves a rendered window name back to the [since, until)
// window it claims: the full date, the month-day and the step-offset forms
// alike. This is what makes the legibility test mean something: a fragment like
// "7d @ 2026-07" fails here instead of satisfying a substring check, and the
// arithmetic is the test's own (local midnight, whole calendar spans) rather
// than a call back into Span.Window.
func parseWindowChip(label string, now time.Time) (since, until time.Time, ok bool) {
	days := map[string]int{"1d": 1, "7d": 7, "30d": 30}
	if head, tail, cut := strings.Cut(label, " @ "); cut {
		n, known := days[head]
		if !known {
			return time.Time{}, time.Time{}, false
		}
		if len(tail) == len("01-02") {
			tail = strconv.Itoa(now.Year()) + "-" + tail
		}
		s, err := time.ParseInLocation("2006-01-02", tail, now.Location())
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		return s, s.AddDate(0, 0, n), true
	}
	head, back, cut := strings.Cut(label, "-")
	if !cut {
		return time.Time{}, time.Time{}, false
	}
	n, known := days[head]
	steps, err := strconv.Atoi(back)
	if !known || err != nil || steps < 1 {
		return time.Time{}, time.Time{}, false
	}
	// The live window's first local day is n-1 days back; each step moves the
	// whole span again.
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	s := mid.AddDate(0, 0, -(n-1)-n*steps)
	return s, s.AddDate(0, 0, n), true
}

// TestRangeChipNamesTheWindowAtEveryWidth is the issue #45 legibility gate. The
// header used to render the ONE longest label and let the frame cut it: at 42
// columns the chip read "RANGE < 7d @ 2026" and at 60 "RANGE < 7d @ 2026-07",
// text that looks like a date and is not one. At every width the chip must now
// carry a COMPLETE name that resolves back to the window on screen.
func TestRangeChipNamesTheWindowAtEveryWidth(t *testing.T) {
	f := &fakeData{}
	m := newPinnedModel(t, f, windowClock)
	for i := 0; i < 2; i++ {
		tm, cmd := m.Update(keyMsg("["))
		m = runPending(t, tm.(Model), cmd)
	}
	wantSince, wantUntil := m.span().Window(windowClock)

	for _, w := range []int{42, 60, 80} {
		m = send(m, tea.WindowSizeMsg{Width: w, Height: 30})
		frame := ansiWindow.ReplaceAllString(m.View().Content, "")
		label, ok := rangeChipLabel(frame)
		if !ok {
			t.Errorf("w=%d: no complete range chip in the frame; the window label was cut:\n%s",
				w, frame)
			continue
		}
		since, until, ok := parseWindowChip(label, windowClock)
		if !ok {
			t.Errorf("w=%d: range chip %q does not name a window:\n%s", w, label, frame)
			continue
		}
		if !since.Equal(wantSince) || !until.Equal(wantUntil) {
			t.Errorf("w=%d: range chip %q names [%v, %v), want the shown window [%v, %v)",
				w, label, since, until, wantSince, wantUntil)
		}
		// A complete chip is not enough on its own: the header can pay for it by
		// letting the frame clip whatever sits to its right, which is how the
		// help chip used to vanish from 42 through 50 columns. The ladder exists
		// so the label yields precision instead of costing a neighbour. Nothing
		// else in the package notices if it stops doing that.
		if !strings.Contains(frame, "? help") {
			t.Errorf("w=%d: the help chip was clipped away to fit the range label %q:\n%s",
				w, label, frame)
		}
	}

	// Back at the live edge the chip names the range and never a day, at every
	// one of those widths.
	for i := 0; i < 2; i++ {
		tm, cmd := m.Update(keyMsg("]"))
		m = runPending(t, tm.(Model), cmd)
	}
	for _, w := range []int{42, 60, 80} {
		m = send(m, tea.WindowSizeMsg{Width: w, Height: 30})
		frame := ansiWindow.ReplaceAllString(m.View().Content, "")
		label, ok := rangeChipLabel(frame)
		if !ok {
			t.Errorf("w=%d: the live frame carries no complete range chip:\n%s", w, frame)
			continue
		}
		if label != m.rng.Label() {
			t.Errorf("w=%d: live range chip = %q, want the range label %q", w, label, m.rng.Label())
		}
	}
}

// windowSource records every filter it serves so a test can assert WHICH window
// the stepped load actually queried.
type windowSource struct {
	fakeData
	mu      sync.Mutex
	filters []store.Filter
}

func (s *windowSource) Summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	s.mu.Lock()
	s.filters = append(s.filters, f)
	s.mu.Unlock()
	return s.fakeData.Summarize(ctx, f)
}

func (s *windowSource) reset() {
	s.mu.Lock()
	s.filters = nil
	s.mu.Unlock()
}

func (s *windowSource) seen() []store.Filter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Filter(nil), s.filters...)
}

// TestStepBackQueriesPreviousWindowOffTheUIThread: [ must ride the ordinary
// async load path — zero DataSource queries on the UI thread, all of them in
// the dispatched flight — and every one of those queries must carry the closed
// previous-span window.
func TestStepBackQueriesPreviousWindowOffTheUIThread(t *testing.T) {
	src := &windowSource{}
	m := newPinnedModel(t, src, windowClock)
	src.reset()

	var cmd tea.Cmd
	n := queriesDuring(&src.fakeData, func() {
		tm, c := m.Update(keyMsg("["))
		m, cmd = tm.(Model), c
	})
	if n != 0 {
		t.Fatalf("step-back keypress ran %d queries on the UI thread, want 0", n)
	}
	if cmd == nil {
		t.Fatal("step-back dispatched no load cmd")
	}
	if m.step != -1 {
		t.Fatalf("step = %d, want -1", m.step)
	}

	msg := cmd() // background flight
	n = queriesDuring(&src.fakeData, func() { m = send(m, msg) })
	if n != 0 {
		t.Fatalf("apply-side reload ran %d queries on the UI thread, want 0", n)
	}

	wantSince, wantUntil := Span{R: Range7d, Step: -1}.Window(windowClock)
	seen := src.seen()
	if len(seen) == 0 {
		t.Fatal("stepped flight ran no queries")
	}
	for _, f := range seen {
		if f.Until.IsZero() {
			t.Fatalf("stepped query left the window open at the top: %+v", f)
		}
		// The delta-chip window is the span BEFORE the shown one; every other
		// query keys off the shown window itself.
		if f.Since.Equal(wantSince) && f.Until.Equal(wantUntil) {
			continue
		}
		prevSince, prevUntil := Span{R: Range7d, Step: -2}.Window(windowClock)
		if f.Since.Equal(prevSince) && f.Until.Equal(prevUntil) {
			continue
		}
		t.Fatalf("stepped query window = [%v, %v), want the stepped [%v, %v) or its predecessor",
			f.Since, f.Until, wantSince, wantUntil)
	}
}

// TestSteppedWindowIsCacheWarmOnReturn: the stepped windows are quantized the
// same way the live one is, so walking back and forward again must not re-query.
func TestSteppedWindowIsCacheWarmOnReturn(t *testing.T) {
	f := &fakeData{}
	m := newPinnedModel(t, f, windowClock)

	walk := func(k string) {
		t.Helper()
		tm, cmd := m.Update(keyMsg(k))
		m = runPending(t, tm.(Model), cmd)
	}
	walk("[")
	walk("]")

	n := queriesDuring(f, func() {
		walk("[")
		walk("]")
	})
	if n != 0 {
		t.Fatalf("re-walking the same windows ran %d queries, want 0 (cache keys unstable)", n)
	}
}

// TestStepForwardStopsAtPresent: ] must never produce a future window. At the
// live edge it is inert — no state change, no load — and the binding is not
// advertised.
func TestStepForwardStopsAtPresent(t *testing.T) {
	f := &fakeData{}
	m := newPinnedModel(t, f, windowClock)

	gen := m.loadGen
	tm, cmd := m.Update(keyMsg("]"))
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("step-forward at the live edge dispatched a load")
	}
	if m.step != 0 {
		t.Fatalf("step = %d after ] at the live edge, want 0", m.step)
	}
	if m.loadGen != gen {
		t.Fatalf("load generation moved (%d → %d) for an inert keypress", gen, m.loadGen)
	}
	if m.keys.StepFwd.Enabled() {
		t.Error("] advertised at the live edge")
	}

	// Two back, three forward: the third is absorbed at the present.
	for i := 0; i < 2; i++ {
		tm, cmd = m.Update(keyMsg("["))
		m = runPending(t, tm.(Model), cmd)
	}
	if m.step != -2 {
		t.Fatalf("step = %d after two [ presses, want -2", m.step)
	}
	if !m.keys.StepFwd.Enabled() {
		t.Error("] not advertised while the window is behind the present")
	}
	for i := 0; i < 3; i++ {
		tm, cmd = m.Update(keyMsg("]"))
		m = runPending(t, tm.(Model), cmd)
	}
	if m.step != 0 {
		t.Fatalf("step = %d after stepping forward past the present, want 0", m.step)
	}
}

// TestStepKeysInertOnOpenRange: "all" has no span, so neither key is live and
// neither is advertised.
func TestStepKeysInertOnOpenRange(t *testing.T) {
	f := &fakeData{}
	m := newPinnedModel(t, f, windowClock)
	m.rng = RangeAll
	m.syncStepKeys()

	for _, k := range []string{"[", "]"} {
		tm, cmd := m.Update(keyMsg(k))
		got := tm.(Model)
		if cmd != nil {
			t.Fatalf("%q dispatched a load on the open-ended range", k)
		}
		if got.step != 0 {
			t.Fatalf("%q moved the window (step = %d) on the open-ended range", k, got.step)
		}
	}
	if m.keys.StepBack.Enabled() || m.keys.StepFwd.Enabled() {
		t.Error("window stepping advertised on the open-ended range")
	}
}

// TestSteppedWindowIsNamedInTheFrame: the rendered frame must name the window
// being shown, so a stepped view is never mistaken for "now". The help overlay
// must likewise list only the step keys that can act.
func TestSteppedWindowIsNamedInTheFrame(t *testing.T) {
	f := &fakeData{}
	m := newPinnedModel(t, f, windowClock)

	live := ansiWindow.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(live, "7d") {
		t.Fatalf("live frame does not name the range:\n%s", live)
	}
	if strings.Contains(live, "@ 2026-") {
		t.Fatalf("live frame carries a stepped window label:\n%s", live)
	}

	tm, cmd := m.Update(keyMsg("["))
	m = runPending(t, tm.(Model), cmd)

	want := Span{R: Range7d, Step: -1}.Label(windowClock)
	frame := ansiWindow.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(frame, want) {
		t.Fatalf("stepped frame does not name the window %q:\n%s", want, frame)
	}

	m.showHelp = true
	m.layout()
	help := ansiWindow.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(help, "prev window") || !strings.Contains(help, "next window") {
		t.Fatalf("help does not describe both step keys while stepped:\n%s", help)
	}

	// The longer label must not blow the frame out at the usable floor. The floor
	// is the app frame's INTERIOR, so the smallest terminal that still renders the
	// dashboard is the floor plus the frame's own two cells per axis.
	m.showHelp = false
	m = send(m, tea.WindowSizeMsg{
		Width:  views.MinUsableW + 2*views.AppFrame,
		Height: views.MinUsableH + 2*views.AppFrame,
	})
	small := ansiWindow.ReplaceAllString(m.View().Content, "")
	floorH := views.MinUsableH + 2*views.AppFrame
	if lines := strings.Count(small, "\n") + 1; lines > floorH {
		t.Fatalf("stepped frame at the usable floor is %d rows, want <= %d:\n%s",
			lines, floorH, small)
	}
	if strings.Contains(small, "too small") {
		t.Fatalf("the usable floor now renders the resize card, so this pins nothing:\n%s", small)
	}
}

// TestCycleRangeReturnsToTheLiveWindow: a step offset means nothing once the
// span changes width, so cycling the range must land back on the present.
func TestCycleRangeReturnsToTheLiveWindow(t *testing.T) {
	f := &fakeData{}
	m := newPinnedModel(t, f, windowClock)

	tm, cmd := m.Update(keyMsg("["))
	m = runPending(t, tm.(Model), cmd)
	if m.step != -1 {
		t.Fatalf("step = %d, want -1", m.step)
	}

	tm, cmd = m.Update(keyMsg("t"))
	m = runPending(t, tm.(Model), cmd)
	if m.step != 0 {
		t.Fatalf("step = %d after a range cycle, want 0 (live)", m.step)
	}
	if got := m.spanLabel(); got != m.rng.Label() {
		t.Fatalf("label = %q after a range cycle, want the live %q", got, m.rng.Label())
	}
}

// TestSteppedPrevWindowIsTheSpanBefore: the delta chips compare a stepped
// window against the whole span before it — including for the day range, whose
// live comparison is only the same-length tail of yesterday.
func TestSteppedPrevWindowIsTheSpanBefore(t *testing.T) {
	f := &fakeData{}
	m := newPinnedModel(t, f, windowClock)
	m.rng = RangeToday
	m.step = -1

	since, until, ok := m.prevWindow()
	if !ok {
		t.Fatal("stepped day window reported no previous period")
	}
	wantSince, wantUntil := Span{R: RangeToday, Step: -2}.Window(windowClock)
	if !since.Equal(wantSince) || !until.Equal(wantUntil) {
		t.Fatalf("prev window = [%v, %v), want the full previous day [%v, %v)",
			since, until, wantSince, wantUntil)
	}
}

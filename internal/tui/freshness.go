package tui

import (
	"strconv"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// freshness.go is the single authority for the dashboard's data-freshness
// state (issue #7): one enum replaces the old loaded/loading boolean pair, a
// header chip is its only motion, and the ingest-side complement — the
// heartbeat cell and the dead-collector banner — lives here too.
//
// The J-cut: startLoad flips the enum to FreshCutIn synchronously, so the very
// next frame carries the "◐ sync" chip with the old picture still behind it;
// the data then lands in a single frame on dataLoadedMsg. The L-cut: a failed
// load holds the last good picture and routes the failure to the chip
// (FreshStale) — the full-body error panel renders only while FreshCold, when
// there is no picture to hold.
//
// DECIDED (issue #7, measure-first): chip-only — there is deliberately NO
// picture-dim rung. Warm reloads are cache reads; BenchmarkReload measures
// ~6µs/op, four orders of magnitude under the 120ms p95 bar, so a dim would
// flash for 1-2 frames per 10s refresh tick — a rendering-glitch strobe, not
// information. SGR-2 faint is also unreliable (tmux renders it as normal) and
// does not survive ntcharts' braille cells. Revisit only if reload p95 ever
// exceeds ~300ms; then the dim needs a ~150ms hysteresis latch so it is a
// state, not a flash.

// Freshness is the dashboard's data-freshness state.
type Freshness int

const (
	// FreshCold: no dataset has ever applied — the branded loading screen (or,
	// after a failed first load, the full-body error panel) owns the frame.
	FreshCold Freshness = iota
	// FreshLive: the applied dataset matches the last observed db state.
	FreshLive
	// FreshCutIn: a load is in flight; the last picture is held behind the
	// "◐ sync" chip.
	FreshCutIn
	// FreshStale: the last load failed; the last good picture is held and the
	// failure lives in the chip, aged from lastLoadAt.
	FreshStale
)

// String makes test failures readable.
func (f Freshness) String() string {
	switch f {
	case FreshLive:
		return "live"
	case FreshCutIn:
		return "cutIn"
	case FreshStale:
		return "stale"
	default:
		return "cold"
	}
}

// freshnessChip renders the header chip ladder: "● live / ◐ sync / ◔ stale Nm
// / ○ cold". Glyph+word is the load-bearing channel — the ladder survives
// monochrome unchanged; color is secondary. The stale rung carries the age of
// the last good dataset (from lastLoadAt, re-read every frame; the 10s refresh
// tick guarantees a frame, so no extra timer exists for it) and a truncated
// error so the failure is inspectable where it is signalled.
func (m Model) freshnessChip() string {
	switch m.fresh {
	case FreshLive:
		return lipgloss.NewStyle().Foreground(m.th.Positive).Render("● live")
	case FreshCutIn:
		return lipgloss.NewStyle().Foreground(m.th.Now).Render("◐ sync")
	case FreshStale:
		chip := "◔ stale"
		if !m.lastLoadAt.IsZero() {
			chip += " " + ageShort(m.data.now().Sub(m.lastLoadAt))
		}
		out := lipgloss.NewStyle().Foreground(m.th.Warn).Render(chip)
		if m.err != nil {
			out += " " + m.th.Subtle.Render(Truncate(m.err.Error(), 18))
		}
		return out
	default:
		return m.th.Subtle.Render("○ cold")
	}
}

// ageShort renders a duration as a compact single-unit age: 42s, 4m, 2h, 3d.
func ageShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// heartbeatFrames is the pulse amplitude cycle. The frame index is the beat
// counter, which advances ONLY when a db-mtime advance is observed — the
// glyph therefore moves in lockstep with the daemon's real ingest cycle and a
// frozen glyph reads as a stall. Never a decorative timer.
var heartbeatFrames = []string{"⣀", "⣤", "⣶", "⣿"}

// observeIngest records an observed daemon write: a db mtime later than any
// seen before advances the heartbeat one frame. Called from every refresh-tick
// stat and every load flight's stat, so the beat tracks real writes regardless
// of which path saw them first.
func (m *Model) observeIngest(mt time.Time) {
	if !mt.IsZero() && mt.After(m.ingestMTime) {
		m.ingestMTime = mt
		m.beat++
	}
}

// ingestLag is the time since the last observed daemon write (0 before any).
func (m Model) ingestLag() time.Duration {
	if m.ingestMTime.IsZero() {
		return 0
	}
	return m.data.now().Sub(m.ingestMTime)
}

// collectorStalled reports whether the observed ingest lag exceeds ~3x the
// collection interval — the escalation threshold for the banner. Zero
// observations (db never seen) is cold, not stalled.
func (m Model) collectorStalled() bool {
	if m.ingestMTime.IsZero() {
		return false
	}
	return m.ingestLag() > 3*m.collectEvery
}

// heartbeatCell renders the one-cell ingest pulse for the header. Under
// reducedMotion the glyph is static and the observed ingest age is spelled out
// beside it instead.
func (m Model) heartbeatCell() string {
	if m.ingestMTime.IsZero() {
		return m.th.Subtle.Render(heartbeatFrames[0]) // flatline: no ingest observed yet
	}
	style := lipgloss.NewStyle().Foreground(m.th.Positive)
	if m.collectorStalled() {
		style = lipgloss.NewStyle().Foreground(m.th.Warn)
	}
	if m.reducedMotion {
		return style.Render("⣿") + " " + m.th.Subtle.Render(ageShort(m.ingestLag()))
	}
	return style.Render(heartbeatFrames[int(m.beat)%len(heartbeatFrames)])
}

// bannerRows is the body-row reserve for the dead-collector banner (0 or 1).
func (m Model) bannerRows() int {
	if !m.collectorStalled() || m.lay.BodyH < 2 {
		return 0
	}
	return 1
}

// renderStallBanner renders the one-line dead-collector banner shown in the
// body header area once ingest lag crosses the escalation threshold.
func (m Model) renderStallBanner() string {
	txt := "⚠ no ingest for " + ageShort(m.ingestLag()) +
		" — collector stalled or idle (writes expected ~every " + ageShort(m.collectEvery) + ")"
	w := m.width - 2
	if w < 3 {
		w = 3
	}
	bar := lipgloss.NewStyle().Foreground(m.th.Warn).Bold(true).Padding(0, 1).Render(Truncate(txt, w))
	return lipgloss.NewStyle().MaxWidth(m.width).Render(bar)
}

// renderErrorPanel is the full-body error card, rendered ONLY while cold — with
// no prior good frame there is nothing to hold, so the failure owns the body.
// Warm failures never reach it: handleDataLoaded holds the last picture and
// routes the error to the freshness chip (the L-cut).
func (m Model) renderErrorPanel(lay views.Layout) string {
	w := lay.BodyW
	if w < 8 {
		w = 8
	}
	card := m.th.Errored().Render(lipgloss.JoinVertical(lipgloss.Center,
		lipgloss.NewStyle().Foreground(m.th.Warn).Render("✕ query failed"),
		m.th.Subtle.Render(Truncate(m.err.Error(), w-6)),
		"",
		m.th.Subtle.Render("press r to retry"),
	))
	return lipgloss.Place(w, lay.BodyH, lipgloss.Center, lipgloss.Center, card)
}

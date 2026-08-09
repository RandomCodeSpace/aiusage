package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

var ansiPivot = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestPivotKeyTogglesHero: `p` flips the Overview hero to the leverage pivot and
// back. It is presentation only — the toggle must never dispatch a load, so the
// reducer returns no cmd and the applied dataset is untouched.
func TestPivotKeyTogglesHero(t *testing.T) {
	f := &fakeData{}
	m := newTestModel(t, f)
	if m.heroMode() != views.HeroTrend {
		t.Fatal("hero does not start on the trend")
	}

	before := f.queries()
	tm, cmd := m.Update(keyMsg("p"))
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("pivot toggle dispatched a command; it is a pure re-render")
	}
	if f.queries() != before {
		t.Fatalf("pivot toggle queried the store %d times", f.queries()-before)
	}
	if !m.heroPivot {
		t.Fatal("p did not engage the pivot")
	}

	// "range NNx" only comes from the pivot's magnitude footer, so it proves the
	// body pivoted, not just the panel title.
	out := ansiPivot.ReplaceAllString(m.View().Content, "")
	for _, want := range []string{"leverage", "SCALE ", "x/div", "range "} {
		if !strings.Contains(out, want) {
			t.Fatalf("pivot frame is missing %q:\n%s", want, out)
		}
	}

	m = send(m, keyMsg("p"))
	if m.heroPivot {
		t.Fatal("p did not toggle back to the trend")
	}

	// Only Overview owns a hero: the key must be inert elsewhere.
	m = step(t, m, keyMsg("4")) // Sessions/Browse
	m = send(m, keyMsg("p"))
	if m.heroPivot {
		t.Fatal("p flipped the hero from a view that has none")
	}
}

// TestPivotFloorReachesTheViews: the configured leverage floor is presentation
// policy the views resolve, so it has to arrive in the injected Ctx. Nothing
// else in the model reads Options.LeverageFloor — a dropped assignment would
// leave the dashboard silently on the derived default with no other symptom.
func TestPivotFloorReachesTheViews(t *testing.T) {
	m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db", LeverageFloor: 750_000})
	if got := m.vctx.LeverageFloor; got != 750_000 {
		t.Errorf("views.Ctx.LeverageFloor = %d, want 750000", got)
	}
	// Unset must stay zero: that is the sentinel the view reads as "derive the
	// floor from the bucket span", not a floor of zero tokens.
	if got := NewModel(&fakeData{}, Options{}).vctx.LeverageFloor; got != 0 {
		t.Errorf("unset Options gave Ctx.LeverageFloor = %d, want 0", got)
	}
}

// TestPivotKeyInHelp: an undiscoverable toggle is a hidden feature. The binding
// has to appear in the expanded help overlay.
func TestPivotKeyInHelp(t *testing.T) {
	m := newTestModel(t, &fakeData{})
	m = send(m, keyMsg("?"))
	out := ansiPivot.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(out, "pivot") {
		t.Fatalf("help overlay does not list the pivot binding:\n%s", out)
	}
}

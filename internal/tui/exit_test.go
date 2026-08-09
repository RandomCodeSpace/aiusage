package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// exit_test.go verifies the residue contract from issue #20: whatever ends the
// program — a quit key or a signal — every terminal capability the dashboard
// turned on must be turned back off, in reverse order of enablement, before the
// process lets go of the terminal.

// syncBuf is a concurrency-safe sink for the program's output: Bubble Tea's
// renderer writes from its own goroutine while the test reads.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The escape sequences the two declared capabilities enable and disable. They
// are matched literally rather than through the ansi package so a silent change
// in what we emit shows up here as a failing test, not as a passing tautology.
const (
	altScreenOn  = "\x1b[?1049h"
	altScreenOff = "\x1b[?1049l"
	mouseCellOn  = "\x1b[?1002h" // button-event (cell motion) tracking
	mouseCellOff = "\x1b[?1002l"
	mouseSgrOn   = "\x1b[?1006h" // SGR extended coordinates
	mouseSgrOff  = "\x1b[?1006l"
	mouseAnyOff  = "\x1b[?1003l" // any-motion tracking, reset alongside
)

// runToCompletion drives a real Program over pipes and returns everything it
// wrote plus the run error. quit is called once the program is up.
func runToCompletion(t *testing.T, quit func(p *tea.Program, cancel context.CancelFunc)) (string, error) {
	t.Helper()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	out := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db"})
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithInput(pr),
		tea.WithOutput(out),
		tea.WithoutSignals(),
	)

	errc := make(chan error, 1)
	go func() {
		_, err := p.Run()
		errc <- err
	}()

	// Give the renderer a frame to enable its modes before asking it to stop.
	p.Send(tea.WindowSizeMsg{Width: 120, Height: 40})
	time.Sleep(50 * time.Millisecond)
	quit(p, cancel)

	select {
	case err := <-errc:
		return out.String(), err
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit within 5s")
		return "", nil
	}
}

// assertNoResidue checks that every capability the dashboard declares is both
// enabled and then disabled, and that the disable comes last — a reset that is
// emitted before the last enable would leave the terminal dirty.
func assertNoResidue(t *testing.T, out, path string) {
	t.Helper()
	for _, on := range []string{altScreenOn, mouseCellOn, mouseSgrOn} {
		if !strings.Contains(out, on) {
			t.Fatalf("%s: capability %q was never enabled — this test proves nothing", path, quoteSeq(on))
		}
	}
	for _, pair := range []struct{ on, off string }{
		{altScreenOn, altScreenOff},
		{mouseCellOn, mouseCellOff},
		{mouseSgrOn, mouseSgrOff},
	} {
		off := strings.LastIndex(out, pair.off)
		if off < 0 {
			t.Errorf("%s: %q left enabled — residue in the user's terminal", path, quoteSeq(pair.on))
			continue
		}
		if on := strings.LastIndex(out, pair.on); off < on {
			t.Errorf("%s: %q was re-enabled after its reset", path, quoteSeq(pair.on))
		}
	}
	// Cell-motion reporting is torn down together with any-motion tracking, so a
	// half-enabled mouse state cannot survive either.
	if !strings.Contains(out, mouseAnyOff) {
		t.Errorf("%s: mouse mode %q not reset", path, quoteSeq(mouseAnyOff))
	}
}

// quoteSeq renders an escape sequence readably in failure output.
func quoteSeq(s string) string { return strings.ReplaceAll(s, "\x1b", "ESC") }

// TestExitLeavesNoResidueOnQuit: the ordinary quit path restores the terminal.
func TestExitLeavesNoResidueOnQuit(t *testing.T) {
	out, err := runToCompletion(t, func(p *tea.Program, _ context.CancelFunc) {
		p.Send(tea.KeyPressMsg{Code: 'q'})
	})
	if err != nil {
		t.Fatalf("quit returned %v", err)
	}
	assertNoResidue(t, out, "quit")
}

// TestExitLeavesNoResidueOnSignal: a cancelled context is what a signal turns
// into (signalContext), and it must tear the terminal down just as completely.
func TestExitLeavesNoResidueOnSignal(t *testing.T) {
	out, err := runToCompletion(t, func(_ *tea.Program, cancel context.CancelFunc) {
		cancel()
	})
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		t.Fatalf("signal teardown returned %v, want a killed-program error", err)
	}
	assertNoResidue(t, out, "signal")
}

// TestSignalContextCancelsOnSIGHUP: SIGHUP is the one signal Bubble Tea does
// not handle itself — unhandled, it kills the process outright and leaves the
// alt screen and mouse reporting on. signalContext is what closes that hole.
func TestSignalContextCancelsOnSIGHUP(t *testing.T) {
	ctx, stop := signalContext(context.Background())
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("raise SIGHUP: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("SIGHUP did not cancel the program context — a hangup would leave residue")
	}
}

// TestSignalContextLeavesSIGINTAndSIGTERMToBubbleTea: re-registering them here
// would cancel the context first, downgrading Bubble Tea's graceful quit (final
// frame flushed, then teardown) into a kill. This pins that we do not.
func TestSignalContextLeavesSIGINTAndSIGTERMToBubbleTea(t *testing.T) {
	ctx, stop := signalContext(context.Background())
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGURG); err != nil {
		t.Fatalf("raise SIGURG: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("an unrelated signal cancelled the program context")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestExitErrMapsSignalTeardownToCleanExit: being asked to stop is not a
// failure. Reporting one would make a hangup look like a crash to whatever
// supervises the process.
func TestExitErrMapsSignalTeardownToCleanExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exitErr(ctx, tea.ErrProgramKilled); err != nil {
		t.Errorf("signal teardown reported %v, want a clean exit", err)
	}

	live := context.Background()
	want := errors.New("boom")
	if got := exitErr(live, want); !errors.Is(got, want) {
		t.Errorf("a real failure was swallowed: got %v, want %v", got, want)
	}
	if got := exitErr(live, tea.ErrProgramKilled); !errors.Is(got, tea.ErrProgramKilled) {
		t.Error("a kill with no signal behind it was reported as a clean exit")
	}
}

// TestViewDeclaresTornDownCapabilities pins the set the teardown above covers:
// a capability added to the View without being reflected here would be enabled
// on entry and never checked on exit.
func TestViewDeclaresTornDownCapabilities(t *testing.T) {
	v := newTestModelWH(t, &fakeData{}, 120, 40).View()
	if !v.AltScreen {
		t.Error("View no longer declares the alt screen")
	}
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("View declares mouse mode %v, want cell motion", v.MouseMode)
	}
	if v.ReportFocus || v.WindowTitle != "" || v.Cursor != nil {
		t.Error("View declares a capability the exit test does not cover")
	}
}

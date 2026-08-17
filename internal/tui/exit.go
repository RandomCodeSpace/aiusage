package tui

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

// exit.go is the TUI's exit hygiene (issue #20): whatever kills the process,
// the terminal must come back exactly as it was handed over — no alt screen, no
// mouse reporting, no stray escape sequences in the user's scrollback.
//
// Who tears down what:
//
//   - The capabilities are DECLARED on the tea.View (render.go): alt screen and
//     cell-motion mouse reporting. Bubble Tea's renderer close path disables
//     exactly the set the last view declared, in reverse order of enablement —
//     alt screen, cursor, bracketed paste, focus reporting, mouse modes, window
//     title — so a capability that is never declared is never left enabled.
//   - That close path runs from Program.shutdown, which every exit goes
//     through: a normal quit, a killed run, and the recover path after a panic.
//   - Bubble Tea installs its own handler for SIGINT (InterruptMsg) and SIGTERM
//     (QuitMsg), both of which reach shutdown gracefully. SIGHUP it does not
//     handle at all: the default disposition kills the process outright, and the
//     terminal is left in the alt screen with mouse reporting on. That is the
//     one hole, and signalContext closes it.
//
// SIGINT/SIGTERM are deliberately NOT registered here. Re-registering them
// would cancel the program's context first, which downgrades a graceful quit
// (final frame flushed, then teardown) into a kill (teardown only).

// signalContext returns a context cancelled by the signals Bubble Tea does not
// handle itself, plus the stop func that unregisters them.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGHUP)
}

// Run launches the TUI over the given store. It blocks until the user quits.
// Alt-screen and cell-motion mouse mode (nav rail, tabs, rows, bars and KPI
// tiles clickable; wheel scrolls/scrubs) are declared on the tea.View in View();
// the program runs under a signal-aware context so a SIGHUP tears them back
// down instead of dumping escape sequences into the user's shell.
func Run(st store.Store, opt Options) error {
	ctx, stop := signalContext(context.Background())
	defer stop()

	p := tea.NewProgram(NewModel(st, opt), tea.WithContext(ctx))
	_, err := p.Run()
	return exitErr(ctx, err)
}

// exitErr maps a signal-driven teardown to a clean exit. Being asked to stop is
// not a failure, and reporting "program was killed" would make a SIGHUP look
// like a crash in whatever supervises us.
func exitErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil && errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

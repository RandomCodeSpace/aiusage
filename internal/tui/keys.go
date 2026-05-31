package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap holds every binding the root model recognises. It implements the
// bubbles/help.KeyMap interface so the help bar/overlay renders from one source
// of truth.
type KeyMap struct {
	Up       key.Binding
	Down     key.Binding
	Left     key.Binding
	Right    key.Binding
	Top      key.Binding
	Bottom   key.Binding
	NextPane key.Binding // Tab — next tab
	PrevPane key.Binding // Shift+Tab — previous tab
	View1    key.Binding
	View2    key.Binding
	View3    key.Binding
	View4    key.Binding
	Filter   key.Binding
	Sort     key.Binding
	Range    key.Binding
	StepBack key.Binding // [ — step time window earlier
	StepFwd  key.Binding // ] — step time window later
	Enter    key.Binding
	Back     key.Binding
	Refresh  key.Binding
	Help     key.Binding
	Quit     key.Binding
}

// DefaultKeyMap returns the standard bindings described in the spec.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "move ↑"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "move ↓"),
		),
		Left: key.NewBinding(
			key.WithKeys("left", "h"),
			key.WithHelp("←/h", "scrub ←"),
		),
		Right: key.NewBinding(
			key.WithKeys("right", "l"),
			key.WithHelp("→/l", "scrub →"),
		),
		Top: key.NewBinding(
			key.WithKeys("g", "home"),
			key.WithHelp("g", "start"),
		),
		Bottom: key.NewBinding(
			key.WithKeys("G", "end"),
			key.WithHelp("G", "end/live"),
		),
		NextPane: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("⇥", "next tab"),
		),
		PrevPane: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("⇧⇥", "prev tab"),
		),
		View1: key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "overview")),
		View2: key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "by-tool")),
		View3: key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "by-model")),
		View4: key.NewBinding(key.WithKeys("4"), key.WithHelp("4", "sessions")),
		Filter: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "filter"),
		),
		Sort: key.NewBinding(
			key.WithKeys("s"),
			key.WithHelp("s", "sort"),
		),
		Range: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "range"),
		),
		StepBack: key.NewBinding(
			key.WithKeys("["),
			key.WithHelp("[", "window ←"),
		),
		StepFwd: key.NewBinding(
			key.WithKeys("]"),
			key.WithHelp("]", "window →"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("⏎", "drill"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc", "backspace"),
			key.WithHelp("esc", "back"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "reload"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
	}
}

// ShortHelp implements help.KeyMap: the compact one-line footer.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.NextPane, k.Left, k.Enter, k.Range, k.Sort, k.Filter, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap: the expanded multi-column overlay.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.View1, k.View2, k.View3, k.View4},
		{k.NextPane, k.PrevPane, k.Up, k.Down, k.Left, k.Right},
		{k.Enter, k.Back, k.Top, k.Bottom},
		{k.Range, k.StepBack, k.StepFwd, k.Sort, k.Filter},
		{k.Refresh, k.Help, k.Quit},
	}
}

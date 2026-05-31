package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// select.go centralises selection + scrub-sync helpers shared by the keyboard
// and mouse handlers so the two input paths mutate identical state.

// selectionRows returns the rows for the current bar-based view (By-Tool /
// By-Model), or nil.
func (m Model) selectionRows() []store.Bucket {
	switch m.view {
	case ViewByTool:
		return m.byTool.Rows
	case ViewByModel:
		return m.byModel.Rows
	default:
		return nil
	}
}

// selectionCount returns the number of selectable bars in the current view.
func (m Model) selectionCount() int { return len(m.selectionRows()) }

// currentSelection returns the active bar index for the current view.
func (m Model) currentSelection() int {
	switch m.view {
	case ViewByTool:
		return m.byTool.Selected
	case ViewByModel:
		return m.byModel.Selected
	default:
		return 0
	}
}

// setSelection clamps and applies a bar selection, reloading the selected
// entity's detail trend.
func (m *Model) setSelection(i int) {
	n := m.selectionCount()
	if n == 0 {
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	switch m.view {
	case ViewByTool:
		m.byTool.Selected = i
		m.loadByToolDetail()
	case ViewByModel:
		m.byModel.Selected = i
		m.loadByModelDetail()
	}
}

// moveSelection steps the bar selection by dir.
func (m *Model) moveSelection(dir int) { m.setSelection(m.currentSelection() + dir) }

// moveSelectionFromKey maps up/down/j/k to selection steps on bar views.
func (m *Model) moveSelectionFromKey(msg tea.Msg) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return
	}
	switch km.String() {
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(+1)
	}
}

// selectBar selects the bar matching a clicked name on By-Tool / By-Model. The
// Overview tool bars are read-only (no selection), so a name that isn't found is
// simply a no-op.
func (m *Model) selectBar(name string) {
	rows := m.selectionRows()
	for i, b := range rows {
		dim := "tool"
		if m.view == ViewByModel {
			dim = "model"
		}
		if b.Keys[dim] == name {
			m.setSelection(i)
			return
		}
	}
}

// selectedByToolBucket returns the selected tool bucket.
func (m Model) selectedByToolBucket() (store.Bucket, bool) {
	if m.byTool.Selected < 0 || m.byTool.Selected >= len(m.byTool.Rows) {
		return store.Bucket{}, false
	}
	return m.byTool.Rows[m.byTool.Selected], true
}

// selectedByModelBucket returns the selected model bucket.
func (m Model) selectedByModelBucket() (store.Bucket, bool) {
	if m.byModel.Selected < 0 || m.byModel.Selected >= len(m.byModel.Rows) {
		return store.Bucket{}, false
	}
	return m.byModel.Rows[m.byModel.Selected], true
}

// browseRowCount returns the number of visible Browse rows.
func (m Model) browseRowCount() int { return m.browse.RowCount() }

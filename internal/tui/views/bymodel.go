package views

import (
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// ByModelData feeds the By-Model view: per-model stacked fresh/cache bars
// colored by each model's dominant owning tool, plus a detail card for the
// selected model. ModelTool maps a model id to its dominant tool.
type ByModelData struct {
	Rows        []store.Bucket    // grouped by model, sorted
	Grand       int64             // grand total for share %
	Selected    int               // selected/focused bar index
	SelTrend    []store.Bucket    // selected model's daily trend (ascending)
	SelTrendErr bool              // the trend query failed (distinct from "no rows")
	ModelTool   map[string]string // model id -> dominant owning tool
	RangeLbl    string
	ActivePane  int
}

// ByModel renders the by-model dashboard. Bars cluster by owning-tool color so
// model families read at a glance.
func ByModel(c Ctx, d ByModelData, lay Layout) string {
	owner := func(b store.Bucket) string {
		if d.ModelTool == nil {
			return b.Keys["model"]
		}
		if t, ok := d.ModelTool[b.Keys["model"]]; ok && t != "" {
			return t
		}
		return b.Keys["model"]
	}
	return byEntity(c, byEntityData{
		title:      "BY MODEL · " + d.RangeLbl,
		dim:        "model",
		rows:       d.Rows,
		grand:      d.Grand,
		selected:   d.Selected,
		selTrend:   d.SelTrend,
		selErr:     d.SelTrendErr,
		activePane: d.ActivePane,
		ownerTool:  owner,
		// No fold here: the long tail is folded by SHARE OF TOOL, and a model
		// list is not a tool list — the same model reached through two harnesses
		// is two rows, and collapsing the small ones would hide exactly the
		// comparison this tab exists for. A zero foldCount is what says so.
		reasoning: true,
	}, lay)
}

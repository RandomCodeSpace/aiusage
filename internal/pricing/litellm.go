package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// snapshotJSON is the filtered LiteLLM price snapshot that ships in the binary.
// It is the guaranteed floor of the ladder: an install with no network and no
// config still prices the models this project ingests. Regenerate it from
// upstream (see data/litellm_snapshot.json's _meta block) rather than editing
// rates by hand.
//
//go:embed data/litellm_snapshot.json
var snapshotJSON []byte

// litellmEntry mirrors the price fields of LiteLLM's
// model_prices_and_context_window.json. The embedded snapshot keeps upstream's
// field names verbatim so one decoder serves both the snapshot and the file
// fetched at runtime.
type litellmEntry struct {
	Input        float64 `json:"input_cost_per_token"`
	Output       float64 `json:"output_cost_per_token"`
	CacheRead    float64 `json:"cache_read_input_token_cost"`
	CacheWrite   float64 `json:"cache_creation_input_token_cost"`
	CacheWrite1h float64 `json:"cache_creation_input_token_cost_above_1hr"`
	InputBatch   float64 `json:"input_cost_per_token_batches"`
	OutputBatch  float64 `json:"output_cost_per_token_batches"`
}

func (e litellmEntry) rates() Rates {
	return Rates{
		Input:        e.Input,
		Output:       e.Output,
		CacheRead:    e.CacheRead,
		CacheWrite5m: e.CacheWrite,
		CacheWrite1h: e.CacheWrite1h,
		InputBatch:   e.InputBatch,
		OutputBatch:  e.OutputBatch,
	}
}

// snapshotDoc is the envelope used by both the embedded snapshot and the
// runtime cache: provenance plus the model map. Upstream's own file is a bare
// model map, which decodeModels reads directly.
type snapshotDoc struct {
	Meta struct {
		Source  string `json:"source"`
		Fetched string `json:"fetched"`
	} `json:"_meta"`
	Models map[string]json.RawMessage `json:"models"`
}

// decodeModels turns a raw model map into rates. Entries that do not decode, or
// that publish no usable price, are skipped instead of failing the table: the
// refresh path consumes a file this project does not control, and one odd
// record must not cost the user every price.
func decodeModels(raw map[string]json.RawMessage) map[string]Rates {
	out := make(map[string]Rates, len(raw))
	for name, msg := range raw {
		var e litellmEntry
		if err := json.Unmarshal(msg, &e); err != nil {
			continue
		}
		r := e.rates()
		if !r.Priceable() {
			continue
		}
		out[name] = r
	}
	return out
}

// decodeUpstream parses LiteLLM's own bare model map, as served by the refresh
// URL.
func decodeUpstream(data []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing: parse upstream price table: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("pricing: upstream price table is empty")
	}
	return raw, nil
}

// decodeSnapshot parses the envelope form (embedded snapshot or runtime cache)
// into a table stamped with the given price_source prefix.
func decodeSnapshot(data []byte, prefix string) (*Table, error) {
	var doc snapshotDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("pricing: parse price snapshot: %w", err)
	}
	models := decodeModels(doc.Models)
	if len(models) == 0 {
		return nil, fmt.Errorf("pricing: price snapshot has no priceable models")
	}
	date := doc.Meta.Fetched
	if date == "" {
		date = "unknown"
	}
	return &Table{Source: prefix + "-" + date, Models: models}, nil
}

// embeddedTable parses the embedded snapshot once. A parse failure can only
// mean a corrupted embed, in which case the rung is simply absent and the
// ladder falls through — pricing must never stop the program from running.
var embeddedTable = sync.OnceValue(func() *Table {
	t, err := decodeSnapshot(snapshotJSON, "embedded")
	if err != nil {
		return nil
	}
	return t
})

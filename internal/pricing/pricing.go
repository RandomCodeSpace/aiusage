// Package pricing values observed token usage in micro-USD (millionths of a
// dollar) using an offline-first ladder of price tables.
//
// The ladder, most authoritative first:
//
//  1. config overrides — the user's own rates, always win. A PARTIAL override
//     (say output only) replaces just the rates it names: the rest come from
//     the rung below, because "override this rate" is not "delete the others";
//  2. the runtime-refreshed LiteLLM table cached in the data dir;
//  3. the go:embed'ed filtered LiteLLM snapshot — the guaranteed floor, so a
//     firewalled install still prices;
//  4. nothing. A model no rung can price is UNPRICED, never $0.00.
//
// Integer micro-USD is the unit everywhere: it sums exactly across millions of
// rows, where float dollars drift. The package imports only internal/model.
package pricing

import (
	"math"
	"strings"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// SourceOverride is the price_source stamped when a config override priced the
// event. The other two rungs stamp "litellm-<fetch date>" and
// "embedded-<snapshot date>", built by the table loaders.
const SourceOverride = "override"

// Rates are the per-token USD rates for one model, in LiteLLM's units (dollars
// per single token). A zero field means "not published", not "free"; Cost
// resolves each zero to the documented fallback rate.
type Rates struct {
	Input        float64
	Output       float64
	CacheRead    float64 // 0 -> the input rate: a cache read is a discounted input token
	CacheWrite5m float64 // 0 -> the input rate: writing a 5m entry costs at least the input
	CacheWrite1h float64 // 0 -> the resolved 5m write rate
	InputBatch   float64 // 0 -> the standard input rate (no batch tier published)
	OutputBatch  float64 // 0 -> the standard output rate
}

// Priceable reports whether the rates carry any usable price. An all-zero entry
// is a table row without pricing, which must fall through to the next rung
// rather than value the event at zero.
func (r Rates) Priceable() bool { return r.Input > 0 || r.Output > 0 }

// fill returns r with every unset (zero) rate taken from base. It is how a
// PARTIAL config override composes with the rung it displaces: a user who sets
// only the output rate means "bill output at mine, the rest as published", so
// the fields they left out keep the table's rates instead of valuing those
// tokens at nothing.
func (r Rates) fill(base Rates) Rates {
	if r.Input == 0 {
		r.Input = base.Input
	}
	if r.Output == 0 {
		r.Output = base.Output
	}
	if r.CacheRead == 0 {
		r.CacheRead = base.CacheRead
	}
	if r.CacheWrite5m == 0 {
		r.CacheWrite5m = base.CacheWrite5m
	}
	if r.CacheWrite1h == 0 {
		r.CacheWrite1h = base.CacheWrite1h
	}
	if r.InputBatch == 0 {
		r.InputBatch = base.InputBatch
	}
	if r.OutputBatch == 0 {
		r.OutputBatch = base.OutputBatch
	}
	return r
}

// Charge is the token shape being priced: the counts, plus the three facts that
// change which rate applies (the model/provider identity, the service tier, and
// whether reasoning tokens are billed on top of output or already inside it).
type Charge struct {
	Model       string
	Provider    string
	ServiceTier string

	Input     int64
	Output    int64
	Reasoning int64
	CacheRead int64
	// CacheWrite5m / CacheWrite1h split the cache-creation tokens by the
	// requested entry lifetime. A source that reports no split puts everything
	// in CacheWrite5m, which is the documented fallback (the 1h write is the
	// dearer of the two, so this never over-bills).
	CacheWrite5m int64
	CacheWrite1h int64

	// AdditiveReasoning bills Reasoning at the output rate on top of Output.
	// False means reasoning is already contained in Output (see
	// model.ReasoningModeFor) and billing it again would double charge.
	AdditiveReasoning bool
}

// Tokens is the total token count being charged, used to tell "no usage, so
// genuinely zero" apart from "priced at zero", which is never reported.
func (c Charge) Tokens() int64 {
	return c.Input + c.Output + c.CacheRead + c.CacheWrite5m + c.CacheWrite1h + c.Reasoning
}

// ChargeFor derives the Charge for a stored usage event: token counts straight
// off the event, the reasoning-billing rule from the event's tool, and the
// cache-write split from the transient CacheTTL enrichment. A split that does
// not add up to the recorded cache-creation count is discarded in favour of
// "all 5m" — a partial split would silently drop billable tokens.
func ChargeFor(e model.UsageEvent) Charge {
	w5, w1 := e.CacheTTL.Ephemeral5m, e.CacheTTL.Ephemeral1h
	if w5 < 0 || w1 < 0 || w5+w1 != e.CacheCreationTokens {
		w5, w1 = e.CacheCreationTokens, 0
	}
	return Charge{
		Model:             e.Model,
		Provider:          e.Provider,
		ServiceTier:       e.ServiceTier,
		Input:             e.InputTokens,
		Output:            e.OutputTokens,
		Reasoning:         e.ReasoningTokens,
		CacheRead:         e.CacheReadTokens,
		CacheWrite5m:      w5,
		CacheWrite1h:      w1,
		AdditiveReasoning: model.ReasoningModeFor(e.Tool) == model.ReasoningAdditive,
	}
}

// Cost values a charge at these rates, in micro-USD, rounding half up. The
// result is never negative: token counts and rates are both non-negative.
func (r Rates) Cost(c Charge) int64 {
	in, out := r.Input, r.Output
	if isBatchTier(c.ServiceTier) {
		if r.InputBatch > 0 {
			in = r.InputBatch
		}
		if r.OutputBatch > 0 {
			out = r.OutputBatch
		}
	}
	cacheRead := r.CacheRead
	if cacheRead == 0 {
		cacheRead = in
	}
	write5m := r.CacheWrite5m
	if write5m == 0 {
		write5m = in
	}
	write1h := r.CacheWrite1h
	if write1h == 0 {
		write1h = write5m
	}

	usd := float64(c.Input)*in +
		float64(c.Output)*out +
		float64(c.CacheRead)*cacheRead +
		float64(c.CacheWrite5m)*write5m +
		float64(c.CacheWrite1h)*write1h
	if c.AdditiveReasoning {
		usd += float64(c.Reasoning) * out
	}
	return microUSD(usd)
}

// isBatchTier reports whether a provider service tier is a batch tier, which
// LiteLLM prices in its *_batches fields (roughly half the standard rate).
func isBatchTier(tier string) bool {
	return strings.Contains(strings.ToLower(tier), "batch")
}

// microUSD converts dollars to millionths, rounding half up. Charges are never
// negative, so "half up" and "half away from zero" coincide.
func microUSD(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(math.Floor(usd*1e6 + 0.5))
}

// Table is one rung of the ladder: a model->rates map plus the price_source
// string to stamp when it prices something.
type Table struct {
	Source string
	Models map[string]Rates
}

// Lookup resolves rates for a (provider, model) pair, trying the provider's
// namespaced LiteLLM key before the bare model id so a proxied model is priced
// at the proxy's rates when the table publishes them. A table row with no
// usable price counts as a miss so the next rung gets a chance.
func (t *Table) Lookup(provider, name string) (Rates, bool) {
	if t == nil || len(t.Models) == 0 {
		return Rates{}, false
	}
	for _, key := range lookupKeys(provider, name) {
		if r, ok := t.Models[key]; ok && r.Priceable() {
			return r, true
		}
	}
	return Rates{}, false
}

// lookupKeys lists the table keys to try for a (provider, model) pair, most
// specific first: the provider-namespaced forms, the model id as stored, its
// lowercase form, and finally the id with any leading "vendor/" segment
// stripped (opencode and openrouter-style ids arrive pre-namespaced).
func lookupKeys(provider, name string) []string {
	n := strings.TrimSpace(name)
	if n == "" {
		return nil
	}
	keys := make([]string, 0, 6)
	for _, p := range providerPrefixes(provider) {
		keys = append(keys, p+n)
	}
	keys = append(keys, n)
	if l := strings.ToLower(n); l != n {
		keys = append(keys, l)
	}
	if i := strings.IndexByte(n, '/'); i >= 0 && i+1 < len(n) {
		keys = append(keys, n[i+1:])
	}
	return keys
}

// providerPrefixes maps a billing provider to the LiteLLM key namespaces that
// carry its rates. LiteLLM keys Anthropic and OpenAI models bare as well, which
// the caller covers with the un-prefixed candidate.
func providerPrefixes(provider string) []string {
	switch provider {
	case model.ProviderGoogle:
		return []string{"gemini/", "vertex_ai/"}
	case model.ProviderGitHub:
		return []string{"github_copilot/"}
	case model.ProviderOpenAI:
		return []string{"openai/"}
	case model.ProviderAnthropic:
		return []string{"anthropic/"}
	case "":
		return nil
	default:
		// Data-driven providers (opencode's providerID, hermes'
		// billing_provider) already match LiteLLM's namespace convention for
		// the vendors they name.
		return []string{provider + "/"}
	}
}

// Price resolves a charge against the ladder and returns the cost in micro-USD
// plus the price_source that produced it. ok=false means unpriced: NO rung
// could value this charge shape. A rung that knows the model but publishes no
// rate for the tokens being charged (an output-only row against an input-only
// charge) is a gap in that table, not a free request, and gets the same
// recovery an unpriceable row gets — the next rung down is tried. Only when the
// ladder runs out is the charge reported unpriced, never $0.00.
func (e *Engine) Price(c Charge) (int64, string, bool) {
	if e == nil {
		return 0, "", false
	}
	tables := e.tables()
	for i, t := range tables {
		r, ok := t.Lookup(c.Provider, c.Model)
		if !ok {
			continue
		}
		source := t.Source
		if t == e.overrides {
			r, source = mergeOverride(r, source, tables[i+1:], c)
		}
		micro := r.Cost(c)
		if micro == 0 && c.Tokens() > 0 {
			continue // this rung prices nothing this charge is made of
		}
		return micro, source, true
	}
	return 0, "", false
}

// mergeOverride completes a config override from the first lower rung that
// knows the same model — the rung the override displaced. Replacing that rung's
// whole row with a partial override is what used to leave input, cache and
// batch tokens valued at zero for a model whose output rate was the only one
// the user wanted changed.
//
// The stamp names both tables ("override+litellm-<date>") whenever the lower
// rung actually moved THIS charge's price, so price_source never credits the
// override with a number it did not produce on its own; a charge the override
// prices unaided still stamps a plain "override".
func mergeOverride(r Rates, source string, lower []*Table, c Charge) (Rates, string) {
	for _, t := range lower {
		base, ok := t.Lookup(c.Provider, c.Model)
		if !ok {
			continue
		}
		merged := r.fill(base)
		if merged.Cost(c) == r.Cost(c) {
			return r, source
		}
		return merged, source + "+" + t.Source
	}
	return r, source
}

// PriceEvent prices a usage event. It is the collector-facing entry point and
// satisfies collect.Pricer.
func (e *Engine) PriceEvent(ev model.UsageEvent) (int64, string, bool) {
	return e.Price(ChargeFor(ev))
}

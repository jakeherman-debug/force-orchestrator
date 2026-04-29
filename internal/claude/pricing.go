package claude

import (
	"log"
	"strings"
	"sync"
)

// Per-model pricing for the Claude CLI (D2 T1-1).
//
// The fleet computes per-attempt cost at TaskHistory-write time so the
// per-task spend dog and dashboard sums don't have to re-derive prices on
// every query. The table is keyed by the canonical model id Claude reports
// in the JSON output's `usage`/`message.model` block (e.g.
// "claude-opus-4-5", "claude-sonnet-4-5"). Both shapes are accepted —
// CLI versions vary on prefix / suffix conventions.
//
// Operator-controlled, NOT LLM-controlled: only an operator commit can
// change the table (compile-time const + UpdatePriceTable). The store-
// layer PriceInputPerMillion / PriceOutputPerMillion constants remain as
// the legacy single-rate fallback used by SpendRateDollars; they should
// match a representative working model so trailing-hour aggregates stay
// in the same ballpark across fleets that haven't yet rolled in the per-
// model prices.

// ModelPrice is the per-million-token price pair for one model.
type ModelPrice struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

// defaultPriceTable is the static, compile-time price table. The keys are
// stored normalised (lower-case, "anthropic/" prefix stripped). Lookups
// route through the same normalisation in CostUSD so callers can pass the
// raw model id from the CLI without worrying about case or prefix shape.
//
// Prices reflect the public list as of D2 (Apr 2026). Update via this
// table OR via UpdatePriceTable in an operator commit — never at runtime
// from LLM-supplied content.
var defaultPriceTable = map[string]ModelPrice{
	// Claude 4 family (current generation).
	"claude-opus-4-5":     {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	"claude-opus-4-7":     {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	"claude-sonnet-4-5":   {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-sonnet-4-7":   {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-haiku-4-5":    {InputPerMillion: 1.0, OutputPerMillion: 5.0},
	// Legacy / 3.5 family — kept for fleets still pinned to older models.
	"claude-3-5-sonnet":   {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-3-5-haiku":    {InputPerMillion: 0.8, OutputPerMillion: 4.0},
	"claude-3-opus":       {InputPerMillion: 15.0, OutputPerMillion: 75.0},
}

var (
	priceTableMu sync.RWMutex
	priceTable   = func() map[string]ModelPrice {
		m := make(map[string]ModelPrice, len(defaultPriceTable))
		for k, v := range defaultPriceTable {
			m[k] = v
		}
		return m
	}()
)

// normalizeModel returns the lookup key for a raw model id. Lower-cases,
// strips the "anthropic/" provider prefix that some CLI versions emit, and
// trims a trailing "-<date>" suffix (e.g. "claude-opus-4-5-20250414" →
// "claude-opus-4-5") so a published table key matches.
func normalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "anthropic/")
	// Strip trailing date-stamp suffix: "-YYYYMMDD" or "-YYYY-MM-DD".
	// Only strip the suffix if the remainder still looks like a model id
	// (starts with "claude-"); otherwise leave the string alone so
	// future, non-claude providers don't get mangled.
	if strings.HasPrefix(m, "claude-") {
		if i := strings.LastIndex(m, "-"); i > 0 {
			suffix := m[i+1:]
			if len(suffix) == 8 && allDigits(suffix) {
				m = m[:i]
			}
		}
	}
	return m
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// CostUSD returns the per-attempt cost in dollars for the given model and
// token counts. Unknown models log once-per-process and return 0 cost so
// the caller's TaskHistory write doesn't error out on an unrecognised
// CLI report. Negative token counts (defensive — should never happen) are
// treated as 0.
func CostUSD(model string, tokensIn, tokensOut int) float64 {
	if tokensIn < 0 {
		tokensIn = 0
	}
	if tokensOut < 0 {
		tokensOut = 0
	}
	key := normalizeModel(model)
	priceTableMu.RLock()
	p, ok := priceTable[key]
	priceTableMu.RUnlock()
	if !ok {
		logUnknownModel(model)
		return 0
	}
	return float64(tokensIn)*p.InputPerMillion/1_000_000 +
		float64(tokensOut)*p.OutputPerMillion/1_000_000
}

// UpdatePriceTable replaces the entry for one model. Operator-controlled
// path — exposed for an `operator config` command (or a future
// per-fleet override) so the price table can drift independently of a
// recompile. NOT exposed to LLM-authored config writes.
//
// Writes the normalised key so subsequent CostUSD lookups hit the entry
// regardless of the case/prefix shape the CLI emits.
func UpdatePriceTable(model string, in, out float64) {
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	key := normalizeModel(model)
	priceTableMu.Lock()
	priceTable[key] = ModelPrice{InputPerMillion: in, OutputPerMillion: out}
	priceTableMu.Unlock()
}

// ResetPriceTableForTest restores the compile-time defaults. Used by tests
// after an UpdatePriceTable so subsequent tests start from a known table.
func ResetPriceTableForTest() {
	priceTableMu.Lock()
	priceTable = make(map[string]ModelPrice, len(defaultPriceTable))
	for k, v := range defaultPriceTable {
		priceTable[k] = v
	}
	unknownModelsLoggedMu.Lock()
	unknownModelsLogged = map[string]bool{}
	unknownModelsLoggedMu.Unlock()
	priceTableMu.Unlock()
}

// unknownModelsLogged tracks unknown-model warnings already emitted so a
// chatty CLI doesn't spam the operator log. The map persists for the
// process lifetime — restart clears it.
var (
	unknownModelsLoggedMu sync.Mutex
	unknownModelsLogged   = map[string]bool{}
)

func logUnknownModel(model string) {
	key := normalizeModel(model)
	unknownModelsLoggedMu.Lock()
	defer unknownModelsLoggedMu.Unlock()
	if unknownModelsLogged[key] {
		return
	}
	unknownModelsLogged[key] = true
	log.Printf("claude.pricing: unknown model %q — cost will be reported as $0 until UpdatePriceTable is called or the model is added to defaultPriceTable", model)
}

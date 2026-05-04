// Pattern ARCH-002 — unused-exports.
//
// Detects exported symbols (Go: capitalised top-level identifiers in
// non-_test files) that have zero cross-repo consumers per the D8
// graph.
//
// D8 has not yet merged. v1 ships an interface-shaped STUB that
// returns zero hits (the cross-repo lookup degrades to "assume
// in-use"). When D8 lands, the lookupCrossRepoConsumers seam below is
// swapped for the real graph query. Tests that depend on the
// cross-repo lookup are gated behind:
//
//	if os.Getenv("D8_GRAPH_AVAILABLE") == "" {
//	    t.Skip("D8-MERGE-GATE: ARCH-002 cross-repo wiring lights up after D8 lands in main")
//	}
//
// Anti-cheat #4 (no dynamic discovery) is upheld — the STUB is
// statically registered in the same registry as the rest.

package patterns

import (
	"force-orchestrator/internal/archaeologist"
)

type arch002 struct{}

// NewARCH002 returns the ARCH-002 pattern.
func NewARCH002() archaeologist.Pattern { return arch002{} }

func (arch002) ID() string             { return "ARCH-002" }
func (arch002) MinHitsForFeature() int { return 10 }

// Scan is the D8-merge-gated stub. Returns no hits because the
// cross-repo graph is not yet available. The real implementation will:
//
//   1. Walk the repo's .go files (language-aware).
//   2. Extract every exported top-level identifier (Func, Type, Var,
//      Const, Method on exported type).
//   3. Call lookupCrossRepoConsumers(repo.Name, symbolFQN) against
//      the D8 SymbolGraph.
//   4. Emit a Hit when consumer-count == 0 AND the symbol is older
//      than 90d (so we don't flag freshly-landed unused code).
//
// TODO(D8-MERGE): replace the stub return with the walk + graph
// query above once internal/clients/d8graph (or whatever the D8 graph
// client ends up named) is on main.
func (p arch002) Scan(repo *archaeologist.Repo) []archaeologist.Hit {
	if repo == nil || repo.LocalPath == "" {
		return nil
	}
	// D8-MERGE-GATE: stub returns nothing. Once D8 lands the lookup
	// below replaces this early return.
	return nil
}

// lookupCrossRepoConsumers is the seam the D8 wiring will replace.
// Returning -1 (sentinel for "graph unavailable") so callers can
// distinguish "no consumers" from "graph not wired".
//
//nolint:unused // referenced by the post-D8 wiring; kept here so the
// signature is stable across the D8 merge.
func lookupCrossRepoConsumers(repoName, symbolFQN string) int {
	_ = repoName
	_ = symbolFQN
	return -1
}

func init() { Register(NewARCH002()) }

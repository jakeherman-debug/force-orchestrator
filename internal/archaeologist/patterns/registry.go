// Package patterns — D9 Track A — Archaeologist pattern registry.
//
// Each pattern is a separate file in this package implementing the
// archaeologist.Pattern interface. Registration is static (init()
// time) — anti-cheat #4 forbids dynamic pattern discovery in v1.
//
// To add a new pattern:
//   1. Drop a new file ARCH_NNN_<slug>.go in this package.
//   2. Implement archaeologist.Pattern (ID, Scan, MinHitsForFeature).
//   3. Register it from a init() func via Register(NewARCHNNN()).
//
// All() is the canonical iteration order; patterns are returned in
// registration order so test output is stable.
package patterns

import (
	"sort"

	"force-orchestrator/internal/archaeologist"
)

// registry holds every registered Pattern keyed by ID. init() funcs
// in sibling files populate it via Register().
var registry = map[string]archaeologist.Pattern{}

// Register adds a Pattern to the static registry. Panics on duplicate
// ID — duplicate registration is a programming error, not a runtime
// condition. Called only from init() funcs in sibling files; the
// archaeologist agent itself never calls Register.
func Register(p archaeologist.Pattern) {
	if _, dup := registry[p.ID()]; dup {
		panic("archaeologist/patterns: duplicate Pattern ID " + p.ID())
	}
	registry[p.ID()] = p
}

// All returns every registered Pattern in deterministic ID order.
// The Archaeologist's sweep loop calls All() once per
// ArchaeologistSweep task and Scans each in sequence.
func All() []archaeologist.Pattern {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]archaeologist.Pattern, 0, len(ids))
	for _, id := range ids {
		out = append(out, registry[id])
	}
	return out
}

// ByID returns the registered Pattern with the given ID, or nil if no
// such pattern is registered. Used by the migration-proposal handler
// to look up MinHitsForFeature for the threshold check.
func ByID(id string) archaeologist.Pattern {
	return registry[id]
}

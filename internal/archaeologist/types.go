// Package archaeologist — Deliverable 9, Track A.
//
// The Archaeologist is the proactive debt-detection agent. It runs as a
// claim-loop agent (see internal/agents/archaeologist.go) and consumes
// two task types:
//
//   - ArchaeologistSweep: walks a repo's working tree against the
//     pattern registry; each Pattern emits zero or more Hits which are
//     persisted into ArchaeologistFindings (status='open').
//   - ArchaeologistProposeMigration: fires when a pattern's open-hit
//     count exceeds Pattern.MinHitsForFeature(). Calls
//     librarian.Client.EmitCandidate (the operator-ratifiable handoff
//     to D3 Engineering Corps) — Archaeologist proposes; operator
//     ratifies. This is anti-cheat #1: NO auto-dispatching of
//     migrations.
//
// Pattern registration is static — see patterns/registry.go. Dynamic
// pattern discovery is disabled in v1 (anti-cheat #4: registry is
// authoritative).
package archaeologist

// Repo is the per-sweep target shape passed to Pattern.Scan. Field
// names are intentionally short: Pattern implementations consume them
// as-is in tight loops over file walks.
type Repo struct {
	// ID is the SQLite rowid of the Repositories row (used as
	// ArchaeologistFindings.repo_id).
	ID int

	// Name is the canonical Repositories.name value — useful for
	// log messages and detail_json fields. Patterns SHOULD use ID
	// for foreign-key writes.
	Name string

	// LocalPath is the absolute path to the repo's working tree on
	// disk. Patterns MUST treat this as read-only — no writes.
	LocalPath string
}

// Hit is one match emitted by Pattern.Scan. The Archaeologist's loop
// converts a Hit into an ArchaeologistFindings row (UNIQUE on
// pattern_id, repo_id, file_path, line_number — re-emitting the same
// hit silently no-ops via INSERT OR IGNORE).
type Hit struct {
	// FilePath is relative to Repo.LocalPath.
	FilePath string

	// LineNumber is 1-based. Set to 1 when the pattern is file-level
	// (e.g. ARCH-004 stale-config-files).
	LineNumber int

	// DetailJSON is per-pattern auxiliary context (e.g. deprecated
	// symbol, abstraction signature). MUST be valid JSON; "" is
	// normalised to "{}" by the persistence layer.
	DetailJSON string
}

// Pattern is the interface every registered debt pattern implements.
// Implementations live under internal/archaeologist/patterns/, one
// file per pattern. Each is registered via patterns.Register at
// package init() time (see patterns/registry.go).
type Pattern interface {
	// ID returns the canonical pattern identifier — 'ARCH-001'
	// through 'ARCH-NNN'. Used as the ArchaeologistFindings.pattern_id
	// column value.
	ID() string

	// Scan walks the repo's working tree and returns every detected
	// hit. Implementations MUST be deterministic (same input → same
	// output) and language-aware (anti-cheat #2: a Go-API pattern
	// must not scan Rust files). Errors are reported via empty
	// returns + a log line; the sweep loop tolerates per-pattern
	// failures and continues.
	Scan(repo *Repo) []Hit

	// MinHitsForFeature returns the threshold at which the
	// Archaeologist's sweep handler queues an ArchaeologistProposeMigration
	// task. The default reasonable value is 5 (small enough that a
	// real cluster of debt fires fast; large enough that a single
	// false-positive doesn't waste an operator-ratification cycle).
	MinHitsForFeature() int
}

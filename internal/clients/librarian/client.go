// Package librarian defines the client interface for the Fleet Librarian
// service. All production agent code MUST depend on this interface, never
// on a concrete implementation type. Implementations live as siblings:
//
//   - inprocess.go — in-process, backed by holocron.db (D0; the current
//     default; agents queue WriteMemory bounties consumed by the in-process
//     Librarian Spawn loop in internal/agents/librarian.go).
//   - grpc.go      — gRPC client (future, when the D-Lib service form-
//     factor triggers).
//   - shared.go    — shared multi-tenant client (future).
//   - mock.go      — for unit tests; satisfies the interface in-memory.
//
// Pattern P16
// (internal/audittools/audit_pattern_p16_clients_interfaces_test.go)
// enforces that production agents do not import concrete implementation
// struct types from this package — only the Client interface, the data
// types declared below, and the NewInProcess / NewGRPC / NewShared /
// NewMock factory functions.
package librarian

import (
	"context"
	"database/sql"
	"errors"
)

// Client is the contract between agents and the Librarian service. The
// write path is async (queue-backed under the in-process implementation);
// WriteMemory and WriteMemoryTx return the ID of the queued bounty, not
// the eventual FleetMemory row ID — the Spawn loop fills that in when it
// consumes the queue.
type Client interface {
	// GetMemoriesForTask returns every FleetMemory row recorded against
	// the given parent task, ordered newest-first.
	GetMemoriesForTask(ctx context.Context, taskID int) ([]Memory, error)

	// GetMemoriesByScope returns FleetMemory rows matching the provided
	// scope filters (repo, optional time window). An empty Scope is
	// rejected so callers cannot accidentally fan a global scan through
	// this entry point.
	GetMemoriesByScope(ctx context.Context, scope Scope) ([]Memory, error)

	// WriteMemory enqueues a WriteMemory bounty for the in-process
	// Librarian Spawn loop to consume; returns the bounty ID. Callers
	// inside an already-open *sql.Tx MUST use WriteMemoryTx instead so
	// the queue write is atomic with the surrounding state transition.
	WriteMemory(ctx context.Context, memory Memory) (int, error)

	// WriteMemoryTx is the in-transaction sibling of WriteMemory. Only
	// the in-process backing supports it; remote backings (gRPC etc.)
	// return ErrTxNotSupported because a *sql.Tx cannot meaningfully
	// cross a process boundary.
	WriteMemoryTx(ctx context.Context, tx *sql.Tx, memory Memory) (int, error)

	// UpdateMemory rewrites the summary / files-changed / topic-tags
	// fields on an existing memory row. Reserved for operator-driven
	// curation and the post-D4 maintenance dogs; not used by today's
	// agent code paths.
	UpdateMemory(ctx context.Context, memoryID int, update MemoryUpdate) error

	// RemoveMemory deletes a memory row (and its FTS index entry) by ID.
	RemoveMemory(ctx context.Context, memoryID int) error

	// SummarizeForContextOverflow (D2 T1-2) condenses an over-cap LLM
	// prompt to a shorter variant whose UTF-8 byte length is at or
	// below targetBytes. Implementations make a single-turn Claude
	// call (cheapest model available) and return the shortened
	// prompt. The fleet calls this from the claude.go ingress when an
	// agent's assembled prompt exceeds the per-agent byte cap; if
	// the returned summary is still over targetBytes (or this method
	// errors), the caller routes the LLM call to handleInfraFailure.
	//
	// Implementations MUST NOT silently truncate to a smaller value
	// than targetBytes when they cannot summarize cleanly — return
	// the prompt as-is or an error so the caller's overflow path
	// fires correctly.
	SummarizeForContextOverflow(ctx context.Context, prompt string, targetBytes int) (string, error)

	// EmitCandidate writes a Librarian-curated candidate
	// PromotionProposal — the handoff surface from the evolved
	// Librarian to Engineering Corps's ExperimentAuthor (paired-runs.md
	// § Composition with Promotion Pipeline). Rows are written with
	// `kind='candidate'` and `authored_by='librarian'`; this convention
	// (P2 closure note) doubles `authored_by` as the origin column,
	// avoiding a schema migration. Returns the new PromotionProposals.id.
	//
	// Candidates are pending until either ratified (operator approves
	// → status flips via Ratify) or rejected (operator rejects → row
	// stays for the audit trail with rejected_at populated). The EC
	// claim loop polls ListPendingCandidates to discover work.
	EmitCandidate(ctx context.Context, candidate Candidate) (int, error)

	// ListPendingCandidates returns all PromotionProposals rows that
	// represent Librarian-emitted candidates which have not yet been
	// ratified or rejected — i.e. `kind='candidate' AND ratified_at=''
	// AND rejected_at=''`. Newest-first. The EC claim loop reads this
	// to pick up new hypotheses; the dashboard EC tab reads it (joined
	// with kind='promote' rows) for the operator-ratification surface.
	ListPendingCandidates(ctx context.Context) ([]Candidate, error)
}

// Candidate is the handoff payload between the Librarian and EC. The
// shape mirrors the PromotionProposals row but flattens the
// evidence_summary_json string into a Go-side opaque (the caller
// chooses whether to parse it). Authored* fields are populated on
// the read path (ListPendingCandidates); EmitCandidate populates
// AuthoredAt itself via datetime('now').
type Candidate struct {
	ProposalID    int    // zero on emit; populated on read
	HypothesisKey string // becomes PromotionProposals.rule_key
	HypothesisRaw string // becomes PromotionProposals.proposed_content
	EvidenceJSON  string // becomes PromotionProposals.evidence_summary_json (must be valid JSON or "")
	AuthoredAt    string // populated on read; ignored on emit
}

// Memory is the librarian-level view of a FleetMemory row. The write-side
// fields (Task / Files / Feedback / Diff) are what get marshalled into
// the WriteMemory bounty payload; the read-side fields (ID / Outcome /
// Summary / TopicTags / CreatedAt) are populated by the read methods.
type Memory struct {
	// Identity
	ID           int
	ParentTaskID int

	// Provenance
	Repo string

	// Write-side payload (consumed by the Librarian Spawn loop)
	Task     string // task description
	Files    string // comma-separated changed files
	Feedback string // council feedback (success outcome only)
	Diff     string // truncated diff for the Librarian's LLM

	// Read-side fields (filled in by GetMemoriesForTask / GetMemoriesByScope)
	Outcome   string // "success" | "failure"
	Summary   string // 2-4 sentence retrieval-friendly nugget
	TopicTags string // comma-separated 3-6 keywords from the Librarian
	CreatedAt string // SQLite datetime ('YYYY-MM-DD HH:MM:SS', UTC)
}

// Scope filters a GetMemoriesByScope query. At least one of Repo or
// SinceCreatedAt must be set; otherwise the implementation returns
// ErrEmptyScope. Limit caps the result count (zero defaults to 100).
type Scope struct {
	Repo            string // exact repo name match; empty = any repo
	SinceCreatedAt  string // SQLite datetime; "" = no lower bound
	Outcome         string // "success" / "failure" / "" for both
	Limit           int    // 0 → defaultScopeLimit (100); negative is rejected
}

// MemoryUpdate carries the partial-update fields for UpdateMemory. Empty
// strings mean "do not change"; pass an explicit space if you really
// want to clear a field (the implementation maps " " back to ""). This
// keeps the payload composable and avoids a sentinel type.
type MemoryUpdate struct {
	Summary      string
	FilesChanged string
	TopicTags    string
}

// Sentinel errors returned by Client implementations. Callers compare
// with errors.Is.
var (
	// ErrTxNotSupported is returned by remote backings (gRPC, shared)
	// when WriteMemoryTx is called — a *sql.Tx cannot cross processes.
	ErrTxNotSupported = errors.New("librarian: WriteMemoryTx not supported by this backing")

	// ErrEmptyScope is returned by GetMemoriesByScope when the supplied
	// Scope has neither Repo nor SinceCreatedAt set.
	ErrEmptyScope = errors.New("librarian: GetMemoriesByScope requires at least one filter")

	// ErrNotFound is returned by UpdateMemory / RemoveMemory when no row
	// matched the given ID.
	ErrNotFound = errors.New("librarian: memory not found")

	// ErrInvalidLimit is returned when Scope.Limit is negative.
	ErrInvalidLimit = errors.New("librarian: scope limit must be non-negative")
)

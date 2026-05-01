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
	"time"
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

	// GetWeightedMemories (D4 Phase 0) returns the top-K memories for
	// the given scope, ordered by composite quality score
	// (freshness × validation × scope-relevance). Memories whose
	// canonical_id != 0 (i.e. merged into a survivor) are excluded.
	// Replaces direct store.GetFleetMemories calls in agent ingress
	// per Pattern P33.
	//
	// The composite score is freshness_score * (1.0 + validation_score)
	// computed in SQL so the sort is index-friendly. validation_score
	// is clamped to [-1, 1] at write time, so the multiplier lives in
	// [0, 2] — a memory with validation 0 ranks at freshness alone, a
	// fully-positive memory ranks 2× freshness, a fully-negative one
	// ranks 0 (effectively excluded).
	//
	// k <= 0 defaults to 20 (the historic GetFleetMemories cap). An
	// empty Scope is rejected (ErrEmptyScope).
	GetWeightedMemories(ctx context.Context, scope Scope, k int) ([]Memory, error)

	// RecentCommitsDigest (D4 Phase 0) reads the local clone of the
	// supplied repo (via store.GetRepoPath) and returns a structured
	// digest of commits within `window`. Used by Phase 3 (Senate) for
	// per-Senator context. Phase 0 ships the method, Phase 3 wires
	// the call. The git invocation routes through `igit.LogAndRun`
	// so the call is captured in GitOperationLog (Pattern P32).
	//
	// Returns an error (not a panic) when the repo is unregistered or
	// the local path is unreadable.
	RecentCommitsDigest(ctx context.Context, repo string, window time.Duration) (CommitsDigest, error)

	// BootstrapSenatorRules (D4 Phase 0) reads a repo and produces a
	// slice of CandidateRule entries — proposed FleetRules rows with
	// category 'senate' and agent_scope 'senate:<repo>'. Each carries
	// rule body, rationale, and cited evidence. Phase 3 (Senate) will
	// wire the SenatorOnboarding task type that calls this; Phase 0
	// ships the method only.
	//
	// Live-Haiku-gated: when LIVE_HAIKU_DISABLED is set, the
	// implementation returns a deterministic stub fixture so unit
	// tests stay hermetic. Production daemons leave the flag unset
	// and the call routes through claude.CallWithTranscript with the
	// librarian capability profile.
	BootstrapSenatorRules(ctx context.Context, repo string) ([]CandidateRule, error)

	// RefreshSenatorMemoryDigest (D4 Phase 0) produces a SenatorDigest
	// for the supplied repo — the shape Phase 3's `senate-refresh`
	// dog will call to update SenateMemory. Phase 0 ships the method
	// only. LIVE_HAIKU_DISABLED gates as above.
	RefreshSenatorMemoryDigest(ctx context.Context, repo string) (SenatorDigest, error)
}

// CommitsDigest is the per-repo recent-commits view returned by
// RecentCommitsDigest. The shape carries enough signal for a Senator
// or Librarian-LLM to reason about repo activity without having to
// stream the full diff. Each commit's diffstat is the
// `git log --shortstat` line ("X files changed, Y insertions, Z
// deletions") rendered verbatim — analyzers can re-parse it cheaply.
type CommitsDigest struct {
	Repo      string         // canonical repo name from Repositories
	Window    time.Duration  // window applied to git log --since
	Commits   []DigestCommit // newest-first
	Truncated bool           // true if the digest hit the per-call commit cap
}

// DigestCommit is one line of CommitsDigest. SHA + Subject + Author +
// Diffstat are sourced from `git log --shortstat`. AuthorTime is the
// commit's author-date as a SQLite-comparable string (UTC).
type DigestCommit struct {
	SHA        string
	Subject    string
	Author     string
	AuthorTime string
	Diffstat   string
}

// CandidateRule is one rule emitted by BootstrapSenatorRules. The
// shape mirrors a FleetRules row but lives in-memory until Phase 3
// promotes it through the standard candidate pipeline.
type CandidateRule struct {
	RuleKey    string // proposed FleetRules.rule_key (e.g. "senate-<repo>-<slug>")
	Category   string // 'senate' for D4-P0 outputs
	AgentScope string // 'senate:<repo>'
	Body       string // FleetRules.content (the rule body)
	Rationale  string // human-readable WHY (becomes the audit comment)
	Evidence   string // cited evidence — README path, commit shas, etc.
}

// SenatorDigest is the per-repo refresh shape consumed by Phase 3's
// senate-refresh dog. Includes the recent-commits digest plus a
// summary of the public API surface — the two signals a Senator
// needs to keep its rule context fresh.
type SenatorDigest struct {
	Repo               string
	GeneratedAt        string // SQLite UTC timestamp
	APISurfaceSummary  string // one-paragraph summary of public APIs
	RecentCommits      CommitsDigest
	OutstandingRulesK  int    // count of FleetRules currently scoped to this Senator
	NotesForOperator   string // optional human-readable notes the Librarian wants surfaced
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

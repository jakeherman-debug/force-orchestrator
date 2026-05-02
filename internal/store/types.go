package store

// ── Core work item ────────────────────────────────────────────────────────────

type Bounty struct {
	ID                int
	ParentID          int
	TargetRepo        string
	Type              string
	Status            string
	Payload           string
	Owner             string
	RetryCount        int
	InfraFailures     int
	ConvoyID          int
	Checkpoint        string
	BranchName        string
	Priority          int
	TaskTimeout       int // seconds; 0 means use AstromechTimeoutForAttempt progressive default
	MedicRequeueCount int // Fix #6: number of times Medic has requeued this task; hard-capped to bound the A→C→M→A loop
	ReshardGeneration int // Fix #6: generation number for auto-reshard cascade; refuses past cap to bound 1→3→9→27 fanout
	// StageID (D5.5 P2): which ConvoyStages row this task belongs to. Zero
	// means "no stage assignment" — either a non-convoy task or a legacy
	// single-mode convoy task. Non-zero means the task is part of stage
	// `StageID`'s work and the convoy-stage-watch dog will scope dispatch
	// + completion checks to that stage.
	StageID int
}

// ── Planning ──────────────────────────────────────────────────────────────────

type TaskPlan struct {
	TempID    int    `json:"id"`
	Repo      string `json:"repo"`
	Task      string `json:"task"`
	BlockedBy []int  `json:"blocked_by"`
}

// ── Review ────────────────────────────────────────────────────────────────────

// CouncilRuling is the structured response from the Jedi Council LLM.
//
// Fix #8.5 — `Approved` is a `*bool` (not `bool`) so the parser can
// distinguish an LLM that explicitly returned `"approved":false` from
// one that omitted the field entirely. A missing field is ambiguous:
// before this change it silently parsed as `false` and fed a permanent-
// reject loop through MaxRetries without any feedback. Now a nil
// `Approved` is a schema violation that routes through the existing
// council parse-failure budget (`councilParseFailureCap`).
type CouncilRuling struct {
	Approved *bool  `json:"approved"`
	Feedback string `json:"feedback"`
}

// ── Escalation ────────────────────────────────────────────────────────────────

type EscalationSeverity string

const (
	SeverityLow    EscalationSeverity = "LOW"
	SeverityMedium EscalationSeverity = "MEDIUM"
	SeverityHigh   EscalationSeverity = "HIGH"
)

type Escalation struct {
	ID             int
	TaskID         int
	Severity       EscalationSeverity
	Message        string
	Status         string // Open, Acknowledged, Closed
	CreatedAt      string
	AcknowledgedAt string
}

// ── Convoy ────────────────────────────────────────────────────────────────────

// Convoy status values:
//   - Active            — tasks are running
//   - AwaitingDraftPR   — all sub-PRs merged into ask-branch, Diplomat enqueued
//   - DraftPROpen       — draft PR exists on main, waiting for human "Ship it"
//   - Shipped           — draft PR merged; ask-branch cleanup has run or is pending
//   - Abandoned         — draft PR closed without merge, convoy terminated
//   - Completed         — legacy terminal state from pre-PR-flow convoys (no ask_branch)
//   - Failed            — at least one constituent task is Failed or Escalated
type Convoy struct {
	ID                int
	Name              string
	Status            string
	Coordinated       bool
	AskBranch         string // "" until Pilot's CreateAskBranch runs
	AskBranchBaseSHA  string // main's HEAD at ask-branch creation; used for drift detection
	DraftPRURL        string
	DraftPRNumber     int
	DraftPRState      string // Open, Merged, Closed, "" (not yet created)
	ShippedAt         string
	StagingMode       string // 'single' (default + legacy) | 'staged' (D5.5)
	StagingStrategy   string // 'strict' (default + only impl in D5.5) | 'merge_parallel' | 'stacked'
	CreatedAt         string
}

// ── Convoy stages (D5.5) ─────────────────────────────────────────────────────
//
// ConvoyStage is one row in a Commander-drafted phase pipeline for a convoy.
// stages execute in stage_num order (1-indexed). Status lifecycle:
//
//   Pending → Open → AllPRsMerged → AwaitingGate → GatePassed → Verified
//
// Any non-terminal state may transition to Failed (terminal). Legacy
// single-mode convoys carry one auto-created stage 1 row in status=Open
// with gate_type=NULL; D5.5's forward-compat migration handles that.
type ConvoyStage struct {
	ID                 int
	ConvoyID           int
	StageNum           int
	IntentText         string
	Status             string
	GateType           string // empty string represents NULL — meaning no gate (terminal-stage only) or unset
	GateTypeIsNull     bool   // true iff the DB column is NULL
	GateConfigJSON     string
	GateTimeoutMinutes int
	OpenedAt           string
	AllPRsMergedAt     string
	GatePassedAt       string
	CompletedAt        string
}

// Convoy stage status constants. Status changes go through AdvanceStage.
const (
	StageStatusPending       = "Pending"
	StageStatusOpen          = "Open"
	StageStatusAllPRsMerged  = "AllPRsMerged"
	StageStatusAwaitingGate  = "AwaitingGate"
	StageStatusGatePassed    = "GatePassed"
	StageStatusVerified      = "Verified"
	StageStatusFailed        = "Failed"
)

// Convoy staging-mode constants (Convoy.StagingMode).
const (
	StagingModeSingle = "single"
	StagingModeStaged = "staged"
)

// Convoy staging-strategy constants (Convoy.StagingStrategy). Only `strict`
// is implemented in D5.5; `merge_parallel` and `stacked` are reserved for
// future deliverables.
const (
	StagingStrategyStrict        = "strict"
	StagingStrategyMergeParallel = "merge_parallel"
	StagingStrategyStacked       = "stacked"
)

// ── Repository ────────────────────────────────────────────────────────────────

// Repository describes a registered code repo plus its PR-flow configuration.
// PRFlowEnabled defaults to true — repos opt out, not in. Quarantine fields are
// set by the repo-config-check dog when a repo's remote becomes unreachable or
// its origin URL changes; a quarantined repo falls back to the legacy local-merge
// path until the operator re-validates it.
type Repository struct {
	Name             string
	LocalPath        string
	Description      string
	RemoteURL        string
	DefaultBranch    string
	PRTemplatePath   string
	PRFlowEnabled    bool
	QuarantinedAt    string
	QuarantineReason string
	// Mode is the D2 T1-4 tri-state writability flag:
	//   "read_only"   — astromechs cannot claim, destructive ops refuse.
	//   "write"       — repo is fully active.
	//   "quarantined" — read-only behaviour plus dashboard banner +
	//                   [QUARANTINED REPO] mail on claim attempts.
	Mode string
	// ReleaseLabelPattern is D5.5's per-repo regex for the
	// `release_label_present` gate type. Empty string means the repo
	// doesn't use release labels; convoys touching such a repo cannot
	// use the release_label_present gate (planner-time error).
	ReleaseLabelPattern string
}

// ── Per-(convoy, repo) ask-branch ────────────────────────────────────────────
//
// A convoy's tasks may target multiple repos; each touched repo gets its own
// ask-branch and, eventually, its own draft PR. ConvoyAskBranch is the state
// for one (convoy, repo) pair.
type ConvoyAskBranch struct {
	ConvoyID         int
	Repo             string
	AskBranch        string
	AskBranchBaseSHA string
	DraftPRURL       string
	DraftPRNumber    int
	DraftPRState     string // "" | Open | Merged | Closed
	ShippedAt        string
	LastRebasedAt    string
	CreatedAt        string
	// StageID (D5.5) — points at the ConvoyStages.id this ask-branch belongs to.
	// Pre-D5.5 rows are backfilled by runMigrations to the implicit single-stage
	// stage 1 for that convoy. Zero means "stage_id IS NULL" — used on freshly-
	// inserted rows that haven't yet been pinned to a stage. ConvoyReview's
	// per-stage scoping (D5.5 P2 β) filters by this column for staged convoys.
	StageID int
}

// ── Ask-branch sub-PR ────────────────────────────────────────────────────────

// AskBranchPR tracks a single astromech-task-level GitHub PR that targets the
// convoy's ask-branch. State transitions:
//   Open → (CI green) → auto-merged → state=Merged
//   Open → (CI red)   → failure_count++ → Medic CIFailureTriage
//   Open → (closed externally) → state=Closed, task escalated
type AskBranchPR struct {
	ID                  int
	TaskID              int
	ConvoyID            int
	Repo                string
	PRNumber            int
	PRURL               string
	State               string // Open, Merged, Closed
	ChecksState         string // Pending, Success, Failure
	FailureCount        int
	StallRetriggerCount int // sub-PR CI stall diagnosis re-trigger attempts
	SpawnedFixCount     int // Fix #7 (AUDIT-120): lifetime count of Medic-spawned fix tasks on this PR
	MergedAt            string
	CreatedAt           string
}

// ── Persistent agent worktree ─────────────────────────────────────────────────

type AgentWorktree struct {
	AgentName    string
	Repo         string
	WorktreePath string
}

// ── Task history (seance) ─────────────────────────────────────────────────────

type TaskHistoryEntry struct {
	ID           int
	TaskID       int
	Attempt      int
	Agent        string
	SessionID    string
	ClaudeOutput string
	Outcome      string // Completed, Failed, Escalated, Sharded, Timeout
	TokensIn     int
	TokensOut    int
	// CostUSDEstimate (D2 T1-1) is the per-attempt cost in dollars,
	// computed at write time from the model + tokens via
	// claude.pricing.CostUSD.
	CostUSDEstimate float64
	MemoryIDs       string // CSV of FleetMemory.id values injected into this attempt's prompt
	CreatedAt       string
}

// ── Audit log ─────────────────────────────────────────────────────────────────

type AuditEntry struct {
	ID        int
	Actor     string
	Action    string
	TaskID    int
	Detail    string
	CreatedAt string
}

// ── Fleet mail ────────────────────────────────────────────────────────────────

// MailType categorises a message so agents can decide how to act on it.
type MailType string

const (
	MailTypeDirective   MailType = "directive"   // standing orders — injected into system prompt
	MailTypeFeedback    MailType = "feedback"     // council rejection context — injected as prior attempt notes
	MailTypeAlert       MailType = "alert"        // warning — shown prominently in prompt
	MailTypeRemediation MailType = "remediation"  // infra fix notification — informational only
	MailTypeInfo        MailType = "info"         // general informational — appended as context
)

// ── Captain review ────────────────────────────────────────────────────────────

// CaptainRuling is the structured response from the Captain agent.
// The captain checks convoy plan coherence after each Astromech commit —
// it is not a code reviewer (that is the council's job) but a plan coherence check.
type CaptainRuling struct {
	Decision      string          `json:"decision"`       // "approve", "reject", "escalate"
	Feedback      string          `json:"feedback"`       // reason for rejection or escalation; empty on approve
	TaskUpdates   []CaptainUpdate `json:"task_updates"`   // downstream task payload changes
	NewTasks      []CaptainTask   `json:"new_tasks"`      // additional tasks to insert into the convoy
	RejectedFiles []string        `json:"rejected_files"` // files touched out-of-scope; propagated as a SCOPE GUARD section on requeue

	// CitedATs is the convoy-scoped acceptance-test references the LLM
	// claims its rationale relies on (concern #1 / Pattern P20). Each
	// entry MUST carry both convoy_id and at_id; bare at_id rejects at
	// SetProposedAction. Empty slice is fine when the ruling cites no
	// AT (e.g. infra-only diffs) but the field MUST be present so the
	// proposal's structured payload satisfies P23.
	CitedATs []CitedAT `json:"cited_ats"`

	// CitedFleetRules is the list of FleetRules `rule_key` values the
	// LLM claims its rationale relies on. Same P23 contract: empty
	// slice OK, omission produces an empty slice via Go's JSON decode.
	CitedFleetRules []string `json:"cited_fleet_rules"`

	// ClassificationConfidence is the LLM-emitted certainty in the
	// chosen Decision, in [0.0, 1.0]. Validation rejects out-of-range
	// values (proposal validator). 0.0 is reserved for "the LLM did not
	// emit a confidence" — emitCaptainProposal falls back to the
	// deterministic floor (captainConfidenceFromDecision) on a 0
	// reading so a missing field never silently produces a meaningless
	// "0% confident" routing signal.
	ClassificationConfidence float64 `json:"classification_confidence"`
}

// CaptainUpdate is a payload modification for an existing downstream task.
// Only tasks in Pending or Planned status within the same convoy can be updated.
type CaptainUpdate struct {
	ID         int    `json:"id"`
	NewPayload string `json:"new_payload"`
}

// CaptainTask is a new CodeEdit task to be inserted by the Captain.
// BlockedBy references real task IDs already in the convoy (empty = no dependencies).
type CaptainTask struct {
	Repo      string `json:"repo"`
	Task      string `json:"task"`
	BlockedBy []int  `json:"blocked_by"`
}

// ── Fleet memory (cross-task learning) ───────────────────────────────────────

// FleetMemoryEntry records a lesson learned from a successfully completed task.
// Stored by the council on approval and injected into future agents working on the same repo.
type FleetMemoryEntry struct {
	ID           int
	Repo         string
	TaskID       int
	Outcome      string // "success" or "failure"
	Summary      string // task description + outcome reason
	FilesChanged string // comma-separated list of affected files (success only)
	TopicTags    string // comma-separated 3-5 short keywords (e.g. "auth, middleware, jwt")
	Embedding    []byte // reserved: float32 vector blob for future sqlite-vec upgrade
	CreatedAt    string

	// D4 Phase 0 — Librarian evolution: quality-scoring fields. These
	// are populated only by helpers that explicitly SELECT them (the
	// classic GetFleetMemories paths leave them at zero values for
	// backwards compatibility).
	FreshnessScore  float64 // freshness_score column; decays with row age via RecomputeFreshnessScores
	ValidationScore float64 // validation_score column; adjusted by RecordValidation
	RetrievalCount  int     // retrieval_count column; bumped by RecordRetrieval
	LastRetrievedAt string  // last_retrieved_at column ('' if never retrieved)
	CanonicalID     int     // canonical_id column (0 means "this row IS canonical")
}

// ConflictTicket is one row in the ConflictTickets table — a pair of
// FleetMemory rows the librarian-conflict-watch dog flagged as
// contradictory. Operator-surfaced via /api/conflicts/tickets.
type ConflictTicket struct {
	ID             int
	MemoryAID      int
	MemoryBID      int
	Reason         string
	Status         string // 'open' | 'resolved'
	CreatedAt      string
	ResolvedAt     string
	ResolutionNote string
}

// ── Task notes ────────────────────────────────────────────────────────────────

// TaskNote is an operator note attached to a task, injected into agent context at claim time.
type TaskNote struct {
	ID        int
	TaskID    int
	Note      string
	CreatedAt string
}

// ── Proposed convoys ──────────────────────────────────────────────────────────

// ProposedConvoy is a Commander's decomposition plan awaiting Chancellor review.
type ProposedConvoy struct {
	ID        int
	FeatureID int
	PlanJSON  string
	Status    string // pending | approved | rejected | merged
	CreatedAt string
}

// ActiveConvoyInfo is a summary of an active convoy for Chancellor context.
type ActiveConvoyInfo struct {
	ID    int
	Name  string
	Tasks []string
}

// PendingProposalInfo is a summary of another pending ProposedConvoy for Chancellor context.
type PendingProposalInfo struct {
	FeatureID int
	Payload   string
	PlanJSON  string
}

// PendingFeatureInfo is a Feature task not yet planned by Commander.
// Shown to the Chancellor so it can reason about upcoming work dependencies.
type PendingFeatureInfo struct {
	FeatureID int
	Payload   string
}


// ── PR review comments ───────────────────────────────────────────────────────

// PRReviewComment is a single review comment on a draft PR (bot or human).
// Populated by the pr-review-poll dog; classified and dispatched by Diplomat's
// PRReviewTriage. See schema.sql for column semantics.
type PRReviewComment struct {
	ID                   int
	ConvoyID             int
	Repo                 string
	DraftPRNumber        int
	GitHubCommentID      int64
	CommentType          string // "review_comment" | "issue_comment"
	Author               string
	AuthorKind           string // "bot" | "human"
	Body                 string
	Path                 string
	Line                 int
	DiffHunk             string
	ReviewThreadID       string
	InReplyToCommentID   int64
	ThreadDepth          int
	Classification       string
	ClassificationReason string
	SpawnedTaskID        int
	ReplyBody            string
	RepliedAt            string
	ThreadResolvedAt     string
	CreatedAt            string
}

type FleetMail struct {
	ID          int
	FromAgent   string
	ToAgent     string
	Subject     string
	Body        string
	TaskID      int      // optional — links mail to a specific task
	MessageType MailType // how the agent should treat this message
	ReadAt      string   // empty = operator-unread (UI display only)
	ConsumedAt  string   // empty = not yet consumed by an agent
	CreatedAt   string
}

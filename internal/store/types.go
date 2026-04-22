package store

// ── Core work item ────────────────────────────────────────────────────────────

type Bounty struct {
	ID            int
	ParentID      int
	TargetRepo    string
	Type          string
	Status        string
	Payload       string
	Owner         string
	RetryCount    int
	InfraFailures int
	ConvoyID      int
	Checkpoint    string
	BranchName    string
	Priority      int
	TaskTimeout   int // seconds; 0 means use AstromechTimeoutForAttempt progressive default
}

// ── Planning ──────────────────────────────────────────────────────────────────

type TaskPlan struct {
	TempID    int    `json:"id"`
	Repo      string `json:"repo"`
	Task      string `json:"task"`
	BlockedBy []int  `json:"blocked_by"`
}

// ── Review ────────────────────────────────────────────────────────────────────

type CouncilRuling struct {
	Approved bool   `json:"approved"`
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

type Convoy struct {
	ID        int
	Name      string
	Status    string // Active, Completed
	CreatedAt string
}

// ConvoyEvent is a timestamped milestone in a convoy's lifecycle.
type ConvoyEvent struct {
	ID        int
	ConvoyID  int
	EventType string
	Detail    string
	CreatedAt string
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
	CreatedAt    string
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
	Decision    string          `json:"decision"`     // "approve", "reject", "escalate"
	Feedback    string          `json:"feedback"`     // reason for rejection or escalation; empty on approve
	TaskUpdates []CaptainUpdate `json:"task_updates"` // downstream task payload changes
	NewTasks    []CaptainTask   `json:"new_tasks"`    // additional tasks to insert into the convoy
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
	Embedding    []byte // reserved: float32 vector blob for future sqlite-vec upgrade
	CreatedAt    string
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

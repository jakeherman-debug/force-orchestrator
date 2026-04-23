package dashboard

// DashboardStatus is the payload for GET /api/status
type DashboardStatus struct {
	Timestamp         string         `json:"timestamp"`
	DaemonRunning     bool           `json:"daemon_running"`
	DaemonPID         int            `json:"daemon_pid,omitempty"`
	Estopped          bool           `json:"estopped"`
	Tasks             map[string]int `json:"tasks"`
	OpenEscalations   int            `json:"open_escalations"`
	HighEscalations   int            `json:"high_escalations"`
	ActiveConvoys     int            `json:"active_convoys"`
	ReadyToShip       int            `json:"ready_to_ship"` // convoys in DraftPROpen awaiting operator "Ship It"
	UnreadMail        int            `json:"unread_mail"`
	TotalSpendDollars float64        `json:"total_spend_dollars"`
}

// TasksResponse is the payload for GET /api/tasks
type TasksResponse struct {
	Tasks []DashboardTask `json:"tasks"`
	Total int             `json:"total"`
}

// DashboardTask is one row in GET /api/tasks
type DashboardTask struct {
	ID             int     `json:"id"`
	Type           string  `json:"type"`
	Status         string  `json:"status"`
	Repo           string  `json:"repo"`
	Owner          string  `json:"owner"`
	RetryCount     int     `json:"retry_count"`
	ConvoyID       int     `json:"convoy_id"`
	Payload        string  `json:"payload"`
	ErrorLog       string  `json:"error_log,omitempty"`
	LockedAt       string  `json:"locked_at,omitempty"`
	Priority       int     `json:"priority"`
	RuntimeSeconds int     `json:"runtime_seconds"`
	BlockedBy      []int   `json:"blocked_by"`
	CostDollars    float64 `json:"cost_dollars"`
	CreatedAt      string  `json:"created_at"`
}

// DashboardMail is a single fleet mail message
type DashboardMail struct {
	ID          int    `json:"id"`
	FromAgent   string `json:"from_agent"`
	ToAgent     string `json:"to_agent"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	TaskID      int    `json:"task_id"`
	MessageType string `json:"message_type"`
	ReadAt      string `json:"read_at"`
	CreatedAt   string `json:"created_at"`
}

// DashboardMemory is a single fleet memory entry
type DashboardMemory struct {
	ID           int    `json:"id"`
	TaskID       int    `json:"task_id,omitempty"`
	Outcome      string `json:"outcome"`
	Summary      string `json:"summary"`
	FilesChanged string `json:"files_changed,omitempty"`
	TopicTags    string `json:"topic_tags,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// DashboardAttempt is a single history entry. InjectedMemories is populated
// when the attempt recorded a memory_ids snapshot; it contains EXACTLY the
// memories that were fed into the agent's prompt for this attempt (not a
// re-query, which would return today's FTS matches instead of what the
// agent actually saw).
type DashboardAttempt struct {
	Attempt          int               `json:"attempt"`
	Agent            string            `json:"agent"`
	Outcome          string            `json:"outcome"`
	TokensIn         int               `json:"tokens_in"`
	TokensOut        int               `json:"tokens_out"`
	CreatedAt        string            `json:"created_at"`
	InjectedMemories []DashboardMemory `json:"injected_memories,omitempty"`
}

// DashboardTaskDetail is the payload for GET /api/tasks/{id}
type DashboardTaskDetail struct {
	ID             int                `json:"id"`
	Type           string             `json:"type"`
	Status         string             `json:"status"`
	Repo           string             `json:"repo"`
	Owner          string             `json:"owner"`
	ParentID       int                `json:"parent_id"`
	ConvoyID       int                `json:"convoy_id"`
	BranchName     string             `json:"branch_name"`
	BranchURL      string             `json:"branch_url,omitempty"` // web URL to the branch on origin (empty when remote not resolvable)
	PRNumber       int                `json:"pr_number,omitempty"`  // sub-PR number if one is tracked in AskBranchPRs
	PRURL          string             `json:"pr_url,omitempty"`     // sub-PR web URL if a PR has been opened
	PRState        string             `json:"pr_state,omitempty"`   // Open | Merged | Closed
	ConvoyStatus       string         `json:"convoy_status,omitempty"`         // parent convoy's current status
	ConvoyReadyToShip  bool           `json:"convoy_ready_to_ship,omitempty"`  // true only when fleet work is truly done — drives the Ship It shortcut
	RetryCount     int                `json:"retry_count"`
	InfraFailures  int                `json:"infra_failures"`
	Priority       int                `json:"priority"`
	LockedAt       string             `json:"locked_at,omitempty"`
	ErrorLog       string             `json:"error_log,omitempty"`
	BroaderGoal    string             `json:"broader_goal,omitempty"`
	Directive      string             `json:"directive"`
	RuntimeSeconds int                `json:"runtime_seconds"`
	BlockedBy      []int              `json:"blocked_by"`
	CostDollars    float64            `json:"cost_dollars"`
	Memories       []DashboardMemory  `json:"memories"`
	History        []DashboardAttempt `json:"history"`
	Mail           []DashboardMail    `json:"mail"`
}

// DashboardEscalation is a single escalation
type DashboardEscalation struct {
	ID             int    `json:"id"`
	TaskID         int    `json:"task_id"`
	Severity       string `json:"severity"`
	Message        string `json:"message"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	AcknowledgedAt string `json:"acknowledged_at"`
}

// DashboardConvoy is a convoy with progress info.
// PR-flow fields are populated only when the convoy has ConvoyAskBranch rows
// (i.e. it went through the PR-based delivery path rather than legacy merges).
type DashboardConvoy struct {
	ID                int                        `json:"id"`
	Name              string                     `json:"name"`
	Status            string                     `json:"status"`
	CreatedAt         string                     `json:"created_at"`
	Completed         int                        `json:"completed"`
	Total             int                        `json:"total"`
	HasPlanned        bool                       `json:"has_planned"`
	ReadyToShip       bool                       `json:"ready_to_ship"` // DraftPROpen AND fleet work quiesced — operator's turn
	AskBranches       []DashboardAskBranch       `json:"ask_branches,omitempty"`
	SubPRRollup       *DashboardSubPRRollup      `json:"sub_pr_rollup,omitempty"`
	PRReviewRollup    *DashboardPRReviewRollup   `json:"pr_review_rollup,omitempty"`
}

// DashboardPRReviewRollup counts PR review comments by classification for
// display on the convoy card. Populated only for convoys that have draft PRs
// (otherwise there can't be any review comments).
type DashboardPRReviewRollup struct {
	Total           int `json:"total"`
	BotInScope      int `json:"bot_in_scope"`
	BotOutOfScope   int `json:"bot_out_of_scope"`
	BotNotAction    int `json:"bot_not_actionable"`
	BotConflicted   int `json:"bot_conflicted_loop"`
	BotUnclassified int `json:"bot_unclassified"`
	HumanAwaiting   int `json:"human_awaiting"`
	// BotBlocking is unclassified + in_scope_fix whose fix has not yet landed.
	// Non-zero means the convoy has open bot issues that must resolve before shipping.
	BotBlocking int `json:"bot_blocking"`
}

// DashboardPRReviewComment is a single row in the convoy-detail comment table.
type DashboardPRReviewComment struct {
	ID                    int    `json:"id"`
	Repo                  string `json:"repo"`
	DraftPRNumber         int    `json:"draft_pr_number"`
	GitHubCommentID       int64  `json:"github_comment_id"`
	CommentType           string `json:"comment_type"`
	Author                string `json:"author"`
	AuthorKind            string `json:"author_kind"`
	Body                  string `json:"body"`
	Path                  string `json:"path,omitempty"`
	Line                  int    `json:"line,omitempty"`
	Classification        string `json:"classification"`
	ClassificationReason  string `json:"classification_reason,omitempty"`
	SpawnedTaskID         int    `json:"spawned_task_id,omitempty"`
	SpawnedTaskStatus     string `json:"spawned_task_status,omitempty"`
	ReplyBody             string `json:"reply_body,omitempty"`
	RepliedAt             string `json:"replied_at,omitempty"`
	ThreadResolvedAt      string `json:"thread_resolved_at,omitempty"`
	ThreadDepth           int    `json:"thread_depth"`
	CreatedAt             string `json:"created_at"`
}

// DashboardAskBranch is the dashboard view of one per-repo ask-branch's state.
type DashboardAskBranch struct {
	Repo             string `json:"repo"`
	AskBranch        string `json:"ask_branch"`
	AskBranchBaseSHA string `json:"ask_branch_base_sha"`
	DraftPRURL       string `json:"draft_pr_url"`
	DraftPRNumber    int    `json:"draft_pr_number"`
	DraftPRState     string `json:"draft_pr_state"`
	ShippedAt        string `json:"shipped_at,omitempty"`
	LastRebasedAt    string `json:"last_rebased_at,omitempty"`
}

// DashboardSubPRRollup summarises sub-PR state for a convoy.
type DashboardSubPRRollup struct {
	Total          int `json:"total"`
	Open           int `json:"open"`
	Merged         int `json:"merged"`
	Closed         int `json:"closed"`
	ChecksPending  int `json:"checks_pending"`
	ChecksSuccess  int `json:"checks_success"`
	ChecksFailure  int `json:"checks_failure"`
}

// DashboardAgent is a registered agent with its current task
type DashboardAgent struct {
	AgentName     string `json:"agent_name"`
	Repo          string `json:"repo"`
	Role          string `json:"role"`
	CurrentTaskID int    `json:"current_task_id,omitempty"`
	TaskStatus    string `json:"task_status,omitempty"`
	LockedAt      string `json:"locked_at,omitempty"`
}

// StatsResponse is the payload for GET /api/stats
type StatsResponse struct {
	Tasks              map[string]int `json:"tasks"`
	ActiveAgents       int            `json:"active_agents"`
	ActiveConvoys       int            `json:"active_convoys"`
	PendingCount        int            `json:"pending_count"`
	ActiveCount         int            `json:"active_count"`
	CompletedTodayCount int            `json:"completed_today_count"`
}

// DashboardConvoyEvent is a single timeline entry for GET /api/convoys/{id}/events
type DashboardConvoyEvent struct {
	ID        int    `json:"id"`
	ConvoyID  int    `json:"convoy_id"`
	EventType string `json:"event_type"`
	OldValue  string `json:"old_value,omitempty"`
	NewValue  string `json:"new_value,omitempty"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"created_at"`
}

// addTaskBody is the POST /api/add request body
type addTaskBody struct {
	Type           string `json:"type"`
	Payload        string `json:"payload"`
	Repo           string `json:"repo"`
	Priority       int    `json:"priority"`
	IdempotencyKey string `json:"idempotency_key"`
}

// rejectBody is the POST /api/tasks/{id}/reject request body
type rejectBody struct {
	Reason string `json:"reason"`
}

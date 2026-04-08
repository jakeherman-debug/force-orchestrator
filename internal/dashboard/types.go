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
	Outcome      string `json:"outcome"`
	Summary      string `json:"summary"`
	FilesChanged string `json:"files_changed,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// DashboardAttempt is a single history entry
type DashboardAttempt struct {
	Attempt   int    `json:"attempt"`
	Agent     string `json:"agent"`
	Outcome   string `json:"outcome"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	CreatedAt string `json:"created_at"`
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

// DashboardConvoy is a convoy with progress info
type DashboardConvoy struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	Completed  int    `json:"completed"`
	Total      int    `json:"total"`
	HasPlanned bool   `json:"has_planned"`
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

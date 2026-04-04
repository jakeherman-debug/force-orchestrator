package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)


// DashboardStatus is the payload for GET /api/status
type DashboardStatus struct {
	Timestamp       string         `json:"timestamp"`
	DaemonRunning   bool           `json:"daemon_running"`
	DaemonPID       int            `json:"daemon_pid,omitempty"`
	Estopped        bool           `json:"estopped"`
	Tasks           map[string]int `json:"tasks"`
	OpenEscalations int            `json:"open_escalations"`
	HighEscalations int            `json:"high_escalations"`
	ActiveConvoys   int            `json:"active_convoys"`
	UnreadMail      int            `json:"unread_mail"`
}

// DashboardTask is one row in GET /api/tasks
type DashboardTask struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Repo       string `json:"repo"`
	Owner      string `json:"owner"`
	RetryCount int    `json:"retry_count"`
	ConvoyID   int    `json:"convoy_id"`
	Payload    string `json:"payload"`
	ErrorLog   string `json:"error_log,omitempty"`
	LockedAt   string `json:"locked_at,omitempty"`
}

// DashboardMail is a single fleet mail message for GET /api/mail and task detail
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

// DashboardMemory is a single fleet memory entry for GET /api/tasks/{id}
type DashboardMemory struct {
	Outcome      string `json:"outcome"`
	Summary      string `json:"summary"`
	FilesChanged string `json:"files_changed,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// DashboardAttempt is a single history entry for GET /api/tasks/{id}
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
	ID            int                `json:"id"`
	Type          string             `json:"type"`
	Status        string             `json:"status"`
	Repo          string             `json:"repo"`
	Owner         string             `json:"owner"`
	ParentID      int                `json:"parent_id"`
	ConvoyID      int                `json:"convoy_id"`
	BranchName    string             `json:"branch_name"`
	RetryCount    int                `json:"retry_count"`
	InfraFailures int                `json:"infra_failures"`
	LockedAt      string             `json:"locked_at,omitempty"`
	ErrorLog      string             `json:"error_log,omitempty"`
	BroaderGoal   string             `json:"broader_goal,omitempty"`
	Directive     string             `json:"directive"`
	Memories      []DashboardMemory  `json:"memories"`
	History       []DashboardAttempt `json:"history"`
	Mail          []DashboardMail    `json:"mail"`
}

// splitGoalDirective splits a payload into (broaderGoal, directive).
// Commander prefixes subtask payloads with [GOAL: ...]\n\n; everything after is the directive.
// If no prefix is present, the whole payload is the directive.
func splitGoalDirective(payload string) (goal, directive string) {
	if strings.HasPrefix(payload, "[GOAL: ") {
		if end := strings.Index(payload, "]\n\n"); end != -1 {
			return payload[7:end], payload[end+3:]
		}
	}
	return "", payload
}

func handleStatus(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		s := DashboardStatus{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Estopped:  agents.IsEstopped(db),
			Tasks:     map[string]int{},
		}

		rows, _ := db.Query(`SELECT status, COUNT(*) FROM BountyBoard GROUP BY status`)
		if rows != nil {
			for rows.Next() {
				var status string
				var n int
				rows.Scan(&status, &n)
				s.Tasks[status] = n
			}
			rows.Close()
		}

		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&s.OpenEscalations)
		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open' AND severity = 'HIGH'`).Scan(&s.HighEscalations)
		db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE status = 'Active'`).Scan(&s.ActiveConvoys)
		unread, _ := store.MailStats(db, "")
		s.UnreadMail = unread

		if pidBytes, err := os.ReadFile("fleet.pid"); err == nil {
			var pid int
			fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &pid)
			if pid > 0 {
				if proc, err := os.FindProcess(pid); err == nil {
					if proc.Signal(syscall.Signal(0)) == nil {
						s.DaemonRunning = true
						s.DaemonPID = pid
					}
				}
			}
		}

		json.NewEncoder(w).Encode(s)
	}
}

func handleTasks(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		statusFilter := r.URL.Query().Get("status")
		query := `SELECT id, type, status, target_repo, owner, retry_count, convoy_id, payload, IFNULL(error_log,''), IFNULL(locked_at,'')
			FROM BountyBoard`
		args := []any{}
		if statusFilter != "" {
			query += ` WHERE status = ?`
			args = append(args, statusFilter)
		}
		query += ` ORDER BY id DESC LIMIT 200`

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var tasks []DashboardTask
		for rows.Next() {
			var t DashboardTask
			rows.Scan(&t.ID, &t.Type, &t.Status, &t.Repo, &t.Owner, &t.RetryCount,
				&t.ConvoyID, &t.Payload, &t.ErrorLog, &t.LockedAt)
			if len(t.Payload) > 300 {
				t.Payload = t.Payload[:300] + "…"
			}
			tasks = append(tasks, t)
		}
		if tasks == nil {
			tasks = []DashboardTask{}
		}
		json.NewEncoder(w).Encode(tasks)
	}
}

// handleEvents streams holonet.jsonl as server-sent events.
// Handles log rotation: when the file is replaced, the stream reopens it.
func handleEvents(logPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		openLog := func() (*os.File, os.FileInfo) {
			f, err := os.Open(logPath)
			if err != nil {
				return nil, nil
			}
			f.Seek(0, 2) // start at end — only stream new events
			fi, _ := f.Stat()
			return f, fi
		}

		f, fi := openLog()
		if f == nil {
			fmt.Fprintf(w, "data: {\"error\":\"holonet.jsonl not found\"}\n\n")
			return
		}
		defer f.Close()

		flusher, ok := w.(http.Flusher)
		scanner := bufio.NewScanner(f)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if scanner.Scan() {
				line := scanner.Text()
				if line != "" {
					fmt.Fprintf(w, "data: %s\n\n", line)
					if ok {
						flusher.Flush()
					}
				}
			} else {
				time.Sleep(500 * time.Millisecond)
				// Check if the file was rotated (inode changed or file is now smaller)
				if newFI, statErr := os.Stat(logPath); statErr == nil && fi != nil {
					if !os.SameFile(fi, newFI) {
						// Rotation detected — reopen and continue from the new file's start
						f.Close()
						if newF, newFInfo := openLog(); newF != nil {
							f = newF
							fi = newFInfo
						}
					}
				}
				scanner = bufio.NewScanner(f)
			}
		}
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","ts":%d}`, time.Now().Unix())
}

func handleEscalationAck(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Parse id from path: /api/escalations/{id}/ack
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[3] != "ack" {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		agents.AckEscalation(db, id)
		store.LogAudit(db, "dashboard", "ack-escalation", id, "acknowledged via dashboard")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
	}
}

// handleTasksSubroutes dispatches /api/tasks/{id} (GET → detail) and
// /api/tasks/{id}/retry (POST → retry).
func handleTasksSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// parts: ["api","tasks","{id}"] or ["api","tasks","{id}","retry"]
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}

		if len(parts) == 4 && parts[3] == "retry" && r.Method == http.MethodPost {
			var currentStatus string
			db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&currentStatus)
			if currentStatus == "Failed" || currentStatus == "Escalated" {
				store.ResetTask(db, id)
			}
			store.LogAudit(db, "dashboard", "retry", id, "retried via dashboard")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
			return
		}

		if len(parts) == 3 && r.Method == http.MethodGet {
			handleTaskDetail(db, id, w, r)
			return
		}

		http.NotFound(w, r)
	}
}

func handleTaskDetail(db *sql.DB, id int, w http.ResponseWriter, r *http.Request) {
	b, err := store.GetBounty(db, id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	goal, directive := splitGoalDirective(b.Payload)

	// If there's no [GOAL:] prefix but there's a parent, fetch the parent's payload as goal context.
	if goal == "" && b.ParentID > 0 {
		if parent, err2 := store.GetBounty(db, b.ParentID); err2 == nil {
			_, goal = splitGoalDirective(parent.Payload)
			if goal == "" {
				goal = parent.Payload
			}
		}
	}

	// Fetch the same fleet memories the Astromech would receive for this task.
	rawMems := store.GetFleetMemories(db, b.TargetRepo, b.Payload, 10)
	mems := make([]DashboardMemory, 0, len(rawMems))
	for _, m := range rawMems {
		mems = append(mems, DashboardMemory{
			Outcome:      m.Outcome,
			Summary:      m.Summary,
			FilesChanged: m.FilesChanged,
			CreatedAt:    m.CreatedAt,
		})
	}

	// Fetch attempt history (metadata only, no full Claude output).
	rawHist := store.GetTaskHistory(db, id)
	hist := make([]DashboardAttempt, 0, len(rawHist))
	for _, h := range rawHist {
		hist = append(hist, DashboardAttempt{
			Attempt:   h.Attempt,
			Agent:     h.Agent,
			Outcome:   h.Outcome,
			TokensIn:  h.TokensIn,
			TokensOut: h.TokensOut,
			CreatedAt: h.CreatedAt,
		})
	}

	// Fetch mail for this task.
	taskMail := fetchMailForTask(db, id)

	detail := DashboardTaskDetail{
		ID:            b.ID,
		Type:          b.Type,
		Status:        b.Status,
		Repo:          b.TargetRepo,
		Owner:         b.Owner,
		ParentID:      b.ParentID,
		ConvoyID:      b.ConvoyID,
		BranchName:    b.BranchName,
		RetryCount:    b.RetryCount,
		InfraFailures: b.InfraFailures,
		LockedAt:      b.Checkpoint, // reuse field for locked_at via raw query below
		ErrorLog:      "",
		BroaderGoal:   goal,
		Directive:     directive,
		Memories:      mems,
		History:       hist,
		Mail:          taskMail,
	}

	// Fetch locked_at and error_log which aren't in store.Bounty
	db.QueryRow(`SELECT IFNULL(locked_at,''), IFNULL(error_log,'') FROM BountyBoard WHERE id = ?`, id).
		Scan(&detail.LockedAt, &detail.ErrorLog)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(detail)
}

// fetchMailForTask returns all fleet mail associated with a task ID.
func fetchMailForTask(db *sql.DB, taskID int) []DashboardMail {
	rows, err := db.Query(
		`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, IFNULL(read_at,''), created_at
		 FROM Fleet_Mail WHERE task_id = ? ORDER BY created_at DESC`, taskID)
	if err != nil {
		return []DashboardMail{}
	}
	defer rows.Close()
	var out []DashboardMail
	for rows.Next() {
		var m DashboardMail
		rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &m.MessageType, &m.ReadAt, &m.CreatedAt)
		out = append(out, m)
	}
	if out == nil {
		out = []DashboardMail{}
	}
	return out
}

// handleMailList serves GET /api/mail — all fleet mail, newest first.
func handleMailList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		rows, err := db.Query(
			`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, IFNULL(read_at,''), created_at
			 FROM Fleet_Mail ORDER BY created_at DESC LIMIT 200`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var out []DashboardMail
		for rows.Next() {
			var m DashboardMail
			rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &m.MessageType, &m.ReadAt, &m.CreatedAt)
			out = append(out, m)
		}
		if out == nil {
			out = []DashboardMail{}
		}
		json.NewEncoder(w).Encode(out)
	}
}

// handleMailSubroutes dispatches /api/mail/{id}/read (POST).
func handleMailSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// parts: ["api","mail","{id}","read"]
		if len(parts) != 4 || parts[3] != "read" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		store.MarkMailRead(db, id)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, dashboardHTML)
}

func RunDashboard(db *sql.DB, port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", handleStatus(db))
	mux.HandleFunc("/api/tasks", handleTasks(db))
	mux.HandleFunc("/api/events", handleEvents("holonet.jsonl"))
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/api/escalations/", handleEscalationAck(db))
	mux.HandleFunc("/api/tasks/", handleTasksSubroutes(db))
	mux.HandleFunc("/api/mail", handleMailList(db))
	mux.HandleFunc("/api/mail/", handleMailSubroutes(db))
	mux.HandleFunc("/", handleRoot)

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Fleet Dashboard running at http://localhost%s\n", addr)
	fmt.Println("  /          — status page")
	fmt.Println("  /api/status — JSON status")
	fmt.Println("  /api/tasks  — JSON task list")
	fmt.Println("  /api/events — SSE telemetry stream")
	fmt.Println("Press Ctrl+C to stop.")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "Dashboard server error: %v\n", err)
		os.Exit(1)
	}
}

// dashboardHTML is the single-page dashboard served at /. Two tabs: Tasks and Mailbox.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Galactic Fleet Command</title>
<style>
  * { box-sizing: border-box; }
  body { font-family: monospace; background: #0a0a0a; color: #00ff88; margin: 0; padding: 20px; }
  h1 { color: #ffcc00; margin-bottom: 4px; }
  h2 { color: #88ccff; margin-top: 24px; margin-bottom: 8px; }
  .grid { display: flex; gap: 20px; flex-wrap: wrap; }
  .card { background: #111; border: 1px solid #333; padding: 16px; min-width: 160px; border-radius: 4px; }
  .card .val { font-size: 2em; color: #ffcc00; }
  .card .lbl { font-size: 0.8em; color: #888; margin-top: 4px; }
  .warn { color: #ff4444; }
  table { border-collapse: collapse; width: 100%; margin-top: 8px; }
  th { text-align: left; color: #88ccff; border-bottom: 1px solid #333; padding: 4px 8px; }
  td { padding: 4px 8px; border-bottom: 1px solid #1a1a1a; vertical-align: top; }
  tr.clickable { cursor: pointer; }
  tr.clickable:hover td { background: #161616; }
  .status-Locked, .status-UnderReview, .status-UnderCaptainReview { color: #ffcc00; }
  .status-Completed { color: #00ff88; }
  .status-Failed { color: #ff4444; }
  .status-Escalated { color: #ff8800; }
  .status-Pending { color: #aaaaaa; }
  .status-AwaitingCaptainReview { color: #ffaa44; }
  .status-AwaitingCouncilReview { color: #88ccff; }
  #estop-banner { display: none; background: #ff0000; color: white; padding: 8px 16px; font-weight: bold; margin-bottom: 16px; }
  #last-update { color: #555; font-size: 0.8em; margin-top: 8px; }

  /* Tabs */
  .tabs { display: flex; gap: 0; margin-top: 20px; border-bottom: 1px solid #333; }
  .tab { padding: 8px 20px; cursor: pointer; color: #666; border: 1px solid transparent; border-bottom: none; margin-bottom: -1px; }
  .tab.active { color: #ffcc00; border-color: #333; background: #111; }
  .tab-panel { display: none; }
  .tab-panel.active { display: block; }
  .tab-badge { display: inline-block; background: #ff4444; color: #fff; border-radius: 10px; padding: 0 6px; font-size: 0.75em; margin-left: 6px; }

  /* Slide-in panel (shared by task detail and mail detail) */
  .side-panel {
    display: none; position: fixed; right: 0; top: 0; width: 640px; height: 100vh;
    background: #0d0d0d; border-left: 1px solid #333; overflow-y: auto;
    padding: 20px 24px; z-index: 200;
  }
  .side-panel h3 { color: #88ccff; margin: 20px 0 8px; font-size: 0.85em; text-transform: uppercase; letter-spacing: 1px; }
  .side-panel pre {
    white-space: pre-wrap; word-break: break-word; background: #111; padding: 12px;
    border-radius: 4px; font-size: 0.82em; max-height: 220px; overflow-y: auto;
    border: 1px solid #2a2a2a; margin: 0; color: #ddd; line-height: 1.5;
  }
  .panel-header { display: flex; justify-content: space-between; align-items: flex-start; margin-bottom: 12px; }
  .panel-title { font-size: 1.1em; color: #ffcc00; font-weight: bold; }
  .panel-close { background: none; border: none; color: #555; font-size: 1.6em; cursor: pointer; line-height: 1; padding: 0; }
  .panel-close:hover { color: #aaa; }
  .meta-row { display: flex; flex-wrap: wrap; gap: 8px 20px; font-size: 0.82em; color: #888; margin-top: 6px; }
  .meta-row b { color: #ddd; }

  /* Memory cards */
  .mem-card { background: #111; border: 1px solid #2a2a2a; border-radius: 4px; padding: 10px 12px; margin-bottom: 8px; }
  .mem-card.success { border-left: 3px solid #00ff88; }
  .mem-card.failure { border-left: 3px solid #ff4444; }
  .mem-badge { font-size: 0.75em; font-weight: bold; margin-bottom: 4px; }
  .mem-badge.success { color: #00ff88; }
  .mem-badge.failure { color: #ff4444; }
  .mem-summary { font-size: 0.82em; color: #ccc; line-height: 1.4; }
  .mem-files { font-size: 0.75em; color: #555; margin-top: 4px; }

  /* Attempt / mail history tables inside panels */
  .inner-table { width: 100%; border-collapse: collapse; font-size: 0.82em; }
  .inner-table th { color: #88ccff; border-bottom: 1px solid #333; padding: 4px 6px; text-align: left; }
  .inner-table td { padding: 4px 6px; border-bottom: 1px solid #1a1a1a; color: #ccc; }
  .outcome-Completed { color: #00ff88; }
  .outcome-Failed { color: #ff4444; }

  /* Mail inbox */
  .mail-unread td { color: #fff; }
  .mail-unread .mail-subject { font-weight: bold; }
  .mail-type-alert { color: #ff4444; }
  .mail-type-info { color: #888; }
  .mail-type-feedback { color: #88ccff; }
  .mail-type-directive { color: #ffaa44; }

  /* Mail detail body */
  .mail-body { white-space: pre-wrap; word-break: break-word; font-size: 0.85em; color: #ccc; line-height: 1.6; background: #111; padding: 14px; border-radius: 4px; border: 1px solid #2a2a2a; }

  #overlay { display: none; position: fixed; inset: 0; background: rgba(0,0,0,0.5); z-index: 199; }
</style>
</head>
<body>
<h1>&#9733; Galactic Fleet Command Center</h1>
<div id="estop-banner">&#9888; E-STOP ACTIVE — All agents halted. Run: force resume</div>
<div id="last-update">Loading...</div>

<h2>System Status</h2>
<div class="grid" id="status-grid"></div>

<div class="tabs">
  <div class="tab active" onclick="switchTab('tasks')">Tasks</div>
  <div class="tab" onclick="switchTab('mail')">Mailbox <span class="tab-badge" id="mail-badge" style="display:none">0</span></div>
</div>

<!-- Tasks tab -->
<div class="tab-panel active" id="tab-tasks">
  <h2>Active Tasks</h2>
  <table>
    <thead><tr><th>ID</th><th>Status</th><th>Elapsed</th><th>Type</th><th>Repo</th><th>Owner</th><th>Directive</th><th></th></tr></thead>
    <tbody id="task-body"></tbody>
  </table>
</div>

<!-- Mailbox tab -->
<div class="tab-panel" id="tab-mail">
  <h2>Fleet Mailbox</h2>
  <table>
    <thead><tr><th></th><th>From</th><th>To</th><th>Subject</th><th>Task</th><th>Type</th><th>When</th></tr></thead>
    <tbody id="mail-body"></tbody>
  </table>
</div>

<!-- Shared overlay -->
<div id="overlay" onclick="closePanel()"></div>

<!-- Task detail panel -->
<div class="side-panel" id="task-panel">
  <div class="panel-header">
    <div>
      <div class="panel-title" id="tp-title"></div>
      <div class="meta-row" id="tp-meta"></div>
    </div>
    <button class="panel-close" onclick="closePanel()">&#x2715;</button>
  </div>

  <div id="tp-goal-section">
    <h3>Broader Goal</h3>
    <pre id="tp-goal"></pre>
  </div>

  <div>
    <h3>Current Directive</h3>
    <pre id="tp-directive"></pre>
  </div>

  <div id="tp-error-section" style="display:none">
    <h3 style="color:#ff4444">Error Log</h3>
    <pre id="tp-error" style="border-color:#ff4444;background:#110000;color:#ff9999"></pre>
  </div>

  <div id="tp-mail-section" style="display:none">
    <h3>Related Mail (<span id="tp-mail-count">0</span>)</h3>
    <div id="tp-mail"></div>
  </div>

  <div>
    <h3>Fleet Memories (<span id="tp-mem-count">0</span>)</h3>
    <div id="tp-memories"></div>
  </div>

  <div id="tp-hist-section" style="display:none">
    <h3>Attempt History</h3>
    <table class="inner-table">
      <thead><tr><th>#</th><th>Agent</th><th>Outcome</th><th>In</th><th>Out</th><th>When</th></tr></thead>
      <tbody id="tp-history"></tbody>
    </table>
  </div>
</div>

<!-- Mail detail panel -->
<div class="side-panel" id="mail-panel">
  <div class="panel-header">
    <div>
      <div class="panel-title" id="mp-subject"></div>
      <div class="meta-row" id="mp-meta"></div>
    </div>
    <button class="panel-close" onclick="closePanel()">&#x2715;</button>
  </div>
  <div class="mail-body" id="mp-body"></div>
  <div id="mp-task-link" style="display:none;margin-top:16px;font-size:0.85em">
    <span style="color:#888">Task: </span><a id="mp-task-anchor" href="#" style="color:#88ccff"></a>
  </div>
</div>

<script>
const RUNNING = new Set(['Locked','UnderCaptainReview','UnderReview']);

function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

// Strip [GOAL: ...]\n\n prefix, return just the directive text
function directive(payload) {
  if (!payload) return '';
  if (payload.startsWith('[GOAL: ')) {
    const end = payload.indexOf(']\n\n');
    if (end !== -1) return payload.slice(end + 3);
  }
  return payload;
}

function formatElapsed(ts) {
  if (!ts) return '';
  const t = ts.includes('T') ? ts : ts.replace(' ', 'T') + 'Z';
  const secs = Math.floor((Date.now() - new Date(t)) / 1000);
  if (secs < 0) return '';
  const m = Math.floor(secs / 60), s = secs % 60;
  return m > 0 ? m + 'm' + String(s).padStart(2,'0') + 's' : s + 's';
}

function switchTab(name) {
  document.querySelectorAll('.tab').forEach((el,i) => el.classList.toggle('active', ['tasks','mail'][i] === name));
  document.querySelectorAll('.tab-panel').forEach(el => el.classList.remove('active'));
  document.getElementById('tab-' + name).classList.add('active');
  if (name === 'mail') loadMail();
}

setInterval(() => {
  document.querySelectorAll('[data-locked-at]').forEach(el => {
    el.textContent = formatElapsed(el.dataset.lockedAt);
  });
}, 1000);

// ── Task detail ──────────────────────────────────────────────────────────────

async function openTask(id) {
  try {
    const res = await fetch('/api/tasks/' + id);
    if (!res.ok) return;
    const d = await res.json();

    document.getElementById('tp-title').textContent = 'Task #' + d.id + ' — ' + d.type;
    document.getElementById('tp-meta').innerHTML = [
      '<span><b class="status-' + d.status + '">' + esc(d.status) + '</b></span>',
      d.repo   ? '<span>repo: <b>' + esc(d.repo) + '</b></span>' : '',
      d.owner  ? '<span>owner: <b>' + esc(d.owner) + '</b></span>' : '',
      d.branch_name ? '<span>branch: <b>' + esc(d.branch_name) + '</b></span>' : '',
      d.convoy_id   ? '<span>convoy: <b>#' + d.convoy_id + '</b></span>' : '',
      '<span>retries: <b>' + d.retry_count + '</b></span>',
      '<span>infra: <b>' + d.infra_failures + '/5</b></span>',
    ].filter(Boolean).join('');

    const goalSec = document.getElementById('tp-goal-section');
    if (d.broader_goal) {
      document.getElementById('tp-goal').textContent = d.broader_goal;
      goalSec.style.display = '';
    } else {
      goalSec.style.display = 'none';
    }
    document.getElementById('tp-directive').textContent = d.directive || '';

    const errSec = document.getElementById('tp-error-section');
    if (d.error_log) {
      document.getElementById('tp-error').textContent = d.error_log;
      errSec.style.display = '';
    } else {
      errSec.style.display = 'none';
    }

    // Related mail
    const mails = d.mail || [];
    const mailSec = document.getElementById('tp-mail-section');
    document.getElementById('tp-mail-count').textContent = mails.length;
    if (mails.length > 0) {
      document.getElementById('tp-mail').innerHTML = mails.map(m =>
        '<div style="background:#111;border:1px solid #2a2a2a;border-radius:4px;padding:10px 12px;margin-bottom:8px">' +
        '<div style="font-size:0.75em;color:#666;margin-bottom:4px">' + esc(m.from_agent) + ' → ' + esc(m.to_agent) + ' &nbsp;·&nbsp; ' + esc((m.created_at||'').substring(0,16)) + '</div>' +
        '<div style="font-weight:bold;color:#ddd;margin-bottom:6px">' + esc(m.subject) + '</div>' +
        '<div style="font-size:0.82em;color:#aaa;white-space:pre-wrap">' + esc(m.body.substring(0,300)) + (m.body.length>300?'…':'') + '</div>' +
        '</div>'
      ).join('');
      mailSec.style.display = '';
    } else {
      mailSec.style.display = 'none';
    }

    // Fleet memories
    const mems = d.memories || [];
    document.getElementById('tp-mem-count').textContent = mems.length;
    document.getElementById('tp-memories').innerHTML = mems.length === 0
      ? '<div style="color:#444;font-size:0.85em">No memories for this repo yet.</div>'
      : mems.map(m => {
          const ok = m.outcome === 'success';
          return '<div class="mem-card ' + (ok?'success':'failure') + '">' +
            '<div class="mem-badge ' + (ok?'success':'failure') + '">' + (ok?'✓ SUCCESS':'✗ FAILURE') + '</div>' +
            '<div class="mem-summary">' + esc(m.summary.substring(0,200)) + (m.summary.length>200?'…':'') + '</div>' +
            (m.files_changed ? '<div class="mem-files">Files: ' + esc(m.files_changed) + '</div>' : '') +
            '</div>';
        }).join('');

    // Attempt history
    const hist = d.history || [];
    const histSec = document.getElementById('tp-hist-section');
    if (hist.length > 0) {
      document.getElementById('tp-history').innerHTML = hist.map(h =>
        '<tr><td>' + h.attempt + '</td><td>' + esc(h.agent) + '</td>' +
        '<td class="outcome-' + esc(h.outcome) + '">' + esc(h.outcome) + '</td>' +
        '<td>' + (h.tokens_in||0).toLocaleString() + '</td>' +
        '<td>' + (h.tokens_out||0).toLocaleString() + '</td>' +
        '<td style="color:#555">' + esc((h.created_at||'').substring(0,16)) + '</td></tr>'
      ).join('');
      histSec.style.display = '';
    } else {
      histSec.style.display = 'none';
    }

    document.getElementById('mail-panel').style.display = 'none';
    document.getElementById('task-panel').style.display = 'block';
    document.getElementById('overlay').style.display = 'block';
  } catch(e) { console.error(e); }
}

// ── Mail inbox ───────────────────────────────────────────────────────────────

let mailCache = [];

async function loadMail() {
  try {
    const res = await fetch('/api/mail');
    mailCache = await res.json();
    renderMail();
  } catch(e) { console.error(e); }
}

function renderMail() {
  const unread = mailCache.filter(m => !m.read_at).length;
  const badge = document.getElementById('mail-badge');
  badge.textContent = unread;
  badge.style.display = unread > 0 ? '' : 'none';

  document.getElementById('mail-body').innerHTML = (mailCache||[]).map(m => {
    const isUnread = !m.read_at;
    const typeClass = 'mail-type-' + (m.message_type||'info');
    return '<tr class="clickable ' + (isUnread?'mail-unread':'') + '" onclick="openMail(' + m.id + ')">' +
      '<td style="width:8px;padding-right:0">' + (isUnread ? '<span style="color:#ffcc00">●</span>' : '') + '</td>' +
      '<td style="color:#aaa">' + esc(m.from_agent) + '</td>' +
      '<td style="color:#666">' + esc(m.to_agent) + '</td>' +
      '<td class="mail-subject">' + esc(m.subject) + '</td>' +
      '<td style="color:#555">' + (m.task_id ? '<a href="#" onclick="event.stopPropagation();openTask(' + m.task_id + ')" style="color:#88ccff">#' + m.task_id + '</a>' : '') + '</td>' +
      '<td class="' + typeClass + '">' + esc(m.message_type||'info') + '</td>' +
      '<td style="color:#555;white-space:nowrap">' + esc((m.created_at||'').substring(0,16)) + '</td>' +
      '</tr>';
  }).join('');
}

async function openMail(id) {
  const m = mailCache.find(x => x.id === id);
  if (!m) return;

  document.getElementById('mp-subject').textContent = m.subject;
  document.getElementById('mp-meta').innerHTML = [
    '<span>from: <b>' + esc(m.from_agent) + '</b></span>',
    '<span>to: <b>' + esc(m.to_agent) + '</b></span>',
    '<span>' + esc(m.message_type) + '</span>',
    '<span style="color:#555">' + esc((m.created_at||'').substring(0,16)) + '</span>',
  ].join('');
  document.getElementById('mp-body').textContent = m.body;

  const taskLink = document.getElementById('mp-task-link');
  if (m.task_id) {
    document.getElementById('mp-task-anchor').textContent = 'Task #' + m.task_id;
    document.getElementById('mp-task-anchor').onclick = (e) => { e.preventDefault(); openTask(m.task_id); };
    taskLink.style.display = '';
  } else {
    taskLink.style.display = 'none';
  }

  // Mark as read
  if (!m.read_at) {
    await fetch('/api/mail/' + id + '/read', {method:'POST'});
    m.read_at = new Date().toISOString();
    renderMail();
  }

  document.getElementById('task-panel').style.display = 'none';
  document.getElementById('mail-panel').style.display = 'block';
  document.getElementById('overlay').style.display = 'block';
}

function closePanel() {
  document.getElementById('task-panel').style.display = 'none';
  document.getElementById('mail-panel').style.display = 'none';
  document.getElementById('overlay').style.display = 'none';
}

document.addEventListener('keydown', e => { if (e.key === 'Escape') closePanel(); });

// ── Main refresh ─────────────────────────────────────────────────────────────

async function refresh() {
  try {
    const [statusRes, tasksRes] = await Promise.all([fetch('/api/status'), fetch('/api/tasks')]);
    const s = await statusRes.json();
    const tasks = await tasksRes.json();

    document.getElementById('estop-banner').style.display = s.estopped ? 'block' : 'none';
    document.getElementById('last-update').textContent = 'Last updated: ' + new Date(s.timestamp).toLocaleTimeString();

    const t = s.tasks || {};
    const active = (t.Locked||0)+(t.AwaitingCaptainReview||0)+(t.UnderCaptainReview||0)+(t.UnderReview||0)+(t.AwaitingCouncilReview||0);
    document.getElementById('status-grid').innerHTML = [
      ['Pending', t.Pending||0, ''],
      ['Active', active, active>0?'':''],
      ['Completed', t.Completed||0, ''],
      ['Failed', t.Failed||0, t.Failed>0?'warn':''],
      ['Escalated', t.Escalated||0, t.Escalated>0?'warn':''],
      ['Escalations', s.open_escalations, s.high_escalations>0?'warn':''],
      ['Convoys', s.active_convoys, ''],
      ['Unread Mail', s.unread_mail, s.unread_mail>0?'':''],
    ].map(([lbl, val, cls]) =>
      '<div class="card"><div class="val ' + cls + '">' + val + '</div><div class="lbl">' + lbl + '</div></div>'
    ).join('');

    // Update unread badge without a full mail reload
    const mailBadge = document.getElementById('mail-badge');
    if (s.unread_mail > 0) { mailBadge.textContent = s.unread_mail; mailBadge.style.display = ''; }
    else { mailBadge.style.display = 'none'; }

    const nonFinal = (tasks||[]).filter(t => t.status !== 'Completed');
    document.getElementById('task-body').innerHTML = nonFinal.slice(0, 50).map(t => {
      const isRunning = RUNNING.has(t.status);
      const elCell = isRunning && t.locked_at
        ? '<td data-locked-at="' + t.locked_at + '" style="color:#ffcc00;font-variant-numeric:tabular-nums">' + formatElapsed(t.locked_at) + '</td>'
        : '<td style="color:#444">—</td>';
      const taskText = directive(t.payload || '').replace(/\n/g,' ').substring(0, 80);
      let action = '';
      if (t.status === 'Failed' || t.status === 'Escalated') {
        action = '<button onclick="event.stopPropagation();retryTask(' + t.id + ')" style="font-size:0.8em;cursor:pointer">&#x21BA;</button>';
      }
      return '<tr class="clickable" onclick="openTask(' + t.id + ')">' +
        '<td>' + t.id + '</td>' +
        '<td class="status-' + t.status + '">' + t.status + '</td>' +
        elCell +
        '<td>' + esc(t.type) + '</td>' +
        '<td>' + esc(t.repo||'') + '</td>' +
        '<td>' + esc(t.owner||'—') + '</td>' +
        '<td style="color:#bbb">' + esc(taskText) + '</td>' +
        '<td>' + action + '</td></tr>';
    }).join('');
  } catch(e) {
    document.getElementById('last-update').textContent = 'Error: ' + e.message;
  }
}

async function retryTask(id) {
  await fetch('/api/tasks/' + id + '/retry', {method:'POST'});
  refresh();
}

refresh();
setInterval(refresh, 3000);
</script>
</body>
</html>`

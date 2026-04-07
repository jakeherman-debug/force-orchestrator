package dashboard

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/agents"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/util"
)

func jsonCORS(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","ts":%d}`, time.Now().Unix())
}

// ── Status ────────────────────────────────────────────────────────────────────

func handleStatus(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
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
		unread, _ := store.MailStats(db, "", "")
		s.UnreadMail = unread
		s.TotalSpendDollars = store.TotalSpendDollars(db)

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

// ── Tasks list ────────────────────────────────────────────────────────────────

func handleTasks(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		statusFilter := r.URL.Query().Get("status")
		convoyIDStr := r.URL.Query().Get("convoy_id")

		allowedSortBy := map[string]bool{"status": true, "type": true, "created_at": true, "priority": true, "id": true}
		sortBy := r.URL.Query().Get("sort_by")
		if !allowedSortBy[sortBy] {
			sortBy = "id"
		}
		sortDir := r.URL.Query().Get("sort_dir")
		if sortDir != "asc" && sortDir != "desc" {
			sortDir = "desc"
		}

		query := `SELECT id, type, status, target_repo, owner, retry_count, convoy_id,
			payload, IFNULL(error_log,''), IFNULL(locked_at,''), COALESCE(priority,0),
			COALESCE(CAST((julianday('now') - julianday(NULLIF(locked_at,''))) * 86400 AS INTEGER), 0),
			(SELECT GROUP_CONCAT(td.depends_on) FROM TaskDependencies td
			 JOIN BountyBoard dep ON dep.id = td.depends_on
			 WHERE td.task_id = BountyBoard.id AND dep.status != 'Completed'),
			(SELECT COALESCE(SUM(tokens_in),0) FROM TaskHistory WHERE task_id = BountyBoard.id),
			(SELECT COALESCE(SUM(tokens_out),0) FROM TaskHistory WHERE task_id = BountyBoard.id),
			IFNULL(BountyBoard.created_at,'')
			FROM BountyBoard`
		args := []any{}
		var conditions []string
		if statusFilter != "" {
			statuses := strings.Split(statusFilter, ",")
			placeholders := make([]string, len(statuses))
			for i, s := range statuses {
				placeholders[i] = "?"
				args = append(args, strings.TrimSpace(s))
			}
			conditions = append(conditions, `status IN (`+strings.Join(placeholders, ",")+`)`)
		}
		if convoyIDStr != "" {
			convoyID, err := strconv.Atoi(convoyIDStr)
			if err == nil && convoyID != 0 {
				conditions = append(conditions, `convoy_id = ?`)
				args = append(args, convoyID)
			}
		}
		if len(conditions) > 0 {
			query += ` WHERE ` + strings.Join(conditions, ` AND `)
		}
		query += ` ORDER BY ` + sortBy + ` ` + sortDir + ` LIMIT 500`

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var tasks []DashboardTask
		for rows.Next() {
			var t DashboardTask
			var activeBlockersStr sql.NullString
			var tokensIn, tokensOut int
			rows.Scan(&t.ID, &t.Type, &t.Status, &t.Repo, &t.Owner, &t.RetryCount,
				&t.ConvoyID, &t.Payload, &t.ErrorLog, &t.LockedAt, &t.Priority,
				&t.RuntimeSeconds, &activeBlockersStr, &tokensIn, &tokensOut, &t.CreatedAt)
			t.BlockedBy = parseBlockers(activeBlockersStr.String)
			t.CostDollars = store.TaskCostDollars(tokensIn, tokensOut)
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

// ── Task sub-routes ───────────────────────────────────────────────────────────
// GET  /api/tasks/{id}          → detail
// POST /api/tasks/{id}/retry    → retry (Failed/Escalated only)
// POST /api/tasks/{id}/reset    → reset any non-Completed task to Pending
// POST /api/tasks/{id}/cancel   → cancel
// POST /api/tasks/{id}/approve  → operator approve + merge
// POST /api/tasks/{id}/reject   → operator reject with reason (body: {"reason":"..."})

func handleTasksSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}

		if len(parts) == 3 && r.Method == http.MethodGet {
			serveTaskDetail(db, id, w)
			return
		}

		if len(parts) == 4 && r.Method == http.MethodPost {
			switch parts[3] {
			case "retry":
				var currentStatus string
				db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&currentStatus)
				if currentStatus == "Failed" || currentStatus == "Escalated" {
					store.ResetTask(db, id)
				}
				store.LogAudit(db, "dashboard", "retry", id, "retried via dashboard")
				fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)

			case "reset":
				store.ResetTask(db, id)
				store.LogAudit(db, "dashboard", "reset", id, "reset via dashboard")
				fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)

			case "cancel":
				var currentStatus string
				db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&currentStatus)
				if currentStatus == "Completed" {
					http.Error(w, `{"error":"cannot cancel a completed task"}`, http.StatusConflict)
					return
				}
				var cancelBody struct {
					RequeueType string `json:"requeue_type"`
				}
				json.NewDecoder(r.Body).Decode(&cancelBody)
				store.CancelTask(db, id, "Cancelled via dashboard")
				store.LogAudit(db, "dashboard", "cancel", id, "cancelled via dashboard")
				if cancelBody.RequeueType != "" {
					var originalPayload string
					db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, id).Scan(&originalPayload)
					newID := store.AddBounty(db, 0, cancelBody.RequeueType, originalPayload)
					fmt.Fprintf(w, `{"ok":true,"id":%d,"requeued_id":%d}`, id, newID)
				} else {
					fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
				}

			case "approve":
				if err := approveTask(db, id, w); err != nil {
					return
				}
				fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)

			case "reject":
				var body rejectBody
				json.NewDecoder(r.Body).Decode(&body)
				if strings.TrimSpace(body.Reason) == "" {
					http.Error(w, `{"error":"reason is required"}`, http.StatusBadRequest)
					return
				}
				rejectTask(db, id, body.Reason, w)

			default:
				http.NotFound(w, r)
			}
			return
		}
		http.NotFound(w, r)
	}
}

// approveTask mirrors cmdApproveTask, adapted for HTTP (no os.Exit).
func approveTask(db *sql.DB, id int, w http.ResponseWriter) error {
	b, err := store.GetBounty(db, id)
	if err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return err
	}
	reviewable := map[string]bool{
		"AwaitingCouncilReview":  true,
		"UnderReview":            true,
		"AwaitingCaptainReview":  true,
		"UnderCaptainReview":     true,
	}
	if !reviewable[b.Status] {
		http.Error(w,
			fmt.Sprintf(`{"error":"task is not awaiting review (status: %s)"}`, b.Status),
			http.StatusConflict)
		return fmt.Errorf("not reviewable")
	}
	repoPath := store.GetRepoPath(db, b.TargetRepo)
	if repoPath == "" {
		http.Error(w, `{"error":"unknown repository"}`, http.StatusUnprocessableEntity)
		return fmt.Errorf("unknown repo")
	}
	branchName := b.BranchName
	if branchName == "" {
		branchName = fmt.Sprintf("agent/task-%d", id)
	}
	worktreeDir := igit.ResolveWorktreeDir(db, branchName, repoPath, id, agents.BranchAgentName)
	diff := igit.GetDiff(repoPath, branchName)
	if mergeErr := igit.MergeAndCleanup(repoPath, branchName, worktreeDir); mergeErr != nil {
		http.Error(w,
			fmt.Sprintf(`{"error":"merge failed: %s"}`, strings.ReplaceAll(mergeErr.Error(), `"`, `'`)),
			http.StatusInternalServerError)
		return mergeErr
	}
	store.UpdateBountyStatus(db, id, "Completed")
	store.UnblockDependentsOf(db, id)
	if diff != "" {
		files := strings.Join(igit.ExtractDiffFiles(diff), ", ")
		store.StoreFleetMemory(db, b.TargetRepo, b.ID, "success",
			fmt.Sprintf("Task: %s", util.TruncateStr(b.Payload, 400)), files)
	}
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		EventType: "operator_approved",
		Payload:   map[string]any{"task_id": id},
	})
	store.LogAudit(db, "dashboard", "approve", id, "approved and merged via dashboard")
	return nil
}

// rejectTask mirrors cmdRejectTask, adapted for HTTP.
func rejectTask(db *sql.DB, id int, reason string, w http.ResponseWriter) {
	b, err := store.GetBounty(db, id)
	if err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}
	retryCount := store.IncrementRetryCount(db, id)
	if retryCount >= agents.MaxRetries {
		store.FailBounty(db, id, fmt.Sprintf("Operator rejected (final): %s", reason))
	} else {
		newPayload := fmt.Sprintf("%s\n\nOPERATOR FEEDBACK (attempt %d/%d): %s",
			b.Payload, retryCount, agents.MaxRetries, reason)
		store.ReturnTaskForRework(db, id, newPayload)
	}
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		EventType: "operator_rejected",
		Payload:   map[string]any{"task_id": id, "reason": reason},
	})
	store.LogAudit(db, "dashboard", "reject", id, reason)
	fmt.Fprintf(w, `{"ok":true,"id":%d,"attempt":%d,"max":%d}`, id, retryCount, agents.MaxRetries)
}

func serveTaskDetail(db *sql.DB, id int, w http.ResponseWriter) {
	b, err := store.GetBounty(db, id)
	if err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}
	goal, directive := splitGoalDirective(b.Payload)
	if goal == "" && b.ParentID > 0 {
		if parent, err2 := store.GetBounty(db, b.ParentID); err2 == nil {
			_, goal = splitGoalDirective(parent.Payload)
			if goal == "" {
				goal = parent.Payload
			}
		}
	}

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

	rawHist := store.GetTaskHistory(db, id)
	hist := make([]DashboardAttempt, 0, len(rawHist))
	var totalTokensIn, totalTokensOut int
	for _, h := range rawHist {
		hist = append(hist, DashboardAttempt{
			Attempt:   h.Attempt,
			Agent:     h.Agent,
			Outcome:   h.Outcome,
			TokensIn:  h.TokensIn,
			TokensOut: h.TokensOut,
			CreatedAt: h.CreatedAt,
		})
		totalTokensIn += h.TokensIn
		totalTokensOut += h.TokensOut
	}

	blockers := store.GetDependencies(db, id)
	if blockers == nil {
		blockers = []int{}
	}

	var runtimeSecs int
	db.QueryRow(`SELECT COALESCE(CAST((julianday('now') - julianday(NULLIF(locked_at,''))) * 86400 AS INTEGER), 0) FROM BountyBoard WHERE id = ?`, id).
		Scan(&runtimeSecs)

	detail := DashboardTaskDetail{
		ID:             b.ID,
		Type:           b.Type,
		Status:         b.Status,
		Repo:           b.TargetRepo,
		Owner:          b.Owner,
		ParentID:       b.ParentID,
		ConvoyID:       b.ConvoyID,
		BranchName:     b.BranchName,
		RetryCount:     b.RetryCount,
		InfraFailures:  b.InfraFailures,
		Priority:       b.Priority,
		BroaderGoal:    goal,
		Directive:      directive,
		RuntimeSeconds: runtimeSecs,
		BlockedBy:      blockers,
		CostDollars:    store.TaskCostDollars(totalTokensIn, totalTokensOut),
		Memories:       mems,
		History:        hist,
		Mail:           fetchMailForTask(db, id),
	}
	db.QueryRow(`SELECT IFNULL(locked_at,''), IFNULL(error_log,'') FROM BountyBoard WHERE id = ?`, id).
		Scan(&detail.LockedAt, &detail.ErrorLog)

	json.NewEncoder(w).Encode(detail)
}

func parseBlockers(s string) []int {
	if s == "" {
		return []int{}
	}
	parts := strings.Split(s, ",")
	ids := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			ids = append(ids, n)
		}
	}
	return ids
}

func splitGoalDirective(payload string) (goal, directive string) {
	if strings.HasPrefix(payload, "[GOAL: ") {
		if end := strings.Index(payload, "]\n\n"); end != -1 {
			return payload[7:end], payload[end+3:]
		}
	}
	return "", payload
}

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
		rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body,
			&m.TaskID, &m.MessageType, &m.ReadAt, &m.CreatedAt)
		out = append(out, m)
	}
	if out == nil {
		out = []DashboardMail{}
	}
	return out
}

// ── Control ───────────────────────────────────────────────────────────────────

func handleEstop(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		agents.SetEstop(db, true)
		telemetry.EmitEvent(telemetry.EventEstop(true))
		store.LogAudit(db, "dashboard", "estop", 0, "emergency stop via dashboard")
		fmt.Fprintf(w, `{"ok":true}`)
	}
}

func handleResume(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		agents.SetEstop(db, false)
		telemetry.EmitEvent(telemetry.EventEstop(false))
		store.LogAudit(db, "dashboard", "resume", 0, "e-stop cleared via dashboard")
		fmt.Fprintf(w, `{"ok":true}`)
	}
}

// ── Escalations ───────────────────────────────────────────────────────────────

func handleEscalationList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		raw := agents.ListEscalations(db, r.URL.Query().Get("status"))
		out := make([]DashboardEscalation, 0, len(raw))
		for _, e := range raw {
			out = append(out, DashboardEscalation{
				ID:             e.ID,
				TaskID:         e.TaskID,
				Severity:       string(e.Severity),
				Message:        e.Message,
				Status:         e.Status,
				CreatedAt:      e.CreatedAt,
				AcknowledgedAt: e.AcknowledgedAt,
			})
		}
		json.NewEncoder(w).Encode(out)
	}
}

// POST /api/escalations/{id}/ack|close|requeue
func handleEscalationsSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		switch parts[3] {
		case "ack":
			agents.AckEscalation(db, id)
			store.LogAudit(db, "dashboard", "ack-escalation", id, "acknowledged via dashboard")
		case "close":
			agents.CloseEscalation(db, id, false)
			store.LogAudit(db, "dashboard", "close-escalation", id, "closed via dashboard")
		case "requeue":
			agents.CloseEscalation(db, id, true)
			store.LogAudit(db, "dashboard", "requeue-escalation", id, "closed and requeued via dashboard")
		default:
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
	}
}

// ── Convoys ───────────────────────────────────────────────────────────────────

func handleConvoys(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		raw := store.ListConvoys(db)
		out := make([]DashboardConvoy, 0, len(raw))
		for _, c := range raw {
			completed, total := store.ConvoyProgress(db, c.ID)
			var planned int
			db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND status = 'Planned'`, c.ID).Scan(&planned)
			out = append(out, DashboardConvoy{
				ID:         c.ID,
				Name:       c.Name,
				Status:     c.Status,
				CreatedAt:  c.CreatedAt,
				Completed:  completed,
				Total:      total,
				HasPlanned: planned > 0,
			})
		}
		json.NewEncoder(w).Encode(out)
	}
}

// POST /api/convoys/{id}/approve
func handleConvoysSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		switch parts[3] {
		case "approve":
			n := store.ApproveConvoyTasks(db, id)
			store.LogAudit(db, "dashboard", "convoy-approve", id,
				fmt.Sprintf("activated %d planned task(s) via dashboard", n))
			fmt.Fprintf(w, `{"ok":true,"id":%d,"activated":%d}`, id, n)
		default:
			http.NotFound(w, r)
		}
	}
}

// ── Agents ────────────────────────────────────────────────────────────────────

func handleAgents(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		rows, err := db.Query(`
			SELECT a.agent_name, a.repo,
			       IFNULL(b.id, 0), IFNULL(b.status,''), IFNULL(b.locked_at,'')
			FROM Agents a
			LEFT JOIN BountyBoard b ON b.owner = a.agent_name
			    AND b.status IN ('Locked','UnderReview','UnderCaptainReview')
			ORDER BY a.agent_name`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var out []DashboardAgent
		for rows.Next() {
			var ag DashboardAgent
			rows.Scan(&ag.AgentName, &ag.Repo, &ag.CurrentTaskID, &ag.TaskStatus, &ag.LockedAt)
			out = append(out, ag)
		}
		if out == nil {
			out = []DashboardAgent{}
		}
		json.NewEncoder(w).Encode(out)
	}
}

// ── Repos ─────────────────────────────────────────────────────────────────────

func handleRepos(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		rows, err := db.Query(`SELECT name FROM Repositories ORDER BY name`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		if names == nil {
			names = []string{}
		}
		json.NewEncoder(w).Encode(names)
	}
}

// ── Mail ──────────────────────────────────────────────────────────────────────

func handleMailList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
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
			rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body,
				&m.TaskID, &m.MessageType, &m.ReadAt, &m.CreatedAt)
			out = append(out, m)
		}
		if out == nil {
			out = []DashboardMail{}
		}
		json.NewEncoder(w).Encode(out)
	}
}

// POST /api/mail/{id}/read
// POST /api/mail/read-all
func handleMailSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// POST /api/mail/read-all
		if len(parts) == 3 && parts[2] == "read-all" && r.Method == http.MethodPost {
			res, err := db.Exec(`UPDATE Fleet_Mail SET read_at = datetime('now') WHERE read_at = ''`)
			if err != nil {
				http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
				return
			}
			n, _ := res.RowsAffected()
			fmt.Fprintf(w, `{"ok":true,"marked":%d}`, n)
			return
		}
		if len(parts) != 4 || parts[3] != "read" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var id int
		fmt.Sscanf(parts[2], "%d", &id)
		if id <= 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		store.MarkMailRead(db, id)
		fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
	}
}

// ── Knowledge base (Fleet Memory) ────────────────────────────────────────────

// GET  /api/memories?repo=&outcome=&q=&limit=N
// DELETE /api/memories/{id}
func handleMemories(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)

		// DELETE /api/memories/{id}
		if r.Method == http.MethodDelete {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if len(parts) < 3 {
				http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
				return
			}
			var id int
			fmt.Sscanf(parts[2], "%d", &id)
			if id <= 0 {
				http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
				return
			}
			store.DeleteFleetMemory(db, id)
			store.LogAudit(db, "dashboard", "delete-memory", id, "deleted via dashboard")
			fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
			return
		}

		// GET /api/memories
		repoFilter    := r.URL.Query().Get("repo")
		outcomeFilter := r.URL.Query().Get("outcome")
		search        := r.URL.Query().Get("q")
		limit         := 200
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			fmt.Sscanf(lStr, "%d", &limit)
		}

		query := `SELECT id, repo, task_id, outcome, summary, IFNULL(files_changed,''), created_at
		          FROM FleetMemory WHERE 1=1`
		args := []any{}
		if repoFilter != "" {
			query += ` AND repo = ?`
			args = append(args, repoFilter)
		}
		if outcomeFilter != "" {
			query += ` AND outcome = ?`
			args = append(args, outcomeFilter)
		}
		if search != "" {
			query += ` AND (summary LIKE ? OR files_changed LIKE ?)`
			like := "%" + search + "%"
			args = append(args, like, like)
		}
		query += ` ORDER BY id DESC LIMIT ?`
		args = append(args, limit)

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		type MemoryRow struct {
			ID           int    `json:"id"`
			Repo         string `json:"repo"`
			TaskID       int    `json:"task_id"`
			Outcome      string `json:"outcome"`
			Summary      string `json:"summary"`
			FilesChanged string `json:"files_changed"`
			CreatedAt    string `json:"created_at"`
		}
		var out []MemoryRow
		for rows.Next() {
			var m MemoryRow
			rows.Scan(&m.ID, &m.Repo, &m.TaskID, &m.Outcome, &m.Summary, &m.FilesChanged, &m.CreatedAt)
			out = append(out, m)
		}
		if out == nil {
			out = []MemoryRow{}
		}
		json.NewEncoder(w).Encode(out)
	}
}

// newUUID returns a random UUID v4 string.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// insertTypedTask inserts an Investigate or Audit task, optionally scoped to a repo.
// idempotencyKey is stored at insert time so duplicate-check queries can find the row.
func insertTypedTask(db *sql.DB, taskType, repo, payload string, priority int, idempotencyKey string) (int, error) {
	if repo != "" && store.GetRepoPath(db, repo) == "" {
		return 0, fmt.Errorf("unknown repo: %s", repo)
	}
	res, err := db.Exec(
		`INSERT INTO BountyBoard (target_repo, type, status, payload, priority, idempotency_key, created_at)
		 VALUES (?, ?, 'Pending', ?, ?, ?, datetime('now'))`,
		repo, taskType, payload, priority, idempotencyKey)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// ── Add task ──────────────────────────────────────────────────────────────────

func handleAdd(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body addTaskBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Payload) == "" {
			http.Error(w, `{"error":"payload is required"}`, http.StatusBadRequest)
			return
		}

		// Idempotency check — if a task with the same key was created within 60 s, return it.
		if body.IdempotencyKey != "" {
			var existingID int
			err := db.QueryRow(
				`SELECT id FROM BountyBoard WHERE idempotency_key = ? AND created_at > datetime('now', '-60 seconds') LIMIT 1`,
				body.IdempotencyKey).Scan(&existingID)
			if err == nil {
				fmt.Fprintf(w, `{"ok":true,"id":%d,"duplicate":true}`, existingID)
				return
			}
		}

		// Auto type: insert immediately as Classifying so the UI is not blocked.
		if body.Type == "" || strings.EqualFold(body.Type, "auto") {
			key := body.IdempotencyKey
			if key == "" {
				key = newUUID()
			}
			newID, err := store.AddBountyClassifying(db, body.Repo, body.Payload, body.Priority, key)
			if err != nil {
				http.Error(w, `{"error":"failed to insert task"}`, http.StatusInternalServerError)
				return
			}
			store.LogAudit(db, "dashboard", "add-task", newID, "queued Auto (Classifying) via dashboard")
			fmt.Fprintf(w, `{"ok":true,"id":%d}`, newID)
			return
		}

		// Typed tasks — include idempotency_key in the INSERT so the dedup check is meaningful.
		var newID int
		switch body.Type {
		case "Feature":
			res, err := db.Exec(
				`INSERT INTO BountyBoard (parent_id, type, status, payload, priority, idempotency_key, created_at)
				 VALUES (0, 'Feature', 'Pending', ?, ?, ?, datetime('now'))`,
				body.Payload, body.Priority, body.IdempotencyKey)
			if err != nil {
				http.Error(w, `{"error":"failed to insert task"}`, http.StatusInternalServerError)
				return
			}
			id, _ := res.LastInsertId()
			newID = int(id)
		case "CodeEdit":
			http.Error(w, `{"error":"CodeEdit is no longer a valid direct task type. Use Feature instead — all code changes flow through Commander and Chancellor for conflict review."}`, http.StatusBadRequest)
			return
		case "Investigate":
			var err error
			newID, err = insertTypedTask(db, "Investigate", body.Repo, body.Payload, body.Priority, body.IdempotencyKey)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusUnprocessableEntity)
				return
			}
		case "Audit":
			var err error
			newID, err = insertTypedTask(db, "Audit", body.Repo, body.Payload, body.Priority, body.IdempotencyKey)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusUnprocessableEntity)
				return
			}
		default:
			http.Error(w, `{"error":"type must be Feature, Investigate, or Audit"}`, http.StatusBadRequest)
			return
		}
		store.LogAudit(db, "dashboard", "add-task", newID,
			fmt.Sprintf("queued %s via dashboard", body.Type))
		fmt.Fprintf(w, `{"ok":true,"id":%d}`, newID)
	}
}

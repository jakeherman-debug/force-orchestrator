package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ── BountyBoard ───────────────────────────────────────────────────────────────

func GetBounty(db *sql.DB, id int) (*Bounty, error) {
	var b Bounty
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, type, status, payload,
		       owner, retry_count, infra_failures, convoy_id, checkpoint, branch_name,
		       priority, IFNULL(task_timeout,0)
		FROM BountyBoard WHERE id = ?`, id).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Type, &b.Status,
			&b.Payload, &b.Owner, &b.RetryCount, &b.InfraFailures, &b.ConvoyID,
			&b.Checkpoint, &b.BranchName, &b.Priority, &b.TaskTimeout)
	return &b, err
}

// ClaimBounty atomically claims the next available task using optimistic locking.
// Higher-priority tasks (priority DESC) are claimed first; ties broken by id ASC (FIFO).
// A task is claimable only when all its dependencies (in TaskDependencies) are Completed.
func ClaimBounty(db *sql.DB, taskType string, agentName string) (*Bounty, bool) {
	var b Bounty
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, type, status, payload, convoy_id, checkpoint,
		       priority, IFNULL(task_timeout,0)
		FROM BountyBoard
		WHERE status = 'Pending' AND type = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM TaskDependencies td
		    JOIN BountyBoard dep ON dep.id = td.depends_on
		    WHERE td.task_id = BountyBoard.id AND dep.status != 'Completed'
		  )
		  AND (convoy_id = 0 OR NOT EXISTS (
		    SELECT 1 FROM FeatureBlockers fb
		    WHERE fb.blocked_convoy_id = BountyBoard.convoy_id AND fb.resolved_at IS NULL
		  ))
		ORDER BY priority DESC, id ASC
		LIMIT 1`, taskType).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Type, &b.Status,
			&b.Payload, &b.ConvoyID, &b.Checkpoint, &b.Priority, &b.TaskTimeout)
	if err != nil {
		return nil, false
	}
	res, _ := db.Exec(`
		UPDATE BountyBoard SET status = 'Locked', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'Pending'`, agentName, b.ID)
	rows, _ := res.RowsAffected()
	if rows == 1 {
		b.Status = "Locked"
		b.Owner = agentName
		return &b, true
	}
	return nil, false
}

// ClaimForReview atomically claims the next task awaiting council review.
// Higher-priority tasks are reviewed first, matching the claim order used by Astromechs.
func ClaimForReview(db *sql.DB, agentName string) (*Bounty, bool) {
	var b Bounty
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, payload, retry_count, branch_name, convoy_id, priority
		FROM BountyBoard WHERE status = 'AwaitingCouncilReview'
		ORDER BY priority DESC, id ASC
		LIMIT 1`).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Payload, &b.RetryCount, &b.BranchName, &b.ConvoyID, &b.Priority)
	if err != nil {
		return nil, false
	}
	res, _ := db.Exec(`
		UPDATE BountyBoard SET status = 'UnderReview', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'AwaitingCouncilReview'`, agentName, b.ID)
	rows, _ := res.RowsAffected()
	if rows == 1 {
		return &b, true
	}
	return nil, false
}

// ClaimForCaptainReview atomically claims the next task awaiting captain review.
func ClaimForCaptainReview(db *sql.DB, agentName string) (*Bounty, bool) {
	var b Bounty
	err := db.QueryRow(`
		SELECT id, parent_id, target_repo, payload, retry_count, branch_name, convoy_id, priority
		FROM BountyBoard WHERE status = 'AwaitingCaptainReview'
		ORDER BY priority DESC, id ASC
		LIMIT 1`).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Payload, &b.RetryCount, &b.BranchName, &b.ConvoyID, &b.Priority)
	if err != nil {
		return nil, false
	}
	res, _ := db.Exec(`
		UPDATE BountyBoard SET status = 'UnderCaptainReview', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'AwaitingCaptainReview'`, agentName, b.ID)
	rows, _ := res.RowsAffected()
	if rows == 1 {
		return &b, true
	}
	return nil, false
}

// IsConvoyCoordinated reports whether a convoy routes completed tasks through
// the Captain before council review.
func IsConvoyCoordinated(db *sql.DB, convoyID int) bool {
	if convoyID == 0 {
		return false
	}
	var coordinated int
	db.QueryRow(`SELECT coordinated FROM Convoys WHERE id = ?`, convoyID).Scan(&coordinated)
	return coordinated == 1
}

// SetConvoyCoordinated marks a convoy as coordinated so Astromech completions
// route to AwaitingCaptainReview instead of AwaitingCouncilReview.
func SetConvoyCoordinated(db *sql.DB, convoyID int) {
	db.Exec(`UPDATE Convoys SET coordinated = 1 WHERE id = ?`, convoyID)
}

func UpdateBountyStatus(db *sql.DB, id int, newStatus string) {
	db.Exec(`UPDATE BountyBoard SET status = ?, owner = '', locked_at = '' WHERE id = ?`, newStatus, id)
	if newStatus == "Completed" || newStatus == "Failed" || newStatus == "Escalated" {
		FireWebhook(db, id, newStatus)
	}
}

func AddBounty(db *sql.DB, parentID int, taskType, payload string) int {
	res, _ := db.Exec(`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at) VALUES (?, ?, 'Pending', ?, datetime('now'))`,
		parentID, taskType, payload)
	id, _ := res.LastInsertId()
	return int(id)
}

// AddBountyClassifying inserts a task with type='Auto' and status='Classifying'.
// The Inquisitor will classify it and transition it to Pending.
// idempotencyKey is stored immediately so duplicate-check queries can find the row.
func AddBountyClassifying(db *sql.DB, repo, payload string, priority int, idempotencyKey string) (int, error) {
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, idempotency_key, created_at)
		 VALUES (0, ?, 'Auto', 'Classifying', ?, ?, ?, datetime('now'))`,
		repo, payload, priority, idempotencyKey)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

func FailBounty(db *sql.DB, id int, errorMsg string) {
	db.Exec(`UPDATE BountyBoard SET status = 'Failed', owner = '', locked_at = '', error_log = ? WHERE id = ?`,
		errorMsg, id)
	FireWebhook(db, id, "Failed")
}

// MarkConflictPending transitions a task to ConflictPending, indicating it was
// approved by the council but couldn't merge due to a conflict. A resolution
// task has been spawned and will complete this task's work.
func MarkConflictPending(db *sql.DB, id int, msg string) {
	db.Exec(`UPDATE BountyBoard SET status = 'ConflictPending', owner = '', locked_at = '', error_log = ? WHERE id = ?`,
		msg, id)
}

// CancelTask marks a task as Cancelled with a reason. Cancelled is distinct from Failed —
// it reflects deliberate operator action, not an agent error.
// No-op if the task is already Completed. Returns true if the task was cancelled.
func CancelTask(db *sql.DB, id int, reason string) bool {
	res, _ := db.Exec(`UPDATE BountyBoard SET status = 'Cancelled', owner = '', locked_at = '', error_log = ?
		WHERE id = ? AND status != 'Completed'`, reason, id)
	n, _ := res.RowsAffected()
	return n > 0
}

// ResetTask resets a single task to Pending, clearing all error and lock state.
// If the task belongs to a Failed convoy and no other problem tasks remain after
// the reset, the convoy is automatically recovered to Active.
func ResetTask(db *sql.DB, id int) {
	var convoyID int
	db.QueryRow(`SELECT convoy_id FROM BountyBoard WHERE id = ?`, id).Scan(&convoyID)
	db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', error_log = '',
		retry_count = 0, infra_failures = 0, locked_at = '', checkpoint = '', branch_name = ''
		WHERE id = ?`, id)
	AutoRecoverConvoy(db, convoyID, nil)
}

// ResetAllFailed resets all Failed tasks to Pending. Returns the number of tasks reset.
func ResetAllFailed(db *sql.DB) int {
	res, _ := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', error_log = '',
		retry_count = 0, infra_failures = 0, locked_at = '', checkpoint = '', branch_name = ''
		WHERE status = 'Failed'`)
	n, _ := res.RowsAffected()
	return int(n)
}

// ReturnTaskForRework sends a task back to Pending with a new payload (feedback injected).
// branch_name is intentionally preserved so the agent can resume from prior work rather
// than redoing everything from scratch.
func ReturnTaskForRework(db *sql.DB, id int, newPayload string) {
	db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '', payload = ?, checkpoint = ''
		WHERE id = ?`, newPayload, id)
}

// QueueMedicReview spawns a MedicReview task for a permanently-failed bounty.
// The Medic will analyze the failure and decide whether to requeue, shard, or escalate.
// Inherits target_repo, convoy_id, and priority from the source bounty.
func QueueMedicReview(db *sql.DB, b *Bounty, failureType, errorDetail string) int {
	payload := fmt.Sprintf(`{"failure_type":%q,"error":%q}`, failureType, errorDetail)
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (?, ?, 'MedicReview', 'Pending', ?, ?, ?, datetime('now'))`,
		b.ID, b.TargetRepo, payload, b.ConvoyID, b.Priority)
	id, _ := res.LastInsertId()
	return int(id)
}

// AddConvoyTask creates a CodeEdit subtask within a convoy. status should be
// 'Pending' or 'Planned'. Returns the new task ID and any insertion error.
func AddConvoyTask(db *sql.DB, parentID int, repo, payload string, convoyID, priority int, status string) (int, error) {
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (?, ?, 'CodeEdit', ?, ?, ?, ?, datetime('now'))`,
		parentID, repo, status, payload, convoyID, priority)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// AddCodeEditTask creates a CodeEdit task with full field support. Returns the new task ID.
func AddCodeEditTask(db *sql.DB, repo, payload string, convoyID, priority, taskTimeout int) int {
	res, _ := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, task_timeout)
		 VALUES (0, ?, 'CodeEdit', 'Pending', ?, ?, ?, ?)`,
		repo, payload, convoyID, priority, taskTimeout)
	id, _ := res.LastInsertId()
	return int(id)
}

func IncrementRetryCount(db *sql.DB, id int) int {
	db.Exec(`UPDATE BountyBoard SET retry_count = retry_count + 1 WHERE id = ?`, id)
	var count int
	db.QueryRow(`SELECT retry_count FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	return count
}

func IncrementInfraFailures(db *sql.DB, id int) int {
	db.Exec(`UPDATE BountyBoard SET infra_failures = infra_failures + 1 WHERE id = ?`, id)
	var count int
	db.QueryRow(`SELECT infra_failures FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	return count
}

func UpdateCheckpoint(db *sql.DB, id int, checkpoint string) {
	db.Exec(`UPDATE BountyBoard SET checkpoint = ? WHERE id = ?`, checkpoint, id)
}

func SetBranchName(db *sql.DB, id int, branchName string) {
	db.Exec(`UPDATE BountyBoard SET branch_name = ? WHERE id = ?`, branchName, id)
}

func SetBountyPriority(db *sql.DB, id, priority int) {
	db.Exec(`UPDATE BountyBoard SET priority = ? WHERE id = ?`, priority, id)
}

// ── Task dependencies ─────────────────────────────────────────────────────────

// AddDependency records that taskID depends on dependsOn.
// No-op if the dependency already exists (INSERT OR IGNORE).
func AddDependency(db *sql.DB, taskID, dependsOn int) {
	db.Exec(`INSERT OR IGNORE INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, taskID, dependsOn)
}

// AddConvoyTaskTx creates a CodeEdit subtask within a convoy using an existing transaction.
// Mirrors AddConvoyTask for use inside a caller-owned transaction.
func AddConvoyTaskTx(tx *sql.Tx, parentID int, repo, payload string, convoyID, priority int, status string) (int, error) {
	res, err := tx.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (?, ?, 'CodeEdit', ?, ?, ?, ?, datetime('now'))`,
		parentID, repo, status, payload, convoyID, priority)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// AddDependencyTx records that taskID depends on dependsOn using an existing transaction.
// Returns an error so callers can roll back on failure.
func AddDependencyTx(tx *sql.Tx, taskID, dependsOn int) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, taskID, dependsOn)
	return err
}

// GetDependencies returns all task IDs that taskID depends on (its blockers).
func GetDependencies(db *sql.DB, taskID int) []int {
	rows, err := db.Query(`SELECT depends_on FROM TaskDependencies WHERE task_id = ? ORDER BY depends_on ASC`, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

// RemoveDependenciesOf removes all dependencies FOR a task (the task is unblocked entirely).
func RemoveDependenciesOf(db *sql.DB, taskID int) {
	db.Exec(`DELETE FROM TaskDependencies WHERE task_id = ?`, taskID)
}

// UnblockDependentsOf removes all dependency edges that point TO id, freeing any tasks
// that were waiting solely on id. Non-recursive — ClaimBounty handles claimability dynamically.
// Returns the number of dependency edges removed.
func UnblockDependentsOf(db *sql.DB, id int) int {
	res, _ := db.Exec(`DELETE FROM TaskDependencies WHERE depends_on = ?`, id)
	n, _ := res.RowsAffected()
	return int(n)
}

// ── Cost computation ──────────────────────────────────────────────────────────

// Pricing constants for Claude Sonnet (per million tokens).
const (
	PriceInputPerMillion  = 3.0  // $3.00/M input tokens
	PriceOutputPerMillion = 15.0 // $15.00/M output tokens
)

// TaskCostDollars computes the cost in dollars for a given token usage.
func TaskCostDollars(tokensIn, tokensOut int) float64 {
	return float64(tokensIn)*PriceInputPerMillion/1_000_000 +
		float64(tokensOut)*PriceOutputPerMillion/1_000_000
}

// TotalSpendDollars returns the total spend in dollars across all TaskHistory rows.
func TotalSpendDollars(db *sql.DB) float64 {
	var tokensIn, tokensOut int64
	db.QueryRow(`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0) FROM TaskHistory`).
		Scan(&tokensIn, &tokensOut)
	return float64(tokensIn)*PriceInputPerMillion/1_000_000 +
		float64(tokensOut)*PriceOutputPerMillion/1_000_000
}

// ── Task history ──────────────────────────────────────────────────────────────

// RecordTaskHistory inserts a history entry and returns its row ID.
func RecordTaskHistory(db *sql.DB, taskID int, agent, sessionID, output, outcome string) int64 {
	var attempt int
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE task_id = ?`, taskID).Scan(&attempt)
	res, _ := db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome)
		VALUES (?, ?, ?, ?, ?, ?)`, taskID, attempt+1, agent, sessionID, output, outcome)
	id, _ := res.LastInsertId()
	return id
}

// UpdateTaskHistoryTokens records token usage on an existing history row.
func UpdateTaskHistoryTokens(db *sql.DB, historyID int64, tokensIn, tokensOut int) {
	db.Exec(`UPDATE TaskHistory SET tokens_in = ?, tokens_out = ? WHERE id = ?`, tokensIn, tokensOut, historyID)
}

func GetTaskHistory(db *sql.DB, taskID int) []TaskHistoryEntry {
	rows, err := db.Query(`SELECT id, task_id, attempt, agent, session_id, claude_output, outcome,
		IFNULL(tokens_in,0), IFNULL(tokens_out,0), created_at
		FROM TaskHistory WHERE task_id = ? ORDER BY attempt ASC`, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var entries []TaskHistoryEntry
	for rows.Next() {
		var e TaskHistoryEntry
		rows.Scan(&e.ID, &e.TaskID, &e.Attempt, &e.Agent, &e.SessionID, &e.ClaudeOutput, &e.Outcome,
			&e.TokensIn, &e.TokensOut, &e.CreatedAt)
		entries = append(entries, e)
	}
	return entries
}

// ── Fleet memory ──────────────────────────────────────────────────────────────

// StoreFleetMemory saves a lesson learned from a completed or failed task.
// outcome should be "success" or "failure".
// The FTS index is updated explicitly after the main insert — an FTS failure
// is non-fatal and will not roll back the memory record.
func StoreFleetMemory(db *sql.DB, repo string, taskID int, outcome, summary, filesChanged string) {
	res, err := db.Exec(
		`INSERT INTO FleetMemory (repo, task_id, outcome, summary, files_changed) VALUES (?, ?, ?, ?, ?)`,
		repo, taskID, outcome, summary, filesChanged)
	if err != nil {
		return
	}
	if id, idErr := res.LastInsertId(); idErr == nil {
		db.Exec(`INSERT INTO FleetMemory_fts(rowid, summary, files_changed) VALUES (?, ?, ?)`,
			id, summary, filesChanged)
	}
}

// DeleteFleetMemory removes a single memory entry by ID from both the main table
// and the FTS index.
func DeleteFleetMemory(db *sql.DB, id int) bool {
	db.Exec(`DELETE FROM FleetMemory_fts WHERE rowid = ?`, id)
	res, _ := db.Exec(`DELETE FROM FleetMemory WHERE id = ?`, id)
	n, _ := res.RowsAffected()
	return n > 0
}

// sanitizeFTSQuery strips FTS5 special characters and builds an OR query so
// BM25 ranks by vocabulary overlap — any memory sharing terms with the current
// task floats up, even when not every term matches. Words shorter than 2
// characters are dropped as noise.
func sanitizeFTSQuery(q string) string {
	var b strings.Builder
	for _, r := range q {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	var words []string
	for _, w := range strings.Fields(b.String()) {
		if len(w) >= 2 {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return ""
	}
	return strings.Join(words, " OR ")
}

// GetFleetMemories returns relevant memory entries for a repo.
// When query is non-empty, results are ranked by FTS5 BM25 relevance against
// the query text. Falls back to recency order if the query yields no matches
// or is empty — ensuring agents always get some context.
func GetFleetMemories(db *sql.DB, repo, query string, limit int) []FleetMemoryEntry {
	if query != "" {
		if ftsQ := sanitizeFTSQuery(query); ftsQ != "" {
			// Step 1: get BM25-ranked rowids from FTS5.
			// Fetch more than limit to leave room for repo filtering in step 2.
			// Rows are closed before step 2 to avoid blocking the single connection.
			ftsRows, err := db.Query(
				`SELECT rowid FROM FleetMemory_fts WHERE FleetMemory_fts MATCH ? ORDER BY rank LIMIT ?`,
				ftsQ, limit*3)
			var rankedIDs []int
			if err == nil {
				for ftsRows.Next() {
					var id int
					ftsRows.Scan(&id)
					rankedIDs = append(rankedIDs, id)
				}
				ftsRows.Close()
			}

			// Step 2: fetch full records in rank order, filtered by repo.
			if len(rankedIDs) > 0 {
				var entries []FleetMemoryEntry
				for _, id := range rankedIDs {
					if len(entries) >= limit {
						break
					}
					var e FleetMemoryEntry
					err := db.QueryRow(
						`SELECT id, repo, task_id, outcome, summary, files_changed, created_at
						 FROM FleetMemory WHERE id = ? AND repo = ?`, id, repo).
						Scan(&e.ID, &e.Repo, &e.TaskID, &e.Outcome, &e.Summary, &e.FilesChanged, &e.CreatedAt)
					if err == nil {
						entries = append(entries, e)
					}
				}
				if len(entries) > 0 {
					return entries
				}
			}
		}
	}

	// Recency fallback — no query, empty query, or FTS returned nothing
	rows, err := db.Query(`
		SELECT id, repo, task_id, outcome, summary, files_changed, created_at
		FROM FleetMemory
		WHERE repo = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, repo, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var entries []FleetMemoryEntry
	for rows.Next() {
		var e FleetMemoryEntry
		rows.Scan(&e.ID, &e.Repo, &e.TaskID, &e.Outcome, &e.Summary, &e.FilesChanged, &e.CreatedAt)
		entries = append(entries, e)
	}
	return entries
}

// ListAllFleetMemories returns all memories, optionally filtered by repo.
func ListAllFleetMemories(db *sql.DB, repo string, limit int) []FleetMemoryEntry {
	var rows *sql.Rows
	var err error
	if repo != "" {
		rows, err = db.Query(`SELECT id, repo, task_id, outcome, summary, files_changed, created_at FROM FleetMemory WHERE repo = ? ORDER BY created_at DESC, id DESC LIMIT ?`, repo, limit)
	} else {
		rows, err = db.Query(`SELECT id, repo, task_id, outcome, summary, files_changed, created_at FROM FleetMemory ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var entries []FleetMemoryEntry
	for rows.Next() {
		var e FleetMemoryEntry
		rows.Scan(&e.ID, &e.Repo, &e.TaskID, &e.Outcome, &e.Summary, &e.FilesChanged, &e.CreatedAt)
		entries = append(entries, e)
	}
	return entries
}

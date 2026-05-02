package store

import (
	"database/sql"
	"fmt"
)

// ── ConvoyStages CRUD (D5.5 P0) ──────────────────────────────────────────────
//
// A ConvoyStage row represents one phase in a Commander-drafted phase pipeline
// for a convoy. Stages execute in stage_num order (1-indexed). For pre-D5.5
// (legacy) convoys the forward-compat migration auto-creates a single Open
// stage 1 row with gate_type=NULL — so every convoy in the system has at
// least one stage row at all times.
//
// Status lifecycle (linear, with Failed as a terminal sink):
//
//   Pending → Open → AllPRsMerged → AwaitingGate → GatePassed → Verified
//                                                                        \
//                                                                         → Failed
//
// `AdvanceStage` enforces the linear ordering. Skipping intermediate states
// (e.g. Pending → GatePassed) is rejected. Any non-terminal state may
// transition to Failed.

// stageStatusOrder is the linear ordering of non-terminal stage statuses.
// AdvanceStage uses this to reject transitions that skip an intermediate.
var stageStatusOrder = map[string]int{
	StageStatusPending:      0,
	StageStatusOpen:         1,
	StageStatusAllPRsMerged: 2,
	StageStatusAwaitingGate: 3,
	StageStatusGatePassed:   4,
	StageStatusVerified:     5,
}

// CreateStage inserts a new ConvoyStage row. Returns the new row's id.
//
// `gateType` of the empty string maps to SQL NULL (no gate; allowed only on
// the terminal stage of a convoy — but P0 does not enforce that; the
// Commander/planner does at convoy creation time in P2). `gateConfigJSON`
// must be valid JSON; an empty string is normalised to "{}".
func CreateStage(db *sql.DB, convoyID, stageNum int, intent, gateType, gateConfigJSON string) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("CreateStage: convoyID must be > 0 (got %d)", convoyID)
	}
	if stageNum <= 0 {
		return 0, fmt.Errorf("CreateStage: stageNum must be > 0 (got %d)", stageNum)
	}
	cfg := gateConfigJSON
	if cfg == "" {
		cfg = "{}"
	}
	var gateTypeArg any
	if gateType == "" {
		gateTypeArg = nil
	} else {
		gateTypeArg = gateType
	}
	res, err := db.Exec(`INSERT INTO ConvoyStages
		(convoy_id, stage_num, intent_text, status, gate_type, gate_config_json)
		VALUES (?, ?, ?, 'Pending', ?, ?)`,
		convoyID, stageNum, intent, gateTypeArg, cfg)
	if err != nil {
		return 0, fmt.Errorf("CreateStage: insert convoy=%d stage_num=%d: %w", convoyID, stageNum, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateStage: LastInsertId: %w", err)
	}
	return int(id), nil
}

// GetStage returns the stage by primary-key id, or an error if not found.
func GetStage(db *sql.DB, stageID int) (ConvoyStage, error) {
	return scanStage(db.QueryRow(`SELECT
		id, convoy_id, stage_num, IFNULL(intent_text, ''), status,
		gate_type, IFNULL(gate_config_json, '{}'), gate_timeout_minutes,
		IFNULL(opened_at, ''), IFNULL(all_prs_merged_at, ''),
		IFNULL(gate_passed_at, ''), IFNULL(completed_at, '')
		FROM ConvoyStages WHERE id = ?`, stageID))
}

// GetStageByNum returns the stage row keyed on (convoy_id, stage_num).
func GetStageByNum(db *sql.DB, convoyID, stageNum int) (ConvoyStage, error) {
	return scanStage(db.QueryRow(`SELECT
		id, convoy_id, stage_num, IFNULL(intent_text, ''), status,
		gate_type, IFNULL(gate_config_json, '{}'), gate_timeout_minutes,
		IFNULL(opened_at, ''), IFNULL(all_prs_merged_at, ''),
		IFNULL(gate_passed_at, ''), IFNULL(completed_at, '')
		FROM ConvoyStages WHERE convoy_id = ? AND stage_num = ?`, convoyID, stageNum))
}

// ListStages returns every stage for a convoy, ordered by stage_num ASC.
func ListStages(db *sql.DB, convoyID int) ([]ConvoyStage, error) {
	rows, err := db.Query(`SELECT
		id, convoy_id, stage_num, IFNULL(intent_text, ''), status,
		gate_type, IFNULL(gate_config_json, '{}'), gate_timeout_minutes,
		IFNULL(opened_at, ''), IFNULL(all_prs_merged_at, ''),
		IFNULL(gate_passed_at, ''), IFNULL(completed_at, '')
		FROM ConvoyStages WHERE convoy_id = ? ORDER BY stage_num ASC`, convoyID)
	if err != nil {
		return nil, fmt.Errorf("ListStages: query convoy=%d: %w", convoyID, err)
	}
	defer rows.Close()
	var out []ConvoyStage
	for rows.Next() {
		s, err := scanStage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListStages: rows iter convoy=%d: %w", convoyID, rErr)
	}
	return out, nil
}

// rowScanner is the common Scan interface implemented by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanStage(r rowScanner) (ConvoyStage, error) {
	var (
		s        ConvoyStage
		gateType sql.NullString
	)
	err := r.Scan(&s.ID, &s.ConvoyID, &s.StageNum, &s.IntentText, &s.Status,
		&gateType, &s.GateConfigJSON, &s.GateTimeoutMinutes,
		&s.OpenedAt, &s.AllPRsMergedAt, &s.GatePassedAt, &s.CompletedAt)
	if err != nil {
		return ConvoyStage{}, err
	}
	s.GateType = gateType.String
	s.GateTypeIsNull = !gateType.Valid
	return s, nil
}

// AdvanceStage transitions a stage's status. Validates the transition against
// the linear ordering Pending → Open → AllPRsMerged → AwaitingGate →
// GatePassed → Verified. Any non-terminal state may transition to Failed
// (terminal). Stamps the corresponding timestamp column on the new status.
//
// Stamped timestamps:
//
//	Open         → opened_at
//	AllPRsMerged → all_prs_merged_at
//	GatePassed   → gate_passed_at
//	Verified     → completed_at
//	Failed       → completed_at (terminal — Failed and Verified share the
//	               completion stamp)
//
// Returns an error if the transition is illegal or if no row exists.
func AdvanceStage(db *sql.DB, stageID int, newStatus string) error {
	if stageID <= 0 {
		return fmt.Errorf("AdvanceStage: stageID must be > 0 (got %d)", stageID)
	}
	cur, err := GetStage(db, stageID)
	if err != nil {
		return fmt.Errorf("AdvanceStage: load stage %d: %w", stageID, err)
	}
	if err := validateStageTransition(cur.Status, newStatus); err != nil {
		return err
	}
	switch newStatus {
	case StageStatusOpen:
		_, err = db.Exec(`UPDATE ConvoyStages SET status = ?, opened_at = datetime('now') WHERE id = ?`, newStatus, stageID)
	case StageStatusAllPRsMerged:
		_, err = db.Exec(`UPDATE ConvoyStages SET status = ?, all_prs_merged_at = datetime('now') WHERE id = ?`, newStatus, stageID)
	case StageStatusAwaitingGate:
		_, err = db.Exec(`UPDATE ConvoyStages SET status = ? WHERE id = ?`, newStatus, stageID)
	case StageStatusGatePassed:
		_, err = db.Exec(`UPDATE ConvoyStages SET status = ?, gate_passed_at = datetime('now') WHERE id = ?`, newStatus, stageID)
	case StageStatusVerified, StageStatusFailed:
		_, err = db.Exec(`UPDATE ConvoyStages SET status = ?, completed_at = datetime('now') WHERE id = ?`, newStatus, stageID)
	default:
		// validateStageTransition already rejected this — defensive only.
		return fmt.Errorf("AdvanceStage: unknown newStatus %q", newStatus)
	}
	if err != nil {
		return fmt.Errorf("AdvanceStage: update stage %d → %s: %w", stageID, newStatus, err)
	}
	return nil
}

// validateStageTransition enforces the linear progression rule. From any
// non-terminal status, only the immediate next status (or Failed) is allowed.
// Verified and Failed are terminal — no further transition is permitted.
func validateStageTransition(curStatus, newStatus string) error {
	if curStatus == StageStatusVerified || curStatus == StageStatusFailed {
		return fmt.Errorf("AdvanceStage: cannot transition from terminal status %q (requested %q)", curStatus, newStatus)
	}
	if newStatus == StageStatusFailed {
		// any non-terminal state may move to Failed
		return nil
	}
	curIdx, ok := stageStatusOrder[curStatus]
	if !ok {
		return fmt.Errorf("AdvanceStage: unknown current status %q", curStatus)
	}
	nextIdx, ok := stageStatusOrder[newStatus]
	if !ok {
		return fmt.Errorf("AdvanceStage: unknown target status %q", newStatus)
	}
	if nextIdx != curIdx+1 {
		return fmt.Errorf("AdvanceStage: illegal transition %s → %s (must advance by exactly one step or move to Failed)", curStatus, newStatus)
	}
	return nil
}

// CurrentInFlightStage returns the convoy's currently-active stage — the one
// whose ConvoyReview pass should run against. "In flight" means the stage is
// past Pending (the planner has opened it for work) and before Verified
// (the gate hasn't yet passed). For a single-stage convoy this is the
// implicit stage 1 (which the forward-compat migration created in 'Open'
// status). For staged convoys with multiple stages, exactly one stage at a
// time is in flight under the strict strategy.
//
// Returns the stage with the lowest stage_num among rows whose status is in
// {Open, AllPRsMerged, AwaitingGate, GatePassed}. If no such stage exists
// (every stage already Verified, or every stage still Pending/Failed), the
// fallback is the lowest-numbered non-terminal stage. Returns an error only
// on DB failure; "no stages at all" returns an explicit error so callers can
// log + degrade rather than silently choose an arbitrary scope.
func CurrentInFlightStage(db *sql.DB, convoyID int) (ConvoyStage, error) {
	if convoyID <= 0 {
		return ConvoyStage{}, fmt.Errorf("CurrentInFlightStage: convoyID must be > 0 (got %d)", convoyID)
	}
	// Preferred: the lowest-numbered stage in an "in flight" status. The
	// strict strategy guarantees at most one stage occupies these states at
	// a time, but ORDER BY stage_num makes the choice deterministic if two
	// rows ever co-occur (e.g. mid-transition, or under merge_parallel
	// once it lands).
	rows, err := db.Query(`SELECT
		id, convoy_id, stage_num, IFNULL(intent_text, ''), status,
		gate_type, IFNULL(gate_config_json, '{}'), gate_timeout_minutes,
		IFNULL(opened_at, ''), IFNULL(all_prs_merged_at, ''),
		IFNULL(gate_passed_at, ''), IFNULL(completed_at, '')
		FROM ConvoyStages
		WHERE convoy_id = ?
		  AND status IN (?, ?, ?, ?)
		ORDER BY stage_num ASC LIMIT 1`,
		convoyID,
		StageStatusOpen, StageStatusAllPRsMerged, StageStatusAwaitingGate, StageStatusGatePassed)
	if err != nil {
		return ConvoyStage{}, fmt.Errorf("CurrentInFlightStage: query convoy=%d: %w", convoyID, err)
	}
	defer rows.Close()
	if rows.Next() {
		s, scanErr := scanStage(rows)
		if scanErr != nil {
			return ConvoyStage{}, fmt.Errorf("CurrentInFlightStage: scan convoy=%d: %w", convoyID, scanErr)
		}
		return s, nil
	}

	// Fallback: lowest-numbered non-terminal stage (Pending). Convoys whose
	// stage 1 is still Pending (planner just emitted them) need a scope too
	// so a too-eager ConvoyReview doesn't crash; the per-stage scope still
	// degrades correctly because the stage's ask-branches aren't open yet.
	fallback, err := db.Query(`SELECT
		id, convoy_id, stage_num, IFNULL(intent_text, ''), status,
		gate_type, IFNULL(gate_config_json, '{}'), gate_timeout_minutes,
		IFNULL(opened_at, ''), IFNULL(all_prs_merged_at, ''),
		IFNULL(gate_passed_at, ''), IFNULL(completed_at, '')
		FROM ConvoyStages
		WHERE convoy_id = ?
		  AND status NOT IN (?, ?)
		ORDER BY stage_num ASC LIMIT 1`,
		convoyID, StageStatusVerified, StageStatusFailed)
	if err != nil {
		return ConvoyStage{}, fmt.Errorf("CurrentInFlightStage: fallback query convoy=%d: %w", convoyID, err)
	}
	defer fallback.Close()
	if fallback.Next() {
		s, scanErr := scanStage(fallback)
		if scanErr != nil {
			return ConvoyStage{}, fmt.Errorf("CurrentInFlightStage: fallback scan convoy=%d: %w", convoyID, scanErr)
		}
		return s, nil
	}
	return ConvoyStage{}, fmt.Errorf("CurrentInFlightStage: convoy=%d has no in-flight or pending stages", convoyID)
}

// GetRepositoryReleaseLabelPattern returns the per-repo regex used by the
// `release_label_present` gate (D5.5 P3), or '' if the repo doesn't use
// release labels. Returns an error if the repo is not registered.
func GetRepositoryReleaseLabelPattern(db *sql.DB, repoName string) (string, error) {
	if repoName == "" {
		return "", fmt.Errorf("GetRepositoryReleaseLabelPattern: repoName must be non-empty")
	}
	var pattern string
	err := db.QueryRow(`SELECT IFNULL(release_label_pattern, '') FROM Repositories WHERE name = ?`, repoName).Scan(&pattern)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("GetRepositoryReleaseLabelPattern: repo %q not registered", repoName)
	}
	if err != nil {
		return "", fmt.Errorf("GetRepositoryReleaseLabelPattern: query repo %q: %w", repoName, err)
	}
	return pattern, nil
}

// SetRepositoryReleaseLabelPattern updates the per-repo release-label regex.
// Pass '' to clear (meaning the repo doesn't use release labels). Returns an
// error if the repo is not registered.
func SetRepositoryReleaseLabelPattern(db *sql.DB, repoName, pattern string) error {
	if repoName == "" {
		return fmt.Errorf("SetRepositoryReleaseLabelPattern: repoName must be non-empty")
	}
	res, err := db.Exec(`UPDATE Repositories SET release_label_pattern = ? WHERE name = ?`, pattern, repoName)
	if err != nil {
		return fmt.Errorf("SetRepositoryReleaseLabelPattern: update repo %q: %w", repoName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("SetRepositoryReleaseLabelPattern: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("SetRepositoryReleaseLabelPattern: repo %q not registered", repoName)
	}
	return nil
}

// ── Stage audit trail (D5.5 P4) ──────────────────────────────────────────────
//
// Stage transitions (operator advance, operator abort, automatic dog advance)
// each append an AuditLog row so the dashboard can render a per-stage history
// without scanning every other audit action. The convention:
//
//   actor    — operator name ("operator-x") or "convoy-stage-watch-dog"
//   action   — one of the stage-action prefix values below
//   task_id  — the convoy_id (we re-use this column as the convoy handle since
//              stage rows have no task_id; queries filter by action prefix +
//              detail.stage_num for stage-scoped views)
//   detail   — JSON {"stage_num":N,"old_status":"...","new_status":"...",
//              "reason":"...","gate_evaluation_summary":"..."}
//
// `ListStageAuditLog` returns rows for one stage, newest-first.

const (
	// AuditActionStageAdvance — operator clicked "Advance" on the dashboard.
	AuditActionStageAdvance = "stage_advance"
	// AuditActionStageAbort — operator clicked "Abort" on the dashboard.
	AuditActionStageAbort = "stage_abort"
	// AuditActionStageAutoAdvance — convoy-stage-watch dog advanced the stage
	// based on a gate evaluation outcome.
	AuditActionStageAutoAdvance = "stage_auto_advance"
)

// stageActionPrefix is the SQL LIKE prefix that matches every stage-related
// audit action. Used by ListStageAuditLog to scope queries; lets us add new
// stage_* actions in the future without changing the query.
const stageActionPrefix = "stage_"

// LogStageAudit appends an AuditLog row recording a stage state transition.
// The detail blob is stored as JSON so per-stage drill-downs can decode it.
//
// Returns an error on insert failure. Unlike LogAudit (the legacy void
// helper), this mutator threads the error per CLAUDE.md "no silent failures":
// stage audit trail is operator-actionable, so silently dropping a row would
// hide the only durable record of who pushed which gate.
func LogStageAudit(db *sql.DB, actor, action string, convoyID, stageNum int, oldStatus, newStatus, reason, gateEvalSummary string) error {
	if db == nil {
		return fmt.Errorf("LogStageAudit: db is nil")
	}
	if convoyID <= 0 {
		return fmt.Errorf("LogStageAudit: convoyID must be > 0 (got %d)", convoyID)
	}
	if stageNum <= 0 {
		return fmt.Errorf("LogStageAudit: stageNum must be > 0 (got %d)", stageNum)
	}
	if action == "" {
		return fmt.Errorf("LogStageAudit: action must be non-empty")
	}
	detail := fmt.Sprintf(
		`{"stage_num":%d,"old_status":%q,"new_status":%q,"reason":%q,"gate_evaluation_summary":%q}`,
		stageNum, oldStatus, newStatus, reason, gateEvalSummary)
	if _, err := db.Exec(
		`INSERT INTO AuditLog (actor, action, task_id, detail) VALUES (?, ?, ?, ?)`,
		actor, action, convoyID, detail); err != nil {
		return fmt.Errorf("LogStageAudit: insert convoy=%d stage=%d action=%s: %w", convoyID, stageNum, action, err)
	}
	return nil
}

// ListStageAuditLog returns every audit row for a given (convoyID, stageNum)
// in descending id order (newest first). Filters on action LIKE 'stage_%'
// and detail stage_num match. Returns an empty slice if no rows match.
func ListStageAuditLog(db *sql.DB, convoyID, stageNum int) ([]AuditEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("ListStageAuditLog: db is nil")
	}
	if convoyID <= 0 {
		return nil, fmt.Errorf("ListStageAuditLog: convoyID must be > 0 (got %d)", convoyID)
	}
	if stageNum <= 0 {
		return nil, fmt.Errorf("ListStageAuditLog: stageNum must be > 0 (got %d)", stageNum)
	}
	// Match the JSON shape LogStageAudit writes: `"stage_num":N,`. The
	// trailing comma anchors the boundary so stage_num=1 doesn't match
	// stage_num=10 etc.
	stagePat := fmt.Sprintf(`%%"stage_num":%d,%%`, stageNum)
	rows, err := db.Query(`SELECT id, actor, action, task_id, detail, created_at
		FROM AuditLog
		WHERE action LIKE ?
		  AND task_id = ?
		  AND detail LIKE ?
		ORDER BY id DESC`,
		stageActionPrefix+"%", convoyID, stagePat)
	if err != nil {
		return nil, fmt.Errorf("ListStageAuditLog: query convoy=%d stage=%d: %w", convoyID, stageNum, err)
	}
	defer rows.Close()
	out := make([]AuditEntry, 0)
	for rows.Next() {
		var e AuditEntry
		if sErr := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.TaskID, &e.Detail, &e.CreatedAt); sErr != nil {
			return nil, fmt.Errorf("ListStageAuditLog: scan: %w", sErr)
		}
		out = append(out, e)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListStageAuditLog: rows iter: %w", rErr)
	}
	return out, nil
}

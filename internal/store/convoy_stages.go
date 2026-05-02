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

package store

import (
	"database/sql"
	"fmt"
	"log"
)

// ConvoyProgress returns (completed, total) task counts for a convoy.
// Cancelled tasks are excluded from total — they represent intentionally removed scope,
// not blocking work. A convoy with 2 done + 2 cancelled shows 2/2, not 2/4.
func ConvoyProgress(db *sql.DB, convoyID int) (completed, total int) {
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status != 'Cancelled'`, convoyID).Scan(&total); err != nil {
		log.Printf("ConvoyProgress: scan total error for convoy %d: %v", convoyID, err)
		return
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status = 'Completed'`, convoyID).Scan(&completed); err != nil {
		log.Printf("ConvoyProgress: scan completed error for convoy %d: %v", convoyID, err)
		return
	}
	return
}

// CreateConvoy creates a named convoy and returns its ID.
//
// D5.5: every newly-created convoy is single-mode by default (legacy shape).
// To keep the invariant "every convoy has at least one ConvoyStage row" true
// at all times — not just after a daemon restart that re-runs the
// forward-compat migration — we also insert a stage 1 row in status='Open'
// with gate_type=NULL, mirroring exactly what runMigrations does for
// pre-existing convoys.
func CreateConvoy(db *sql.DB, name string) (int, error) {
	res, err := db.Exec(`INSERT INTO Convoys (name, status) VALUES (?, 'Active')`, name)
	if err != nil {
		return 0, err
	}
	idRaw, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateConvoy: LastInsertId: %w", err)
	}
	id := int(idRaw)
	// Forward-compat invariant: every single-mode convoy carries a single
	// Open stage 1 row with NULL gate. Future P2 staged-convoy constructors
	// will use a different code path to insert their stage list explicitly.
	if _, err := db.Exec(`INSERT INTO ConvoyStages
		(convoy_id, stage_num, intent_text, status, gate_type, gate_config_json, opened_at)
		VALUES (?, 1, '', 'Open', NULL, '{}', datetime('now'))`, id); err != nil {
		return 0, fmt.Errorf("CreateConvoy: insert stage 1 for convoy %d: %w", id, err)
	}
	return id, nil
}

// StagedStageSpec is the spec for one stage to land into a freshly-created
// staged convoy via CreateStagedConvoy. Used by the Commander/Chancellor
// staged-plan handoff (D5.5 P2). Each stage gets one ConvoyStages row;
// gateConfigJSON is stored verbatim — the planner is responsible for emitting
// well-formed gate JSON (see internal/agents/commander.staging_validator).
type StagedStageSpec struct {
	StageNum       int    // 1-indexed, contiguous
	Intent         string // human-readable rationale (ConvoyStages.intent_text)
	GateType       string // "" → NULL (terminal stages only)
	GateConfigJSON string // valid JSON; "" normalised to "{}"
}

// CreateStagedConvoy creates a Commander-drafted staged convoy: one Convoys
// row in staging_mode='staged' + one ConvoyStages row per spec. Stage 1 is
// stamped Open at create time (mirroring CreateConvoy's behavior for the
// auto-stage-1 of single-mode convoys); stages 2+ land Pending and are
// promoted to Open by the convoy-stage-watch dog when the prior stage's gate
// passes.
//
// Returns the new convoy ID and the per-stage row IDs (in stage_num order)
// so the caller can stamp BountyBoard.stage_id on tasks.
//
// On any failure the entire creation rolls back so the caller never sees a
// half-created convoy.
//
// stagingStrategy must be one of the registered values (StagingStrategyStrict
// in D5.5; merge_parallel/stacked are forward-compat enum values rejected at
// the planner-validator layer, not here). This helper does not enforce the
// "only strict is implemented" rule — that lives in the staging validator —
// so future deliverables can flip the flag without touching this constructor.
func CreateStagedConvoy(db *sql.DB, name, stagingStrategy string, stages []StagedStageSpec) (convoyID int, stageIDs []int, err error) {
	if name == "" {
		return 0, nil, fmt.Errorf("CreateStagedConvoy: name must be non-empty")
	}
	if len(stages) == 0 {
		return 0, nil, fmt.Errorf("CreateStagedConvoy: stages must be non-empty")
	}
	if stagingStrategy == "" {
		stagingStrategy = StagingStrategyStrict
	}
	// Verify stages are 1-indexed and contiguous so the ConvoyStages
	// UNIQUE(convoy_id, stage_num) constraint is respected and the watcher
	// dog's "next stage" lookup never gaps.
	for i, s := range stages {
		if s.StageNum != i+1 {
			return 0, nil, fmt.Errorf("CreateStagedConvoy: stages must be 1-indexed contiguous; got stage_num=%d at index %d", s.StageNum, i)
		}
	}

	tx, txErr := db.Begin()
	if txErr != nil {
		return 0, nil, fmt.Errorf("CreateStagedConvoy: begin tx: %w", txErr)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	res, execErr := tx.Exec(
		`INSERT INTO Convoys (name, status, staging_mode, staging_strategy) VALUES (?, 'Active', ?, ?)`,
		name, StagingModeStaged, stagingStrategy)
	if execErr != nil {
		return 0, nil, fmt.Errorf("CreateStagedConvoy: insert convoy %q: %w", name, execErr)
	}
	idRaw, idErr := res.LastInsertId()
	if idErr != nil {
		return 0, nil, fmt.Errorf("CreateStagedConvoy: LastInsertId: %w", idErr)
	}
	convoyID = int(idRaw)

	stageIDs = make([]int, 0, len(stages))
	for _, s := range stages {
		cfg := s.GateConfigJSON
		if cfg == "" {
			cfg = "{}"
		}
		var gateTypeArg any
		if s.GateType == "" {
			gateTypeArg = nil
		} else {
			gateTypeArg = s.GateType
		}
		// Stage 1 lands Open immediately so astromechs can begin work;
		// stages 2+ land Pending and are promoted by the watcher dog when
		// the prior stage's gate passes.
		var sres sql.Result
		if s.StageNum == 1 {
			sres, execErr = tx.Exec(
				`INSERT INTO ConvoyStages
					(convoy_id, stage_num, intent_text, status, gate_type, gate_config_json, opened_at)
					VALUES (?, ?, ?, ?, ?, ?, datetime('now'))`,
				convoyID, s.StageNum, s.Intent, StageStatusOpen, gateTypeArg, cfg)
		} else {
			sres, execErr = tx.Exec(
				`INSERT INTO ConvoyStages
					(convoy_id, stage_num, intent_text, status, gate_type, gate_config_json)
					VALUES (?, ?, ?, ?, ?, ?)`,
				convoyID, s.StageNum, s.Intent, StageStatusPending, gateTypeArg, cfg)
		}
		if execErr != nil {
			return 0, nil, fmt.Errorf("CreateStagedConvoy: insert stage %d for convoy %d: %w", s.StageNum, convoyID, execErr)
		}
		sIDRaw, sidErr := sres.LastInsertId()
		if sidErr != nil {
			return 0, nil, fmt.Errorf("CreateStagedConvoy: LastInsertId for stage %d: %w", s.StageNum, sidErr)
		}
		stageIDs = append(stageIDs, int(sIDRaw))
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return 0, nil, fmt.Errorf("CreateStagedConvoy: commit: %w", commitErr)
	}
	return convoyID, stageIDs, nil
}

// SetConvoyStaging updates the staging_mode + staging_strategy on an existing
// convoy. Used when the Commander emits a staged-mode plan but the convoy row
// was already created via the legacy single-mode path. Validates inputs but
// does not enforce strategy-vs-implementation gating (the planner does that).
func SetConvoyStaging(db *sql.DB, convoyID int, mode, strategy string) error {
	if convoyID <= 0 {
		return fmt.Errorf("SetConvoyStaging: convoyID must be > 0 (got %d)", convoyID)
	}
	if mode != StagingModeSingle && mode != StagingModeStaged {
		return fmt.Errorf("SetConvoyStaging: unknown mode %q", mode)
	}
	if strategy == "" {
		strategy = StagingStrategyStrict
	}
	_, err := db.Exec(`UPDATE Convoys SET staging_mode = ?, staging_strategy = ? WHERE id = ?`,
		mode, strategy, convoyID)
	if err != nil {
		return fmt.Errorf("SetConvoyStaging: update convoy %d: %w", convoyID, err)
	}
	return nil
}

// ApproveConvoyTasks transitions all Planned tasks in a convoy to Pending.
// Returns the number of tasks activated.
func ApproveConvoyTasks(db *sql.DB, convoyID int) int {
	res, _ := db.Exec(`UPDATE BountyBoard SET status = 'Pending' WHERE convoy_id = ? AND status = 'Planned'`, convoyID)
	n, _ := res.RowsAffected()
	return int(n)
}

// AutoRecoverConvoy resets a Failed convoy back to Active if no problem tasks remain.
// Called automatically after a task is completed or reset. Safe to call with convoyID=0.
func AutoRecoverConvoy(db *sql.DB, convoyID int, logger interface{ Printf(string, ...any) }) {
	if convoyID == 0 {
		return
	}
	var convoyStatus string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&convoyStatus)
	if convoyStatus != "Failed" {
		return
	}
	var problemCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND status IN ('Failed','Escalated')`, convoyID).Scan(&problemCount)
	if problemCount == 0 {
		db.Exec(`UPDATE Convoys SET status = 'Active' WHERE id = ?`, convoyID)
		if logger != nil {
			logger.Printf("Convoy #%d auto-recovered to Active (no remaining problem tasks)", convoyID)
		}
	}
}

// ResetConvoyTasks resets all Failed/Escalated tasks in a convoy back to Pending.
// Returns the number of tasks reset.
func ResetConvoyTasks(db *sql.DB, convoyID int) int {
	res, _ := db.Exec(`
		UPDATE BountyBoard
		SET status = 'Pending', owner = '', locked_at = '', error_log = '', retry_count = 0, infra_failures = 0, checkpoint = '', branch_name = ''
		WHERE convoy_id = ? AND status IN ('Failed', 'Escalated')`, convoyID)
	n, _ := res.RowsAffected()
	return int(n)
}

// CancelConvoyPendingTasks cancels all Planned/Pending tasks in a convoy.
// Returns the number of tasks cancelled.
func CancelConvoyPendingTasks(db *sql.DB, convoyID int) int {
	res, _ := db.Exec(`
		UPDATE BountyBoard
		SET status = 'Cancelled', owner = '', error_log = 'Operator rejected convoy plan'
		WHERE convoy_id = ? AND status IN ('Planned', 'Pending')`, convoyID)
	n, _ := res.RowsAffected()
	return int(n)
}

// RecoverStaleConvoys scans all Failed convoys and auto-recovers those that have no
// remaining problem tasks. Call at daemon startup to fix up convoys that were manually
// reset via CLI or DB without going through the normal task-completion path.
func RecoverStaleConvoys(db *sql.DB) {
	rows, err := db.Query(`SELECT id FROM Convoys WHERE status = 'Failed'`)
	if err != nil {
		return
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			log.Printf("RecoverStaleConvoys: scan failed: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy.go:RecoverStaleConvoys: rows iter error: %v", rErr)
	}
	rows.Close()
	for _, id := range ids {
		AutoRecoverConvoy(db, id, nil)
	}
}

// ListConvoys returns all convoys ordered by creation date.
func ListConvoys(db *sql.DB) []Convoy {
	rows, err := db.Query(`SELECT
		id, name, status, IFNULL(coordinated, 0),
		IFNULL(ask_branch, ''), IFNULL(ask_branch_base_sha, ''),
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		IFNULL(staging_mode, 'single'), IFNULL(staging_strategy, 'strict'),
		created_at
		FROM Convoys ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var convoys []Convoy
	for rows.Next() {
		var (
			c           Convoy
			coordinated int
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &coordinated,
			&c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.StagingMode, &c.StagingStrategy,
			&c.CreatedAt); err != nil {
			log.Printf("ListConvoys: scan error: %v", err)
			return nil
		}
		c.Coordinated = coordinated == 1
		convoys = append(convoys, c)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy.go:ListConvoys: rows iter error: %v", rErr)
	}
	return convoys
}

// GetConvoy returns the full Convoy row, or nil if not found.
func GetConvoy(db *sql.DB, convoyID int) *Convoy {
	var (
		c           Convoy
		coordinated int
	)
	err := db.QueryRow(`SELECT
		id, name, status, IFNULL(coordinated, 0),
		IFNULL(ask_branch, ''), IFNULL(ask_branch_base_sha, ''),
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		IFNULL(staging_mode, 'single'), IFNULL(staging_strategy, 'strict'),
		created_at
		FROM Convoys WHERE id = ?`, convoyID).
		Scan(&c.ID, &c.Name, &c.Status, &coordinated,
			&c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.StagingMode, &c.StagingStrategy,
			&c.CreatedAt)
	if err != nil {
		return nil
	}
	c.Coordinated = coordinated == 1
	return &c
}

// SetConvoyAskBranch records the ask-branch and its base SHA on main. Called by
// Pilot after CreateAskBranch completes (branch cut and pushed). Both values
// must be non-empty — an empty ask_branch is the signal for main-drift-watch
// to skip the convoy, and an empty base_sha makes drift detection impossible.
func SetConvoyAskBranch(db *sql.DB, convoyID int, branch, baseSHA string) error {
	if branch == "" || baseSHA == "" {
		return fmt.Errorf("SetConvoyAskBranch: branch and baseSHA must be non-empty (got %q, %q)", branch, baseSHA)
	}
	// Fix #9: ref validator at ingress. Prevents CVE-2017-1000117-class
	// strings from landing in Convoys.ask_branch and flowing into a
	// `git push --force-with-lease origin <branch>` call downstream.
	if err := validateRefName(branch); err != nil {
		return fmt.Errorf("SetConvoyAskBranch: %w", err)
	}
	_, err := db.Exec(`UPDATE Convoys SET ask_branch = ?, ask_branch_base_sha = ? WHERE id = ?`,
		branch, baseSHA, convoyID)
	return err
}

// UpdateConvoyAskBranchBaseSHA rewrites the stored base SHA after a successful
// rebase onto main. Called by Pilot.RebaseAskBranch when the rebase lands; the
// branch name does not change.
func UpdateConvoyAskBranchBaseSHA(db *sql.DB, convoyID int, newBaseSHA string) error {
	if newBaseSHA == "" {
		return fmt.Errorf("UpdateConvoyAskBranchBaseSHA: newBaseSHA must be non-empty")
	}
	_, err := db.Exec(`UPDATE Convoys SET ask_branch_base_sha = ? WHERE id = ?`,
		newBaseSHA, convoyID)
	return err
}

// SetConvoyDraftPR records the draft PR created by Diplomat. state should be
// "Open" at creation time; draft-pr-watch transitions it to Merged or Closed.
func SetConvoyDraftPR(db *sql.DB, convoyID int, url string, number int, state string) error {
	_, err := db.Exec(`UPDATE Convoys SET draft_pr_url = ?, draft_pr_number = ?, draft_pr_state = ? WHERE id = ?`,
		url, number, state, convoyID)
	return err
}

// UpdateConvoyDraftPRState transitions the draft PR state (Open → Merged/Closed).
// When state == "Merged", also stamps shipped_at.
func UpdateConvoyDraftPRState(db *sql.DB, convoyID int, state string) error {
	if state == "Merged" {
		_, err := db.Exec(`UPDATE Convoys SET draft_pr_state = ?, shipped_at = datetime('now') WHERE id = ?`,
			state, convoyID)
		return err
	}
	_, err := db.Exec(`UPDATE Convoys SET draft_pr_state = ? WHERE id = ?`, state, convoyID)
	return err
}

// SetConvoyStatus updates the convoy lifecycle status. Separate from status
// transitions driven by individual task completions (AutoRecoverConvoy etc.) —
// used for PR-flow state machine moves: Active → AwaitingDraftPR → DraftPROpen
// → Shipped / Abandoned.
func SetConvoyStatus(db *sql.DB, convoyID int, status string) error {
	_, err := db.Exec(`UPDATE Convoys SET status = ? WHERE id = ?`, status, convoyID)
	return err
}

// SetConvoyStatusTx is the transactional sibling of SetConvoyStatus.
func SetConvoyStatusTx(tx *sql.Tx, convoyID int, status string) error {
	_, err := tx.Exec(`UPDATE Convoys SET status = ? WHERE id = ?`, status, convoyID)
	return err
}

// ActiveConvoysMissingAskBranch returns convoy IDs that are Active but have at
// least one touched repo without a ConvoyAskBranch row. Correctly handles the
// multi-repo case: a convoy with repos [api, monolith] where api has a branch
// but monolith doesn't is still returned (monolith needs backfilling).
//
// Used by the Layer C lazy-backfill inquisitor check to enqueue CreateAskBranch
// tasks. Note: CreateAskBranch itself fans out per-repo, so a single task per
// convoy is sufficient — the query only needs to find convoys where some repo
// is missing, not enumerate which repo.
func ActiveConvoysMissingAskBranch(db *sql.DB) []int {
	rows, err := db.Query(`
		SELECT DISTINCT c.id
		FROM Convoys c
		JOIN BountyBoard b ON b.convoy_id = c.id
		WHERE c.status = 'Active'
		  AND b.type = 'CodeEdit'
		  AND IFNULL(b.target_repo, '') != ''
		  AND b.status != 'Cancelled'
		  AND NOT EXISTS (
		    SELECT 1 FROM ConvoyAskBranches cab
		    WHERE cab.convoy_id = c.id AND cab.repo = b.target_repo
		  )
		ORDER BY c.id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy.go:ActiveConvoysMissingAskBranch: rows iter error: %v", rErr)
	}
	return ids
}

package store

import (
	"log"
	"database/sql"
	"fmt"
	"strings"
)

// protectedAskBranchNames mirrors git.protectedBranchNames at the store
// ingress. Kept in sync manually because store must not import internal/git
// (that would break the existing layering). Any branch name that hits this
// set is rejected at DB-write time, so Fix #0's destructive-op guards never
// see a corrupt "main" row to begin with.
var protectedAskBranchNames = map[string]struct{}{
	"main":       {},
	"master":     {},
	"develop":    {},
	"trunk":      {},
	"production": {},
	"prod":       {},
	"head":       {},
}

// stripRefPrefix unwraps "refs/heads/<name>" / "origin/<name>" before
// comparison so a caller that passes a fully-qualified ref is still caught.
func stripRefPrefix(branch string) string {
	out := strings.TrimSpace(branch)
	for _, p := range []string{"refs/heads/", "refs/remotes/origin/", "origin/"} {
		if strings.HasPrefix(out, p) {
			out = out[len(p):]
		}
	}
	return out
}

// isProtectedAskBranchName returns true for "main"/"master"/... names. See
// protectedAskBranchNames for the authoritative list.
func isProtectedAskBranchName(branch string) bool {
	canonical := strings.ToLower(stripRefPrefix(branch))
	_, denied := protectedAskBranchNames[canonical]
	return denied
}

// ── ConvoyAskBranches CRUD ───────────────────────────────────────────────────
//
// Per-(convoy, repo) ask-branch state. Every repo touched by a convoy has one
// row here. The row is created by Pilot.CreateAskBranch when Pilot first cuts
// the branch, updated when the branch is rebased, and its draft_pr_* fields
// are set by Diplomat when the final PR opens.
//
// The scalar ask_branch / draft_pr_* fields on Convoys predate this table and
// are left in place for backwards compatibility; new code uses this table so
// multi-repo convoys work correctly.

// UpsertConvoyAskBranch records a freshly-cut ask-branch. Returns an error if
// the convoy/repo combo already has an ask-branch stored with a DIFFERENT name
// (would indicate bad state). Overwriting the base SHA on an existing row is
// allowed because a rebase updates it.
func UpsertConvoyAskBranch(db *sql.DB, convoyID int, repo, askBranch, baseSHA string) error {
	if convoyID <= 0 || repo == "" || askBranch == "" || baseSHA == "" {
		return fmt.Errorf("UpsertConvoyAskBranch: all fields required (convoy=%d repo=%q branch=%q sha=%q)",
			convoyID, repo, askBranch, baseSHA)
	}
	// Fix #9: ingress ref validator. Reject branch names that would be
	// re-interpreted as flags by git (CVE-2017-1000117 class) or contain
	// git-check-ref-format-forbidden characters. Runs BEFORE the protected-
	// branch denylist so we fail with the most specific error.
	if err := validateRefName(askBranch); err != nil {
		return fmt.Errorf("UpsertConvoyAskBranch: %w", err)
	}
	// Fix #0: ingress guard. Reject ask_branch="main"/"master"/... at DB-write
	// time so a single manual edit or corrupt migration can't become the
	// DB-supplied input to completeAskBranchResolution's force-push.
	if isProtectedAskBranchName(askBranch) {
		return fmt.Errorf("UpsertConvoyAskBranch: refusing to store ask_branch=%q — matches protected/default branch denylist", askBranch)
	}
	// If a row exists with a different branch name, something is badly wrong —
	// we'd be silently overwriting a branch that may still have open PRs against
	// it. Refuse and force the caller to think about it.
	var existingBranch string
	err := db.QueryRow(`SELECT ask_branch FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`,
		convoyID, repo).Scan(&existingBranch)
	if err == nil && existingBranch != "" && existingBranch != askBranch {
		return fmt.Errorf("UpsertConvoyAskBranch: convoy %d repo %s already has ask-branch %q; refusing to overwrite with %q",
			convoyID, repo, existingBranch, askBranch)
	}

	_, err = db.Exec(`INSERT INTO ConvoyAskBranches
		(convoy_id, repo, ask_branch, ask_branch_base_sha)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(convoy_id, repo) DO UPDATE SET
			ask_branch_base_sha = excluded.ask_branch_base_sha`,
		convoyID, repo, askBranch, baseSHA)
	return err
}

// GetConvoyAskBranch fetches the ask-branch row for a (convoy, repo) pair,
// or nil if none exists yet.
func GetConvoyAskBranch(db *sql.DB, convoyID int, repo string) *ConvoyAskBranch {
	var c ConvoyAskBranch
	err := db.QueryRow(`SELECT
		convoy_id, repo, ask_branch, ask_branch_base_sha,
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		IFNULL(last_rebased_at, ''), IFNULL(created_at, ''),
		IFNULL(stage_id, 0)
		FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`, convoyID, repo).
		Scan(&c.ConvoyID, &c.Repo, &c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.LastRebasedAt, &c.CreatedAt, &c.StageID)
	if err != nil {
		return nil
	}
	return &c
}

// ListConvoyAskBranches returns all ask-branch rows for a convoy (one per repo).
func ListConvoyAskBranches(db *sql.DB, convoyID int) []ConvoyAskBranch {
	rows, err := db.Query(`SELECT
		convoy_id, repo, ask_branch, ask_branch_base_sha,
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		IFNULL(last_rebased_at, ''), IFNULL(created_at, ''),
		IFNULL(stage_id, 0)
		FROM ConvoyAskBranches WHERE convoy_id = ? ORDER BY repo ASC`, convoyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ConvoyAskBranch
	for rows.Next() {
		var c ConvoyAskBranch
		if err := rows.Scan(&c.ConvoyID, &c.Repo, &c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.LastRebasedAt, &c.CreatedAt, &c.StageID); err == nil {
			out = append(out, c)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy_ask_branches.go:ListConvoyAskBranches: rows iter error: %v", rErr)
	}
	return out
}

// ListConvoyAskBranchesByStage returns all ask-branch rows scoped to a single
// stage of a convoy. Used by D5.5 P2 β ConvoyReview per-stage scoping: a
// staged convoy's review at stage N walks only stage-N's ask-branches, never
// the cross-stage union. Returns an empty slice if stageID is zero or no
// matching rows exist.
func ListConvoyAskBranchesByStage(db *sql.DB, convoyID, stageID int) []ConvoyAskBranch {
	if stageID <= 0 {
		return nil
	}
	rows, err := db.Query(`SELECT
		convoy_id, repo, ask_branch, ask_branch_base_sha,
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		IFNULL(last_rebased_at, ''), IFNULL(created_at, ''),
		IFNULL(stage_id, 0)
		FROM ConvoyAskBranches WHERE convoy_id = ? AND stage_id = ?
		ORDER BY repo ASC`, convoyID, stageID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ConvoyAskBranch
	for rows.Next() {
		var c ConvoyAskBranch
		if err := rows.Scan(&c.ConvoyID, &c.Repo, &c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.LastRebasedAt, &c.CreatedAt, &c.StageID); err == nil {
			out = append(out, c)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy_ask_branches.go:ListConvoyAskBranchesByStage: rows iter error: %v", rErr)
	}
	return out
}

// ListAllConvoyAskBranches returns every ask-branch row in the DB. Used by
// main-drift-watch to enumerate active integration branches across all convoys.
func ListAllConvoyAskBranches(db *sql.DB) []ConvoyAskBranch {
	rows, err := db.Query(`SELECT
		convoy_id, repo, ask_branch, ask_branch_base_sha,
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		IFNULL(last_rebased_at, ''), IFNULL(created_at, ''),
		IFNULL(stage_id, 0)
		FROM ConvoyAskBranches ORDER BY convoy_id, repo`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ConvoyAskBranch
	for rows.Next() {
		var c ConvoyAskBranch
		if err := rows.Scan(&c.ConvoyID, &c.Repo, &c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.LastRebasedAt, &c.CreatedAt, &c.StageID); err == nil {
			out = append(out, c)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy_ask_branches.go:ListAllConvoyAskBranches: rows iter error: %v", rErr)
	}
	return out
}

// UpdateConvoyAskBranchBase updates the stored base SHA and last_rebased_at after
// a successful rebase. Leaves the branch name and PR fields alone.
func UpdateConvoyAskBranchBase(db *sql.DB, convoyID int, repo, newBaseSHA string) error {
	if newBaseSHA == "" {
		return fmt.Errorf("UpdateConvoyAskBranchBase: newBaseSHA required")
	}
	_, err := db.Exec(`UPDATE ConvoyAskBranches
		SET ask_branch_base_sha = ?, last_rebased_at = datetime('now')
		WHERE convoy_id = ? AND repo = ?`,
		newBaseSHA, convoyID, repo)
	return err
}

// UpdateConvoyAskBranchBaseTx is the transactional sibling of UpdateConvoyAskBranchBase.
func UpdateConvoyAskBranchBaseTx(tx *sql.Tx, convoyID int, repo, newBaseSHA string) error {
	if newBaseSHA == "" {
		return fmt.Errorf("UpdateConvoyAskBranchBaseTx: newBaseSHA required")
	}
	_, err := tx.Exec(`UPDATE ConvoyAskBranches
		SET ask_branch_base_sha = ?, last_rebased_at = datetime('now')
		WHERE convoy_id = ? AND repo = ?`,
		newBaseSHA, convoyID, repo)
	return err
}

// SetConvoyAskBranchDraftPR records the draft PR opened by Diplomat for a
// (convoy, repo). state should be "Open" at creation time.
func SetConvoyAskBranchDraftPR(db *sql.DB, convoyID int, repo, url string, number int, state string) error {
	_, err := db.Exec(`UPDATE ConvoyAskBranches
		SET draft_pr_url = ?, draft_pr_number = ?, draft_pr_state = ?
		WHERE convoy_id = ? AND repo = ?`,
		url, number, state, convoyID, repo)
	return err
}

// UpdateConvoyAskBranchDraftState transitions a (convoy, repo)'s draft PR state.
// When state == "Merged", also stamps shipped_at.
func UpdateConvoyAskBranchDraftState(db *sql.DB, convoyID int, repo, state string) error {
	if state == "Merged" {
		_, err := db.Exec(`UPDATE ConvoyAskBranches
			SET draft_pr_state = ?, shipped_at = datetime('now')
			WHERE convoy_id = ? AND repo = ?`, state, convoyID, repo)
		return err
	}
	_, err := db.Exec(`UPDATE ConvoyAskBranches SET draft_pr_state = ? WHERE convoy_id = ? AND repo = ?`,
		state, convoyID, repo)
	return err
}

// DeleteConvoyAskBranch removes a (convoy, repo) row. Called by CleanupAskBranch
// after the branch has been deleted from origin.
func DeleteConvoyAskBranch(db *sql.DB, convoyID int, repo string) error {
	_, err := db.Exec(`DELETE FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`, convoyID, repo)
	return err
}

// ConvoyReposTouched returns the distinct target_repo values for a convoy's
// CodeEdit tasks — the set of repos that need ask-branches. Excludes empty
// repos (non-code tasks) and Cancelled tasks. Used by Pilot's CreateAskBranch
// handler to fan out per-repo branch creation.
// ConvoyReadyToShip returns true iff the convoy is actually waiting on an
// operator "Ship It" click — not merely that the draft PR exists.
//
// The distinction matters: `Convoys.status = 'DraftPROpen'` is set the moment
// Diplomat opens the draft PR against main, which is usually BEFORE the
// fleet's self-healing work (ConvoyReview fix tasks, rebase conflicts, bot
// review comments) has finished. A convoy with 8 pending CodeEdits is
// technically DraftPROpen but is obviously not ready to ship.
//
// Ready iff ALL of:
//   1. Convoys.status = 'DraftPROpen'
//   2. Zero non-terminal tasks with convoy_id = convoyID (catches CodeEdits,
//      REBASE_CONFLICT resolvers, ConvoyReview fix tasks, CIFailureTriage,
//      etc.)
//   3. Zero Pending/Locked ConvoyReview tasks scoped to this convoy. Post-
//      Fix A (AUDIT-011 read-side), QueueConvoyReview stamps convoy_id on
//      the row so this check collapses into an indexed equality predicate.
//
// Condition (3) catches the brief window between "convoy looks quiescent"
// and "dog fires a new review" — treated as not-ready until the review
// completes. Post-Fix A (AUDIT-011 read-side) the convoy_id column on the
// ConvoyReview row makes this lookup an index probe; the active-tasks check
// (condition 2) already subsumes non-ConvoyReview rows.
func ConvoyReadyToShip(db *sql.DB, convoyID int) bool {
	if convoyID <= 0 {
		return false
	}
	var status string
	if err := db.QueryRow(`SELECT IFNULL(status,'') FROM Convoys WHERE id = ?`, convoyID).Scan(&status); err != nil {
		return false
	}
	if status != "DraftPROpen" {
		return false
	}
	var active int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE convoy_id = ?
		  AND status NOT IN ('Completed','Cancelled','Failed')`, convoyID).Scan(&active)
	if active > 0 {
		return false
	}
	// Fix A (AUDIT-011 read-side): structured convoy_id column.
	// QueueConvoyReview stamps convoy_id on the row, so this lookup uses
	// idx_bounty_convoy_status rather than a payload-LIKE full-table scan.
	var reviewPending int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'ConvoyReview'
		  AND status IN ('Pending','Locked')
		  AND convoy_id = ?`,
		convoyID).Scan(&reviewPending)
	return reviewPending == 0
}

// ListReadyToShipConvoyIDs returns the IDs of every convoy whose self-healing
// work is done and which is waiting on an operator Ship It click. Batch form
// of ConvoyReadyToShip used by the dashboard's /api/status count.
//
// Fix A (AUDIT-011 read-side): the inner NOT EXISTS ConvoyReview gate now
// uses the structured convoy_id column. QueueConvoyReview stamps it on the
// row, and once a task like that is subsumed by the active-tasks NOT EXISTS
// above, the ConvoyReview clause is technically redundant for the already-
// subsumed case but kept distinct because ConvoyReview rows can precede
// their convoy's quiescence (see invariant (3) of ConvoyReadyToShip).
func ListReadyToShipConvoyIDs(db *sql.DB) []int {
	rows, err := db.Query(`
		SELECT c.id FROM Convoys c
		WHERE c.status = 'DraftPROpen'
		  AND NOT EXISTS (
		    SELECT 1 FROM BountyBoard b
		    WHERE b.convoy_id = c.id
		      AND b.status NOT IN ('Completed','Cancelled','Failed')
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM BountyBoard r
		    WHERE r.type = 'ConvoyReview'
		      AND r.status IN ('Pending','Locked')
		      AND r.convoy_id = c.id
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
		log.Printf("convoy_ask_branches.go:ListReadyToShipConvoyIDs: rows iter error: %v", rErr)
	}
	return ids
}

// HasActiveAskBranchConflict returns true when the convoy has a non-terminal
// REBASE_CONFLICT CodeEdit pinned to the ask-branch itself (spawned by Pilot
// in pilot_rebase.go, payload starts with "[REBASE_CONFLICT for convoy #N").
// Callers that spawn new CodeEdits into a convoy should gate on this — piling
// work onto an ask-branch whose tip is still broken creates cascading conflicts
// for every task that touches the same files.
func HasActiveAskBranchConflict(db *sql.DB, convoyID int) bool {
	if convoyID <= 0 {
		return false
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE convoy_id = ?
		  AND type = 'CodeEdit'
		  AND status NOT IN ('Completed','Cancelled','Failed')
		  AND payload LIKE '[REBASE_CONFLICT for convoy #' || ? || ' %'`,
		convoyID, convoyID).Scan(&n)
	return n > 0
}

func ConvoyReposTouched(db *sql.DB, convoyID int) []string {
	rows, err := db.Query(`SELECT DISTINCT target_repo FROM BountyBoard
		WHERE convoy_id = ? AND type = 'CodeEdit' AND IFNULL(target_repo, '') != '' AND status != 'Cancelled'
		ORDER BY target_repo ASC`, convoyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var repos []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err == nil {
			repos = append(repos, r)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy_ask_branches.go:ConvoyReposTouched: rows iter error: %v", rErr)
	}
	return repos
}

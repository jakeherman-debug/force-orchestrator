package store

import (
	"database/sql"
	"fmt"
)

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
		IFNULL(last_rebased_at, ''), IFNULL(created_at, '')
		FROM ConvoyAskBranches WHERE convoy_id = ? AND repo = ?`, convoyID, repo).
		Scan(&c.ConvoyID, &c.Repo, &c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.LastRebasedAt, &c.CreatedAt)
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
		IFNULL(last_rebased_at, ''), IFNULL(created_at, '')
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
			&c.LastRebasedAt, &c.CreatedAt); err == nil {
			out = append(out, c)
		}
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
		IFNULL(last_rebased_at, ''), IFNULL(created_at, '')
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
			&c.LastRebasedAt, &c.CreatedAt); err == nil {
			out = append(out, c)
		}
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
	return repos
}

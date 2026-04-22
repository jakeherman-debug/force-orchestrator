package dashboard

import (
	"database/sql"
	"fmt"
	"strings"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// dashboardShipConvoy promotes every draft PR for a convoy to ready-for-review.
// Does NOT merge — merge-on-ship is an operator choice exposed via the CLI
// (`force convoy ship <id> --merge squash`). From the dashboard, the button's
// safest default is "promote to ready and let CI / human review do the rest".
//
// Returns (numberPromoted, error).
func dashboardShipConvoy(db *sql.DB, convoyID int) (int, error) {
	convoy := store.GetConvoy(db, convoyID)
	if convoy == nil {
		return 0, fmt.Errorf("convoy %d not found", convoyID)
	}
	if convoy.Status != "DraftPROpen" {
		return 0, fmt.Errorf("convoy %d is not in DraftPROpen state (status=%s)", convoyID, convoy.Status)
	}

	branches := store.ListConvoyAskBranches(db, convoyID)
	ghc := gh.NewClient()
	var promoted int
	var failures []string
	for _, ab := range branches {
		if ab.DraftPRNumber == 0 {
			continue
		}
		repo := store.GetRepo(db, ab.Repo)
		var cwd, ghRepo string
		if repo != nil {
			cwd = repo.LocalPath
			ghRepo = deriveGHRepoForDashboard(repo.RemoteURL)
		}
		if err := ghc.PRReady(cwd, ghRepo, ab.DraftPRNumber); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ab.Repo, err))
			continue
		}
		promoted++
	}
	store.LogAudit(db, "dashboard", "convoy-ship", convoyID,
		fmt.Sprintf("promoted=%d failures=%d", promoted, len(failures)))
	if len(failures) > 0 {
		return promoted, fmt.Errorf("partial: %s", strings.Join(failures, "; "))
	}
	return promoted, nil
}

// ShipSummaryAskBranch holds per-repo ask-branch info for the ship-it modal.
type ShipSummaryAskBranch struct {
	Repo          string `json:"repo"`
	AskBranch     string `json:"ask_branch"`
	DraftPRURL    string `json:"draft_pr_url"`
	DraftPRNumber int    `json:"draft_pr_number"`
	DraftPRState  string `json:"draft_pr_state"`
}

// ShipSummarySubPRRollup aggregates CI state across all sub-PRs.
type ShipSummarySubPRRollup struct {
	Open      int `json:"open"`
	Merged    int `json:"merged"`
	Closed    int `json:"closed"`
	CIPending int `json:"ci_pending"`
	CISuccess int `json:"ci_success"`
	CIFailure int `json:"ci_failure"`
}

// ShipSummaryTaskStats holds task completion counts for the convoy.
type ShipSummaryTaskStats struct {
	Completed int `json:"completed"`
	Total     int `json:"total"`
}

// ShipSummaryResponse is the payload for GET /api/convoys/{id}/ship-summary.
type ShipSummaryResponse struct {
	ConvoyID     int                    `json:"convoy_id"`
	ConvoyName   string                 `json:"convoy_name"`
	ConvoyStatus string                 `json:"convoy_status"`
	AskBranches  []ShipSummaryAskBranch `json:"ask_branches"`
	SubPRRollup  ShipSummarySubPRRollup `json:"sub_pr_rollup"`
	TaskStats    ShipSummaryTaskStats   `json:"task_stats"`
}

// buildShipSummary assembles a ShipSummaryResponse for the given convoy.
// Returns (response, httpStatusCode, error).
func buildShipSummary(db *sql.DB, convoyID int) (*ShipSummaryResponse, int, error) {
	convoy := store.GetConvoy(db, convoyID)
	if convoy == nil {
		return nil, 404, fmt.Errorf("convoy %d not found", convoyID)
	}
	if convoy.Status != "DraftPROpen" {
		return nil, 400, fmt.Errorf("convoy %d is not in DraftPROpen state (status=%s)", convoyID, convoy.Status)
	}

	branches := store.ListConvoyAskBranches(db, convoyID)
	askBranches := make([]ShipSummaryAskBranch, 0, len(branches))
	for _, ab := range branches {
		askBranches = append(askBranches, ShipSummaryAskBranch{
			Repo:          ab.Repo,
			AskBranch:     ab.AskBranch,
			DraftPRURL:    ab.DraftPRURL,
			DraftPRNumber: ab.DraftPRNumber,
			DraftPRState:  ab.DraftPRState,
		})
	}

	rollup := store.RollupAskBranchPRs(db, convoyID)
	subPRRollup := ShipSummarySubPRRollup{
		Open:      rollup.Open,
		Merged:    rollup.Merged,
		Closed:    rollup.Closed,
		CIPending: rollup.ChecksPending,
		CISuccess: rollup.ChecksSuccess,
		CIFailure: rollup.ChecksFailure,
	}

	var completed, total int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ?`, convoyID).Scan(&total)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND status = 'Completed'`, convoyID).Scan(&completed)

	return &ShipSummaryResponse{
		ConvoyID:     convoy.ID,
		ConvoyName:   convoy.Name,
		ConvoyStatus: convoy.Status,
		AskBranches:  askBranches,
		SubPRRollup:  subPRRollup,
		TaskStats:    ShipSummaryTaskStats{Completed: completed, Total: total},
	}, 200, nil
}

// deriveGHRepoForDashboard mirrors the owner/name extraction used elsewhere.
// Kept local to avoid cross-package dependency.
func deriveGHRepoForDashboard(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}
	if strings.HasPrefix(remoteURL, "git@") {
		if idx := strings.Index(remoteURL, ":"); idx > 0 {
			return strings.TrimSuffix(remoteURL[idx+1:], ".git")
		}
		return ""
	}
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(remoteURL, scheme) {
			rest := strings.TrimPrefix(remoteURL, scheme)
			if idx := strings.Index(rest, "/"); idx > 0 {
				return strings.TrimSuffix(rest[idx+1:], ".git")
			}
		}
	}
	return ""
}

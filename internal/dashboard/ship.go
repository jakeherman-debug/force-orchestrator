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

package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// DiffFileStat holds per-file addition/deletion counts extracted from a unified diff.
type DiffFileStat struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// AskBranchDiffSummary holds the diff statistics for one ask-branch.
type AskBranchDiffSummary struct {
	AskBranch      string         `json:"ask_branch"`
	DraftPRNumber  int            `json:"draft_pr_number"`
	DraftPRURL     string         `json:"draft_pr_url"`
	Files          []DiffFileStat `json:"files"`
	TotalAdditions int            `json:"total_additions"`
	TotalDeletions int            `json:"total_deletions"`
}

// ConvoyDiffSummaryResponse is the JSON envelope for GET /api/convoys/{id}/diff-summary.
type ConvoyDiffSummaryResponse struct {
	AskBranches []AskBranchDiffSummary `json:"ask_branches"`
}

// parseDiffStats parses a unified diff and returns per-file addition/deletion counts.
// Lines beginning with "+" (but not "+++") are additions; lines beginning with "-"
// (but not "---") are deletions.
func parseDiffStats(diff string) []DiffFileStat {
	var files []DiffFileStat
	var current *DiffFileStat
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if current != nil {
				files = append(files, *current)
			}
			parts := strings.Fields(line)
			path := ""
			if len(parts) == 4 {
				path = strings.TrimPrefix(parts[3], "b/")
			}
			current = &DiffFileStat{Path: path}
		} else if current != nil {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				current.Additions++
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				current.Deletions++
			}
		}
	}
	if current != nil {
		files = append(files, *current)
	}
	return files
}

// handleConvoyDiffSummary handles GET /api/convoys/{id}/diff-summary.
// It iterates over the convoy's ask-branches, calls GetDiff for each branch
// in its repo's worktree, and returns per-file and totals addition/deletion counts.
func handleConvoyDiffSummary(db *sql.DB, convoyID int, w http.ResponseWriter) {
	askBranches := store.ListConvoyAskBranches(db, convoyID)

	result := make([]AskBranchDiffSummary, 0, len(askBranches))
	for _, ab := range askBranches {
		repoPath := store.GetRepoPath(db, ab.Repo)
		if repoPath == "" {
			continue
		}
		diff := igit.GetDiff(repoPath, ab.AskBranch)
		files := parseDiffStats(diff)
		if files == nil {
			files = []DiffFileStat{}
		}
		var totalAdd, totalDel int
		for _, f := range files {
			totalAdd += f.Additions
			totalDel += f.Deletions
		}
		result = append(result, AskBranchDiffSummary{
			AskBranch:      ab.AskBranch,
			DraftPRNumber:  ab.DraftPRNumber,
			DraftPRURL:     ab.DraftPRURL,
			Files:          files,
			TotalAdditions: totalAdd,
			TotalDeletions: totalDel,
		})
	}

	json.NewEncoder(w).Encode(ConvoyDiffSummaryResponse{AskBranches: result})
}

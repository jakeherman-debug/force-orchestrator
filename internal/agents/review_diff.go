package agents

import (
	"database/sql"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// reviewDiff returns the diff a reviewer (Captain, Council, Medic) should
// evaluate for a single astromech task. The rule:
//
//   - If the task belongs to a convoy AND that convoy has an ask-branch for
//     the task's repo, diff the agent branch against the ask-branch tip
//     (three-dot via `origin/<askbranch>`). The agent branched off the
//     ask-branch, so only changes unique to THIS task show up.
//   - Otherwise (legacy / pre-PR-flow path), fall back to diffing against
//     main. That's the correct baseline when there's no ask-branch.
//
// This closes the root cause behind the convoy 35/37 failure cascade: the
// three-dot diff against main showed 7+ hours of main's drift as "phantom
// additions," which the Captain interpreted as scope violations and the
// ConvoyReview interpreted as gaps — neither of which actually existed.
//
// For performance, all lookups stay in the store package; no git subprocess
// other than the single diff command is added.
func reviewDiff(db *sql.DB, repoPath string, b *store.Bounty) string {
	if b == nil || repoPath == "" || b.BranchName == "" {
		return ""
	}
	if b.ConvoyID > 0 {
		if ab := store.GetConvoyAskBranch(db, b.ConvoyID, b.TargetRepo); ab != nil && ab.AskBranch != "" {
			return igit.GetDiffFromBase(repoPath, "origin/"+ab.AskBranch, b.BranchName)
		}
	}
	return igit.GetDiff(repoPath, b.BranchName)
}

// reviewCommitsAhead mirrors reviewDiff but returns one-line log output —
// used by auto-complete checks ("is there any net-new work here?"). Same
// base-selection rule.
func reviewCommitsAhead(db *sql.DB, repoPath string, b *store.Bounty) string {
	if b == nil || repoPath == "" || b.BranchName == "" {
		return ""
	}
	if b.ConvoyID > 0 {
		if ab := store.GetConvoyAskBranch(db, b.ConvoyID, b.TargetRepo); ab != nil && ab.AskBranch != "" {
			return igit.CommitsAheadOf(repoPath, "origin/"+ab.AskBranch, b.BranchName)
		}
	}
	return igit.CommitsAhead(repoPath, b.BranchName)
}

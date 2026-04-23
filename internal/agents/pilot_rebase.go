package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"force-orchestrator/internal/gh"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// ── Pilot — RebaseAskBranch ────────────────────────────────────────────────
//
// Queued by the main-drift-watch dog when an ask-branch's stored base_sha
// differs from origin's current HEAD. Pilot:
//
//   1. rebases the ask-branch onto origin/<default>
//   2. on clean rebase: force-pushes and updates stored base_sha
//   3. on conflict: spawns a RebaseConflict CodeEdit task for an astromech
//      to resolve, then marks the Pilot task Completed (the actual resolution
//      is the astromech's job; Pilot's retry cap lives on the conflict chain)

// maxDriftBehind — hard limit on how many commits an ask-branch may fall behind
// main before Pilot gives up and escalates instead of rebasing. A convoy that
// far behind has a scoping problem.
const maxDriftBehind = 50

type rebasePayload struct {
	ConvoyID int    `json:"convoy_id"`
	Repo     string `json:"repo"`
}

// QueueRebaseAskBranch enqueues a Pilot rebase task for a (convoy, repo).
// Idempotent at the caller level: main-drift-watch checks for existing
// Pending/Locked tasks before queueing another.
func QueueRebaseAskBranch(db *sql.DB, convoyID int, repo string) (int, error) {
	if convoyID <= 0 || repo == "" {
		return 0, fmt.Errorf("QueueRebaseAskBranch: convoyID and repo required")
	}
	payload, _ := json.Marshal(rebasePayload{ConvoyID: convoyID, Repo: repo})
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'RebaseAskBranch', 'Pending', ?, 3, datetime('now'))`,
		repo, string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

func runRebaseAskBranch(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload rebasePayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	ab := store.GetConvoyAskBranch(db, payload.ConvoyID, payload.Repo)
	if ab == nil {
		// Nothing to rebase — the ask-branch was cleaned up between queue and run.
		logger.Printf("RebaseAskBranch #%d: convoy %d repo %s has no ask-branch — completing as no-op",
			bounty.ID, payload.ConvoyID, payload.Repo)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}
	repo := store.GetRepo(db, payload.Repo)
	if repo == nil || repo.LocalPath == "" {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("repo %s not registered or missing local_path", payload.Repo))
		return
	}

	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	// Drift guard: if the branch is dramatically behind main, escalate rather
	// than attempt a mega-rebase that's likely to conflict on dozens of files.
	if behind, ahead, err := countCommitsAheadBehind(repo.LocalPath, ab.AskBranch, defaultBranch); err == nil {
		_ = ahead
		if behind > maxDriftBehind {
			escMsg := fmt.Sprintf("Ask-branch %s on %s is %d commits behind %s (limit %d) — convoy needs manual intervention",
				ab.AskBranch, payload.Repo, behind, defaultBranch, maxDriftBehind)
			logger.Printf("RebaseAskBranch #%d: %s", bounty.ID, escMsg)
			store.SendMail(db, "Pilot", "operator",
				fmt.Sprintf("[DRIFT LIMIT] Convoy #%d repo %s too far behind main", payload.ConvoyID, payload.Repo),
				escMsg, bounty.ID, store.MailTypeAlert)
			store.FailBounty(db, bounty.ID, escMsg)
			return
		}
	}

	newTip, rebaseErr := igit.RebaseBranchOnto(repo.LocalPath, ab.AskBranch, defaultBranch)
	if rebaseErr != nil {
		// Conflict — spawn a RebaseConflict CodeEdit task on the ask-branch
		// itself (not an agent branch). The astromech resolves conflict
		// markers and commits directly onto the ask-branch. When Jedi Council
		// approves, openSubPRForApprovedTask detects branch_name == ask-branch
		// and takes the ask-branch-resolution path (force-push + SHA update,
		// no sub-PR). Pilot's job ends here; the follow-up is driven through
		// the normal astromech → council flow.
		logger.Printf("RebaseAskBranch #%d: rebase conflict — spawning RebaseConflict task", bounty.ID)
		idKey := "rebase-conflict:askbranch:" + ab.AskBranch
		conflictPayload := fmt.Sprintf("[REBASE_CONFLICT for convoy #%d repo %s]\n\nAsk-branch: %s\nBase branch: %s\n\nThe rebase onto %s conflicted. Merge %s into %s, resolve conflict markers, and commit. The ask-branch will be force-pushed after council review.",
			payload.ConvoyID, payload.Repo, ab.AskBranch, defaultBranch, defaultBranch, defaultBranch, ab.AskBranch)
		conflictTaskID, existed, addErr := store.AddConvoyTaskIdempotent(db, idKey, bounty.ID, payload.Repo, conflictPayload, payload.ConvoyID, 5, "Pending")
		if addErr != nil {
			store.FailBounty(db, bounty.ID, fmt.Sprintf("queue ask-branch conflict: %v", addErr))
			return
		}
		if existed {
			logger.Printf("RebaseAskBranch #%d: conflict — reusing existing task #%d on %s", bounty.ID, conflictTaskID, ab.AskBranch)
			store.UpdateBountyStatus(db, bounty.ID, "Completed")
			return
		}
		// Stamp the branch name so the astromech resumes directly on the ask-branch.
		store.SetBranchName(db, conflictTaskID, ab.AskBranch)
		store.SendMail(db, "Pilot", "astromech",
			fmt.Sprintf("[REBASE CONFLICT] Task #%d — resolve and commit on %s", conflictTaskID, ab.AskBranch),
			fmt.Sprintf("A rebase of %s onto %s produced conflicts. Merge %s into the ask-branch, resolve the markers, and commit. Council review will approve and Pilot-equivalent logic will push to origin.\n\nUnderlying error:\n%v",
				ab.AskBranch, defaultBranch, defaultBranch, rebaseErr),
			conflictTaskID, store.MailTypeFeedback)
		// Pilot's job on conflict is done — astromech takes over.
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		store.LogAudit(db, "Pilot", "rebase-conflict", bounty.ID,
			fmt.Sprintf("spawned RebaseConflict task #%d", conflictTaskID))
		return
	}

	// Clean rebase — force-push with lease, update stored base SHA.
	if pushErr := igit.ForcePushBranch(repo.LocalPath, ab.AskBranch); pushErr != nil {
		cls := gh.ClassifyError(pushErr.Error())
		if cls.ShouldRetry() {
			// Put the task back to Pending so the next Pilot tick retries.
			// If the UPDATE itself fails, fall through to FailBounty below so
			// the task doesn't get stuck Locked — better a clean Failed than
			// a phantom-lock that the stale-lock detector has to clean up.
			if _, err := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '', error_log = ? WHERE id = ?`,
				fmt.Sprintf("transient push error (class=%s): %v", cls, pushErr), bounty.ID); err != nil {
				logger.Printf("RebaseAskBranch #%d: requeue UPDATE failed (%v) — failing task instead", bounty.ID, err)
				store.FailBounty(db, bounty.ID, fmt.Sprintf("force-push failed and requeue failed: %v / %v", pushErr, err))
				return
			}
			logger.Printf("RebaseAskBranch #%d: transient push error (class=%s) — requeued", bounty.ID, cls)
			return
		}
		store.FailBounty(db, bounty.ID, fmt.Sprintf("force-push failed (class=%s): %v", cls, pushErr))
		return
	}

	if err := store.UpdateConvoyAskBranchBase(db, payload.ConvoyID, payload.Repo, newTip); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("failed to record new base SHA: %v", err))
		return
	}
	logger.Printf("RebaseAskBranch #%d: convoy %d repo %s rebased onto %s, new tip %s",
		bounty.ID, payload.ConvoyID, payload.Repo, defaultBranch, newTip[:minInt(8, len(newTip))])
	store.UpdateBountyStatus(db, bounty.ID, "Completed")
}

// countCommitsAheadBehind returns (behind, ahead, error) — how many commits
// branch is behind / ahead of base, using `git rev-list --count --left-right`.
// Sscanf errors propagate as an error so a malformed git output can't silently
// leave behind=0 and bypass the drift-cap escalation.
func countCommitsAheadBehind(repoPath, branch, base string) (behind, ahead int, err error) {
	out, err := igit.RunCmd(repoPath, "rev-list", "--count", "--left-right",
		"refs/remotes/origin/"+base+"..."+branch)
	if err != nil {
		return 0, 0, fmt.Errorf("rev-list: %s", strings.TrimSpace(out))
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("rev-list: unexpected output %q", out)
	}
	if _, scanErr := fmt.Sscanf(parts[0], "%d", &behind); scanErr != nil {
		return 0, 0, fmt.Errorf("rev-list: parse behind=%q: %w", parts[0], scanErr)
	}
	if _, scanErr := fmt.Sscanf(parts[1], "%d", &ahead); scanErr != nil {
		return 0, 0, fmt.Errorf("rev-list: parse ahead=%q: %w", parts[1], scanErr)
	}
	return
}

// ── main-drift-watch dog ─────────────────────────────────────────────────────

// dogMainDriftWatch is the per-cycle detector for ask-branch drift against main.
// Cheap: one `git ls-remote` per ask-branch (milliseconds). Heavy work (the
// rebase) only runs when we've actually detected a SHA change, so idle repos
// cost almost nothing.
func dogMainDriftWatch(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	rows := store.ListAllConvoyAskBranches(db)
	if len(rows) == 0 {
		return nil
	}

	// Group rows by repo so we only ls-remote each repo once per cycle.
	type repoKey struct{ repoName, localPath string }
	checkedHeadSHA := map[repoKey]string{}
	for _, ab := range rows {
		repoCfg := store.GetRepo(db, ab.Repo)
		if repoCfg == nil || repoCfg.LocalPath == "" {
			continue
		}
		key := repoKey{ab.Repo, repoCfg.LocalPath}
		headSHA, cached := checkedHeadSHA[key]
		if !cached {
			sha, err := igit.RemoteHeadSHA(repoCfg.LocalPath)
			if err != nil {
				logger.Printf("main-drift-watch: ls-remote %s failed: %v", ab.Repo, err)
				continue
			}
			headSHA = sha
			checkedHeadSHA[key] = headSHA
		}
		if headSHA == ab.AskBranchBaseSHA {
			continue // no drift
		}
		// Drift detected — but only queue if not already queued/in-flight.
		// Boundary-match on convoy_id so id=1 doesn't dedup against 10, 100, etc.
		var existing int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE type = 'RebaseAskBranch' AND status IN ('Pending', 'Locked')
			  AND target_repo = ?
			  AND (payload LIKE '%"convoy_id":' || ? || ',%'
			    OR payload LIKE '%"convoy_id":' || ? || '}%')`,
			ab.Repo, ab.ConvoyID, ab.ConvoyID).Scan(&existing)
		if existing > 0 {
			continue
		}
		id, qErr := QueueRebaseAskBranch(db, ab.ConvoyID, ab.Repo)
		if qErr != nil {
			logger.Printf("main-drift-watch: queue failed for convoy %d repo %s: %v",
				ab.ConvoyID, ab.Repo, qErr)
			continue
		}
		logger.Printf("main-drift-watch: convoy %d repo %s drifted (origin=%s stored=%s) — queued RebaseAskBranch #%d",
			ab.ConvoyID, ab.Repo, headSHA[:minInt(8, len(headSHA))],
			ab.AskBranchBaseSHA[:minInt(8, len(ab.AskBranchBaseSHA))], id)
	}
	return nil
}

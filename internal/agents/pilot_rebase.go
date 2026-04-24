package agents

import (
	"context"
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

// maxAskBranchConflicts caps the serial ask-branch rebase-conflict retries per
// (convoy, repo) pair. Idempotency dedup alone only suppresses concurrent
// spawns; nothing stopped a conflict CodeEdit from terminating Failed, freeing
// the key, and letting the next 15-min main-drift-watch tick queue ANOTHER
// rebase that produced the same conflict. Past this cap, Pilot stops spawning
// rebases and escalates — each Claude cycle costs real money and the
// idempotency-key-only path burned hourly cycles per stuck convoy
// (Fix #6, AUDIT-028, AUDIT-119).
const maxAskBranchConflicts = 3

type rebasePayload struct {
	ConvoyID int    `json:"convoy_id"`
	Repo     string `json:"repo"`
}

// QueueRebaseAskBranch enqueues a Pilot rebase task for a (convoy, repo).
// Idempotent (Fix #3, AUDIT-035): canonical key `rebase-askbranch:<convoy>:<repo>`
// via idx_bounty_idem. Two concurrent main-drift-watch ticks observing the
// same drift window cannot both insert a task — the loser sees DO NOTHING
// fire and picks up the existing row.
func QueueRebaseAskBranch(db *sql.DB, convoyID int, repo string) (int, error) {
	if convoyID <= 0 || repo == "" {
		return 0, fmt.Errorf("QueueRebaseAskBranch: convoyID and repo required")
	}
	payload, _ := json.Marshal(rebasePayload{ConvoyID: convoyID, Repo: repo})
	key := fmt.Sprintf("rebase-askbranch:%d:%s", convoyID, repo)
	id, _, err := store.AddIdempotentTask(db, key,
		0, repo, "RebaseAskBranch", string(payload), convoyID, 3, "Pending")
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Fix #8e: ctx threads from SpawnPilot's claim loop so a hung fetch/rebase/
// push during ask-branch rebase cancels on daemon shutdown.
func runRebaseAskBranch(ctx context.Context, db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload rebasePayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); fbErr != nil {
			logger.Printf("RebaseAskBranch #%d: FailBounty after invalid payload failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	ab := store.GetConvoyAskBranch(db, payload.ConvoyID, payload.Repo)
	if ab == nil {
		// Nothing to rebase — the ask-branch was cleaned up between queue and run.
		logger.Printf("RebaseAskBranch #%d: convoy %d repo %s has no ask-branch — completing as no-op",
			bounty.ID, payload.ConvoyID, payload.Repo)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RebaseAskBranch #%d: failed to mark Completed after no-op (convoy %d repo %s): %v — main-drift-watch will retry", bounty.ID, payload.ConvoyID, payload.Repo, err)
		}
		return
	}
	repo := store.GetRepo(db, payload.Repo)
	if repo == nil || repo.LocalPath == "" {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("repo %s not registered or missing local_path", payload.Repo)); fbErr != nil {
			logger.Printf("RebaseAskBranch #%d: FailBounty after repo-not-registered failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	// Fix #0: CLAUDE.md's worktree-isolation invariant requires
	// GetDefaultBranch(repo.LocalPath) here. Hardcoding "main" looped forever
	// on master-default repos: a rebase onto a nonexistent "main" ref failed,
	// main-drift-watch re-queued, repeat.
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = igit.GetDefaultBranch(ctx, repo.LocalPath)
	}

	// Drift guard: if the branch is dramatically behind main, escalate rather
	// than attempt a mega-rebase that's likely to conflict on dozens of files.
	if behind, ahead, err := countCommitsAheadBehind(ctx, repo.LocalPath, ab.AskBranch, defaultBranch); err == nil {
		_ = ahead
		if behind > maxDriftBehind {
			escMsg := fmt.Sprintf("Ask-branch %s on %s is %d commits behind %s (limit %d) — convoy needs manual intervention",
				ab.AskBranch, payload.Repo, behind, defaultBranch, maxDriftBehind)
			logger.Printf("RebaseAskBranch #%d: %s", bounty.ID, escMsg)
			store.SendMail(db, "Pilot", "operator",
				fmt.Sprintf("[DRIFT LIMIT] Convoy #%d repo %s too far behind main", payload.ConvoyID, payload.Repo),
				escMsg, bounty.ID, store.MailTypeAlert)
			if fbErr := store.FailBounty(db, bounty.ID, escMsg); fbErr != nil {
				logger.Printf("RebaseAskBranch #%d: FailBounty after drift-limit (convoy %d repo %s behind=%d) failed: %v — stale-lock detector will recover", bounty.ID, payload.ConvoyID, payload.Repo, behind, fbErr)
			}
			return
		}
	}

	// Fix #6 (AUDIT-028): refuse to burn another Claude conflict-resolution
	// cycle if we've already spent maxAskBranchConflicts on this ask-branch.
	// Escalate once; subsequent main-drift-watch ticks will see the same
	// counter value and dogMainDriftWatch's cap check (also Fix #6) keeps
	// them from enqueueing duplicate Pilot tasks.
	if attempts := store.GetFailedRebaseAttempts(db, payload.ConvoyID, payload.Repo); attempts >= maxAskBranchConflicts {
		escMsg := fmt.Sprintf("Ask-branch %s on %s has exhausted the conflict-retry cap (%d failed rebase-conflict attempts) — human review required",
			ab.AskBranch, payload.Repo, attempts)
		logger.Printf("RebaseAskBranch #%d: %s", bounty.ID, escMsg)
		if _, escErr := CreateEscalation(db, bounty.ID, store.SeverityHigh, escMsg); escErr != nil {
			logger.Printf("RebaseAskBranch #%d: CreateEscalation failed for convoy %d repo %s cap-hit: %v — operator mail + FailBounty still fire", bounty.ID, payload.ConvoyID, payload.Repo, escErr)
		}
		store.SendMail(db, "Pilot", "operator",
			fmt.Sprintf("[REBASE CAP] Convoy #%d repo %s ask-branch rebase-conflict cap hit", payload.ConvoyID, payload.Repo),
			escMsg, bounty.ID, store.MailTypeAlert)
		if fbErr := store.FailBounty(db, bounty.ID, escMsg); fbErr != nil {
			logger.Printf("RebaseAskBranch #%d: FailBounty after rebase-cap hit failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	newTip, rebaseErr := igit.RebaseBranchOnto(ctx, repo.LocalPath, ab.AskBranch, defaultBranch)
	if rebaseErr != nil {
		// Before escalating to an astromech, try a non-LLM fallback: merge
		// the default branch into the ask-branch with `-X union` strategy.
		// For the common "both sides appended text" case (CLAUDE.md, README,
		// lockfiles, release notes — the exact files that blocked the fleet
		// on tasks 519/537 with 5 Claude CLI infra failures each), union
		// merge produces a clean resolution deterministically. LLM conflict
		// resolution is reserved for structural conflicts that genuinely
		// need judgement.
		logger.Printf("RebaseAskBranch #%d: rebase conflicted — attempting union-merge fallback", bounty.ID)
		unionTip, unionErr := igit.MergeWithUnionStrategy(ctx, repo.LocalPath, ab.AskBranch, "refs/remotes/origin/"+defaultBranch,
			fmt.Sprintf("merge %s into %s via union strategy (auto-recovery from rebase conflict)", defaultBranch, ab.AskBranch))
		if unionErr == nil {
			// Union merge succeeded in a temp worktree; the local ref now
			// points at the merge commit. Push it.
			if pushErr := igit.ForcePushBranch(ctx, repo.LocalPath, ab.AskBranch); pushErr != nil {
				logger.Printf("RebaseAskBranch #%d: union merge succeeded but push failed: %v — falling through to astromech", bounty.ID, pushErr)
			} else {
				if err := store.UpdateConvoyAskBranchBase(db, payload.ConvoyID, payload.Repo, unionTip); err != nil {
					logger.Printf("RebaseAskBranch #%d: UpdateConvoyAskBranchBase after union-merge failed: %v — main-drift-watch reads Convoys.ask_branch_base_sha; a stale SHA triggers a follow-up rebase cycle", bounty.ID, err)
				}
				logger.Printf("RebaseAskBranch #%d: union-merge fallback recovered ask-branch; new tip=%s",
					bounty.ID, unionTip[:minInt(8, len(unionTip))])
				store.LogAudit(db, "Pilot", "rebase-union-merge", bounty.ID,
					fmt.Sprintf("auto-resolved conflict via union merge; saved %s", ab.AskBranch))
				if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
					logger.Printf("RebaseAskBranch #%d: failed to mark Completed after union-merge recovery (convoy %d repo %s): %v — main-drift-watch will retry", bounty.ID, payload.ConvoyID, payload.Repo, err)
				}
				return
			}
		} else {
			logger.Printf("RebaseAskBranch #%d: union merge also failed (%v) — spawning astromech resolution", bounty.ID, unionErr)
		}

		// Both rebase and union merge failed — conflicts are structural.
		// Spawn a RebaseConflict CodeEdit task on the ask-branch itself
		// (not an agent branch). The astromech resolves conflict markers
		// and commits directly onto the ask-branch. When Jedi Council
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
			if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("queue ask-branch conflict: %v", addErr)); fbErr != nil {
				logger.Printf("RebaseAskBranch #%d: FailBounty after conflict-queue failure failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
			}
			return
		}
		// Fix #6 (AUDIT-028, AUDIT-119): count each serial attempt. The
		// cap check at the top of this function refuses subsequent runs
		// past maxAskBranchConflicts.
		newAttempts := store.IncrementFailedRebaseAttempts(db, payload.ConvoyID, payload.Repo)
		logger.Printf("RebaseAskBranch #%d: convoy %d repo %s — failed_rebase_attempts=%d/%d",
			bounty.ID, payload.ConvoyID, payload.Repo, newAttempts, maxAskBranchConflicts)
		if existed {
			logger.Printf("RebaseAskBranch #%d: conflict — reusing existing task #%d on %s", bounty.ID, conflictTaskID, ab.AskBranch)
			if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
				logger.Printf("RebaseAskBranch #%d: failed to mark Completed after reusing conflict task #%d: %v — main-drift-watch will retry", bounty.ID, conflictTaskID, err)
			}
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
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RebaseAskBranch #%d: failed to mark Completed after spawning conflict task #%d: %v — main-drift-watch will retry", bounty.ID, conflictTaskID, err)
		}
		store.LogAudit(db, "Pilot", "rebase-conflict", bounty.ID,
			fmt.Sprintf("spawned RebaseConflict task #%d", conflictTaskID))
		return
	}

	// Clean rebase — force-push with lease, update stored base SHA.
	if pushErr := igit.ForcePushBranch(ctx, repo.LocalPath, ab.AskBranch); pushErr != nil {
		cls := gh.ClassifyError(pushErr.Error())
		if cls.ShouldRetry() {
			// Put the task back to Pending so the next Pilot tick retries.
			// If the UPDATE itself fails, fall through to FailBounty below so
			// the task doesn't get stuck Locked — better a clean Failed than
			// a phantom-lock that the stale-lock detector has to clean up.
			if _, err := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '', error_log = ? WHERE id = ?`,
				fmt.Sprintf("transient push error (class=%s): %v", cls, pushErr), bounty.ID); err != nil {
				logger.Printf("RebaseAskBranch #%d: requeue UPDATE failed (%v) — failing task instead", bounty.ID, err)
				if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("force-push failed and requeue failed: %v / %v", pushErr, err)); fbErr != nil {
					logger.Printf("RebaseAskBranch #%d: FailBounty after push+requeue failure failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
				}
				return
			}
			logger.Printf("RebaseAskBranch #%d: transient push error (class=%s) — requeued", bounty.ID, cls)
			return
		}
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("force-push failed (class=%s): %v", cls, pushErr)); fbErr != nil {
			logger.Printf("RebaseAskBranch #%d: FailBounty after permanent push failure failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	if err := store.UpdateConvoyAskBranchBase(db, payload.ConvoyID, payload.Repo, newTip); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("failed to record new base SHA: %v", err)); fbErr != nil {
			logger.Printf("RebaseAskBranch #%d: FailBounty after base-SHA-record failure failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}
	// Fix #6: a clean rebase means the ask-branch caught up — any past
	// conflict-retry history is irrelevant. Reset the counter so a future
	// drift that happens to conflict gets a fresh budget.
	store.ResetFailedRebaseAttempts(db, payload.ConvoyID, payload.Repo)
	logger.Printf("RebaseAskBranch #%d: convoy %d repo %s rebased onto %s, new tip %s",
		bounty.ID, payload.ConvoyID, payload.Repo, defaultBranch, newTip[:minInt(8, len(newTip))])
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("RebaseAskBranch #%d: failed to mark Completed after clean rebase (convoy %d repo %s): %v — main-drift-watch will retry", bounty.ID, payload.ConvoyID, payload.Repo, err)
	}
}

// countCommitsAheadBehind returns (behind, ahead, error) — how many commits
// branch is behind / ahead of base, using `git rev-list --count --left-right`.
// Sscanf errors propagate as an error so a malformed git output can't silently
// leave behind=0 and bypass the drift-cap escalation.
// Fix #8e: ctx threads from the caller (Pilot's claim ctx).
func countCommitsAheadBehind(ctx context.Context, repoPath, branch, base string) (behind, ahead int, err error) {
	out, err := igit.RunCmd(ctx, repoPath, "rev-list", "--count", "--left-right",
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
// Fix #8e: ctx threads from RunDogs (per-dog 5m ctx, derived from inquisitor
// tick ctx) so the ls-remote network ops cancel on daemon shutdown.
func dogMainDriftWatch(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
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
			sha, err := igit.RemoteHeadSHA(ctx, repoCfg.LocalPath)
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
		// Fix #6 (AUDIT-119): skip queueing for ask-branches that have hit
		// the failed-rebase-attempts cap. The conflict-retry budget lives on
		// ConvoyAskBranches; once exhausted, main-drift-watch must stand
		// down rather than re-queueing every 15 minutes forever.
		if attempts := store.GetFailedRebaseAttempts(db, ab.ConvoyID, ab.Repo); attempts >= maxAskBranchConflicts {
			continue
		}
		// Drift detected — QueueRebaseAskBranch is now race-safe via the
		// `rebase-askbranch:<convoy>:<repo>` canonical idempotency key +
		// idx_bounty_idem (Fix #3), so we no longer need a pre-check. A second
		// tick finding the same drift just gets the existing row back.
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

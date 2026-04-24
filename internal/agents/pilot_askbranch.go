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

// ── CreateAskBranch ──────────────────────────────────────────────────────────
//
// Given a convoy ID in the payload, Pilot enumerates the repos touched by the
// convoy's CodeEdit tasks and cuts an ask-branch in each one. Idempotent: if a
// ConvoyAskBranch row already exists for a (convoy, repo), Pilot skips that
// repo (branch was already cut by a prior run).
//
// The task completes when every repo has a branch. Per-repo failures are
// handled by the retry wrapper — Pilot re-queues the task until everything
// succeeds or the retry cap is reached.

type createAskBranchPayload struct {
	ConvoyID int `json:"convoy_id"`
}

// QueueCreateAskBranch enqueues a CreateAskBranch task for Pilot and blocks
// every existing Pending/Planned CodeEdit task in the convoy on it. This
// prevents astromechs from racing through the Council before the ask-branch
// exists — ClaimBounty won't hand out a blocked task.
//
// Fix #3 (AUDIT-035): canonical idempotency key `create-askbranch:<convoyID>`
// via idx_bounty_idem ensures at most one CreateAskBranch is live per convoy
// at any moment. Dog re-triggers during in-flight execution become no-ops
// rather than stacking duplicate CreateAskBranch tasks on the same convoy.
// The dependency-wiring sweep always runs (idempotent via INSERT OR IGNORE
// in AddDependency) so re-queue attempts still catch any newly-added
// CodeEdit tasks that slipped in after the initial queue.
func QueueCreateAskBranch(db *sql.DB, convoyID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueCreateAskBranch: convoyID required")
	}
	payload, _ := json.Marshal(createAskBranchPayload{ConvoyID: convoyID})
	key := fmt.Sprintf("create-askbranch:%d", convoyID)
	pilotTaskID, _, err := store.AddIdempotentTask(db, key,
		0, "", "CreateAskBranch", string(payload), convoyID, 5, "Pending")
	if err != nil {
		return 0, err
	}

	// Block all CodeEdit tasks in this convoy that haven't started yet.
	// Collect IDs first, close the cursor, then write dependencies — keeping
	// the read and write as separate DB operations avoids a connection deadlock
	// on single-connection SQLite pools (including :memory: in tests).
	rows, qErr := db.Query(
		`SELECT id FROM BountyBoard
		 WHERE convoy_id = ? AND type = 'CodeEdit' AND status IN ('Pending', 'Planned')`,
		convoyID)
	if qErr != nil {
		return pilotTaskID, nil // non-fatal; requeue fallback in Council still handles it
	}
	var codeEditIDs []int
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			codeEditIDs = append(codeEditIDs, id)
		}
	}
	rows.Close()
	for _, codeEditID := range codeEditIDs {
		store.AddDependency(db, codeEditID, pilotTaskID)
	}
	return pilotTaskID, nil
}

// AskBranchNameForConvoy generates the ask-branch name for a convoy.
// Format: <github-username>/force/ask-<convoyID>-<slug>, or bare
// force/ask-<convoyID>-<slug> when the username can't be discovered.
// The username prefix is enterprise-convention for branch ownership.
func AskBranchNameForConvoy(convoyID int, convoyName string) string {
	// Convoy names look like "[5] Add user auth" — strip the bracket prefix.
	cleaned := convoyName
	if idx := strings.Index(cleaned, "]"); idx > 0 && strings.HasPrefix(cleaned, "[") {
		cleaned = strings.TrimSpace(cleaned[idx+1:])
	}
	slug := igit.BranchNameSlug(cleaned, 40)
	return fmt.Sprintf("%sforce/ask-%d-%s", igit.BranchPrefix(), convoyID, slug)
}

func runCreateAskBranch(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload createAskBranchPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); fbErr != nil {
			logger.Printf("CreateAskBranch #%d: FailBounty after invalid payload failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}
	if payload.ConvoyID <= 0 {
		if fbErr := store.FailBounty(db, bounty.ID, "payload missing convoy_id"); fbErr != nil {
			logger.Printf("CreateAskBranch #%d: FailBounty after payload validation failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	convoy := store.GetConvoy(db, payload.ConvoyID)
	if convoy == nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("convoy %d not found", payload.ConvoyID)); fbErr != nil {
			logger.Printf("CreateAskBranch #%d: FailBounty after convoy-not-found failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	repos := store.ConvoyReposTouched(db, payload.ConvoyID)
	if len(repos) == 0 {
		// Convoy has no CodeEdit tasks yet — nothing to branch. Mark Completed
		// so we don't loop; when tasks are added later, the Layer C inquisitor
		// check will re-queue if needed.
		logger.Printf("CreateAskBranch #%d: convoy %d has no CodeEdit tasks yet — completing as no-op",
			bounty.ID, payload.ConvoyID)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("CreateAskBranch #%d: failed to mark Completed: %v — stale-lock detector will recover", bounty.ID, err)
		}
		return
	}

	branchName := AskBranchNameForConvoy(payload.ConvoyID, convoy.Name)

	var created []string
	var skipped []string
	var failures []string
	for _, repoName := range repos {
		repo := store.GetRepo(db, repoName)
		if repo == nil {
			failures = append(failures, fmt.Sprintf("%s: not registered", repoName))
			continue
		}
		if !repo.PRFlowEnabled {
			// Repo opted out of the PR flow; astromechs will use legacy local-merge.
			// Don't create a branch — no point.
			skipped = append(skipped, fmt.Sprintf("%s: pr_flow_enabled=false", repoName))
			continue
		}
		if repo.LocalPath == "" || repo.RemoteURL == "" {
			// Layer B backfill should have populated these. If they're empty,
			// flag the repo and skip rather than generate broken branches.
			if err := store.SetRepoPRFlowEnabled(db, repoName, false); err != nil {
				logger.Printf("CreateAskBranch #%d: quarantine pr_flow for %s failed: %v — repo stays enabled; Pilot preflight dog will re-quarantine on next tick", bounty.ID, repoName, err)
			}
			failures = append(failures, fmt.Sprintf("%s: missing local_path or remote_url (pr_flow disabled)", repoName))
			continue
		}

		// Idempotence: if this (convoy, repo) already has an ask-branch, move on.
		if existing := store.GetConvoyAskBranch(db, payload.ConvoyID, repoName); existing != nil {
			skipped = append(skipped, fmt.Sprintf("%s: already has %s", repoName, existing.AskBranch))
			continue
		}

		baseSHA, err := igit.CreateAskBranch(repo.LocalPath, branchName)
		if err != nil {
			cls := gh.ClassifyError(err.Error())
			failures = append(failures, fmt.Sprintf("%s: %v (class=%s)", repoName, err, cls))
			continue
		}
		if err := store.UpsertConvoyAskBranch(db, payload.ConvoyID, repoName, branchName, baseSHA); err != nil {
			failures = append(failures, fmt.Sprintf("%s: store upsert: %v", repoName, err))
			continue
		}
		created = append(created, fmt.Sprintf("%s@%s:%s", repoName, baseSHA[:minInt(8, len(baseSHA))], branchName))
	}

	if len(failures) > 0 {
		// Mark the task failed so Medic can triage. Preserve the successful
		// creations: they stay in ConvoyAskBranches and a subsequent retry
		// will skip them.
		msg := fmt.Sprintf("failures: %s", strings.Join(failures, "; "))
		if len(created) > 0 {
			msg = fmt.Sprintf("partial success (%d created, %d failed): created=%s; failures=%s",
				len(created), len(failures),
				strings.Join(created, ","), strings.Join(failures, ";"))
		}
		if fbErr := store.FailBounty(db, bounty.ID, msg); fbErr != nil {
			logger.Printf("CreateAskBranch #%d: FailBounty after per-repo failures failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		logger.Printf("CreateAskBranch #%d: %s", bounty.ID, msg)
		return
	}

	// Mirror one of the branches onto the legacy Convoys.ask_branch scalar for
	// any code paths still reading that field. For multi-repo convoys this
	// picks the lexicographically first repo — an arbitrary but stable choice.
	if len(created) > 0 {
		firstRepo := repos[0]
		if ab := store.GetConvoyAskBranch(db, payload.ConvoyID, firstRepo); ab != nil {
			if err := store.SetConvoyAskBranch(db, payload.ConvoyID, ab.AskBranch, ab.AskBranchBaseSHA); err != nil {
				logger.Printf("CreateAskBranch #%d: legacy Convoys.ask_branch mirror update failed: %v — per-repo ConvoyAskBranches rows are authoritative, the main-drift-watch dog reads those", bounty.ID, err)
			}
		}
	}

	logger.Printf("CreateAskBranch #%d: convoy %d → created=[%s] skipped=[%s]",
		bounty.ID, payload.ConvoyID, strings.Join(created, ","), strings.Join(skipped, ","))
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("CreateAskBranch #%d: failed to mark Completed: %v — stale-lock detector will recover", bounty.ID, err)
	}
}

// ── CleanupAskBranch ─────────────────────────────────────────────────────────

type cleanupAskBranchPayload struct {
	ConvoyID int `json:"convoy_id"`
}

// QueueCleanupAskBranch enqueues a cleanup task for a convoy (after Shipped /
// Abandoned). Deletes every ConvoyAskBranch row and its origin branch.
func QueueCleanupAskBranch(db *sql.DB, convoyID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueCleanupAskBranch: convoyID required")
	}
	payload, _ := json.Marshal(cleanupAskBranchPayload{ConvoyID: convoyID})
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, '', 'CleanupAskBranch', 'Pending', ?, 0, datetime('now'))`,
		string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// QueueCleanupAskBranchTx is the transactional sibling of QueueCleanupAskBranch.
func QueueCleanupAskBranchTx(tx *sql.Tx, convoyID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueCleanupAskBranchTx: convoyID required")
	}
	payload, _ := json.Marshal(cleanupAskBranchPayload{ConvoyID: convoyID})
	res, err := tx.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, '', 'CleanupAskBranch', 'Pending', ?, 0, datetime('now'))`,
		string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

func runCleanupAskBranch(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload cleanupAskBranchPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); fbErr != nil {
			logger.Printf("CleanupAskBranch #%d: FailBounty after invalid payload failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}
	if payload.ConvoyID <= 0 {
		if fbErr := store.FailBounty(db, bounty.ID, "payload missing convoy_id"); fbErr != nil {
			logger.Printf("CleanupAskBranch #%d: FailBounty after payload validation failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	branches := store.ListConvoyAskBranches(db, payload.ConvoyID)
	var deleted []string
	var failed []string
	for _, ab := range branches {
		repo := store.GetRepo(db, ab.Repo)
		if repo == nil || repo.LocalPath == "" {
			// Repo gone — just remove the DB row.
			if err := store.DeleteConvoyAskBranch(db, ab.ConvoyID, ab.Repo); err != nil {
				logger.Printf("CleanupAskBranch #%d: DeleteConvoyAskBranch(repo-gone) %s failed: %v — row persists, next CleanupAskBranch dog tick will retry", bounty.ID, ab.Repo, err)
			}
			deleted = append(deleted, ab.Repo+"(row-only)")
			continue
		}
		if err := igit.DeleteAskBranch(repo.LocalPath, ab.AskBranch); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", ab.Repo, err))
			continue
		}
		if err := store.DeleteConvoyAskBranch(db, ab.ConvoyID, ab.Repo); err != nil {
			logger.Printf("CleanupAskBranch #%d: DeleteConvoyAskBranch %s failed: %v — remote branch gone but DB row persists; CleanupAskBranch dog will re-run and skip the already-deleted branch", bounty.ID, ab.Repo, err)
		}
		deleted = append(deleted, ab.Repo)
	}
	if len(failed) > 0 {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("cleanup failures: %s", strings.Join(failed, "; "))); fbErr != nil {
			logger.Printf("CleanupAskBranch #%d: FailBounty after cleanup failures failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}
	logger.Printf("CleanupAskBranch #%d: convoy %d → deleted=[%s]",
		bounty.ID, payload.ConvoyID, strings.Join(deleted, ","))
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("CleanupAskBranch #%d: failed to mark Completed: %v — stale-lock detector will recover", bounty.ID, err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

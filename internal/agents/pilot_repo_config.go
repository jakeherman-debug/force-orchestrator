package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"force-orchestrator/internal/store"
)

// ── repo-config-check dog + RevalidateRepoConfig task ───────────────────────
//
// Daily health check for every registered repo's PR-flow configuration:
//   - remote_url still resolves to the recorded origin URL
//   - default_branch still exists on origin
//   - pr_template_path file still exists on disk
//
// Each check that fails triggers a specific healing action:
//   - remote URL changed       → update the stored value
//   - origin unreachable       → quarantine the repo (pr_flow_enabled=0)
//   - default branch renamed   → update the stored value; if neither old nor new
//                                 exist, quarantine
//   - pr_template_path missing → re-run FindPRTemplate (covers template moves)
//
// Quarantined repos fall back to the legacy local-merge path until the
// operator unquarantines (via `force repo set-pr-flow <name> on` after
// fixing the underlying issue).

type revalidatePayload struct {
	Repo string `json:"repo"`
}

// QueueRevalidateRepoConfig enqueues a RevalidateRepoConfig task for a repo.
func QueueRevalidateRepoConfig(db *sql.DB, repoName string) (int, error) {
	if repoName == "" {
		return 0, fmt.Errorf("QueueRevalidateRepoConfig: repoName required")
	}
	payload, _ := json.Marshal(revalidatePayload{Repo: repoName})
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'RevalidateRepoConfig', 'Pending', ?, 1, datetime('now'))`,
		repoName, string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// runRevalidateRepoConfig is Pilot's handler. Read the stored repo config,
// verify each field against reality, heal what can be healed, quarantine
// what can't.
// Fix #8e: ctx threads from SpawnPilot's claim ctx so the ls-remote network
// op cancels on daemon shutdown.
func runRevalidateRepoConfig(ctx context.Context, db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload revalidatePayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); fbErr != nil {
			logger.Printf("RevalidateRepoConfig #%d: FailBounty after invalid payload failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}
	repo := store.GetRepo(db, payload.Repo)
	if repo == nil {
		// Repo was removed; nothing to do.
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: failed to mark Completed after repo %s removed: %v — stale-lock detector will recover", bounty.ID, payload.Repo, err)
		}
		return
	}
	if repo.LocalPath == "" {
		if err := store.QuarantineRepo(db, payload.Repo, "no local_path recorded"); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: QuarantineRepo(%s) failed: %v — repo stays unquarantined; next RevalidateRepoConfig dog tick will retry", bounty.ID, payload.Repo, err)
		}
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: failed to mark Completed after quarantining %s: %v — stale-lock detector will recover", bounty.ID, payload.Repo, err)
		}
		logger.Printf("RevalidateRepoConfig: %s quarantined — no local_path", payload.Repo)
		return
	}

	var issues []string
	var healedAny bool

	// 1. Origin URL.
	currentRemote, remoteErr := repoRemoteURL(repo.LocalPath)
	if remoteErr != nil {
		if qErr := store.QuarantineRepo(db, payload.Repo,
			fmt.Sprintf("origin unreachable: %v", remoteErr)); qErr != nil {
			logger.Printf("RevalidateRepoConfig #%d: QuarantineRepo(%s, origin-unreachable) failed: %v — next RevalidateRepoConfig dog tick will retry", bounty.ID, payload.Repo, qErr)
		}
		store.SendMail(db, "Pilot", "operator",
			fmt.Sprintf("[QUARANTINED] %s — origin unreachable", payload.Repo),
			fmt.Sprintf("Repo '%s' failed RevalidateRepoConfig.\n\nError: %v\n\nThe PR flow is disabled for this repo; tasks fall back to legacy local-merge. Fix the remote and run `force repo set-pr-flow %s on` to re-enable.",
				payload.Repo, remoteErr, payload.Repo),
			0, store.MailTypeAlert)
		logger.Printf("RevalidateRepoConfig: %s quarantined — %v", payload.Repo, remoteErr)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: failed to mark Completed after remote-unreachable quarantine of %s: %v — stale-lock detector will recover", bounty.ID, payload.Repo, err)
		}
		return
	}
	if currentRemote != repo.RemoteURL {
		issues = append(issues, fmt.Sprintf("remote URL changed: %s → %s", repo.RemoteURL, currentRemote))
		if err := store.SetRepoRemoteInfo(db, payload.Repo, currentRemote, repo.DefaultBranch); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: SetRepoRemoteInfo(%s, remote-drift) failed: %v — repo keeps stale URL; RevalidateRepoConfig dog will retry on next tick", bounty.ID, payload.Repo, err)
		}
		healedAny = true
	}

	// 2. Default branch still present.
	currentDefault := repoDefaultBranch(repo.LocalPath)
	if currentDefault == "" {
		if err := store.QuarantineRepo(db, payload.Repo, "default branch undetectable"); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: QuarantineRepo(%s, no-default-branch) failed: %v — next RevalidateRepoConfig dog tick will retry", bounty.ID, payload.Repo, err)
		}
		store.SendMail(db, "Pilot", "operator",
			fmt.Sprintf("[QUARANTINED] %s — default branch undetectable", payload.Repo),
			fmt.Sprintf("Repo '%s' no longer has a detectable default branch (main/master/develop).\n\nFix the repo and re-run `force repo sync`.", payload.Repo),
			0, store.MailTypeAlert)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: failed to mark Completed after default-branch-undetectable quarantine of %s: %v — stale-lock detector will recover", bounty.ID, payload.Repo, err)
		}
		return
	}
	if currentDefault != repo.DefaultBranch {
		issues = append(issues, fmt.Sprintf("default branch changed: %s → %s", repo.DefaultBranch, currentDefault))
		if err := store.SetRepoRemoteInfo(db, payload.Repo, currentRemote, currentDefault); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: SetRepoRemoteInfo(%s, default-branch-drift) failed: %v — RevalidateRepoConfig dog will retry on next tick", bounty.ID, payload.Repo, err)
		}
		healedAny = true
	}

	// 3. PR template path (if recorded) still exists. Empty path is allowed —
	// means "no template" which Diplomat handles via fallback.
	if repo.PRTemplatePath != "" {
		if _, err := os.Stat(repo.PRTemplatePath); err != nil {
			issues = append(issues, fmt.Sprintf("pr_template_path missing: %s (%v)", repo.PRTemplatePath, err))
			// Clear the stale path and re-run FindPRTemplate.
			if sErr := store.SetRepoPRTemplatePath(db, payload.Repo, ""); sErr != nil {
				logger.Printf("RevalidateRepoConfig #%d: SetRepoPRTemplatePath(%s, clear) failed: %v — FindPRTemplate requeue below is the recovery path (Diplomat fallback still works with the stale path)", bounty.ID, payload.Repo, sErr)
			}
			if _, qErr := QueueFindPRTemplate(db, payload.Repo, repo.LocalPath); qErr != nil {
				logger.Printf("RevalidateRepoConfig: could not requeue FindPRTemplate: %v", qErr)
			}
			healedAny = true
		}
	}

	// 4. Ping origin to confirm reachability (cheap ls-remote HEAD).
	// Fix #8e: ctx-bounded so daemon shutdown cancels the network call.
	// Pre-fix this was a bare exec.Command on the Pattern P11 allowlist
	// labelled "ls-remote under dog-level 5m timeout" — accurate in mechanism
	// but the dog timeout descended from a fabricated context.Background, so
	// daemon SIGINT could not cancel a hung ls-remote. Now ctx is the daemon
	// ctx via SpawnPilot → runRevalidateRepoConfig and the dog ctx (5min)
	// derives from the same root.
	if out, pingErr := exec.CommandContext(ctx, "git", "-C", repo.LocalPath, "ls-remote", "--heads",
		"origin", currentDefault).CombinedOutput(); pingErr != nil {
		msg := strings.TrimSpace(string(out))
		if qErr := store.QuarantineRepo(db, payload.Repo, fmt.Sprintf("origin ls-remote failed: %s", msg)); qErr != nil {
			logger.Printf("RevalidateRepoConfig #%d: QuarantineRepo(%s, ls-remote-failed) failed: %v — next RevalidateRepoConfig dog tick will retry", bounty.ID, payload.Repo, qErr)
		}
		store.SendMail(db, "Pilot", "operator",
			fmt.Sprintf("[QUARANTINED] %s — cannot reach origin", payload.Repo),
			fmt.Sprintf("Repo '%s' ls-remote failed: %s\n\nCheck network/auth; re-run `force repo set-pr-flow %s on` to re-enable.",
				payload.Repo, msg, payload.Repo),
			0, store.MailTypeAlert)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RevalidateRepoConfig #%d: failed to mark Completed after ls-remote-failed quarantine of %s: %v — stale-lock detector will recover", bounty.ID, payload.Repo, err)
		}
		return
	}

	if len(issues) > 0 {
		logger.Printf("RevalidateRepoConfig: %s healed — %s", payload.Repo, strings.Join(issues, "; "))
	}
	if healedAny {
		store.LogAudit(db, "Pilot", "repo-config-healed", bounty.ID,
			fmt.Sprintf("%s: %s", payload.Repo, strings.Join(issues, "; ")))
	}
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("RevalidateRepoConfig #%d: failed to mark Completed for %s: %v — stale-lock detector will recover", bounty.ID, payload.Repo, err)
	}
}

// dogRepoConfigCheck is the per-24h dog that enqueues RevalidateRepoConfig
// per registered repo. Guards against duplicates so repeated runs don't pile up.
// Fix #8e: ctx threads from RunDogs → runDog. Body is pure DB so ctx is
// unused here, but the parameter aligns the dog signature with its peers
// for the per-site P11 enforcement.
func dogRepoConfigCheck(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	_ = ctx
	repos := store.ListRepos(db)
	for _, r := range repos {
		var existing int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE type = 'RevalidateRepoConfig' AND target_repo = ?
			  AND status IN ('Pending', 'Locked')`, r.Name).Scan(&existing)
		if existing > 0 {
			continue
		}
		if _, err := QueueRevalidateRepoConfig(db, r.Name); err != nil {
			logger.Printf("repo-config-check: queue for %s failed: %v", r.Name, err)
			continue
		}
	}
	return nil
}

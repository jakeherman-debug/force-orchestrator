package agents

import (
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"force-orchestrator/internal/gh"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// ── PR-flow shared helpers ───────────────────────────────────────────────────
//
// Used by Jedi Council's approval path and by Diplomat (Phase 6) to push branches
// and interact with GitHub. The helpers classify errors so the caller can make
// retry / escalate decisions.

// subPRCITaskStatus is the status assigned to a task once its sub-PR has been
// opened and is waiting for CI to finish. Checked by sub-pr-ci-watch on each
// inquisitor cycle.
const subPRCITaskStatus = "AwaitingSubPRCI"

// openSubPRForApprovedTask is the PR-flow equivalent of MergeAndCleanup.
//
// The caller (Jedi Council) has already approved the task. We:
//
//   1. Push the astromech branch to origin.
//   2. Open a GitHub PR with the astromech branch as head and the convoy's
//      ask-branch as base.
//   3. Record the PR in AskBranchPRs.
//   4. Mark the task AwaitingSubPRCI so sub-pr-ci-watch takes over.
//
// Returns nil on success. On push/gh failure, classifies the error so the
// caller can decide whether to retry (ErrClassTransient/RateLimited) or
// escalate (ErrClassAuthExpired / BranchProtection / Permanent).
func openSubPRForApprovedTask(db *sql.DB, bounty *store.Bounty, agentName, branchName, prTitle, prBody string, logger interface{ Printf(string, ...any) }) (classifiedErr gh.ErrorClass, err error) {
	if bounty.ConvoyID <= 0 {
		return gh.ErrClassPermanent, fmt.Errorf("openSubPRForApprovedTask: bounty %d has no convoy_id", bounty.ID)
	}
	ab := store.GetConvoyAskBranch(db, bounty.ConvoyID, bounty.TargetRepo)
	if ab == nil {
		return gh.ErrClassPermanent, fmt.Errorf("convoy %d repo %s has no ask-branch", bounty.ConvoyID, bounty.TargetRepo)
	}
	repo := store.GetRepo(db, bounty.TargetRepo)
	if repo == nil || repo.LocalPath == "" {
		return gh.ErrClassPermanent, fmt.Errorf("repo %s not registered or has no local_path", bounty.TargetRepo)
	}

	// Rebase-conflict-resolution case: the astromech resolved conflicts
	// directly on the ask-branch (Pilot spawned a CodeEdit with branch_name
	// set to the ask-branch). Opening a PR with head=ask-branch, base=ask-branch
	// is nonsense — we just need to force-push the resolved branch and update
	// the stored base SHA. No sub-PR needed.
	if branchName == ab.AskBranch {
		return completeAskBranchResolution(db, bounty, ab, repo, logger)
	}

	// Step 1: push the branch. Use --force-with-lease so we can retry safely
	// if the astromech pushed and then we pushed again in a rework cycle.
	pushOut, pushErr := exec.Command("git", "-C", repo.LocalPath, "push",
		"--force-with-lease", "-u", "origin", branchName).CombinedOutput()
	if pushErr != nil {
		msg := strings.TrimSpace(string(pushOut))
		cls := gh.ClassifyError(msg)
		logger.Printf("Task %d: push failed (class=%s): %s", bounty.ID, cls, msg)
		return cls, fmt.Errorf("git push: %s", msg)
	}

	// Step 2: open the sub-PR via gh.
	ghc := newGHClient()
	// Derive --repo from the registered repo's remote_url so gh doesn't need
	// to infer it from cwd (avoids ambiguity in worktrees).
	ghRepo := deriveGHRepoFromRemoteURL(repo.RemoteURL)
	res, prErr := ghc.PRCreate(gh.PRCreateRequest{
		Repo:  ghRepo,
		CWD:   repo.LocalPath,
		Title: prTitle,
		Body:  prBody,
		Base:  ab.AskBranch,
		Head:  branchName,
		Draft: false,
	})
	if prErr != nil {
		cls := gh.ClassifyError(prErr.Error())
		logger.Printf("Task %d: gh pr create failed (class=%s): %v", bounty.ID, cls, prErr)
		return cls, prErr
	}

	// Steps 3-4 are atomic: either both land or neither. Without a tx, a crash
	// between them leaves the PR recorded but the task still in its prior
	// status — Jedi Council would re-claim it and attempt to re-open the same PR.
	tx, err := db.Begin()
	if err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, storeErr := store.CreateAskBranchPRTx(tx, bounty.ID, bounty.ConvoyID, bounty.TargetRepo, res.URL, res.Number); storeErr != nil {
		logger.Printf("Task %d: created PR #%d but failed to record it: %v", bounty.ID, res.Number, storeErr)
		return gh.ErrClassPermanent, fmt.Errorf("store PR: %w", storeErr)
	}
	if err := store.UpdateBountyStatusWithErrorTx(tx, bounty.ID, subPRCITaskStatus, ""); err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("update task status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("commit: %w", err)
	}

	logger.Printf("Task %d: sub-PR #%d opened against %s (url=%s)",
		bounty.ID, res.Number, ab.AskBranch, res.URL)
	return "", nil
}

// buildSubPRTitle produces a concise, human-readable title for a sub-PR based
// on the parent task's payload. First line of the payload (capped at 100 chars)
// is the title; the "[GOAL: ...]" Commander prefix is stripped.
func buildSubPRTitle(b *store.Bounty) string {
	payload := b.Payload
	// Strip a leading "[GOAL: ...]\n" prefix Commander sometimes adds.
	if strings.HasPrefix(payload, "[GOAL:") {
		if nl := strings.Index(payload, "\n"); nl != -1 {
			payload = strings.TrimSpace(payload[nl+1:])
		}
	}
	// Take the first non-empty line.
	firstLine := ""
	for _, line := range strings.Split(payload, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			firstLine = trimmed
			break
		}
	}
	if firstLine == "" {
		firstLine = fmt.Sprintf("Task #%d", b.ID)
	}
	if len(firstLine) > 100 {
		firstLine = firstLine[:100]
	}
	return fmt.Sprintf("[task %d] %s", b.ID, firstLine)
}

// buildSubPRBody renders a minimal PR body linking back to the fleet task and
// council feedback. Final convoy-level PRs (Diplomat) get the full template
// treatment; these sub-PRs are intra-convoy and deliberately lightweight.
func buildSubPRBody(b *store.Bounty, ruling store.CouncilRuling) string {
	var body strings.Builder
	body.WriteString(fmt.Sprintf("Fleet task: #%d (convoy #%d)\n\n", b.ID, b.ConvoyID))
	body.WriteString("## Task\n\n")
	body.WriteString(b.Payload)
	body.WriteString("\n\n## Council ruling\n\n")
	if strings.TrimSpace(ruling.Feedback) != "" {
		body.WriteString(ruling.Feedback)
	} else {
		body.WriteString("_Approved with no additional feedback._")
	}
	body.WriteString("\n\n---\n*Auto-generated by Jedi Council. Auto-merges when CI is green.*\n")
	return body.String()
}

// completeAskBranchResolution handles the approval of a RebaseConflict-style
// task whose branch_name is the ask-branch itself. The astromech has committed
// conflict resolutions directly onto the ask-branch; all we need to do is
// force-push the branch and update ConvoyAskBranch.ask_branch_base_sha to the
// new tip. No sub-PR is created — the resolved commits ARE the ask-branch.
func completeAskBranchResolution(db *sql.DB, bounty *store.Bounty, ab *store.ConvoyAskBranch, repo *store.Repository, logger interface{ Printf(string, ...any) }) (gh.ErrorClass, error) {
	// Force-push the ask-branch with lease so a concurrent rewrite can't be
	// silently clobbered.
	pushOut, pushErr := exec.Command("git", "-C", repo.LocalPath, "push",
		"--force-with-lease", "origin", ab.AskBranch).CombinedOutput()
	if pushErr != nil {
		msg := strings.TrimSpace(string(pushOut))
		cls := gh.ClassifyError(msg)
		logger.Printf("Task %d: ask-branch resolution push failed (class=%s): %s", bounty.ID, cls, msg)
		return cls, fmt.Errorf("git push ask-branch: %s", msg)
	}

	// Capture the new tip SHA. The new tip is the local HEAD of the ask-branch
	// after astromech committed.
	tipOut, tipErr := exec.Command("git", "-C", repo.LocalPath, "rev-parse", ab.AskBranch).CombinedOutput()
	if tipErr != nil {
		return gh.ErrClassPermanent, fmt.Errorf("rev-parse %s: %s", ab.AskBranch, strings.TrimSpace(string(tipOut)))
	}
	newTip := strings.TrimSpace(string(tipOut))

	// Atomic DB state transition: update base SHA + mark task Completed +
	// unblock dependents. Any partial failure here leaves the branch pushed to
	// origin but DB state inconsistent — Jedi Council retry would re-push
	// (idempotent) but might still see a stale DB state.
	tx, err := db.Begin()
	if err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := store.UpdateConvoyAskBranchBaseTx(tx, bounty.ConvoyID, bounty.TargetRepo, newTip); err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("record new base SHA: %w", err)
	}
	if err := store.UpdateBountyStatusTx(tx, bounty.ID, "Completed"); err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("update task status: %w", err)
	}
	unblocked, err := store.UnblockDependentsOfTx(tx, bounty.ID)
	if err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("unblock dependents: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("commit: %w", err)
	}

	// Post-commit side effects.
	store.FireWebhook(db, bounty.ID, "Completed")
	if unblocked > 0 {
		logger.Printf("Task %d: unblocked %d dependent(s) after ask-branch resolution", bounty.ID, unblocked)
	}
	logger.Printf("Task %d: ask-branch %s resolved and pushed (new tip=%s)",
		bounty.ID, ab.AskBranch, newTip[:minInt(8, len(newTip))])
	return "", nil
}

// deriveGHRepoFromRemoteURL converts a git remote URL into the owner/name format
// that `gh --repo` accepts. Returns "" if the URL format isn't recognisable —
// callers fall back to letting gh infer from cwd.
//
// Handles: git@github.com:owner/repo.git, https://github.com/owner/repo.git,
// file:///paths (for tests — returns "" so gh infers from cwd).
func deriveGHRepoFromRemoteURL(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}
	// SSH form: git@github.com:owner/repo.git
	if strings.HasPrefix(remoteURL, "git@") {
		if idx := strings.Index(remoteURL, ":"); idx > 0 {
			path := remoteURL[idx+1:]
			return strings.TrimSuffix(path, ".git")
		}
		return ""
	}
	// HTTPS form: https://github.com/owner/repo(.git)
	if strings.HasPrefix(remoteURL, "https://") || strings.HasPrefix(remoteURL, "http://") {
		// Strip scheme and host.
		u := strings.TrimPrefix(strings.TrimPrefix(remoteURL, "https://"), "http://")
		if idx := strings.Index(u, "/"); idx > 0 {
			return strings.TrimSuffix(u[idx+1:], ".git")
		}
		return ""
	}
	// file:// and anything else — let gh infer from cwd.
	return ""
}

// ── sub-pr-ci-watch dog ──────────────────────────────────────────────────────

// missingCITimeout is the fallback for repos where gh pr view does not return
// mergeStateStatus (older GitHub Enterprise or API lag). In normal operation
// mergeStateStatus=CLEAN triggers an immediate merge before this fires.
const missingCITimeout = 10 * time.Minute

// subPRCIStaleLimit is the first bell for CI stall. At this age we diagnose
// the stuck-vs-slow distinction and, for stuck-runner cases, try a targeted
// empty-commit re-trigger instead of escalating.
const subPRCIStaleLimit = 2 * time.Hour

// subPRCIHardLimit is the fallback ceiling. Past this age we escalate
// regardless of diagnosis — either CI is genuinely broken or GitHub itself
// isn't recovering.
const subPRCIHardLimit = 6 * time.Hour

// subPRMaxStallRetriggers caps empty-commit re-trigger attempts per PR to
// prevent the self-healing loop from thrashing forever.
const subPRMaxStallRetriggers = 2

// subPRAutoMergeStrategy decides the merge strategy for sub-PRs. Squash keeps
// ask-branch history linear (one commit per task); operators can override by
// setting force config subpr_merge_strategy to 'merge' or 'rebase'.
func subPRAutoMergeStrategy(db *sql.DB) string {
	if v := store.GetConfig(db, "subpr_merge_strategy", ""); v != "" {
		return v
	}
	return "squash"
}

// dogSubPRCIWatch is the per-inquisitor-cycle job that polls sub-PR state.
//
// For each AskBranchPR in state=Open:
//   - gh pr view to check whether the PR has been externally closed/merged
//   - gh pr checks to compute the rollup (Success / Failure / Pending)
//   - on Success: gh pr merge --auto (GitHub merges when required checks pass)
//   - on Failure: record the new checks_state and bump failure_count. Phase 4
//     replaces this with Medic CIFailureTriage enqueuing; for now we escalate.
//   - on externally Closed: mark the task Escalated with reason.
//   - if the PR was opened >10 min ago and still has zero checks reported,
//     escalate with guidance about missing CI config.
func dogSubPRCIWatch(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	prs := store.ListOpenAskBranchPRs(db)
	if len(prs) == 0 {
		return nil
	}
	ghc := newGHClient()
	for _, pr := range prs {
		handleSubPRPoll(db, ghc, pr, logger)
	}
	return nil
}

// newGHClient is the factory for gh.Client instances. Tests swap this out with
// SetGHClientFactory to inject a stub Runner without touching the call sites.
var newGHClient = func() *gh.Client { return gh.NewClient() }

// SetGHClientFactory installs a custom gh.Client factory for the duration of
// a test. Call the returned cleanup fn (or use t.Cleanup) to restore the
// default. Safe for use from tests in other packages — the variable is
// package-scoped but exposed through this helper.
func SetGHClientFactory(f func() *gh.Client) (restore func()) {
	prev := newGHClient
	newGHClient = f
	return func() { newGHClient = prev }
}

// handleSubPRPoll processes a single sub-PR's state change. Split out so it's
// unit-testable with a fake gh client.
func handleSubPRPoll(db *sql.DB, ghc *gh.Client, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	repo := store.GetRepo(db, pr.Repo)
	var cwd, ghRepo string
	if repo != nil {
		cwd = repo.LocalPath
		ghRepo = deriveGHRepoFromRemoteURL(repo.RemoteURL)
	}

	// If the associated task is already terminal (escalated, failed, cancelled),
	// close the sub-PR on GitHub and stop tracking it. This prevents rebase
	// loops from continuing to spawn work against dead tasks.
	if bounty, _ := store.GetBounty(db, pr.TaskID); bounty != nil {
		switch bounty.Status {
		case "Escalated", "Failed", "Cancelled":
			logger.Printf("sub-pr-ci-watch: sub-PR #%d task %d is %s — closing PR and stopping tracking",
				pr.PRNumber, pr.TaskID, bounty.Status)
			if cwd != "" && ghRepo != "" {
				if closeErr := ghc.PRClose(cwd, ghRepo, pr.PRNumber); closeErr != nil {
					logger.Printf("sub-pr-ci-watch: close PR #%d on GitHub failed: %v", pr.PRNumber, closeErr)
				}
			}
			store.MarkAskBranchPRClosed(db, pr.ID)
			return
		}
	}

	view, viewErr := ghc.PRView(cwd, ghRepo, pr.PRNumber)
	if viewErr != nil {
		logger.Printf("sub-pr-ci-watch: pr view #%d failed: %v", pr.PRNumber, viewErr)
		return
	}

	// Externally merged (rare — human merged the sub-PR directly).
	if view.Merged {
		onSubPRMerged(db, pr, logger)
		return
	}
	// Externally closed (without merge).
	if strings.EqualFold(view.State, "CLOSED") {
		onSubPRClosedExternally(db, pr, logger)
		return
	}

	// Fast path: check mergeStateStatus before polling individual CI checks.
	// CLEAN means GitHub considers all required checks satisfied and the PR
	// is mergeable — covers both "CI green" and "no CI configured" without
	// any timeout. BLOCKED with no failing checks means a branch-protection
	// rule (required reviewer, mandatory status check we can't trigger) is
	// the blocker — escalate immediately rather than spinning for 2 hours.
	switch strings.ToUpper(view.MergeStateStatus) {
	case "CLEAN":
		mergeSubPRDirect(db, ghc, pr, logger)
		return
	case "BLOCKED":
		// BLOCKED covers multiple root causes; checks disambiguate.
		checks, rollup, _ := ghc.PRChecks(cwd, ghRepo, pr.PRNumber)
		switch rollup {
		case gh.ChecksFailure:
			// CI is the blocker — Medic triage.
			onSubPRCIFailed(db, pr, logger)
		case gh.ChecksPending:
			if len(checks) == 0 {
				// No CI configured at all but PR is BLOCKED — branch protection rule.
				onSubPRMergeBlocked(db, pr, logger)
			}
			// else: CI is actively running (checks present, not yet resolved) — wait.
		case gh.ChecksSuccess:
			// All checks green but still BLOCKED — required reviewer, CODEOWNERS, etc.
			onSubPRMergeBlocked(db, pr, logger)
		}
		return
	case "BEHIND", "DIRTY":
		// Both states mean the agent branch needs to be rebased onto the
		// current ask-branch tip:
		//   BEHIND — ask-branch advanced after this sub-PR opened (a sibling
		//            sub-PR merged in); fast-forward-style rebase.
		//   DIRTY  — the merge would conflict; the rebase will try cleanly,
		//            and if it truly conflicts, Pilot's RebaseAgentBranch
		//            spawns a RebaseConflict CodeEdit for an astromech.
		// Either way: queue the Pilot task and let the self-healing chain
		// take over. Only escalate if that chain itself exhausts (which it
		// does via the CodeEdit retry cap, not here).
		queueAgentBranchRebase(db, pr, strings.ToUpper(view.MergeStateStatus), logger)
		return
	}

	// mergeStateStatus is "" or "UNKNOWN" — GitHub hasn't computed it yet (API
	// lag on fresh PRs) or we're on an older GHE. Fall back to check-based logic.
	checks, rollup, checksErr := ghc.PRChecks(cwd, ghRepo, pr.PRNumber)
	if checksErr != nil {
		logger.Printf("sub-pr-ci-watch: pr checks #%d failed: %v", pr.PRNumber, checksErr)
		return
	}

	if string(rollup) != pr.ChecksState {
		_ = store.UpdateAskBranchPRChecks(db, pr.ID, string(rollup))
	}

	switch rollup {
	case gh.ChecksSuccess:
		strategy := subPRAutoMergeStrategy(db)
		if mErr := ghc.PRMergeAuto(cwd, ghRepo, pr.PRNumber, strategy); mErr != nil {
			logger.Printf("sub-pr-ci-watch: auto-merge #%d failed: %v", pr.PRNumber, mErr)
			return
		}
		// Don't mark SubPRMerged here — wait for gh pr view to report merged=true.
	case gh.ChecksFailure:
		onSubPRCIFailed(db, pr, logger)
	case gh.ChecksPending:
		// mergeStateStatus was UNKNOWN so we can't use it. Fall back to the
		// age-based heuristic: 10m with zero checks → probably no CI.
		createdAt, parseErr := time.Parse("2006-01-02 15:04:05", pr.CreatedAt)
		if parseErr != nil {
			return
		}
		age := time.Since(createdAt)
		if age > subPRCIStaleLimit {
			onSubPRStalled(db, ghc, pr, logger)
			return
		}
		if age > missingCITimeout && len(checks) == 0 {
			onSubPRMissingCI(db, ghc, pr, logger)
		}
	}
}

func onSubPRMerged(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	// All four writes (mark PR merged, complete task, unblock deps, queue memory
	// task) must land atomically. A partial failure here is catastrophic: if we
	// mark the PR merged but don't complete the task, sub-pr-ci-watch will never
	// re-pick it up (PR no longer in state=Open), so the task is stuck forever.
	memoryPayload := fmt.Sprintf(`{"task":%q,"files":"","feedback":"merged via PR #%d","diff":"","repo":%q}`,
		"sub-PR merged", pr.PRNumber, pr.Repo)

	tx, err := db.Begin()
	if err != nil {
		logger.Printf("sub-pr-ci-watch: begin tx for sub-PR #%d merge: %v", pr.PRNumber, err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if err := store.MarkAskBranchPRMergedTx(tx, pr.ID); err != nil {
		logger.Printf("sub-pr-ci-watch: mark merged #%d failed: %v", pr.PRNumber, err)
		return
	}
	if err := store.UpdateBountyStatusTx(tx, pr.TaskID, "Completed"); err != nil {
		logger.Printf("sub-pr-ci-watch: update task %d status failed: %v", pr.TaskID, err)
		return
	}
	unblocked, err := store.UnblockDependentsOfTx(tx, pr.TaskID)
	if err != nil {
		logger.Printf("sub-pr-ci-watch: unblock dependents of task %d failed: %v", pr.TaskID, err)
		return
	}
	if _, err := store.AddBountyTx(tx, pr.TaskID, "WriteMemory", memoryPayload); err != nil {
		logger.Printf("sub-pr-ci-watch: queue WriteMemory for task %d failed: %v", pr.TaskID, err)
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("sub-pr-ci-watch: commit sub-PR #%d merge failed: %v", pr.PRNumber, err)
		return
	}

	// Post-commit side effects: fire webhook only after DB state is durable.
	store.FireWebhook(db, pr.TaskID, "Completed")
	if unblocked > 0 {
		logger.Printf("Task %d: unblocked %d dependent(s) after sub-PR merge", pr.TaskID, unblocked)
	}
	logger.Printf("sub-pr-ci-watch: task %d completed via sub-PR #%d merge", pr.TaskID, pr.PRNumber)
}

func onSubPRClosedExternally(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	msg := fmt.Sprintf("sub-PR #%d was closed without merge — requires operator decision", pr.PRNumber)
	if err := escalateSubPR(db, pr, store.SeverityMedium, msg); err != nil {
		logger.Printf("sub-pr-ci-watch: task %d escalation tx failed: %v", pr.TaskID, err)
		return
	}
	logger.Printf("sub-pr-ci-watch: task %d escalated — PR closed externally", pr.TaskID)
}

func onSubPRCIFailed(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	// Fetch the parent task's branch name so Medic can route a fix task to it.
	// Read outside the tx — it's a static property of the task row.
	parent, _ := store.GetBounty(db, pr.TaskID)
	branch := ""
	if parent != nil {
		branch = parent.BranchName
	}

	// Atomic sequence: increment failure_count + dedup-check + queue triage.
	// Without a tx, an increment could land without a triage queue — the next
	// tick would see a bumped count and queue another triage, creating an
	// off-by-one in Medic's retry cap enforcement.
	tx, err := db.Begin()
	if err != nil {
		logger.Printf("sub-pr-ci-watch: begin tx for sub-PR #%d CI failure: %v", pr.PRNumber, err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	newCount, countErr := store.IncrementAskBranchPRFailureCountTx(tx, pr.ID)
	if countErr != nil {
		logger.Printf("sub-pr-ci-watch: increment failure count #%d failed: %v", pr.ID, countErr)
		return
	}

	// Dedup check: boundary-match on JSON token so sub_pr_row_id=1 doesn't dedup
	// against 10, 11, 100, etc. JSON always follows the ID with `,` or `}`.
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'CIFailureTriage' AND status IN ('Pending', 'Locked')
		  AND (payload LIKE '%"sub_pr_row_id":' || ? || ',%'
		    OR payload LIKE '%"sub_pr_row_id":' || ? || '}%')`,
		pr.ID, pr.ID).Scan(&existing); err != nil {
		logger.Printf("sub-pr-ci-watch: dedup query failed for sub-PR #%d: %v", pr.PRNumber, err)
		return
	}
	if existing > 0 {
		// Commit so the failure_count increment lands even though no triage was queued.
		if err := tx.Commit(); err != nil {
			logger.Printf("sub-pr-ci-watch: commit failure count for #%d failed: %v", pr.PRNumber, err)
			return
		}
		logger.Printf("sub-pr-ci-watch: sub-PR #%d CI failed (count=%d) — triage already queued", pr.PRNumber, newCount)
		return
	}

	triageID, qErr := QueueCIFailureTriageTx(tx, ciTriagePayload{
		SubPRRowID: pr.ID, Repo: pr.Repo, PRNumber: pr.PRNumber,
		Branch: branch, TaskID: pr.TaskID,
	})
	if qErr != nil {
		// Can't queue triage — escalate instead. Roll back the tx so failure_count
		// isn't left bumped (we'll go down the escalation path outside the tx),
		// then escalate via the atomic escalateSubPR helper.
		tx.Rollback() //nolint:errcheck
		logger.Printf("sub-pr-ci-watch: failed to queue CIFailureTriage: %v — escalating directly", qErr)
		msg := fmt.Sprintf("sub-PR #%d CI failed and Medic triage could not be queued: %v", pr.PRNumber, qErr)
		if escErr := escalateSubPR(db, pr, store.SeverityMedium, msg); escErr != nil {
			logger.Printf("sub-pr-ci-watch: escalation tx failed: %v", escErr)
		}
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("sub-pr-ci-watch: commit CIFailureTriage for #%d failed: %v", pr.PRNumber, err)
		return
	}
	logger.Printf("sub-pr-ci-watch: sub-PR #%d CI failed (count=%d) — queued Medic triage #%d", pr.PRNumber, newCount, triageID)
}

// escalateSubPR atomically closes the AskBranchPR row, inserts an escalation,
// and sets the parent task to Escalated — all in one transaction so a partial
// failure never leaves the PR open while the task is already Escalated (or
// vice-versa). Fires the Escalated webhook only after commit so a rolled-back
// tx never emits spurious notifications.
func escalateSubPR(db *sql.DB, pr store.AskBranchPR, severity store.EscalationSeverity, msg string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := store.MarkAskBranchPRClosedTx(tx, pr.ID); err != nil {
		return fmt.Errorf("mark PR closed: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO Escalations (task_id, severity, message, status) VALUES (?, ?, ?, 'Open')`,
		pr.TaskID, string(severity), msg); err != nil {
		return fmt.Errorf("insert escalation: %w", err)
	}
	if err := store.UpdateBountyStatusWithErrorTx(tx, pr.TaskID, "Escalated", msg); err != nil {
		return fmt.Errorf("update task status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Post-commit: fire webhook so subscribers see the Escalated transition.
	store.FireWebhook(db, pr.TaskID, "Escalated")
	return nil
}

// triggerStalledRerunFn is the dependency-injection point for re-triggering
// a stuck CI run by force-pushing an empty commit. Tests swap it out.
var triggerStalledRerunFn = func(repoPath, branch, message string) error {
	return igit.TriggerCIRerun(repoPath, branch, message)
}

// SetTriggerStalledRerunForTest lets external packages install a fake re-trigger
// function for the duration of a test. Call the returned restore fn (or use
// t.Cleanup) to put the real impl back.
func SetTriggerStalledRerunForTest(fn func(repoPath, branch, message string) error) (restore func()) {
	prev := triggerStalledRerunFn
	triggerStalledRerunFn = fn
	return func() { triggerStalledRerunFn = prev }
}

// onSubPRStalled diagnoses a CI stall and attempts self-healing before
// escalating. Flow:
//
//  1. If we're past the hard limit, escalate unconditionally — either the
//     branch is truly broken or GitHub itself is not recovering.
//  2. Otherwise inspect per-check state via `gh pr checks`:
//     - All checks QUEUED (no runner ever picked up): stuck runner. If we
//       haven't hit the re-trigger cap, push an empty commit to force a new
//       check suite. Increment the retrigger counter.
//     - Any check IN_PROGRESS or PENDING: actively running but slow. Wait
//       another cycle without touching the branch — we'll re-check in 5 min.
//     - No checks at all: this is a misconfig-or-runner-outage case. Re-trigger
//       once (same empty-commit path) to shake GitHub out of it.
//  3. If re-trigger fails or we've already retried the max number of times,
//     escalate with a diagnosis-aware message.
//
// The counter (stall_retrigger_count on AskBranchPRs) persists across dog
// ticks so we can't loop forever — two re-triggers then escalate.
func onSubPRStalled(db *sql.DB, ghc interface {
	PRChecks(cwd, repo string, number int) ([]gh.PRCheck, gh.ChecksState, error)
}, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	// Age calc — hard-limit shortcut so we don't burn cycles diagnosing a
	// PR that's been stuck half a day.
	age := timeSinceCreatedAt(pr.CreatedAt)
	if age > subPRCIHardLimit {
		msg := fmt.Sprintf("sub-PR #%d: CI pending over %v (hard limit) — giving up on automated recovery", pr.PRNumber, subPRCIHardLimit)
		if err := escalateSubPR(db, pr, store.SeverityMedium, msg); err != nil {
			logger.Printf("sub-pr-ci-watch: task %d hard-limit escalation failed: %v", pr.TaskID, err)
			return
		}
		logger.Printf("sub-pr-ci-watch: task %d escalated — CI pending over hard limit %v", pr.TaskID, subPRCIHardLimit)
		return
	}

	// Already tried the max number of re-triggers — escalate now rather than
	// looping forever on a branch GitHub just won't run.
	if pr.StallRetriggerCount >= subPRMaxStallRetriggers {
		msg := fmt.Sprintf("sub-PR #%d: CI stall re-triggered %d× with no recovery — GitHub runner or workflow config issue", pr.PRNumber, pr.StallRetriggerCount)
		if err := escalateSubPR(db, pr, store.SeverityMedium, msg); err != nil {
			logger.Printf("sub-pr-ci-watch: task %d retrigger-cap escalation failed: %v", pr.TaskID, err)
			return
		}
		logger.Printf("sub-pr-ci-watch: task %d escalated — re-trigger cap reached", pr.TaskID)
		return
	}

	// Per-check diagnosis: we need to know WHY checks are pending. If a runner
	// actually picked up the work, IN_PROGRESS is our signal to keep waiting;
	// if everything is QUEUED, no runner ever engaged and we should nudge.
	repo := store.GetRepo(db, pr.Repo)
	cwd, ghRepo := "", ""
	if repo != nil {
		cwd = repo.LocalPath
		ghRepo = deriveGHRepoFromRemoteURL(repo.RemoteURL)
	}
	checks, _, checksErr := ghc.PRChecks(cwd, ghRepo, pr.PRNumber)
	if checksErr != nil {
		// If we can't even list checks, we can't diagnose — wait another tick.
		logger.Printf("sub-pr-ci-watch: stall diagnosis for #%d failed (%v) — deferring to next tick", pr.PRNumber, checksErr)
		return
	}

	// Scan checks: if any are IN_PROGRESS, the runner engaged and CI is just
	// slow — don't intervene. Every other state (QUEUED, PENDING, WAITING, or
	// an empty check list) is a runner/workflow-dispatch problem we can try
	// to unstick with an empty-commit push.
	inProgress := false
	for _, c := range checks {
		if strings.ToUpper(c.State) == "IN_PROGRESS" {
			inProgress = true
			break
		}
	}

	if inProgress {
		// Genuinely slow CI — the runner is chewing on it. Leave it alone; the
		// next tick will check again. Not a stall we need to intervene on.
		logger.Printf("sub-pr-ci-watch: sub-PR #%d still IN_PROGRESS at %v — deferring (slow CI, not stuck)", pr.PRNumber, age)
		return
	}

	// All checks QUEUED or there are simply zero checks — both are symptoms
	// of a runner/workflow problem we can try to unstick. Push an empty commit
	// to force a new check suite.
	branch := pr.Repo
	parent, _ := store.GetBounty(db, pr.TaskID)
	if parent != nil && parent.BranchName != "" {
		branch = parent.BranchName
	}
	if cwd == "" || branch == "" {
		// Missing infra — can't re-trigger, fall through to escalation path.
		msg := fmt.Sprintf("sub-PR #%d: CI stalled and repo/branch info missing for re-trigger", pr.PRNumber)
		if err := escalateSubPR(db, pr, store.SeverityMedium, msg); err != nil {
			logger.Printf("sub-pr-ci-watch: task %d missing-infra escalation failed: %v", pr.TaskID, err)
		}
		return
	}

	retriggerMsg := fmt.Sprintf("ci: retrigger stalled run (sub-PR #%d)", pr.PRNumber)
	if err := triggerStalledRerunFn(cwd, branch, retriggerMsg); err != nil {
		logger.Printf("sub-pr-ci-watch: sub-PR #%d re-trigger push failed: %v — escalating", pr.PRNumber, err)
		msg := fmt.Sprintf("sub-PR #%d: CI stall re-trigger failed: %v", pr.PRNumber, err)
		if escErr := escalateSubPR(db, pr, store.SeverityMedium, msg); escErr != nil {
			logger.Printf("sub-pr-ci-watch: task %d retrigger-fail escalation failed: %v", pr.TaskID, escErr)
		}
		return
	}
	newCount, _ := store.IncrementStallRetriggerCount(db, pr.ID)
	logger.Printf("sub-pr-ci-watch: sub-PR #%d stuck on QUEUED checks — pushed empty commit to re-trigger (attempt %d/%d)",
		pr.PRNumber, newCount, subPRMaxStallRetriggers)
}

// timeSinceCreatedAt parses pr.CreatedAt (SQLite datetime format) and returns
// the elapsed time since. Returns 0 on parse failure so callers treat the PR
// as fresh rather than wrongly escalating.
func timeSinceCreatedAt(createdAt string) time.Duration {
	t, err := time.Parse("2006-01-02 15:04:05", createdAt)
	if err != nil {
		return 0
	}
	return time.Since(t)
}

// queueAgentBranchRebase is the shared self-healing path for BEHIND and
// DIRTY sub-PRs. Both states are resolved by rebasing the agent branch onto
// the current ask-branch tip; Pilot's RebaseAgentBranch handler does a clean
// rebase when possible and spawns a RebaseConflict CodeEdit task for an
// astromech when the rebase itself conflicts.
//
// reason is just the merge-state label ("BEHIND" / "DIRTY") for log context.
// Idempotent via QueueRebaseAgentBranch's internal dedup on sub_pr_row_id.
func queueAgentBranchRebase(db *sql.DB, pr store.AskBranchPR, reason string, logger interface{ Printf(string, ...any) }) {
	bountyRow, _ := store.GetBounty(db, pr.TaskID)
	if bountyRow == nil || bountyRow.BranchName == "" {
		logger.Printf("sub-pr-ci-watch: sub-PR #%d %s but task %d has no branch_name — skipping rebase",
			pr.PRNumber, reason, pr.TaskID)
		return
	}
	ab := store.GetConvoyAskBranch(db, pr.ConvoyID, pr.Repo)
	if ab == nil {
		logger.Printf("sub-pr-ci-watch: sub-PR #%d %s but no ask-branch found for convoy %d / %s",
			pr.PRNumber, reason, pr.ConvoyID, pr.Repo)
		return
	}
	taskID, qErr := QueueRebaseAgentBranch(db, rebaseAgentPayload{
		SubPRRowID: pr.ID,
		TaskID:     pr.TaskID,
		Branch:     bountyRow.BranchName,
		AskBranch:  ab.AskBranch,
		ConvoyID:   pr.ConvoyID,
		Repo:       pr.Repo,
	})
	if qErr != nil {
		// Loop cap hit or other unrecoverable error — escalate the task and
		// stop tracking the sub-PR so the dog doesn't retry indefinitely.
		logger.Printf("sub-pr-ci-watch: sub-PR #%d %s — QueueRebaseAgentBranch failed: %v — escalating",
			pr.PRNumber, reason, qErr)
		msg := fmt.Sprintf("sub-PR #%d rebase loop terminated: %v", pr.PRNumber, qErr)
		if escErr := escalateSubPR(db, pr, store.SeverityHigh, msg); escErr != nil {
			logger.Printf("sub-pr-ci-watch: escalation failed for task %d: %v", pr.TaskID, escErr)
		}
		return
	}
	if taskID > 0 {
		logger.Printf("sub-pr-ci-watch: sub-PR #%d %s — queued RebaseAgentBranch #%d for branch %s",
			pr.PRNumber, reason, taskID, bountyRow.BranchName)
	} else {
		logger.Printf("sub-pr-ci-watch: sub-PR #%d %s — RebaseAgentBranch already queued, skipping",
			pr.PRNumber, reason)
	}
}

// onSubPRMergeBlocked handles a sub-PR that GitHub reports as BLOCKED with no
// failing CI checks. This means a branch-protection rule (required reviewer,
// mandatory status check we cannot trigger, CODEOWNERS, etc.) is preventing
// auto-merge. Self-healing is not possible — the operator must adjust repo
// settings or merge manually.
func onSubPRMergeBlocked(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	msg := fmt.Sprintf(
		"sub-PR #%d is BLOCKED by branch protection (all CI checks pass / no CI). "+
			"A required reviewer, mandatory status check, or CODEOWNERS rule is preventing auto-merge. "+
			"Either merge manually on GitHub or adjust repo branch protection settings.",
		pr.PRNumber,
	)
	if err := escalateSubPR(db, pr, store.SeverityMedium, msg); err != nil {
		logger.Printf("sub-pr-ci-watch: task %d escalation tx failed: %v", pr.TaskID, err)
		return
	}
	logger.Printf("sub-pr-ci-watch: task %d escalated — sub-PR #%d BLOCKED by branch protection", pr.TaskID, pr.PRNumber)
}

// mergeSubPRDirect merges a sub-PR immediately without waiting for CI. Called
// when mergeStateStatus=CLEAN (covers both "no CI" and "CI already green") or
// as a fallback after missingCITimeout with zero checks. The Jedi Council
// review is the quality gate; CI is additive.
func mergeSubPRDirect(db *sql.DB, ghc *gh.Client, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	repo := store.GetRepo(db, pr.Repo)
	if repo == nil {
		logger.Printf("sub-pr-ci-watch: task %d direct merge: repo %q not found", pr.TaskID, pr.Repo)
		return
	}
	strategy := subPRAutoMergeStrategy(db)
	logger.Printf("sub-pr-ci-watch: task %d merging PR #%d directly (%s)", pr.TaskID, pr.PRNumber, strategy)
	if mErr := ghc.PRMerge(repo.LocalPath, deriveGHRepoFromRemoteURL(repo.RemoteURL), pr.PRNumber, strategy); mErr != nil {
		logger.Printf("sub-pr-ci-watch: task %d direct merge #%d failed: %v", pr.TaskID, pr.PRNumber, mErr)
		return
	}
	onSubPRMerged(db, pr, logger)
}

// onSubPRMissingCI is the legacy name used by cycle1 tests; delegates to mergeSubPRDirect.
func onSubPRMissingCI(db *sql.DB, ghc *gh.Client, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	mergeSubPRDirect(db, ghc, pr, logger)
}

package agents

import (
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"force-orchestrator/internal/gh"
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

	// Step 3: record the PR.
	if _, storeErr := store.CreateAskBranchPR(db, bounty.ID, bounty.ConvoyID, bounty.TargetRepo, res.URL, res.Number); storeErr != nil {
		logger.Printf("Task %d: created PR #%d but failed to record it: %v", bounty.ID, res.Number, storeErr)
		return gh.ErrClassPermanent, fmt.Errorf("store PR: %w", storeErr)
	}

	// Step 4: transition task to AwaitingSubPRCI. Also clear owner/locked_at so
	// the task doesn't appear stuck in Locked state — sub-pr-ci-watch drives
	// all further transitions.
	if _, err := db.Exec(`UPDATE BountyBoard
		SET status = ?, owner = '', locked_at = '', error_log = ''
		WHERE id = ?`, subPRCITaskStatus, bounty.ID); err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("update task status: %w", err)
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

	// Update the stored base SHA to the new tip. The new tip is the local
	// HEAD of the ask-branch after astromech committed.
	tipOut, tipErr := exec.Command("git", "-C", repo.LocalPath, "rev-parse", ab.AskBranch).CombinedOutput()
	if tipErr != nil {
		return gh.ErrClassPermanent, fmt.Errorf("rev-parse %s: %s", ab.AskBranch, strings.TrimSpace(string(tipOut)))
	}
	newTip := strings.TrimSpace(string(tipOut))
	if err := store.UpdateConvoyAskBranchBase(db, bounty.ConvoyID, bounty.TargetRepo, newTip); err != nil {
		return gh.ErrClassPermanent, fmt.Errorf("record new base SHA: %w", err)
	}

	// Mark the task Completed directly — there's no CI step because no PR
	// was opened. Unblock any dependents.
	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	if n := store.UnblockDependentsOf(db, bounty.ID); n > 0 {
		logger.Printf("Task %d: unblocked %d dependent(s) after ask-branch resolution", bounty.ID, n)
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

// missingCITimeout is how long we wait for at least one CI check to appear on
// a sub-PR before escalating. Ten minutes is generous — Jenkins typically
// starts within seconds of a push; an empty check list past this threshold
// means the repo isn't wired up and the task needs operator attention.
const missingCITimeout = 10 * time.Minute

// subPRCIStaleLimit caps how long a sub-PR's CI may keep us in Pending state
// before we flag the task stalled. 2 hours is far beyond normal CI runtimes
// and points to a stuck build.
const subPRCIStaleLimit = 2 * time.Hour

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

	// Poll CI state.
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
		// Queue auto-merge. Squash by default.
		strategy := subPRAutoMergeStrategy(db)
		if mErr := ghc.PRMergeAuto(cwd, ghRepo, pr.PRNumber, strategy); mErr != nil {
			logger.Printf("sub-pr-ci-watch: auto-merge #%d failed: %v", pr.PRNumber, mErr)
			return
		}
		// Don't mark SubPRMerged here — wait for gh pr view to report merged=true.
		// GitHub's auto-merge happens asynchronously; the next tick will confirm.
	case gh.ChecksFailure:
		onSubPRCIFailed(db, pr, logger)
	case gh.ChecksPending:
		// Two staleness gates apply:
		//   (a) missingCITimeout (10m): if the PR has ZERO checks reported for
		//       this long, the repo almost certainly has no CI configured for
		//       the ask-branch target. Escalate early — waiting 2h for a CI
		//       run that isn't coming is operator grief.
		//   (b) subPRCIStaleLimit (2h): hard upper bound. Any Pending sub-PR
		//       this old points to a stuck CI system that needs attention.
		createdAt, parseErr := time.Parse("2006-01-02 15:04:05", pr.CreatedAt)
		if parseErr != nil {
			return
		}
		age := time.Since(createdAt)
		if age > subPRCIStaleLimit {
			onSubPRStalled(db, pr, logger)
			return
		}
		// Early escalation: zero checks AFTER missingCITimeout. An empty checks
		// slice IS the signal — GitHub would return >=1 check the moment any
		// workflow is triggered on the push.
		if age > missingCITimeout && len(checks) == 0 {
			onSubPRMissingCI(db, pr, logger)
		}
	}
}

func onSubPRMerged(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	if err := store.MarkAskBranchPRMerged(db, pr.ID); err != nil {
		logger.Printf("sub-pr-ci-watch: mark merged #%d failed: %v", pr.PRNumber, err)
		return
	}
	store.UpdateBountyStatus(db, pr.TaskID, "Completed")
	if n := store.UnblockDependentsOf(db, pr.TaskID); n > 0 {
		logger.Printf("Task %d: unblocked %d dependent(s) after sub-PR merge", pr.TaskID, n)
	}
	// Legacy memory-write task — keeps the Librarian in the loop even under PR flow.
	// The payload is lightweight here (no diff); Librarian falls back to the task
	// description. Phase 7 could enrich this if we want proper convoy-level memories.
	payload := fmt.Sprintf(`{"task":%q,"files":"","feedback":"merged via PR #%d","diff":"","repo":%q}`,
		"sub-PR merged", pr.PRNumber, pr.Repo)
	store.AddBounty(db, pr.TaskID, "WriteMemory", payload)
	logger.Printf("sub-pr-ci-watch: task %d completed via sub-PR #%d merge", pr.TaskID, pr.PRNumber)
}

func onSubPRClosedExternally(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	_ = store.MarkAskBranchPRClosed(db, pr.ID)
	msg := fmt.Sprintf("sub-PR #%d was closed without merge — requires operator decision", pr.PRNumber)
	// Task status update is the critical transition here — if it fails, the
	// task stays in AwaitingSubPRCI and sub-pr-ci-watch will loop. We create
	// the escalation regardless so the operator at least sees the PR was closed.
	if _, err := db.Exec(`UPDATE BountyBoard SET status = 'Escalated', owner = '', locked_at = '', error_log = ? WHERE id = ?`, msg, pr.TaskID); err != nil {
		logger.Printf("sub-pr-ci-watch: task %d update failed (%v) — will retry next tick; escalation still created", pr.TaskID, err)
	}
	CreateEscalation(db, pr.TaskID, store.SeverityMedium, msg)
	logger.Printf("sub-pr-ci-watch: task %d escalated — PR closed externally", pr.TaskID)
}

func onSubPRCIFailed(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	newCount, _ := store.IncrementAskBranchPRFailureCount(db, pr.ID)
	// Avoid piling up duplicate CIFailureTriage tasks for the same PR — if one is
	// already Pending or Locked, let it run before queueing another.
	// Boundary-match on the JSON token so sub_pr_row_id=1 doesn't dedup against
	// 10, 11, 100, etc. The JSON emitted by QueueCIFailureTriage always follows
	// the ID with `,` or `}`, so we look for both shapes.
	var existing int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'CIFailureTriage' AND status IN ('Pending', 'Locked')
		  AND (payload LIKE '%"sub_pr_row_id":' || ? || ',%'
		    OR payload LIKE '%"sub_pr_row_id":' || ? || '}%')`,
		pr.ID, pr.ID).Scan(&existing)
	if existing > 0 {
		logger.Printf("sub-pr-ci-watch: sub-PR #%d CI failed (count=%d) — triage already queued", pr.PRNumber, newCount)
		return
	}
	// Fetch the parent task's branch name so Medic can route a fix task to it.
	parent, _ := store.GetBounty(db, pr.TaskID)
	branch := ""
	if parent != nil {
		branch = parent.BranchName
	}
	triageID, err := QueueCIFailureTriage(db, ciTriagePayload{
		SubPRRowID: pr.ID, Repo: pr.Repo, PRNumber: pr.PRNumber,
		Branch: branch, TaskID: pr.TaskID,
	})
	if err != nil {
		logger.Printf("sub-pr-ci-watch: failed to queue CIFailureTriage: %v — escalating directly", err)
		msg := fmt.Sprintf("sub-PR #%d CI failed and Medic triage could not be queued: %v", pr.PRNumber, err)
		if _, uErr := db.Exec(`UPDATE BountyBoard SET status = 'Escalated', owner = '', locked_at = '', error_log = ? WHERE id = ?`, msg, pr.TaskID); uErr != nil {
			logger.Printf("sub-pr-ci-watch: task %d status update also failed (%v); escalation still recorded", pr.TaskID, uErr)
		}
		CreateEscalation(db, pr.TaskID, store.SeverityMedium, msg)
		return
	}
	logger.Printf("sub-pr-ci-watch: sub-PR #%d CI failed (count=%d) — queued Medic triage #%d", pr.PRNumber, newCount, triageID)
}

func onSubPRStalled(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	msg := fmt.Sprintf("sub-PR #%d: CI pending over %v — stuck or misconfigured", pr.PRNumber, subPRCIStaleLimit)
	CreateEscalation(db, pr.TaskID, store.SeverityMedium, msg)
	logger.Printf("sub-pr-ci-watch: task %d escalated — Pending for %v", pr.TaskID, subPRCIStaleLimit)
}

// onSubPRMissingCI handles the specific case of a sub-PR with ZERO checks
// reported after missingCITimeout (10m). This almost always means the repo
// hasn't configured CI for branches targeting the ask-branch. Escalate so
// the operator can wire up CI or manually approve-and-merge; otherwise the
// task would wait subPRCIStaleLimit (2h) for no reason.
//
// Dedupe: we emit the escalation only once per sub-PR (the failure_count
// tracking serves as a soft marker). On subsequent dog ticks we re-check
// the zero-checks condition, but only escalate if there's no existing
// Open escalation for this task.
func onSubPRMissingCI(db *sql.DB, pr store.AskBranchPR, logger interface{ Printf(string, ...any) }) {
	var openEsc int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations
		WHERE task_id = ? AND status = 'Open'`, pr.TaskID).Scan(&openEsc)
	if openEsc > 0 {
		return
	}
	msg := fmt.Sprintf("sub-PR #%d: no CI checks reported in %v — ask-branch likely has no CI configured. Operator: wire up CI or manually merge/reject.",
		pr.PRNumber, missingCITimeout)
	CreateEscalation(db, pr.TaskID, store.SeverityLow, msg)
	logger.Printf("sub-pr-ci-watch: task %d flagged — no CI checks after %v", pr.TaskID, missingCITimeout)
}

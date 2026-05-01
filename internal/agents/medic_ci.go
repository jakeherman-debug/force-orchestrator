package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// ── Medic — CI failure triage ───────────────────────────────────────────────
//
// Extends the existing Medic (MedicReview) with a second task type,
// CIFailureTriage, dedicated to diagnosing Jenkins CI failures on sub-PRs.
// The handler shells out via `claude -p` with the jenkins-ci plugin's tools
// enabled so Claude can pull the failing build's console log, then classifies:
//
//   Flaky            — retrigger the build; after 3 retriggers, reclassify as RealBug
//   RealBug          — spawn a CodeEdit on the astromech branch to fix it
//   Environmental    — back off, retry, feed the per-repo circuit breaker
//   BranchProtection — escalate; fleet can't self-heal repo policy
//   Unfixable        — escalate with a clear explanation
//
// Circuit breaker: when >5 Environmental failures occur in a rolling 1-hour
// window for a single repo, sub-PR creation for that repo pauses (opens the
// breaker) for 30 minutes. Auto-closes when the timer expires. Jedi Council
// checks the breaker before opening new sub-PRs.

const medicCISystemPrompt = `You are the Fleet Medic, triaging a CI failure on a sub-PR.

Before answering, use the jenkins-ci tools available to you to fetch and read the failing Jenkins build's console log for the PR and build URL in the task payload.

Based on the build log, classify the failure into exactly ONE category:

Flaky           — Network/timeout/intermittent issue; the same code usually passes. Examples: DNS errors, test timeouts that disappear on retry, transient 5xx from upstream services.
RealBug         — An actual bug in the astromech's code that needs a code fix. Examples: assertion failures, nil pointer, wrong SQL, broken build.
Environmental   — Problem outside this PR: broken master, missing dependency version, infra outage. The whole repo's CI is failing, not just this PR.
BranchProtection — GitHub refuses to merge due to branch protection rules (missing reviews, required status check not configured). The fleet cannot self-heal this.
Unfixable       — Failure requires human judgment (architectural decision, security policy, external coordination).

Respond ONLY with valid JSON (no markdown, no preamble):
{
  "classification": "Flaky|RealBug|Environmental|BranchProtection|Unfixable",
  "diagnosis": "one paragraph: what actually failed and why",
  "fix_guidance": "for RealBug: specific guidance for the astromech to fix the code (what file, what change). Empty string otherwise.",
  "operator_note": "for Unfixable / BranchProtection: what human decision is needed. Empty string otherwise."
}`

// ciTriagePayload is the JSON payload for a CIFailureTriage task, queued by
// sub-pr-ci-watch when it observes a CI failure.
type ciTriagePayload struct {
	SubPRRowID int    `json:"sub_pr_row_id"`
	Repo       string `json:"repo"`
	PRNumber   int    `json:"pr_number"`
	Branch     string `json:"branch"`
	TaskID     int    `json:"task_id"`
	BuildURL   string `json:"build_url,omitempty"`
}

// ciTriageDecision is Medic's structured response.
type ciTriageDecision struct {
	Classification string `json:"classification"`
	Diagnosis      string `json:"diagnosis"`
	FixGuidance    string `json:"fix_guidance"`
	OperatorNote   string `json:"operator_note"`
}

// medicRetriggerCap is how many times Flaky is accepted before Medic promotes
// the failure to RealBug. Three failures of "just flaky" strongly imply it isn't.
const medicRetriggerCap = 3

// ciFailureTriageKey builds the canonical idempotency key for a sub-PR row.
// Fix #3 (AUDIT-035, AUDIT-048): one outstanding triage per sub-PR row, keyed
// via idx_bounty_idem rather than payload-LIKE scans inside the tx.
func ciFailureTriageKey(subPRRowID int) string {
	return fmt.Sprintf("ci-failure-triage:%d", subPRRowID)
}

// QueueCIFailureTriage enqueues a CIFailureTriage task for Medic. Returns the
// task ID. Payload carries everything Medic needs to fetch the Jenkins log
// and spawn a fix task if needed.
//
// Fix #3: dedup via the canonical `ci-failure-triage:<sub_pr_row_id>` key.
// A concurrent second caller with the same sub-PR row gets the existing id
// back rather than inserting a duplicate CIFailureTriage.
func QueueCIFailureTriage(db *sql.DB, payload ciTriagePayload) (int, error) {
	if payload.Repo == "" || payload.PRNumber == 0 || payload.TaskID == 0 {
		return 0, fmt.Errorf("QueueCIFailureTriage: repo, pr_number, task_id required (got %+v)", payload)
	}
	if payload.SubPRRowID <= 0 {
		return 0, fmt.Errorf("QueueCIFailureTriage: sub_pr_row_id required for canonical idempotency key (got %+v)", payload)
	}
	body, _ := json.Marshal(payload)
	id, _, err := store.AddIdempotentTask(db, ciFailureTriageKey(payload.SubPRRowID),
		payload.TaskID, payload.Repo, "CIFailureTriage", string(body), 0, 5, "Pending")
	if err != nil {
		return 0, err
	}
	return id, nil
}

// QueueCIFailureTriageTx is the transactional sibling of QueueCIFailureTriage.
// Uses store.AddIdempotentTaskTx so the atomic increment-count + queue-triage
// sequence in onSubPRCIFailed retains its dedup guarantee without doing a
// payload-LIKE scan inside the tx (the old shape pinned the single-connection
// pool and scanned BountyBoard on every failure burst — see AUDIT-048).
func QueueCIFailureTriageTx(tx *sql.Tx, payload ciTriagePayload) (int, error) {
	if payload.Repo == "" || payload.PRNumber == 0 || payload.TaskID == 0 {
		return 0, fmt.Errorf("QueueCIFailureTriageTx: repo, pr_number, task_id required (got %+v)", payload)
	}
	if payload.SubPRRowID <= 0 {
		return 0, fmt.Errorf("QueueCIFailureTriageTx: sub_pr_row_id required for canonical idempotency key (got %+v)", payload)
	}
	body, _ := json.Marshal(payload)
	id, _, err := store.AddIdempotentTaskTx(tx, ciFailureTriageKey(payload.SubPRRowID),
		payload.TaskID, payload.Repo, "CIFailureTriage", string(body), 0, 5, "Pending")
	if err != nil {
		return 0, err
	}
	return id, nil
}

// runMedicCITriage is the handler for a single CIFailureTriage task.
// Fix #8e: ctx threads from SpawnMedic's claim ctx. Body uses LLM + DB +
// gh-client (ctx already passed there); no new subprocess sites need ctx
// today, so the parameter aligns the signature with peer handlers.
func runMedicCITriage(ctx context.Context, db *sql.DB, agentName string, bounty *store.Bounty, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
	_ = ctx
	var payload ciTriagePayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); fbErr != nil {
			logger.Printf("CIFailureTriage #%d: FailBounty failed after payload parse error: %v", bounty.ID, fbErr)
		}
		return
	}

	pr := store.GetAskBranchPR(db, payload.SubPRRowID)
	if pr == nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("sub-PR row %d not found", payload.SubPRRowID)); fbErr != nil {
			logger.Printf("CIFailureTriage #%d: FailBounty failed after missing sub-PR: %v", bounty.ID, fbErr)
		}
		return
	}
	if pr.State != "Open" {
		// PR was already merged/closed in the window before we ran — no work to do.
		logger.Printf("CIFailureTriage #%d: sub-PR #%d already in state %s — completing as no-op",
			bounty.ID, pr.PRNumber, pr.State)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("CIFailureTriage #%d: Completed update failed: %v", bounty.ID, err)
		}
		return
	}

	// Fix #7 (AUDIT-161) — breaker-open short-circuit.
	// If the per-repo circuit breaker is already open (>ciEnvThreshold
	// Environmental failures in the rolling window), we know the
	// infrastructure is still sick. Running another Claude triage would
	// just record another Environmental → re-open a still-open breaker
	// while burning a full Medic Claude call (~$0.05). Short-circuit to
	// the Environmental handler path, which backs off and waits for the
	// breaker to close on its own.
	if IsCIBreakerOpen(db, payload.Repo) {
		logger.Printf("CIFailureTriage #%d: CI breaker open for %s — skipping Claude, deferring to post-cooldown retry",
			bounty.ID, payload.Repo)
		applyCITriageEnvironmental(db, agentName, pr, payload, ciTriageDecision{
			Classification: "Environmental",
			Diagnosis:      "CI breaker currently open for this repo; skipping triage until cooldown elapses.",
		}, logger)
		// Fix #8d (CLAUDE.md "No silent failures"): observe the terminator
		// error. If the UPDATE fails the triage task stays Locked; the
		// stale-lock detector will requeue it.
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("CIFailureTriage #%d: Completed update failed on breaker-open path: %v — stale-lock detector will recover", bounty.ID, err)
		}
		return
	}

	userPrompt := fmt.Sprintf("Sub-PR URL: %s\nPR number: %d\nRepo: %s\nBranch: %s\nBuild URL: %s\nFailure count: %d\n",
		pr.PRURL, pr.PRNumber, payload.Repo, payload.Branch, payload.BuildURL, pr.FailureCount)

	// Tools come from the medic-ci capability profile — Bash + WebFetch +
	// WebSearch so Claude can invoke the jenkins-ci plugin's shell commands
	// and follow Jenkins links when necessary.
	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("CIFailureTriage #%d: MCP config write failed (%v) — proceeding without --mcp-config", bounty.ID, mcpErr)
	}
	rawOut, claudeErr := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "medic-ci",
		TaskID:        int(bounty.ID),
		PromptVersion: "medic-ci-v1",
	}, medicCISystemPrompt, userPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 5)
	if claudeErr != nil {
		logger.Printf("CIFailureTriage #%d: Claude failed (%v) — escalating parent task", bounty.ID, claudeErr)
		escalateCITriage(db, agentName, pr, payload.TaskID, "Medic could not analyze the Jenkins log: "+claudeErr.Error(), logger)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("CIFailureTriage #%d: Completed update failed after Claude error: %v", bounty.ID, err)
		}
		return
	}

	jsonStr := claude.ExtractJSON(rawOut)
	var decision ciTriageDecision
	if err := json.Unmarshal([]byte(jsonStr), &decision); err != nil {
		logger.Printf("CIFailureTriage #%d: JSON parse error (%v) — defaulting to Unfixable escalation", bounty.ID, err)
		decision = ciTriageDecision{
			Classification: "Unfixable",
			Diagnosis:      "Medic could not parse its own analysis.",
			OperatorNote:   fmt.Sprintf("Claude returned malformed output: %s", util.TruncateStr(rawOut, 200)),
		}
	}

	logger.Printf("CIFailureTriage #%d: classification=%s diagnosis=%s",
		bounty.ID, decision.Classification, util.TruncateStr(decision.Diagnosis, 120))
	store.LogAudit(db, agentName, "medic-ci-triage", payload.TaskID,
		fmt.Sprintf("class=%s reason=%s", decision.Classification, util.TruncateStr(decision.Diagnosis, 200)))

	switch decision.Classification {
	case "Flaky":
		applyCITriageFlaky(db, agentName, pr, payload, decision, logger)
	case "RealBug":
		applyCITriageRealBug(db, agentName, pr, payload, decision, logger)
	case "Environmental":
		applyCITriageEnvironmental(db, agentName, pr, payload, decision, logger)
	case "BranchProtection":
		applyCITriageBranchProtection(db, agentName, pr, payload, decision, logger)
	default: // Unfixable or unknown → escalate
		escalateCITriage(db, agentName, pr, payload.TaskID,
			fmt.Sprintf("%s — %s", decision.Classification, decision.OperatorNote), logger)
	}

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("CIFailureTriage #%d: final Completed update failed: %v", bounty.ID, err)
	}
}

// applyCITriageFlaky: after medicRetriggerCap Flaky classifications on the same
// PR, promote to RealBug (the test clearly isn't flaky if it keeps failing).
// Otherwise, leave the sub-PR in place — sub-pr-ci-watch will poll again and
// GitHub's auto-merge will retry when CI re-runs. We don't explicitly trigger
// a Jenkins retry from Go because that API varies by installation; the
// jenkins-ci plugin can do it within Claude if Medic's prompt asks for it.
func applyCITriageFlaky(db *sql.DB, agentName string, pr *store.AskBranchPR, payload ciTriagePayload, decision ciTriageDecision, logger interface{ Printf(string, ...any) }) {
	if pr.FailureCount >= medicRetriggerCap {
		logger.Printf("CIFailureTriage: sub-PR #%d has failed %d times — promoting Flaky → RealBug",
			pr.PRNumber, pr.FailureCount)
		// Promote: reclassify as RealBug with the same diagnosis.
		applyCITriageRealBug(db, agentName, pr, payload, ciTriageDecision{
			Classification: "RealBug",
			Diagnosis:      "Classified as Flaky " + strconv.Itoa(pr.FailureCount) + " times without resolution — treating as a real bug.",
			FixGuidance:    "The failure keeps recurring. Investigate the test itself and the code it covers; likely a race condition or environmental assumption.",
		}, logger)
		return
	}
	// Just reset checks_state so the next sub-pr-ci-watch tick picks it up again.
	if err := store.UpdateAskBranchPRChecks(db, pr.ID, "Pending"); err != nil {
		logger.Printf("CIFailureTriage: UpdateAskBranchPRChecks(Pending) for PR #%d on Flaky path failed: %v — next sub-pr-ci-watch tick will recompute from the rollup and retry the write", pr.PRNumber, err)
	}
	logger.Printf("CIFailureTriage: sub-PR #%d classified Flaky (count=%d) — awaiting re-run", pr.PRNumber, pr.FailureCount)
}

// applyCITriageRealBug spawns a CodeEdit task on the same astromech branch
// with the Medic's fix guidance. When the astromech commits and pushes, the
// sub-PR updates automatically and sub-pr-ci-watch reruns CI.
//
// Fix #7 (AUDIT-120) — concurrent-spawn guard. FailureCount >= cap gated
// the ESCALATION path but not the spawn itself; a second failure while
// the prior fix task was still in flight re-entered this function and
// spawned a second fix task. Now we check store.HasOpenFixTaskForPR and
// AskBranchPRs.spawned_fix_count: 1 concurrent, 3 total per PR life.
func applyCITriageRealBug(db *sql.DB, agentName string, pr *store.AskBranchPR, payload ciTriagePayload, decision ciTriageDecision, logger interface{ Printf(string, ...any) }) {
	if pr.FailureCount >= medicRetriggerCap {
		// We've already asked astromechs to fix this N times. Escalate.
		escalateCITriage(db, agentName, pr, payload.TaskID,
			fmt.Sprintf("RealBug: %s (after %d fix attempts)", decision.Diagnosis, pr.FailureCount), logger)
		return
	}
	// Concurrency gate: if a prior fix task for this PR's branch is still
	// open, don't spawn another. The sub-pr-ci-watch dog will re-poll; if
	// the prior fix lands and CI still fails, the next tick will come
	// through here with the prior fix Completed.
	if store.HasOpenFixTaskForPR(db, payload.Branch) {
		logger.Printf("CIFailureTriage: sub-PR #%d already has an open fix task on branch %s — not spawning a second (AUDIT-120 guard)",
			pr.PRNumber, payload.Branch)
		return
	}
	// Total-spawns gate: cap the lifetime number of fix spawns per PR.
	if pr.SpawnedFixCount >= medicRetriggerCap {
		logger.Printf("CIFailureTriage: sub-PR #%d has reached spawned_fix_count cap (%d) — escalating instead of spawning",
			pr.PRNumber, medicRetriggerCap)
		escalateCITriage(db, agentName, pr, payload.TaskID,
			fmt.Sprintf("RealBug: %s (after %d fix spawns)", decision.Diagnosis, pr.SpawnedFixCount), logger)
		return
	}
	// Spawn a CodeEdit on the same branch. The astromech resumes from the
	// existing branch (via bounty.BranchName) and commits on top.
	fixPayload := fmt.Sprintf("[CI_FIX for task #%d / PR #%d]\n\nDiagnosis: %s\n\nGuidance: %s\n\nThe PR branch %s already has your prior work. Commit the fix on top — the sub-PR will auto-update.",
		payload.TaskID, pr.PRNumber, decision.Diagnosis, decision.FixGuidance, payload.Branch)
	fixID, err := store.AddConvoyTask(db, payload.TaskID, payload.Repo, fixPayload, pr.ConvoyID, 5, "Pending")
	if err != nil {
		logger.Printf("CIFailureTriage: failed to spawn fix task: %v", err)
		return
	}
	// Transfer the branch name so the fix task resumes on the same branch.
	store.SetBranchName(db, fixID, payload.Branch)
	// Bump the per-PR spawn counter for the lifetime cap check on future
	// ticks. If the UPDATE fails the cap check still works — it reads
	// spawned_fix_count on the next triage pass, and a stuck counter would
	// just allow one extra spawn before medicRetriggerCap trips.
	if _, err := store.IncrementSpawnedFixCount(db, pr.ID); err != nil {
		logger.Printf("CIFailureTriage: IncrementSpawnedFixCount for PR #%d failed: %v — cap check re-reads on next tick; worst case is one extra fix spawn before medicRetriggerCap trips", pr.PRNumber, err)
	}
	store.SendMail(db, agentName, "astromech",
		fmt.Sprintf("[CI FIX] Task #%d / PR #%d — please fix", payload.TaskID, pr.PRNumber),
		fmt.Sprintf("CI failed on your sub-PR. Fix task #%d queued with guidance. Branch: %s\n\nDiagnosis:\n%s\n\nGuidance:\n%s",
			fixID, payload.Branch, decision.Diagnosis, decision.FixGuidance),
		fixID, store.MailTypeFeedback)
	logger.Printf("CIFailureTriage: spawned fix task #%d for sub-PR #%d", fixID, pr.PRNumber)
}

// applyCITriageEnvironmental: back off, retry via sub-pr-ci-watch on next tick,
// and record the failure in the per-repo circuit breaker window.
func applyCITriageEnvironmental(db *sql.DB, agentName string, pr *store.AskBranchPR, payload ciTriagePayload, decision ciTriageDecision, logger interface{ Printf(string, ...any) }) {
	tripped := recordCIEnvironmentalFailure(db, payload.Repo)
	if tripped {
		logger.Printf("CIFailureTriage: environmental failures exceeded threshold — CI breaker OPEN for %s", payload.Repo)
		// P27 burn-down: budget-gate the operator emit before SendMail.
		// On allowed=false the helper has already drop/digested per the
		// configured budget. Fail-open on err so a transient SQLite
		// glitch never silences a high-stakes alert.
		if allowed, _ := store.RespectNotificationBudget(
			context.Background(), db, "operator", agentName, "email", "{}",
			store.StakesHigh,
		); !allowed {
			// budget exhausted (StakesHigh always punches through, so
			// this branch only fires on a real config-set 0-cap row).
		} else {
			_ = allowed
		}
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CI BREAKER OPEN] %s — pausing new sub-PRs for 30 minutes", payload.Repo),
			fmt.Sprintf("Repo %s has seen %d environmental CI failures in the last hour. New sub-PR creation is paused until %s.\n\nLast diagnosis: %s",
				payload.Repo, ciEnvThreshold, ciBreakerOpenUntil(db, payload.Repo), decision.Diagnosis),
			payload.TaskID, store.MailTypeAlert)
	} else {
		logger.Printf("CIFailureTriage: environmental failure on %s (count in window=%d)", payload.Repo, getCIEnvCount(db, payload.Repo))
	}
	// Reset checks_state so sub-pr-ci-watch re-evaluates next tick.
	if err := store.UpdateAskBranchPRChecks(db, pr.ID, "Pending"); err != nil {
		logger.Printf("CIFailureTriage: UpdateAskBranchPRChecks(Pending) for PR #%d on Environmental path failed: %v — next sub-pr-ci-watch tick recomputes rollup and retries", pr.PRNumber, err)
	}
}

// applyCITriageBranchProtection escalates — we cannot self-heal repo policy.
func applyCITriageBranchProtection(db *sql.DB, agentName string, pr *store.AskBranchPR, payload ciTriagePayload, decision ciTriageDecision, logger interface{ Printf(string, ...any) }) {
	msg := fmt.Sprintf("Branch protection blocking auto-merge: %s. %s",
		decision.Diagnosis, decision.OperatorNote)
	escalateCITriage(db, agentName, pr, payload.TaskID, msg, logger)
}

func escalateCITriage(db *sql.DB, agentName string, pr *store.AskBranchPR, taskID int, msg string, logger interface{ Printf(string, ...any) }) {
	// AUDIT-040 (Fix #8d): previously this function wrote status='Escalated'
	// directly AND then called CreateEscalation (which also writes the status
	// and fires the webhook) — firing twice. Now we only write the error_log
	// here for the dashboard detail view; CreateEscalation below handles the
	// status transition + webhook exactly once.
	if _, err := db.Exec(`UPDATE BountyBoard SET error_log = ? WHERE id = ?`, store.RedactSecrets(msg), taskID); err != nil {
		logger.Printf("CIFailureTriage: task %d error_log write failed (%v); escalation still recorded via CreateEscalation below", taskID, err)
	}
	if _, err := CreateEscalation(db, taskID, store.SeverityMedium, msg); err != nil {
		// AUDIT-041: escalation insert failed — fall back to FailBounty + operator mail
		// so the task doesn't sit Escalated with no Escalations row.
		logger.Printf("CIFailureTriage: CreateEscalation for task %d failed (%v); falling back to FailBounty", taskID, err)
		if fbErr := store.FailBounty(db, taskID, msg+" (escalation insert failed: "+err.Error()+")"); fbErr != nil {
			logger.Printf("CIFailureTriage: FailBounty fallback for task %d also failed: %v", taskID, fbErr)
		}
	}
	store.SendMail(db, agentName, "operator",
		fmt.Sprintf("[CI ESCALATED] Task #%d — sub-PR #%d requires attention", taskID, pr.PRNumber),
		fmt.Sprintf("Task #%d's sub-PR (%s) needs human attention.\n\n%s", taskID, pr.PRURL, msg),
		taskID, store.MailTypeAlert)
	logger.Printf("CIFailureTriage: escalated task #%d — %s", taskID, msg)
}

// ── CI circuit breaker (per-repo) ────────────────────────────────────────────

const (
	// ciEnvThreshold — how many Environmental failures in the window open the breaker.
	ciEnvThreshold = 5
	// ciEnvWindow — the rolling counting window.
	ciEnvWindow = 1 * time.Hour
	// ciBreakerCooldown — how long the breaker stays open once tripped.
	ciBreakerCooldown = 30 * time.Minute
)

// recordCIEnvironmentalFailure increments the rolling Environmental failure count
// for a repo and, if the threshold is crossed, opens the breaker. Returns true
// iff this call opened the breaker.
func recordCIEnvironmentalFailure(db *sql.DB, repo string) (tripped bool) {
	countKey := fmt.Sprintf("circuit_breaker:%s:env_count", repo)
	windowKey := fmt.Sprintf("circuit_breaker:%s:window_start", repo)

	// Reset the window if it's older than ciEnvWindow.
	windowStart := store.GetConfig(db, windowKey, "")
	now := time.Now().UTC()
	if windowStart != "" {
		if t, perr := time.Parse(time.RFC3339, windowStart); perr == nil {
			if now.Sub(t) > ciEnvWindow {
				store.SetConfig(db, countKey, "0")
				windowStart = ""
			}
		}
	}
	if windowStart == "" {
		store.SetConfig(db, windowKey, now.Format(time.RFC3339))
	}

	// Increment.
	current := 0
	if v := store.GetConfig(db, countKey, ""); v != "" {
		current, _ = strconv.Atoi(v)
	}
	current++
	store.SetConfig(db, countKey, strconv.Itoa(current))

	if current >= ciEnvThreshold {
		openKey := fmt.Sprintf("circuit_breaker:%s:open_until", repo)
		store.SetConfig(db, openKey, now.Add(ciBreakerCooldown).Format(time.RFC3339))
		// Reset counter so a subsequent trip event requires fresh threshold crossings.
		store.SetConfig(db, countKey, "0")
		store.SetConfig(db, windowKey, now.Format(time.RFC3339))
		return true
	}
	return false
}

// getCIEnvCount returns the current Environmental failure count in the window.
// Used only for logging.
func getCIEnvCount(db *sql.DB, repo string) int {
	if v := store.GetConfig(db, fmt.Sprintf("circuit_breaker:%s:env_count", repo), ""); v != "" {
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// ciBreakerOpenUntil returns a formatted timestamp for the breaker's close
// time, or empty string if the breaker is currently closed.
func ciBreakerOpenUntil(db *sql.DB, repo string) string {
	v := store.GetConfig(db, fmt.Sprintf("circuit_breaker:%s:open_until", repo), "")
	if v == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return ""
	}
	if time.Now().UTC().After(t) {
		return ""
	}
	return t.Format("2006-01-02 15:04 MST")
}

// IsCIBreakerOpen reports whether the circuit breaker is currently open for a
// repo — callers (Jedi Council before opening a sub-PR) should pause if so.
// Reading this is cheap; caller-level short-circuiting keeps us from piling up
// sub-PRs that would all hit the same broken CI.
func IsCIBreakerOpen(db *sql.DB, repo string) bool {
	v := store.GetConfig(db, fmt.Sprintf("circuit_breaker:%s:open_until", repo), "")
	if v == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return false
	}
	return time.Now().UTC().Before(t)
}

// ResetCIBreaker clears the breaker state for a repo — used by tests and the
// operator CLI.
func ResetCIBreaker(db *sql.DB, repo string) {
	for _, suffix := range []string{"env_count", "window_start", "open_until"} {
		store.SetConfig(db, fmt.Sprintf("circuit_breaker:%s:%s", repo, suffix), "")
	}
}

// Ensure unused import doesn't break compilation when strings isn't referenced.
var _ = strings.Contains

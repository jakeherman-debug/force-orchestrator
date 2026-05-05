package agents

import (
	"log"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"force-orchestrator/internal/agents/adversarial"
	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/clients/codeartifact"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/notify"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// ConvoyStatusAwaitingSupplyRecheck is the convoy lifecycle status the
// AwaitingSupplyRecheck gate stamps onto Convoys.status when a convoy
// has SUPPLY-* deferrals that can't currently be replayed (CodeArtifact
// token still expired) AND the operator's "Ship It" surface must refuse
// to advance the convoy until the deferrals resolve. The dashboard +
// CLI ship handlers gate on `Status == "DraftPROpen"` — anything else
// (including this string) blocks the ship action with the standard
// "convoy is not in DraftPROpen state" error, which is the desired UX.
const ConvoyStatusAwaitingSupplyRecheck = "AwaitingSupplyRecheck"

// ── Diplomat — ConvoyReview ──────────────────────────────────────────────────
//
// Claimed by Diplomat alongside ShipConvoy and PRReviewTriage. Runs one LLM
// pass over the full ask-branch diff vs main, checking it against the convoy's
// commissioned task list. Finds gaps (work not delivered), regressions (correct
// code deleted), and incorrectness (change does the opposite of what was asked).
//
// On clean: marks Completed + mails operator "[CONVOY REVIEW PASSED]".
// On findings: spawns one CodeEdit fix task per finding (capped), marks Completed.
//
// The convoy-review-watch dog re-triggers a fresh ConvoyReview once those fix
// tasks complete, creating a self-healing loop that terminates when a pass
// finds nothing to act on.

const convoyReviewSystemPrompt = `You are the Fleet Diplomat performing a completeness review of a convoy's draft PR.

You are given:
  - convoy_name: the name of this convoy
  - convoy_tasks: a summary of every task the convoy was commissioned to deliver
  - diff: the full unified diff of the ask-branch vs main — exactly what would land if the operator ships today

Your job is to verify that the diff faithfully delivers everything in convoy_tasks, and nothing harmful crept in.

Identify findings in exactly THREE categories:
  gap        — work that was commissioned in convoy_tasks but is MISSING from the diff entirely
  regression — code that was correct and intentional that the diff REMOVES or UNDOES
  incorrect  — a change that does the OPPOSITE of what was asked (e.g. a revert of the desired behavior)

Do NOT flag:
  - style issues, refactoring opportunities, unrelated improvements, or tech debt
  - partial implementations that are functionally correct (e.g. fewer test cases than expected is fine if the core logic is present)
  - anything not derivable from the diff + the task descriptions

For each finding, the "fix" field must be a complete, self-contained CodeEdit task payload that could be handed directly to an astromech with no additional context. Include specific file paths and the exact change required.
The "repo" field must be one of the repo names from the convoy_tasks summary.

If the diff correctly delivers what was commissioned, return status "clean" with an empty findings array.

Respond ONLY with valid JSON (no markdown, no preamble):
{
  "status": "clean" | "needs_work",
  "findings": [
    {
      "type": "gap" | "regression" | "incorrect",
      "description": "one sentence explaining what is wrong",
      "fix": "complete CodeEdit payload for an astromech to fix this",
      "repo": "repo-name"
    }
  ]
}

The "status" field is REQUIRED and MUST be exactly "clean" or "needs_work".` + promptInjectionClause

type convoyReviewPayload struct {
	ConvoyID int `json:"convoy_id"`
}

type convoyReviewResult struct {
	Status   string                `json:"status"` // "clean" | "needs_work"
	Findings []convoyReviewFinding `json:"findings"`
}

type convoyReviewFinding struct {
	Type        string `json:"type"`        // "gap" | "regression" | "incorrect"
	Description string `json:"description"`
	Fix         string `json:"fix"`
	Repo        string `json:"repo"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
}

// Fix #7 — Cost loop guards for ConvoyReview.
//
//   - convoyReviewParseFailureCap: after N LLM parse failures on the same
//     ConvoyReview row, escalate (was: silently Complete, letting the dog
//     retrigger forever).
//   - convoyReviewDefaultMaxFindings: cap on spawned fix tasks per pass.
//     Dropped from 5 → 2 (AUDIT-006). Operator override via SystemConfig
//     key "convoy_review_max_findings" still honoured.
//   - convoyReviewCleanMarker: written to last_findings_fingerprint when
//     a pass returns "clean" with no findings. Distinguishes a true clean
//     pass from deferred-completion rows (empty fingerprint because we
//     didn't run the full pipeline). hasPriorCleanPass looks for this
//     sentinel to decide whether the "no new findings after clean"
//     invariant applies.
const (
	convoyReviewParseFailureCap    = 2
	convoyReviewDefaultMaxFindings = 2
	convoyReviewCleanMarker        = "CLEAN"
)

// findingFingerprint builds a stable per-finding hash from the structural
// identity of a finding (repo + file + line + type + summary). Descriptions
// get hashed to a short SHA256 slice so incidental wording drift across
// LLM passes doesn't defeat dedup — but meaningfully different findings
// still produce distinct fingerprints.
func findingFingerprint(f convoyReviewFinding) string {
	// Normalise: trim whitespace, lowercase, collapse runs of spaces. The
	// LLM sometimes emits "Flusher Removed" vs "flusher removed" across
	// passes on the same finding — don't let capitalisation break dedup.
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
	}
	h := sha256.New()
	fmt.Fprintf(h, "repo=%s\nfile=%s\nline=%d\ntype=%s\nsummary=%s\n",
		norm(f.Repo), norm(f.File), f.Line, norm(f.Type), norm(f.Description))
	return hex.EncodeToString(h.Sum(nil))
}

// findingSetFingerprint builds a single fingerprint for the full findings
// set by sorting per-finding fingerprints and hashing the concatenation.
// Order-insensitive: [A, B] and [B, A] produce the same set fingerprint.
// Empty set returns "" so "no findings" never collides with "findings".
func findingSetFingerprint(findings []convoyReviewFinding) string {
	if len(findings) == 0 {
		return ""
	}
	hashes := make([]string, 0, len(findings))
	for _, f := range findings {
		hashes = append(hashes, findingFingerprint(f))
	}
	sort.Strings(hashes)
	h := sha256.New()
	for _, fh := range hashes {
		h.Write([]byte(fh))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// lastCompletedFindingsFingerprint returns the most recent Completed
// ConvoyReview's stored finding-set fingerprint for this convoy. Empty
// string means "no prior pass" OR "prior pass was clean" OR "prior pass
// was deferred" — the caller treats all three as "no fingerprint to
// compare against." Only a non-empty, non-CLEAN-marker value is used to
// short-circuit pass-N-matches-pass-(N-1) loops.
func lastCompletedFindingsFingerprint(db *sql.DB, convoyID int) string {
	var fp string
	// Fix A (AUDIT-011 read-side): use the structured convoy_id column instead
	// of payload-LIKE. QueueConvoyReview stamps convoy_id on the row, so this
	// lookup is an O(log n) index probe via idx_bounty_convoy_status instead of
	// a full-table scan with brittle JSON-boundary matching.
	db.QueryRow(`SELECT IFNULL(last_findings_fingerprint, '') FROM BountyBoard
		WHERE type = 'ConvoyReview' AND status = 'Completed'
		  AND convoy_id = ?
		  AND IFNULL(last_findings_fingerprint, '') NOT IN ('', ?)
		ORDER BY id DESC LIMIT 1`,
		convoyID, convoyReviewCleanMarker).Scan(&fp)
	return fp
}

// hasPriorCleanPass returns true iff ANY prior Completed ConvoyReview for
// this convoy was stamped with the convoyReviewCleanMarker sentinel —
// meaning that pass's LLM returned status="clean" (distinct from a
// deferred-completion row, which also has empty/default fingerprint).
// Used to gate "only re-verify, don't discover new issues" after the
// first clean pass.
func hasPriorCleanPass(db *sql.DB, convoyID int) bool {
	var n int
	// Fix A (AUDIT-011 read-side): structured convoy_id column.
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'ConvoyReview' AND status = 'Completed'
		  AND convoy_id = ?
		  AND IFNULL(last_findings_fingerprint, '') = ?`,
		convoyID, convoyReviewCleanMarker).Scan(&n)
	return n > 0
}

// incrementParseFailureCount atomically bumps BountyBoard.parse_failure_count
// and returns the new value. Caller uses the returned value to decide
// between retry (1 → retry with critic note), soft-complete, or hard escalate.
func incrementParseFailureCount(db *sql.DB, taskID int) int {
	db.Exec(`UPDATE BountyBoard SET parse_failure_count = parse_failure_count + 1 WHERE id = ?`, taskID)
	var n int
	db.QueryRow(`SELECT IFNULL(parse_failure_count, 0) FROM BountyBoard WHERE id = ?`, taskID).Scan(&n)
	return n
}

// QueueConvoyReview enqueues a ConvoyReview task for the convoy.
// Idempotent: returns 0, nil if one is already Pending or Locked.
//
// Fix #3 (AUDIT-035): the dedup is backed by the canonical idempotency key
// `convoy-review:<convoyID>` and the partial UNIQUE idx_bounty_idem, replacing
// the previous brittle payload-LIKE dedup. Two concurrent callers for the
// same convoy cannot both land a row.
func QueueConvoyReview(db *sql.DB, convoyID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueConvoyReview: convoyID required")
	}
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	key := fmt.Sprintf("convoy-review:%d", convoyID)
	id, existed, err := store.AddIdempotentTask(db, key,
		0, "", "ConvoyReview", string(payload), convoyID, 5, "Pending")
	if err != nil {
		return 0, err
	}
	if existed {
		// Preserve the pre-fix contract: "already queued" returns (0, nil)
		// rather than the existing id, matching the existing callers in
		// Diplomat / dogConvoyReviewWatch which only care about new work.
		return 0, nil
	}
	return id, nil
}

// Fix #8e: ctx threads from SpawnDiplomat's claim ctx so the diff lookups
// cancel on daemon shutdown.
func runConvoyReview(ctx context.Context, db *sql.DB, agentName string, bounty *store.Bounty, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
	var payload convoyReviewPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if ferr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); ferr != nil {
			logger.Printf("ConvoyReview #%d: FailBounty(invalid payload) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
		}
		return
	}
	if payload.ConvoyID <= 0 {
		if ferr := store.FailBounty(db, bounty.ID, "payload missing convoy_id"); ferr != nil {
			logger.Printf("ConvoyReview #%d: FailBounty(missing convoy_id) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
		}
		return
	}

	// Loop-detection: if this convoy has already completed too many review passes,
	// escalate rather than spawning indefinitely.
	// Fix A (AUDIT-011 read-side): structured convoy_id column.
	const maxPasses = 5
	var completedPasses int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'ConvoyReview' AND status = 'Completed'
		  AND convoy_id = ?`,
		payload.ConvoyID).Scan(&completedPasses)
	if completedPasses >= maxPasses {
		escMsg := fmt.Sprintf("Convoy #%d has required %d+ ConvoyReview passes — manual inspection needed",
			payload.ConvoyID, maxPasses)
		logger.Printf("ConvoyReview #%d: %s", bounty.ID, escMsg)
		if _, eerr := CreateEscalation(db, bounty.ID, store.SeverityHigh, escMsg); eerr != nil {
			logger.Printf("ConvoyReview #%d: CreateEscalation(loop cap, convoy %d) failed (%v); stale-lock detector will recover", bounty.ID, payload.ConvoyID, eerr)
		}
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
			fmt.Sprintf("[CONVOY REVIEW] Convoy #%d requires manual review", payload.ConvoyID),
			escMsg, bounty.ID, store.MailTypeAlert)
		if ferr := store.FailBounty(db, bounty.ID, escMsg); ferr != nil {
			logger.Printf("ConvoyReview #%d: FailBounty(loop cap) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
		}
		return
	}

	convoy := store.GetConvoy(db, payload.ConvoyID)
	if convoy == nil {
		if ferr := store.FailBounty(db, bounty.ID, fmt.Sprintf("convoy %d not found", payload.ConvoyID)); ferr != nil {
			logger.Printf("ConvoyReview #%d: FailBounty(convoy %d not found) failed (%v); stale-lock detector will recover", bounty.ID, payload.ConvoyID, ferr)
		}
		return
	}

	// D5 P4 γ — AwaitingSupplyRecheck gate.
	//
	// Before we spend an LLM call on this pass, check whether any
	// ask-branch in the convoy carries SUPPLY-* findings still in
	// disposition='token_expired'. If so:
	//   - If CodeArtifact is reachable: replay each branch inline. A
	//     successful replay flips rows to resolved_late / superseded;
	//     the gate then passes and ConvoyReview proceeds normally
	//     (any newly-inserted block rows from still_flagged outcomes
	//     flow through the regular ISB pipeline, not this gate).
	//   - If CodeArtifact is still down (or replay deps unwired):
	//     stamp the convoy with status='AwaitingSupplyRecheck', fire a
	//     one-shot Slack ping, and exit ConvoyReview early. The
	//     operator's "Ship It" surface refuses to advance any convoy
	//     not in 'DraftPROpen' (see internal/dashboard/ship.go +
	//     cmd/force/convoy_pr.go), so this status is a hard block.
	//     The supply-token-recheck dog (slice β) will eventually
	//     replay the deferrals and the convoy can be moved back to
	//     DraftPROpen via the standard PR-state transition path.
	if blocked, reason, gateErr := evaluateSupplyRecheckGate(ctx, db, payload.ConvoyID, logger); gateErr != nil {
		// Gate-evaluation errors are NOT fatal to ConvoyReview itself —
		// the gate is an optimisation / safety check, and a transient
		// CA / DB hiccup shouldn't poison the whole pass. Log and
		// proceed; the standard ISB block-eval downstream will still
		// catch any unresolved blocks via SecurityFindings.
		logger.Printf("ConvoyReview #%d: supply-recheck gate evaluation error (continuing): %v", bounty.ID, gateErr)
	} else if blocked {
		logger.Printf("ConvoyReview #%d: convoy %d blocked by AwaitingSupplyRecheck gate — %s",
			bounty.ID, payload.ConvoyID, reason)
		if serr := store.SetConvoyStatus(db, payload.ConvoyID, ConvoyStatusAwaitingSupplyRecheck); serr != nil {
			logger.Printf("ConvoyReview #%d: SetConvoyStatus(AwaitingSupplyRecheck) for convoy %d failed (%v); convoy-review-watch will retry",
				bounty.ID, payload.ConvoyID, serr)
		}
		// Best-effort operator ping via notify.Dispatch (D11 substrate).
		// awaiting_supply_recheck is a Tier-2 category — defaults to mail
		// only. Failures here are non-fatal — the supply-token-recheck dog
		// fires its own ping (supply_token_expired, Tier-1) on the next tick.
		label := fmt.Sprintf("[SUPPLY] Convoy #%d (%s) — %s", payload.ConvoyID, convoy.Name, reason)
		body := fmt.Sprintf("ConvoyReview deferred convoy %d (%s): %s\n\nRun `umt artifacts` to refresh the CodeArtifact token; the supply-token-recheck dog will replay the deferrals on its next tick.",
			payload.ConvoyID, convoy.Name, reason)
		if nerr := notify.Dispatch(ctx, db, "awaiting_supply_recheck", payload.ConvoyID, label, body); nerr != nil {
			logger.Printf("ConvoyReview #%d: notify.Dispatch failed (continuing): %v", bounty.ID, nerr)
		}
		// Mark the bounty Completed — we're not failing the review,
		// we're deferring it pending recheck. The convoy-review-watch
		// dog will requeue once the supply-token-recheck dog clears
		// the deferrals AND the convoy returns to DraftPROpen.
		if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
			logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, AwaitingSupplyRecheck) failed (%v); convoy-review-watch will retry", bounty.ID, uerr)
		}
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CONVOY GATE] Convoy '%s' (#%d) — AwaitingSupplyRecheck", convoy.Name, payload.ConvoyID),
			fmt.Sprintf("ConvoyReview deferred: %s\n\nRun `umt artifacts` to refresh the CodeArtifact token; the supply-token-recheck dog will replay the deferrals on its next tick.", reason),
			bounty.ID, store.MailTypeAlert)
		return
	}

	// γ1 — atomic cycle snapshot (concern #6, exit criterion 14a).
	//
	// Begin the cycle BEFORE building the diff so the spec is frozen at
	// the moment we start the pass — operator-ratified amendments that
	// land while we're computing the diff are deferred to the NEXT
	// cycle. The frozenSpec is the only spec the cycle ever evaluates
	// against; do NOT re-read Convoys.verification_spec_json mid-cycle.
	cycleID, frozenSpec, cycleErr := store.BeginConvoyReviewCycle(db, payload.ConvoyID)
	if cycleErr != nil {
		// A cycle row failure should not block the LLM-side review — log
		// and continue with cycleID=0 so we still complete the bounty.
		// The conflicted-loop / drift gates downstream still operate
		// on BountyBoard rows, so safety-net behaviour is preserved.
		logger.Printf("ConvoyReview #%d: BeginConvoyReviewCycle(convoy %d) failed (%v); continuing without cycle row",
			bounty.ID, payload.ConvoyID, cycleErr)
		cycleID = 0
		frozenSpec = ""
	} else {
		logger.Printf("ConvoyReview #%d: cycle #%d begun for convoy %d (frozen spec %d bytes)",
			bounty.ID, cycleID, payload.ConvoyID, len(frozenSpec))
	}

	// completeCycle is the single sink for stamping cycle_completed_at +
	// outcomes_json + fix_tasks_spawned_json. Called from each exit path.
	// Idempotent: a no-op when cycleID==0 (the begin-failed branch above).
	// CompleteConvoyReviewCycle itself rejects double-completion, so a
	// path that calls this twice silently logs the second attempt.
	cycleCompleted := false
	completeCycle := func(verdict, outcomesJSON string, fixTaskIDs []int) {
		if cycleID == 0 || cycleCompleted {
			return
		}
		cycleCompleted = true
		if err := store.CompleteConvoyReviewCycle(db, cycleID, verdict, outcomesJSON, fixTaskIDs); err != nil {
			logger.Printf("ConvoyReview #%d: CompleteConvoyReviewCycle(cycle %d, verdict %s) failed (%v)",
				bounty.ID, cycleID, verdict, err)
		}
	}
	// Final-resort completion: any return path that forgot to call
	// completeCycle gets stamped with verdict="incomplete" so the cycle
	// row is never left with cycle_completed_at='' indefinitely.
	defer func() {
		if cycleID > 0 && !cycleCompleted {
			completeCycle("incomplete", `{}`, nil)
		}
	}()

	// D5.5 P2 β — per-stage Senate review hook.
	//
	// queuePerStageSenateReviewIfStaged fires once at any post-LLM exit
	// path (clean pass, needs_work spawn, deferred-completion gates,
	// no-ask-branches early return) for staged convoys. The Senate task
	// reads the stage's intent + diff and applies its memory-driven
	// advice. The senateHookArmed flag is flipped on AFTER the LLM
	// completes (or after the no-branches short-circuit) so parse-fail
	// escalation paths and the loop-cap escalation path do NOT fire the
	// hook — those paths terminate ConvoyReview without producing a
	// reviewable verdict, and queueing a Senate task on a row that's
	// failing to even produce JSON would just amplify the problem.
	//
	// currentStage is declared in the next block (and re-bound there); the
	// closure captures the named local by reference so the defer reads the
	// post-block value at exec time.
	senateHookArmed := false
	var currentStage store.ConvoyStage
	defer func() {
		if senateHookArmed {
			queuePerStageSenateReviewIfStaged(db, convoy, currentStage, bounty.ID, logger)
		}
	}()

	// D5.5 P2 β — per-stage scoping.
	//
	// For staged convoys, the review walks ONLY the ask-branches belonging
	// to the currently in-flight stage; the LLM sees the stage's intent +
	// just that stage's diff. For single-mode convoys (every D3/D4/D5-era
	// convoy + new convoys whose Commander didn't opt into staged) the
	// behaviour is unchanged — every ask-branch on the convoy is in scope.
	//
	// Determine current stage up-front so the no-ask-branches early return,
	// the diff-base computation, the prompt assembly, and the per-stage
	// Senate hook all reference the same stage row. CurrentInFlightStage
	// errors are non-fatal: log and degrade to convoy-wide scope so a
	// stage-state hiccup never poisons the whole review pipeline (matches
	// the supply-recheck gate's fail-open posture above).
	stage, stageErr := store.CurrentInFlightStage(db, payload.ConvoyID)
	if stageErr != nil {
		logger.Printf("ConvoyReview #%d: CurrentInFlightStage(convoy %d) failed (%v) — degrading to convoy-wide scope",
			bounty.ID, payload.ConvoyID, stageErr)
	}
	currentStage = stage

	// Build the diff for each ask-branch repo. Truncate to avoid overwhelming the LLM.
	diffCapBytes := getIntConfig(db, "convoy_review_diff_cap", 80*1024)
	var branches []store.ConvoyAskBranch
	if convoy.StagingMode == store.StagingModeStaged && currentStage.ID > 0 {
		branches = store.ListConvoyAskBranchesByStage(db, payload.ConvoyID, currentStage.ID)
		logger.Printf("ConvoyReview #%d: staged convoy %d — scoping to stage %d (id=%d, status=%s, %d ask-branch(es))",
			bounty.ID, payload.ConvoyID, currentStage.StageNum, currentStage.ID, currentStage.Status, len(branches))
	} else {
		branches = store.ListConvoyAskBranches(db, payload.ConvoyID)
	}
	if len(branches) == 0 {
		logger.Printf("ConvoyReview #%d: convoy %d has no ask-branches in scope — completing as clean",
			bounty.ID, payload.ConvoyID)
		completeCycle("clean", `{"reason":"no_ask_branches"}`, nil)
		if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
			logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, no ask-branches) failed (%v); convoy-review-watch will retry", bounty.ID, uerr)
		}
		// Per-stage Senate hook fires on the no-branches path too — the
		// stage's audit trail records the intent-only review.
		senateHookArmed = true
		return
	}

	var diffBlocks strings.Builder
	for _, ab := range branches {
		repoCfg := store.GetRepo(db, ab.Repo)
		if repoCfg == nil || repoCfg.LocalPath == "" {
			logger.Printf("ConvoyReview #%d: repo %s not registered — skipping", bounty.ID, ab.Repo)
			continue
		}
		// Diff against the recorded ask_branch_base_sha instead of main's
		// current tip. The base is the commit main was at when the ask-branch
		// was cut; diffing against it shows only what the convoy has added
		// since. Diffing against main's live HEAD would show main's drift as
		// phantom additions (the root cause behind tasks like 449 being flagged
		// as "missing work" even after the work had merged to the ask-branch).
		base := ab.AskBranchBaseSHA
		if base == "" {
			base = igit.GetDefaultBranch(ctx, repoCfg.LocalPath)
		}
		diff := igit.GetDiffFromBase(ctx, repoCfg.LocalPath, base, ab.AskBranch)
		if diff == "" {
			fmt.Fprintf(&diffBlocks, "=== %s/%s ===\n(no changes vs base %s)\n\n",
				ab.Repo, ab.AskBranch, util.TruncateStr(base, 12))
			continue
		}
		if len(diff) > diffCapBytes {
			diff = diff[:diffCapBytes] + "\n... (truncated — diff exceeds cap)\n"
		}
		fmt.Fprintf(&diffBlocks, "=== %s/%s (vs base %s) ===\n%s\n",
			ab.Repo, ab.AskBranch, util.TruncateStr(base, 12), diff)
	}

	// γ2 — verification_spec_json consumer (concern #6, exit criterion 6).
	//
	// Evaluate the frozen spec's ATs against the assembled diff text.
	// Failures merge with the LLM-review findings into a unified list and
	// flow through the existing fix-task spawn path. ParseVerificationSpec
	// silently accepts an empty/missing spec — convoys without a declared
	// spec just see specFindings=nil and behave exactly as before.
	//
	// Pattern P20 (slice α — AT-id scope integrity): the EvaluateConvoySpec
	// call passes payload.ConvoyID into ATResultsToFindings, which prefixes
	// every finding with "Convoy #N / AT-X" (UI labeling discipline).
	// Lookups inside the helpers are scoped by spec object — never bare
	// at_id queries — preserving the (convoy_id, at_id) compound-key
	// invariant.
	specObj, atResults, specFindings, specErr := EvaluateConvoySpec(ctx, db, payload.ConvoyID, frozenSpec, diffBlocks.String(), logger)
	if specErr != nil {
		logger.Printf("ConvoyReview #%d: spec parse failed (%v) — proceeding with LLM-only review",
			bounty.ID, specErr)
		atResults = nil
		specFindings = nil
		specObj = nil
	}
	_ = specObj // reserved for Captain re-justification path; not yet wired
	if len(specFindings) > 0 {
		logger.Printf("ConvoyReview #%d: spec evaluation produced %d AT failure(s) for convoy %d",
			bounty.ID, len(specFindings), payload.ConvoyID)
	}

	convoyTasks := summarizeConvoyTasks(db, payload.ConvoyID)

	// Fix #8.5 — wrap attacker-controllable inputs (task payloads in the
	// convoy-tasks summary, the full ask-branch diff) in <user_content>
	// sentinel tags. The system prompt's promptInjectionClause tells the
	// model to treat everything inside as data.
	//
	// D5.5 P2 β — for staged convoys, prepend the current stage's intent to
	// the prompt. The LLM gets "this stage was supposed to deliver X" as
	// context alongside the stage-scoped diff, which sharpens the gap /
	// regression / incorrect classification (the LLM no longer has to
	// reverse-engineer stage boundaries from convoy-wide task descriptions).
	stagePrefix := ""
	if convoy.StagingMode == store.StagingModeStaged && currentStage.ID > 0 {
		stagePrefix = fmt.Sprintf("Stage %d intent: %s\n\n", currentStage.StageNum, currentStage.IntentText)
	}
	userPrompt := fmt.Sprintf("%sconvoy_name: %s\n\nconvoy_tasks:\n%s\n\ndiff:\n%s",
		stagePrefix,
		convoy.Name,
		WrapUserContent("convoy_tasks", convoyTasks),
		WrapUserContent("diff", diffBlocks.String()))

	logger.Printf("ConvoyReview #%d: running pass %d/%d for convoy %d (%s)",
		bounty.ID, completedPasses+1, maxPasses, payload.ConvoyID, convoy.Name)

	result, err := runConvoyReviewLLM(ctx, db, userPrompt, profile, logger)
	if err != nil {
		// Fix #7 (AUDIT-007): parse failures are tracked via
		// BountyBoard.parse_failure_count on THIS ConvoyReview row. We
		// allow one retry on the same row with a critic note, and
		// escalate after reaching convoyReviewParseFailureCap (=2). The
		// old behaviour marked Completed on the second fail, which let
		// the 5-min dog requeue with no memory — burning ~$5/pass × 5
		// passes.
		nFail := incrementParseFailureCount(db, bounty.ID)
		logger.Printf("ConvoyReview #%d: parse failed (%v) — parse_failure_count=%d/%d",
			bounty.ID, err, nFail, convoyReviewParseFailureCap)

		if nFail >= convoyReviewParseFailureCap {
			escMsg := fmt.Sprintf("ConvoyReview #%d hit %d parse failures for convoy %d — LLM cannot produce valid JSON; manual inspection required",
				bounty.ID, nFail, payload.ConvoyID)
			logger.Printf("ConvoyReview #%d: %s", bounty.ID, escMsg)
			if _, eerr := CreateEscalation(db, bounty.ID, store.SeverityHigh, escMsg); eerr != nil {
				logger.Printf("ConvoyReview #%d: CreateEscalation(parse-fail cap, convoy %d) failed (%v); stale-lock detector will recover", bounty.ID, payload.ConvoyID, eerr)
			}
			store.SendMail(db, agentName, "operator",
				fmt.Sprintf("[CONVOY REVIEW PARSE FAILURE] Convoy #%d", payload.ConvoyID),
				escMsg, bounty.ID, store.MailTypeAlert)
			completeCycle("escalated", `{"reason":"parse_failure_cap"}`, nil)
			if ferr := store.FailBounty(db, bounty.ID, escMsg); ferr != nil {
				logger.Printf("ConvoyReview #%d: FailBounty(parse-fail cap) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
			}
			return
		}

		// First failure: try once more with a critic note on the same row.
		logger.Printf("ConvoyReview #%d: retrying with critic note (attempt %d/%d)",
			bounty.ID, nFail+1, convoyReviewParseFailureCap)
		retryPrompt := userPrompt + "\n\nIMPORTANT: Your previous response could not be parsed as JSON. Respond ONLY with valid JSON matching the schema above — no markdown, no preamble, no trailing text."
		result, err = runConvoyReviewLLM(ctx, db, retryPrompt, profile, logger)
		if err != nil {
			// Retry also failed — bump counter, and on cap, escalate (Fix #7).
			nFail = incrementParseFailureCount(db, bounty.ID)
			escMsg := fmt.Sprintf("ConvoyReview #%d hit %d parse failures for convoy %d — LLM cannot produce valid JSON; manual inspection required",
				bounty.ID, nFail, payload.ConvoyID)
			logger.Printf("ConvoyReview #%d: %s", bounty.ID, escMsg)
			if _, eerr := CreateEscalation(db, bounty.ID, store.SeverityHigh, escMsg); eerr != nil {
				logger.Printf("ConvoyReview #%d: CreateEscalation(parse-fail retry, convoy %d) failed (%v); stale-lock detector will recover", bounty.ID, payload.ConvoyID, eerr)
			}
			store.SendMail(db, agentName, "operator",
				fmt.Sprintf("[CONVOY REVIEW PARSE FAILURE] Convoy #%d", payload.ConvoyID),
				escMsg, bounty.ID, store.MailTypeAlert)
			completeCycle("escalated", `{"reason":"parse_failure_retry"}`, nil)
			if ferr := store.FailBounty(db, bounty.ID, escMsg); ferr != nil {
				logger.Printf("ConvoyReview #%d: FailBounty(parse-fail retry) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
			}
			return
		}
	}

	// D5.5 P2 β — LLM ran successfully; the per-stage Senate hook is now
	// armed. Every downstream exit path (clean pass, conflicted-loop
	// escalation, post-clean drift, active-tasks defer, ask-branch
	// conflict defer, needs_work spawn) fires the hook via the deferred
	// closure above. Parse-failure escalation paths exit BEFORE this point
	// and intentionally do NOT fire the hook.
	senateHookArmed = true

	// γ2 — merge spec AT failures into the LLM finding set BEFORE the
	// "clean pass" check. A clean LLM pass with failing ATs is NOT clean.
	if len(specFindings) > 0 {
		result.Findings = append(result.Findings, specFindings...)
		if result.Status == "clean" {
			result.Status = "needs_work"
		}
	}

	// D3 fix-loop-1 (slice δ) — adversarial pair sampling.
	// Exit criterion 10: surface ConvoyReview-vs-critic disagreements
	// against a sampled fraction of decisions. The pair runs in
	// the background; we capture the join handle so this function
	// can wait on it before returning. Runs AFTER γ2's spec-findings
	// merge so the critic evaluates the same final result the
	// downstream branches act on. Pre-D4 race baseline fix — see
	// jedi_council.go's matching wiring for the rationale; ctx
	// bounds the wait so SIGINT short-circuits cleanly.
	primaryOutcomeBytes, _ := json.Marshal(result)
	reasoning := fmt.Sprintf("status=%s findings=%d", result.Status, len(result.Findings))
	pairHandle, _ := WrapHotPathAdversarialPair(ctx, db, adversarial.PrimaryDecision{
		DecisionID:    int64(bounty.ID),
		Agent:         adversarial.AgentConvoyReview,
		Outcome:       string(primaryOutcomeBytes),
		Reasoning:     reasoning,
		PromptVersion: "convoy-review-v1",
	}, logger)
	defer func() { _ = pairHandle.Wait(ctx) }()

	if result.Status == "clean" || len(result.Findings) == 0 {
		logger.Printf("ConvoyReview #%d: convoy %d passed — no findings (pass %d)",
			bounty.ID, payload.ConvoyID, completedPasses+1)
		// Clean pass (Fix #7): stamp last_findings_fingerprint with the sentinel
		// marker so hasPriorCleanPass can distinguish a true clean pass from a
		// deferred-completion row (active tasks / ask-branch conflict gates).
		db.Exec(`UPDATE BountyBoard SET last_findings_fingerprint = ? WHERE id = ?`, convoyReviewCleanMarker, bounty.ID)
		completeCycle("clean", SerializeATResults(atResults), nil)
		if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
			logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, clean pass) failed (%v); convoy-review-watch will retry", bounty.ID, uerr)
		}
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CONVOY REVIEW PASSED] Convoy '%s' (#%d) — pass %d",
				convoy.Name, payload.ConvoyID, completedPasses+1),
			fmt.Sprintf("ConvoyReview completed %d pass(es) for convoy '%s'.\n\nThe ask-branch diff correctly delivers everything that was commissioned. Ready to ship.",
				completedPasses+1, convoy.Name),
			bounty.ID, store.MailTypeInfo)
		return
	}

	// Fix #7 (AUDIT-006) — Pass-to-pass finding fingerprint dedup.
	//
	// Compute a stable fingerprint of this pass's finding set. If the
	// previous Completed ConvoyReview's fingerprint matches, pass-N and
	// pass-(N-1) produced an identical set — the fleet spawned fix tasks
	// last pass, they were supposed to resolve these findings, and yet
	// they came back verbatim. That's a conflicted-loop signature:
	// escalate rather than spawning another identical fix-task batch that
	// will loop again.
	currFP := findingSetFingerprint(result.Findings)
	prevFP := lastCompletedFindingsFingerprint(db, payload.ConvoyID)
	if prevFP != "" && prevFP == currFP {
		escMsg := fmt.Sprintf("ConvoyReview #%d: convoy %d findings unchanged after pass %d (same %d finding(s) across consecutive passes) — conflicted_loop, fix tasks are not resolving the issues",
			bounty.ID, payload.ConvoyID, completedPasses+1, len(result.Findings))
		logger.Printf("ConvoyReview #%d: %s", bounty.ID, escMsg)
		if _, eerr := CreateEscalation(db, bounty.ID, store.SeverityHigh, escMsg); eerr != nil {
			logger.Printf("ConvoyReview #%d: CreateEscalation(conflicted_loop, convoy %d) failed (%v); stale-lock detector will recover", bounty.ID, payload.ConvoyID, eerr)
		}
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CONVOY REVIEW LOOP] Convoy '%s' (#%d) — %d fix tasks did not resolve findings",
				convoy.Name, payload.ConvoyID, len(result.Findings)),
			escMsg, bounty.ID, store.MailTypeAlert)
		// Persist the fingerprint so a future pass can also detect the
		// identity and short-circuit without another LLM call.
		db.Exec(`UPDATE BountyBoard SET last_findings_fingerprint = ? WHERE id = ?`, currFP, bounty.ID)
		completeCycle("loop", SerializeATResults(atResults), nil)
		if ferr := store.FailBounty(db, bounty.ID, escMsg); ferr != nil {
			logger.Printf("ConvoyReview #%d: FailBounty(conflicted_loop) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
		}
		return
	}

	// Fix #7 (AUDIT-006) — Clean-pass gate.
	//
	// Only the first pass may discover NEW findings. Once any prior pass
	// has returned clean for this convoy, subsequent passes are re-
	// verification only — they MUST find either the same fingerprints as
	// a prior pass (regression verification) or nothing at all. If the
	// LLM starts surfacing new issues after a clean pass, the diff
	// either drifted or the LLM is hallucinating; either way, stop
	// spawning fix tasks and escalate.
	if hasPriorCleanPass(db, payload.ConvoyID) {
		escMsg := fmt.Sprintf("ConvoyReview #%d: convoy %d had a prior clean pass but pass %d surfaced %d new finding(s) — either the ask-branch diff drifted or the LLM is re-reviewing inconsistently; escalating instead of spawning more fix tasks",
			bounty.ID, payload.ConvoyID, completedPasses+1, len(result.Findings))
		logger.Printf("ConvoyReview #%d: %s", bounty.ID, escMsg)
		if _, eerr := CreateEscalation(db, bounty.ID, store.SeverityMedium, escMsg); eerr != nil {
			logger.Printf("ConvoyReview #%d: CreateEscalation(post-clean drift, convoy %d) failed (%v); stale-lock detector will recover", bounty.ID, payload.ConvoyID, eerr)
		}
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CONVOY REVIEW DRIFT] Convoy '%s' (#%d) — new findings after clean pass",
				convoy.Name, payload.ConvoyID),
			escMsg, bounty.ID, store.MailTypeAlert)
		db.Exec(`UPDATE BountyBoard SET last_findings_fingerprint = ? WHERE id = ?`, currFP, bounty.ID)
		completeCycle("escalated", `{"reason":"post_clean_drift"}`, nil)
		if ferr := store.FailBounty(db, bounty.ID, escMsg); ferr != nil {
			logger.Printf("ConvoyReview #%d: FailBounty(post-clean drift) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
		}
		return
	}

	// Don't spawn fix tasks if non-infrastructure work is still in flight for
	// this convoy — the diff is still changing. Complete so the dog re-triggers
	// once those tasks settle. Deliberately DO NOT persist the fingerprint
	// here: the diff is moving, so a "same findings next tick" comparison
	// would be against a diff-state the convoy has since mutated.
	var activeConvoyTasks int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE convoy_id = ? AND status NOT IN ('Completed','Cancelled','Failed')
		  AND type NOT IN (`+store.InfrastructureTaskTypesSQLList()+`)`,
		payload.ConvoyID).Scan(&activeConvoyTasks)
	if activeConvoyTasks > 0 {
		logger.Printf("ConvoyReview #%d: %d active task(s) in convoy %d — completing without spawning (diff still moving)",
			bounty.ID, activeConvoyTasks, payload.ConvoyID)
		completeCycle("deferred", `{"reason":"active_convoy_tasks"}`, nil)
		if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
			logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, active-tasks defer) failed (%v); convoy-review-watch will retry", bounty.ID, uerr)
		}
		return
	}

	// Also gate on an unresolved ask-branch conflict. Spawning fix tasks onto
	// an ask-branch whose tip is broken would stack more conflicts onto the
	// same branch — wait for the astromech to resolve the existing conflict
	// before piling on more work. The dog re-triggers once the conflict clears.
	// Same reasoning on fingerprint: don't persist — the branch tip is broken.
	if store.HasActiveAskBranchConflict(db, payload.ConvoyID) {
		logger.Printf("ConvoyReview #%d: convoy %d has an unresolved ask-branch REBASE_CONFLICT — deferring fix-task spawn",
			bounty.ID, payload.ConvoyID)
		completeCycle("deferred", `{"reason":"ask_branch_conflict"}`, nil)
		if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
			logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, ask-branch conflict defer) failed (%v); convoy-review-watch will retry", bounty.ID, uerr)
		}
		return
	}

	// Spawn fix tasks, capped to avoid runaway task creation.
	// Fix #7 (AUDIT-006): default dropped from 5 → 2. Operator can still
	// override via SystemConfig key "convoy_review_max_findings". Five
	// findings × five passes = 25 Astromech sessions per convoy (~$50-$100).
	// With 2 × 5 + a fingerprint short-circuit, worst case is ≤10 sessions.
	maxFindings := getIntConfig(db, "convoy_review_max_findings", convoyReviewDefaultMaxFindings)
	findings := result.Findings
	if len(findings) > maxFindings {
		logger.Printf("ConvoyReview #%d: %d findings — capping to %d for this pass",
			bounty.ID, len(findings), maxFindings)
		findings = findings[:maxFindings]
	}

	spawned := 0
	spawnedTaskIDs := make([]int, 0, len(findings))
	for _, f := range findings {
		// Validate repo: must be one of the convoy's repos.
		repo := f.Repo
		if repo == "" && len(branches) == 1 {
			repo = branches[0].Repo
		}
		repoCfg := store.GetRepo(db, repo)
		if repoCfg == nil {
			logger.Printf("ConvoyReview #%d: finding references unknown repo %q — using first ask-branch repo",
				bounty.ID, repo)
			repo = branches[0].Repo
		}

		// Find the ask-branch for this repo to pin the task.
		askBranch := ""
		for _, ab := range branches {
			if ab.Repo == repo {
				askBranch = ab.AskBranch
				break
			}
		}

		taskPayload := fmt.Sprintf("[CONVOY_REVIEW_FIX convoy #%d pass %d — %s]\n\n%s\n\n%s",
			payload.ConvoyID, completedPasses+1, f.Type, f.Description, f.Fix)
		taskID, addErr := store.AddConvoyTask(db, bounty.ID, repo, taskPayload, payload.ConvoyID, 5, "Pending")
		if addErr != nil {
			logger.Printf("ConvoyReview #%d: failed to spawn fix task for finding %q: %v", bounty.ID, f.Description, addErr)
			continue
		}
		if askBranch != "" {
			store.SetBranchName(db, taskID, askBranch)
		}
		logger.Printf("ConvoyReview #%d: spawned fix task #%d (%s) — %s",
			bounty.ID, taskID, f.Type, util.TruncateStr(f.Description, 80))
		spawned++
		spawnedTaskIDs = append(spawnedTaskIDs, taskID)
	}

	logger.Printf("ConvoyReview #%d: convoy %d — %d finding(s), %d fix task(s) spawned",
		bounty.ID, payload.ConvoyID, len(findings), spawned)
	// Persist the finding-set fingerprint (Fix #7) so the NEXT ConvoyReview
	// pass can short-circuit on an identical set (same fix tasks didn't
	// resolve the issues → conflicted_loop instead of another spawn).
	db.Exec(`UPDATE BountyBoard SET last_findings_fingerprint = ? WHERE id = ?`, currFP, bounty.ID)
	completeCycle("needs_work", SerializeATResults(atResults), spawnedTaskIDs)
	if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
		logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, %d fix tasks spawned) failed (%v); convoy-review-watch will retry", bounty.ID, spawned, uerr)
	}
}

// queuePerStageSenateReviewIfStaged is the D5.5 P2 β per-stage Senate hook.
//
// For staged convoys with an in-flight stage, queues one SenateReview task
// scoped to that stage (carrying convoy_id + stage_id, not feature_id —
// see store.QueueStageSenateReview). For single-mode convoys or convoys
// with no resolvable stage row, this is a no-op so legacy ConvoyReview
// behaviour is unchanged.
//
// Errors are logged but never propagated — the Senate hook is advisory
// (the per-Senator memory-driven advice is a layered safety net, not a
// gate), and a transient queue failure shouldn't poison the ConvoyReview
// pipeline. The dog re-fires ConvoyReview after fix tasks complete, which
// re-arms the hook on the next pass.
func queuePerStageSenateReviewIfStaged(db *sql.DB, convoy *store.Convoy, stage store.ConvoyStage, bountyID int, logger interface{ Printf(string, ...any) }) {
	if convoy == nil {
		return
	}
	if convoy.StagingMode != store.StagingModeStaged {
		// Single-mode convoys don't fire the per-stage hook — the legacy
		// SenateReview path (Feature-scoped, queued by Commander's
		// QueueSenateReviewHook) covers them.
		return
	}
	if stage.ID <= 0 {
		// No in-flight stage resolved — degraded path; skip silently.
		// The standard ConvoyReview verdict still lands.
		return
	}
	taskID, err := store.QueueStageSenateReview(db, convoy.ID, stage.ID)
	if err != nil {
		logger.Printf("ConvoyReview #%d: per-stage Senate hook for convoy %d stage %d failed: %v",
			bountyID, convoy.ID, stage.StageNum, err)
		return
	}
	if taskID > 0 {
		logger.Printf("ConvoyReview #%d: queued per-stage SenateReview #%d (convoy %d stage %d)",
			bountyID, taskID, convoy.ID, stage.StageNum)
	} else {
		logger.Printf("ConvoyReview #%d: per-stage SenateReview already pending for convoy %d stage %d (idempotent skip)",
			bountyID, convoy.ID, stage.StageNum)
	}
}

func runConvoyReviewLLM(ctx context.Context, db *sql.DB, userPrompt string, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) (convoyReviewResult, error) {
	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("ConvoyReview LLM: MCP config write failed (%v) — proceeding without --mcp-config", mcpErr)
	}
	// D3 P1: append every active FleetRules row scoped to 'convoy-review'.
	// The interface{Printf} logger doesn't satisfy *log.Logger so pass nil
	// — the inject helper falls back to silent fail-open.
	systemPrompt := AppendFleetRulesToPrompt(ctx, db, "convoy-review", convoyReviewSystemPrompt, nil)
	raw, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "convoy-review",
		PromptVersion: "convoy-review-v1",
	}, systemPrompt, userPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 1)
	if err != nil {
		return convoyReviewResult{}, fmt.Errorf("claude CLI: %w", err)
	}
	jsonStr := claude.ExtractJSON(raw)
	var result convoyReviewResult
	// Fix #8.5 — strict-field decode. Model-upgrade drift (e.g. a new
	// "severity" field on findings) surfaces as parse error rather than
	// silently flowing through to spawned fix-task payloads.
	if parseErr := strictJSONUnmarshal([]byte(jsonStr), &result); parseErr != nil {
		logger.Printf("ConvoyReview LLM: parse error %v; raw=%s", parseErr, util.TruncateStr(raw, 200))
		return convoyReviewResult{}, parseErr
	}
	if result.Status == "" {
		return convoyReviewResult{}, fmt.Errorf("LLM returned empty status")
	}
	// Fix #8.5 — sanitize LLM-authored finding.Fix payloads against
	// signal tokens BEFORE the spawning path. A finding whose "fix"
	// field contained `[SCOPE GUARD` or `[CONFLICT_BRANCH:` would
	// corrupt the Captain's scope-guard or Pilot's conflict-handling
	// protocol on the spawned child task.
	for _, f := range result.Findings {
		if err := SanitizeLLMPayload(f.Fix); err != nil {
			return convoyReviewResult{}, fmt.Errorf("finding fix rejected: %w", err)
		}
		if err := SanitizeLLMPayload(f.Description); err != nil {
			return convoyReviewResult{}, fmt.Errorf("finding description rejected: %w", err)
		}
	}
	return result, nil
}

// dogConvoyReviewWatch re-triggers ConvoyReview for DraftPROpen convoys whose
// previous fix tasks have all completed. Also acts as a safety net for convoys
// that missed the Diplomat fast-path trigger.
// Fix #8e: ctx threads from RunDogs → runDog. Body is pure DB so ctx unused
// here; signature aligns dog functions for per-site P11 enforcement.
func dogConvoyReviewWatch(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	_ = ctx
	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'DraftPROpen'`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type convoy struct{ id int; name string }
	var convoys []convoy
	for rows.Next() {
		var c convoy
		if err := rows.Scan(&c.id, &c.name); err != nil {
			logger.Printf("dogConvoyReviewWatch: scan failed: %v", err)
			continue
		}
		convoys = append(convoys, c)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("convoy_review.go:dogConvoyReviewWatch: rows iter error: %v", rErr)
	}

	for _, c := range convoys {
		// Skip if a ConvoyReview is already pending or running.
		// Fix A (AUDIT-011 read-side): structured convoy_id column.
		var pending int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
			  AND convoy_id = ?`,
			c.id).Scan(&pending)
		if pending > 0 {
			continue
		}

		// Skip if any CodeEdit task spawned by a ConvoyReview for this convoy is still active.
		var activeFixTasks int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard child
			JOIN BountyBoard parent ON child.parent_id = parent.id
			WHERE parent.type = 'ConvoyReview'
			  AND child.convoy_id = ?
			  AND child.status NOT IN ('Completed','Cancelled','Failed')`,
			c.id).Scan(&activeFixTasks)
		if activeFixTasks > 0 {
			continue
		}

		// Skip if any non-infrastructure task in the convoy is still in flight —
		// reviewing against a moving diff produces fix tasks that duplicate
		// in-progress work.
		var activeConvoyTasks int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE convoy_id = ? AND status NOT IN ('Completed','Cancelled','Failed')
			  AND type NOT IN (`+store.InfrastructureTaskTypesSQLList()+`)`,
			c.id).Scan(&activeConvoyTasks)
		if activeConvoyTasks > 0 {
			continue
		}

		// Skip if the ask-branch itself has an unresolved REBASE_CONFLICT.
		// Queuing a ConvoyReview while the tip is broken would just produce a
		// no-spawn pass (same gate fires inside runConvoyReview), so save the
		// LLM call and wait for the astromech to resolve it.
		if store.HasActiveAskBranchConflict(db, c.id) {
			continue
		}

		taskID, qErr := QueueConvoyReview(db, c.id)
		if qErr != nil {
			logger.Printf("convoy-review-watch: queue failed for convoy %d: %v", c.id, qErr)
			continue
		}
		if taskID > 0 {
			logger.Printf("convoy-review-watch: queued ConvoyReview #%d for convoy %d (%s)",
				taskID, c.id, c.name)
		}
	}
	return nil
}

// ── D5 P4 γ — AwaitingSupplyRecheck gate helpers ─────────────────────────
//
// evaluateSupplyRecheckGate inspects every ask-branch in the convoy for
// SUPPLY-* findings still in disposition='token_expired'. It returns:
//   - (false, "", nil)   — no deferrals, OR replay succeeded for all of
//                          them. ConvoyReview proceeds normally.
//   - (true,  reason, nil) — deferrals exist AND we cannot resolve them
//                            right now (CodeArtifact still down, deps
//                            unwired, or replay still surfaces them).
//   - (false, "", err)   — gate-evaluation infrastructure error (CA
//                          health probe returned a non-token error,
//                          replay helper failed unexpectedly). Caller
//                          treats this as "log + proceed" so a flaky
//                          gate doesn't block the entire ConvoyReview
//                          pipeline.
//
// Replay-side caveat: if the replay produces still_flagged outcomes,
// those are real SUPPLY-* block findings, not deferrals — the
// disposition='token_expired' rows are flipped to 'superseded' and new
// disposition='block' rows are inserted. The gate then PASSES (returns
// false) because there are no remaining token_expired rows; the
// downstream ConvoyReview LLM pass + ISB block-eval pick up the new
// block findings via the standard SecurityFindings path.
func evaluateSupplyRecheckGate(ctx context.Context, db *sql.DB, convoyID int, logger interface{ Printf(string, ...any) }) (bool, string, error) {
	branches := store.ListConvoyAskBranches(db, convoyID)
	if len(branches) == 0 {
		// No ask-branches → no deferrals possible on this convoy.
		return false, "", nil
	}

	// Fast path: read-only count of token_expired SUPPLY-* findings on
	// the convoy's ask-branches. If zero, gate is clean regardless of
	// CodeArtifact state — no need to even probe Health.
	deferralCount, perBranchCount, countErr := countConvoyDeferrals(db, branches)
	if countErr != nil {
		return false, "", fmt.Errorf("evaluateSupplyRecheckGate: count deferrals for convoy %d: %w", convoyID, countErr)
	}
	if deferralCount == 0 {
		return false, "", nil
	}

	// Deferrals exist. If replay deps aren't wired (production daemon
	// without RegisterSupplyRecheckDeps, or test isolation), we can't
	// replay — fall back to read-only block.
	deps := getSupplyRecheckDeps()
	if deps == nil || deps.CA == nil {
		return true, formatDeferralReason(deferralCount, perBranchCount), nil
	}

	// Probe CA health. ErrTokenExpired → block (the supply-token-
	// recheck dog will recover and replay; we just need to keep this
	// convoy from shipping in the meantime). Any other error class is
	// a gate-evaluation failure — log + proceed, since blocking on a
	// transient network blip would be heavy-handed.
	if err := deps.CA.Health(ctx); err != nil {
		if errors.Is(err, codeartifact.ErrTokenExpired) {
			return true, formatDeferralReason(deferralCount, perBranchCount), nil
		}
		return false, "", fmt.Errorf("evaluateSupplyRecheckGate: CA health probe: %w", err)
	}

	// Health OK — try inline replay per ask-branch. The replay helper
	// flips dispositions on rows it processes; once we re-count, any
	// remaining token_expired rows mean replay was incomplete (e.g.
	// missing rule adapter), which still warrants the gate block.
	if deps.RepoResolver == nil {
		// Misconfigured deps. Fall back to read-only block — log the
		// missing wiring as a warning so the operator notices.
		logger.Printf("evaluateSupplyRecheckGate: SupplyRecheckDeps.RepoResolver is nil — falling back to read-only block")
		return true, formatDeferralReason(deferralCount, perBranchCount), nil
	}

	logger.Printf("evaluateSupplyRecheckGate: convoy %d has %d deferral(s) — replaying inline (CA healthy)",
		convoyID, deferralCount)
	for _, ab := range branches {
		if ab.AskBranch == "" {
			continue
		}
		if _, err := supplydeferral.ReplayPendingDeferralsForBranch(ctx, db, ab.AskBranch, deps.RepoResolver, deps.Rules, supplydeferralLogger{logger}); err != nil {
			// Replay had partial failures. Don't escalate to a hard
			// gate-evaluation error — the still_flagged path inserts
			// new block rows, and the partial-failure case is the
			// dog's responsibility to resolve on the next 30-min tick.
			// We DO want to fall through to the recount below so a
			// branch whose replay actually flipped all rows passes.
			logger.Printf("evaluateSupplyRecheckGate: convoy %d branch %s replay had partial failures (continuing): %v",
				convoyID, ab.AskBranch, err)
		}
	}

	// Re-count: if any token_expired rows remain after replay, block.
	// (Replay flips successful rows to resolved_late / superseded; only
	// rules with no registered adapter or rows whose branch is missing
	// stay token_expired.)
	remaining, perBranchAfter, recountErr := countConvoyDeferrals(db, branches)
	if recountErr != nil {
		return false, "", fmt.Errorf("evaluateSupplyRecheckGate: recount after replay for convoy %d: %w", convoyID, recountErr)
	}
	if remaining > 0 {
		return true, formatDeferralReason(remaining, perBranchAfter), nil
	}
	logger.Printf("evaluateSupplyRecheckGate: convoy %d — replay resolved all %d deferral(s); gate passes",
		convoyID, deferralCount)
	return false, "", nil
}

// countConvoyDeferrals tallies SUPPLY-* findings still in
// disposition='token_expired' across every ask-branch of a convoy.
// Returns (totalCount, perBranchCount, err). The per-branch map keys
// are ask-branch names; values are the count for that branch. Branches
// with zero deferrals are omitted from the map.
//
// SecurityFindings doesn't carry a branch column — branch lives in the
// JSON payload — so we route through ListPendingDeferrals(branch),
// which parses + filters server-side. One QueryRow per branch keeps the
// total work O(branches × deferrals_in_branch); typical convoys have
// 1–3 branches.
func countConvoyDeferrals(db *sql.DB, branches []store.ConvoyAskBranch) (int, map[string]int, error) {
	total := 0
	perBranch := map[string]int{}
	for _, ab := range branches {
		if ab.AskBranch == "" {
			continue
		}
		rows, err := supplydeferral.ListPendingDeferrals(db, ab.AskBranch)
		if err != nil {
			return 0, nil, fmt.Errorf("countConvoyDeferrals(%s): %w", ab.AskBranch, err)
		}
		if n := len(rows); n > 0 {
			perBranch[ab.AskBranch] = n
			total += n
		}
	}
	return total, perBranch, nil
}

// formatDeferralReason builds the operator-facing reason string for a
// blocked AwaitingSupplyRecheck gate. Format:
//
//	"N SUPPLY-* checks deferred (token expired); run umt artifacts to unblock"
//
// When the per-branch map has a single entry, the branch name is also
// embedded for quick triage. The phrasing matches the spec example in
// docs/roadmap.md § "Layer 2 — ConvoyReview gate".
func formatDeferralReason(total int, perBranch map[string]int) string {
	if total == 1 {
		// Singular form for the common 1-finding case.
		for branch := range perBranch {
			return fmt.Sprintf("1 SUPPLY-* check deferred on %s (token expired); run umt artifacts to unblock", branch)
		}
		return "1 SUPPLY-* check deferred (token expired); run umt artifacts to unblock"
	}
	if len(perBranch) == 1 {
		for branch, n := range perBranch {
			return fmt.Sprintf("%d SUPPLY-* checks deferred on %s (token expired); run umt artifacts to unblock", n, branch)
		}
	}
	// Multi-branch: deterministic ordering for stable test assertions.
	keys := make([]string, 0, len(perBranch))
	for b := range perBranch {
		keys = append(keys, b)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, b := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", b, perBranch[b]))
	}
	return fmt.Sprintf("%d SUPPLY-* checks deferred across %d branch(es) [%s] (token expired); run umt artifacts to unblock",
		total, len(keys), strings.Join(parts, ", "))
}

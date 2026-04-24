package agents

import (
	"log"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"force-orchestrator/internal/claude"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

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
func runConvoyReview(ctx context.Context, db *sql.DB, agentName string, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
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

	// Build the diff for each ask-branch repo. Truncate to avoid overwhelming the LLM.
	diffCapBytes := getIntConfig(db, "convoy_review_diff_cap", 80*1024)
	branches := store.ListConvoyAskBranches(db, payload.ConvoyID)
	if len(branches) == 0 {
		logger.Printf("ConvoyReview #%d: convoy %d has no ask-branches — completing as clean",
			bounty.ID, payload.ConvoyID)
		if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
			logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, no ask-branches) failed (%v); convoy-review-watch will retry", bounty.ID, uerr)
		}
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

	convoyTasks := summarizeConvoyTasks(db, payload.ConvoyID)

	// Fix #8.5 — wrap attacker-controllable inputs (task payloads in the
	// convoy-tasks summary, the full ask-branch diff) in <user_content>
	// sentinel tags. The system prompt's promptInjectionClause tells the
	// model to treat everything inside as data.
	userPrompt := fmt.Sprintf("convoy_name: %s\n\nconvoy_tasks:\n%s\n\ndiff:\n%s",
		convoy.Name,
		WrapUserContent("convoy_tasks", convoyTasks),
		WrapUserContent("diff", diffBlocks.String()))

	logger.Printf("ConvoyReview #%d: running pass %d/%d for convoy %d (%s)",
		bounty.ID, completedPasses+1, maxPasses, payload.ConvoyID, convoy.Name)

	result, err := runConvoyReviewLLM(userPrompt, logger)
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
			if ferr := store.FailBounty(db, bounty.ID, escMsg); ferr != nil {
				logger.Printf("ConvoyReview #%d: FailBounty(parse-fail cap) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
			}
			return
		}

		// First failure: try once more with a critic note on the same row.
		logger.Printf("ConvoyReview #%d: retrying with critic note (attempt %d/%d)",
			bounty.ID, nFail+1, convoyReviewParseFailureCap)
		retryPrompt := userPrompt + "\n\nIMPORTANT: Your previous response could not be parsed as JSON. Respond ONLY with valid JSON matching the schema above — no markdown, no preamble, no trailing text."
		result, err = runConvoyReviewLLM(retryPrompt, logger)
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
			if ferr := store.FailBounty(db, bounty.ID, escMsg); ferr != nil {
				logger.Printf("ConvoyReview #%d: FailBounty(parse-fail retry) failed (%v); stale-lock detector will recover", bounty.ID, ferr)
			}
			return
		}
	}

	if result.Status == "clean" || len(result.Findings) == 0 {
		logger.Printf("ConvoyReview #%d: convoy %d passed — no findings (pass %d)",
			bounty.ID, payload.ConvoyID, completedPasses+1)
		// Clean pass (Fix #7): stamp last_findings_fingerprint with the sentinel
		// marker so hasPriorCleanPass can distinguish a true clean pass from a
		// deferred-completion row (active tasks / ask-branch conflict gates).
		db.Exec(`UPDATE BountyBoard SET last_findings_fingerprint = ? WHERE id = ?`, convoyReviewCleanMarker, bounty.ID)
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
	}

	logger.Printf("ConvoyReview #%d: convoy %d — %d finding(s), %d fix task(s) spawned",
		bounty.ID, payload.ConvoyID, len(findings), spawned)
	// Persist the finding-set fingerprint (Fix #7) so the NEXT ConvoyReview
	// pass can short-circuit on an identical set (same fix tasks didn't
	// resolve the issues → conflicted_loop instead of another spawn).
	db.Exec(`UPDATE BountyBoard SET last_findings_fingerprint = ? WHERE id = ?`, currFP, bounty.ID)
	if uerr := store.UpdateBountyStatus(db, bounty.ID, "Completed"); uerr != nil {
		logger.Printf("ConvoyReview #%d: UpdateBountyStatus(Completed, %d fix tasks spawned) failed (%v); convoy-review-watch will retry", bounty.ID, spawned, uerr)
	}
}

func runConvoyReviewLLM(userPrompt string, logger interface{ Printf(string, ...any) }) (convoyReviewResult, error) {
	raw, err := claude.AskClaudeCLI(convoyReviewSystemPrompt, userPrompt, "", 1)
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

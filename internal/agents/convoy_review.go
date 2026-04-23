package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
}`

type convoyReviewPayload struct {
	ConvoyID int `json:"convoy_id"`
}

type convoyReviewResult struct {
	Status   string               `json:"status"` // "clean" | "needs_work"
	Findings []convoyReviewFinding `json:"findings"`
}

type convoyReviewFinding struct {
	Type        string `json:"type"`        // "gap" | "regression" | "incorrect"
	Description string `json:"description"`
	Fix         string `json:"fix"`
	Repo        string `json:"repo"`
}

// QueueConvoyReview enqueues a ConvoyReview task for the convoy.
// Idempotent: returns 0, nil if one is already Pending or Locked.
func QueueConvoyReview(db *sql.DB, convoyID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueConvoyReview: convoyID required")
	}
	var existing int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
		  AND (payload LIKE '%"convoy_id":' || ? || ',%'
		    OR payload LIKE '%"convoy_id":' || ? || '}%')`,
		convoyID, convoyID).Scan(&existing)
	if existing > 0 {
		return 0, nil
	}
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, '', 'ConvoyReview', 'Pending', ?, 5, datetime('now'))`,
		string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

func runConvoyReview(db *sql.DB, agentName string, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload convoyReviewPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))
		return
	}
	if payload.ConvoyID <= 0 {
		store.FailBounty(db, bounty.ID, "payload missing convoy_id")
		return
	}

	// Loop-detection: if this convoy has already completed too many review passes,
	// escalate rather than spawning indefinitely.
	const maxPasses = 5
	var completedPasses int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'ConvoyReview' AND status = 'Completed'
		  AND (payload LIKE '%"convoy_id":' || ? || ',%'
		    OR payload LIKE '%"convoy_id":' || ? || '}%')`,
		payload.ConvoyID, payload.ConvoyID).Scan(&completedPasses)
	if completedPasses >= maxPasses {
		escMsg := fmt.Sprintf("Convoy #%d has required %d+ ConvoyReview passes — manual inspection needed",
			payload.ConvoyID, maxPasses)
		logger.Printf("ConvoyReview #%d: %s", bounty.ID, escMsg)
		CreateEscalation(db, bounty.ID, store.SeverityHigh, escMsg)
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CONVOY REVIEW] Convoy #%d requires manual review", payload.ConvoyID),
			escMsg, bounty.ID, store.MailTypeAlert)
		store.FailBounty(db, bounty.ID, escMsg)
		return
	}

	convoy := store.GetConvoy(db, payload.ConvoyID)
	if convoy == nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("convoy %d not found", payload.ConvoyID))
		return
	}

	// Build the diff for each ask-branch repo. Truncate to avoid overwhelming the LLM.
	diffCapBytes := getIntConfig(db, "convoy_review_diff_cap", 80*1024)
	branches := store.ListConvoyAskBranches(db, payload.ConvoyID)
	if len(branches) == 0 {
		logger.Printf("ConvoyReview #%d: convoy %d has no ask-branches — completing as clean",
			bounty.ID, payload.ConvoyID)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
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
			base = igit.GetDefaultBranch(repoCfg.LocalPath)
		}
		diff := igit.GetDiffFromBase(repoCfg.LocalPath, base, ab.AskBranch)
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

	userPrompt := fmt.Sprintf("convoy_name: %s\n\nconvoy_tasks:\n%s\n\ndiff:\n%s",
		convoy.Name, convoyTasks, diffBlocks.String())

	logger.Printf("ConvoyReview #%d: running pass %d/%d for convoy %d (%s)",
		bounty.ID, completedPasses+1, maxPasses, payload.ConvoyID, convoy.Name)

	result, err := runConvoyReviewLLM(userPrompt, logger)
	if err != nil {
		// One retry with a critic note appended.
		logger.Printf("ConvoyReview #%d: first parse failed (%v) — retrying", bounty.ID, err)
		retryPrompt := userPrompt + "\n\nIMPORTANT: Your previous response could not be parsed as JSON. Respond ONLY with valid JSON matching the schema above — no markdown, no preamble, no trailing text."
		result, err = runConvoyReviewLLM(retryPrompt, logger)
		if err != nil {
			// Second failure: log and complete so the dog retries next tick.
			logger.Printf("ConvoyReview #%d: second parse failed (%v) — completing to unblock dog retry", bounty.ID, err)
			store.UpdateBountyStatus(db, bounty.ID, "Completed")
			return
		}
	}

	if result.Status == "clean" || len(result.Findings) == 0 {
		logger.Printf("ConvoyReview #%d: convoy %d passed — no findings (pass %d)",
			bounty.ID, payload.ConvoyID, completedPasses+1)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CONVOY REVIEW PASSED] Convoy '%s' (#%d) — pass %d",
				convoy.Name, payload.ConvoyID, completedPasses+1),
			fmt.Sprintf("ConvoyReview completed %d pass(es) for convoy '%s'.\n\nThe ask-branch diff correctly delivers everything that was commissioned. Ready to ship.",
				completedPasses+1, convoy.Name),
			bounty.ID, store.MailTypeInfo)
		return
	}

	// Don't spawn fix tasks if non-infrastructure work is still in flight for
	// this convoy — the diff is still changing. Complete so the dog re-triggers
	// once those tasks settle.
	var activeConvoyTasks int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE convoy_id = ? AND status NOT IN ('Completed','Cancelled','Failed')
		  AND type NOT IN (`+store.InfrastructureTaskTypesSQLList()+`)`,
		payload.ConvoyID).Scan(&activeConvoyTasks)
	if activeConvoyTasks > 0 {
		logger.Printf("ConvoyReview #%d: %d active task(s) in convoy %d — completing without spawning (diff still moving)",
			bounty.ID, activeConvoyTasks, payload.ConvoyID)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}

	// Also gate on an unresolved ask-branch conflict. Spawning fix tasks onto
	// an ask-branch whose tip is broken would stack more conflicts onto the
	// same branch — wait for the astromech to resolve the existing conflict
	// before piling on more work. The dog re-triggers once the conflict clears.
	if store.HasActiveAskBranchConflict(db, payload.ConvoyID) {
		logger.Printf("ConvoyReview #%d: convoy %d has an unresolved ask-branch REBASE_CONFLICT — deferring fix-task spawn",
			bounty.ID, payload.ConvoyID)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}

	// Spawn fix tasks, capped to avoid runaway task creation.
	maxFindings := getIntConfig(db, "convoy_review_max_findings", 5)
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
	store.UpdateBountyStatus(db, bounty.ID, "Completed")
}

func runConvoyReviewLLM(userPrompt string, logger interface{ Printf(string, ...any) }) (convoyReviewResult, error) {
	raw, err := claude.AskClaudeCLI(convoyReviewSystemPrompt, userPrompt, "", 1)
	if err != nil {
		return convoyReviewResult{}, fmt.Errorf("claude CLI: %w", err)
	}
	jsonStr := claude.ExtractJSON(raw)
	var result convoyReviewResult
	if parseErr := json.Unmarshal([]byte(jsonStr), &result); parseErr != nil {
		logger.Printf("ConvoyReview LLM: parse error %v; raw=%s", parseErr, util.TruncateStr(raw, 200))
		return convoyReviewResult{}, parseErr
	}
	if result.Status == "" {
		return convoyReviewResult{}, fmt.Errorf("LLM returned empty status")
	}
	return result, nil
}

// dogConvoyReviewWatch re-triggers ConvoyReview for DraftPROpen convoys whose
// previous fix tasks have all completed. Also acts as a safety net for convoys
// that missed the Diplomat fast-path trigger.
func dogConvoyReviewWatch(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'DraftPROpen'`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type convoy struct{ id int; name string }
	var convoys []convoy
	for rows.Next() {
		var c convoy
		rows.Scan(&c.id, &c.name)
		convoys = append(convoys, c)
	}

	for _, c := range convoys {
		// Skip if a ConvoyReview is already pending or running.
		var pending int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
			  AND (payload LIKE '%"convoy_id":' || ? || ',%'
			    OR payload LIKE '%"convoy_id":' || ? || '}%')`,
			c.id, c.id).Scan(&pending)
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

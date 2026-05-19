package agents

// D10 — PRHandoffSynthesis Diplomat task type.
//
// Auto-generated per-PR reviewer narrative. Default OFF per repo
// (Repositories.handoff_synthesis_enabled=0); opt-in via the flag.
// QueuePRHandoffSynthesis enforces the flag at queue time AND
// runPRHandoffSynthesis re-checks it at run time so a flag flip in
// flight cannot bypass the gate.
//
// Mission shape (roadmap §D10):
//
//   1. Read convoy diff (igit helpers) per repo.
//   2. Pull Council / Captain rulings + ConvoyReview findings + any
//      Senate reviews from the holocron.
//   3. Compose a reviewer-focused narrative via Claude using the
//      Diplomat capability profile (no new model selection).
//   4. Post the narrative as a top-level comment on the draft PR via
//      the existing internal/gh client.
//   5. Land a row in PRHandoffSyntheses (audit + experiment trail).
//
// The narrative is for HUMAN reviewers — it explains, in 4-6 short
// sections, what changed, why it changed, what the fleet's review
// gates already said about it, and what the human reviewer should
// pay close attention to. It is NOT a replacement for the PR body
// (Diplomat already writes that during ShipConvoy); it is a separate
// "here's how to read this PR" comment.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// prHandoffSynthesisPayload is the on-wire BountyBoard.payload shape
// for one PRHandoffSynthesis task.
type prHandoffSynthesisPayload struct {
	ConvoyID int `json:"convoy_id"`
	// ExperimentArm is stamped by the queuer when the task is
	// generated under the D10-handoff experiment harness. Empty for
	// non-experiment-bound runs (the v1 default).
	ExperimentArm string `json:"experiment_arm,omitempty"`
}

// prHandoffSynthesisSystemPrompt instructs Claude to compose a
// reviewer-focused narrative. Distinct from diplomatSystemPrompt
// (which writes the PR body itself); this prompt produces a
// secondary "how to read this PR" comment.
const prHandoffSynthesisSystemPrompt = `You are the Fleet Diplomat composing a reviewer-focused handoff comment on a draft PR.

The PR body itself was already written and posted. Your job here is a SECONDARY comment that helps a human reviewer read the PR efficiently. Address the reviewer directly ("you'll want to look at..."), not the author.

You will receive:
1. The convoy name + ask-branch + repo.
2. Summaries of completed CodeEdit tasks in the convoy (one bullet each).
3. The diff's file-list.
4. The most recent ConvoyReview verdict + outcomes summary (if any).
5. Any Council / Captain rulings recorded for the convoy (if any).
6. Any Senate reviews recorded for the convoy's parent feature (if any).

Produce a Markdown comment with these sections, in this order, omitting any section whose source data is empty:

## What changed
A 1-2 sentence summary of the convoy's intent, in plain prose.

## Where to look first
2-4 bullet points naming files or hot spots the reviewer should focus on. Prefer concrete file paths from the diff.

## What the fleet's review gates said
A short paragraph summarising the ConvoyReview verdict + Council/Captain rulings. Quote verdict strings verbatim ("clean" / "needs_work" / etc.) when the source data carries them. If the Senate weighed in, name each Senator and their position (concur / amend / dissent).

## Things you might want to push back on
A short list (or "_no flags from the fleet's gates_" if the fleet was uniformly happy). Surface specific concerns the gates raised; do NOT invent objections.

Rules:
- Stay under 5000 characters total.
- No secrets, API keys, or internal hostnames.
- Do NOT include task IDs that aren't in the supplied context.
- Do NOT include meta-commentary about being an AI or about this comment being auto-generated. The deployment layer adds the AUTO-GENERATED footer itself.
- Plain Markdown. No HTML. No images.

Respond with ONLY the comment body, starting at the first section heading. No preamble.`

// QueuePRHandoffSynthesis enqueues one PRHandoffSynthesis task for
// the given convoy. Returns (0, nil) when the convoy's primary repo
// has handoff_synthesis_enabled=0 — anti-cheat #1 (default OFF). The
// idempotency key is convoy-scoped so a re-queue while one is
// already pending is a no-op.
func QueuePRHandoffSynthesis(db *sql.DB, convoyID int, experimentArm string) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueuePRHandoffSynthesis: convoyID required")
	}
	// Anti-cheat #1: gate at queue time. If NO repo touched by this
	// convoy has handoff_synthesis_enabled=1, refuse to enqueue. The
	// runtime path also re-checks (so a flag flip mid-flight cannot
	// silently leak), but failing fast at queue time keeps the
	// BountyBoard tidy for the no-op case.
	if !convoyHasHandoffSynthesisEnabled(db, convoyID) {
		return 0, nil
	}
	payload, err := json.Marshal(prHandoffSynthesisPayload{
		ConvoyID:      convoyID,
		ExperimentArm: experimentArm,
	})
	if err != nil {
		return 0, fmt.Errorf("QueuePRHandoffSynthesis: marshal payload: %w", err)
	}
	key := fmt.Sprintf("pr-handoff-synthesis:%d", convoyID)
	id, existed, err := store.AddIdempotentTask(
		db, key, 0, "", "PRHandoffSynthesis", string(payload),
		convoyID, 4, "Pending",
	)
	if err != nil {
		return 0, err
	}
	if existed {
		// Mirror QueueConvoyReview's contract: a re-queue against an
		// already-pending row returns (0, nil). Callers that only
		// care about "did I create new work?" read the result as a
		// truthy/falsy signal.
		return 0, nil
	}
	return id, nil
}

// convoyHasHandoffSynthesisEnabled returns true iff at least one
// repo touched by the convoy's ConvoyAskBranch rows carries
// handoff_synthesis_enabled=1. A convoy with no ask-branches (legacy
// shape) is treated as NOT enabled — the gate is conservative.
func convoyHasHandoffSynthesisEnabled(db *sql.DB, convoyID int) bool {
	branches := store.ListConvoyAskBranches(db, convoyID)
	for _, ab := range branches {
		if store.HandoffSynthesisEnabled(db, ab.Repo) {
			return true
		}
	}
	return false
}

// runPRHandoffSynthesis is the Diplomat handler for a claimed
// PRHandoffSynthesis bounty. Single-pass: build context, call
// Claude, post the comment, record the row, complete the bounty.
//
// Failure modes are explicit per CLAUDE.md "no silent failures":
// every error path terminates in store.FailBounty / UpdateBountyStatus.
func runPRHandoffSynthesis(ctx context.Context, db *sql.DB, agentName string, bounty *store.Bounty, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
	var payload prHandoffSynthesisPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if ferr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); ferr != nil {
			logger.Printf("PRHandoffSynthesis #%d: FailBounty(invalid payload) failed: %v", bounty.ID, ferr)
		}
		return
	}
	if payload.ConvoyID <= 0 {
		if ferr := store.FailBounty(db, bounty.ID, "payload missing convoy_id"); ferr != nil {
			logger.Printf("PRHandoffSynthesis #%d: FailBounty(missing convoy_id) failed: %v", bounty.ID, ferr)
		}
		return
	}

	// Anti-cheat #1 re-check: a flag flip between queue and run must
	// NOT silently leak. If no repo in the convoy is enabled now,
	// complete the bounty as a no-op (NOT fail; "operator turned the
	// flag back off" is a legitimate transition).
	if !convoyHasHandoffSynthesisEnabled(db, payload.ConvoyID) {
		logger.Printf("PRHandoffSynthesis #%d: convoy %d has no handoff_synthesis_enabled repo — no-op completion",
			bounty.ID, payload.ConvoyID)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("PRHandoffSynthesis #%d: UpdateBountyStatus(Completed) on no-op failed: %v", bounty.ID, err)
		}
		return
	}

	convoy := store.GetConvoy(db, payload.ConvoyID)
	if convoy == nil {
		if ferr := store.FailBounty(db, bounty.ID, fmt.Sprintf("convoy %d not found", payload.ConvoyID)); ferr != nil {
			logger.Printf("PRHandoffSynthesis #%d: FailBounty(missing convoy) failed: %v", bounty.ID, ferr)
		}
		return
	}

	branches := store.ListConvoyAskBranches(db, payload.ConvoyID)
	if len(branches) == 0 {
		logger.Printf("PRHandoffSynthesis #%d: convoy %d has no ask-branches — no-op completion",
			bounty.ID, payload.ConvoyID)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("PRHandoffSynthesis #%d: UpdateBountyStatus(Completed) on empty-branches no-op failed: %v", bounty.ID, err)
		}
		return
	}

	// Process every (convoy, repo) ask-branch with an Open draft PR
	// AND the per-repo flag enabled. The default-OFF gate is a
	// per-repo decision — a multi-repo convoy can have one repo
	// enrolled and another not; only the enrolled ones receive the
	// reviewer narrative.
	var posted []string
	var skipped []string
	var failures []string
	for _, ab := range branches {
		if ab.DraftPRURL == "" || ab.DraftPRNumber == 0 {
			skipped = append(skipped, fmt.Sprintf("%s(no-draft-pr)", ab.Repo))
			continue
		}
		if !store.HandoffSynthesisEnabled(db, ab.Repo) {
			skipped = append(skipped, fmt.Sprintf("%s(flag-off)", ab.Repo))
			continue
		}
		if err := postHandoffSynthesisForBranch(ctx, db, convoy, ab, profile, payload.ExperimentArm, logger); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ab.Repo, err))
			continue
		}
		posted = append(posted, ab.Repo)
	}

	if len(failures) > 0 {
		msg := fmt.Sprintf("PRHandoffSynthesis partial: posted=%v skipped=%v failures=%v",
			posted, skipped, failures)
		if err := store.FailBounty(db, bounty.ID, msg); err != nil {
			logger.Printf("PRHandoffSynthesis #%d: FailBounty failed: %v; partial state still recorded above", bounty.ID, err)
		}
		logger.Printf("PRHandoffSynthesis #%d: %s", bounty.ID, msg)
		return
	}

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("PRHandoffSynthesis #%d: UpdateBountyStatus(Completed) failed: %v", bounty.ID, err)
	}
	logger.Printf("PRHandoffSynthesis #%d: convoy %d posted=%v skipped=%v",
		bounty.ID, payload.ConvoyID, posted, skipped)
}

// postHandoffSynthesisForBranch handles one (convoy, repo) ask-branch:
// build context, call Claude, post the comment, record the row.
func postHandoffSynthesisForBranch(ctx context.Context, db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, profile *capabilities.Profile, experimentArm string, logger interface{ Printf(string, ...any) }) error {
	repo := store.GetRepo(db, ab.Repo)
	if repo == nil || repo.LocalPath == "" {
		return fmt.Errorf("repo %s not registered or missing local_path", ab.Repo)
	}

	context := buildHandoffSynthesisContext(ctx, db, convoy, ab, repo)
	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("PRHandoffSynthesis: diplomat MCP config write failed (%v) — proceeding without --mcp-config", mcpErr)
	}
	userPrompt := fmt.Sprintf("CONVOY CONTEXT FOR REVIEWER NARRATIVE:\n%s",
		util.TruncateStr(context, 16000))
	raw, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "diplomat",
		TaskID:        int(convoy.ID),
		PromptVersion: "diplomat-pr-handoff-v1",
	}, prHandoffSynthesisSystemPrompt, userPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 2)
	if err != nil {
		return fmt.Errorf("Claude call: %w", err)
	}
	body := strings.TrimSpace(stripMarkdownFences(raw))
	if body == "" {
		return fmt.Errorf("Claude returned empty body")
	}
	// Append a short auto-generated footer so reviewers know the
	// comment came from the fleet (not from the code-author). The
	// system prompt forbids the model from doing this itself.
	body = appendHandoffSynthesisFooter(body, convoy.Name, experimentArm)

	// Post via gh REST API (D17 P2B): switch from `gh pr comment` to
	// `gh api POST repos/{repo}/issues/{pr}/comments` so the returned
	// comment ID is captured and stored in PRHandoffSyntheses.comment_id.
	// ghRepo must be non-empty for the API path to be valid; fall back to
	// PostIssueComment (no ID) when we can't derive the owner/name.
	ghc := newGHClient()
	ghRepo := deriveGHRepoFromRemoteURL(repo.RemoteURL)
	var capturedCommentID int64
	if ghRepo != "" {
		id, postErr := ghc.PostIssueCommentGetID(repo.LocalPath, ghRepo, ab.DraftPRNumber, body)
		if postErr != nil {
			return fmt.Errorf("gh api post comment: %w", postErr)
		}
		capturedCommentID = id
	} else {
		// No owner/name available (e.g. file:// remote in tests) — fall
		// back to the legacy subcommand which doesn't surface the ID.
		if err := ghc.PostIssueComment(repo.LocalPath, ghRepo, ab.DraftPRNumber, body); err != nil {
			return fmt.Errorf("gh pr comment: %w", err)
		}
	}

	// Record the audit row with the captured REST comment ID.
	if _, err := store.InsertPRHandoffSynthesis(db, store.PRHandoffSynthesis{
		ConvoyID:      convoy.ID,
		PRURL:         ab.DraftPRURL,
		PostedAt:      store.NowSQLite(),
		ExperimentArm: experimentArm,
		CommentID:     capturedCommentID,
	}); err != nil {
		// Posted comment + missing audit row is preferable to the
		// reverse — log and continue. The dashboard will not show
		// the row, but the operator sees the comment on GitHub.
		logger.Printf("PRHandoffSynthesis: InsertPRHandoffSynthesis(%d, %s) failed (%v) — comment is posted but audit row missing",
			convoy.ID, ab.DraftPRURL, err)
	}
	logger.Printf("PRHandoffSynthesis: posted reviewer narrative on %s/%s PR #%d (comment_id=%d)",
		ab.Repo, ab.AskBranch, ab.DraftPRNumber, capturedCommentID)
	return nil
}

// buildHandoffSynthesisContext assembles the LLM context. Order
// mirrors the system prompt's enumeration so Claude reads sections
// in a stable order.
func buildHandoffSynthesisContext(ctx context.Context, db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, repo *store.Repository) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Convoy name: %s\n", convoy.Name)
	fmt.Fprintf(&sb, "Repository: %s\n", ab.Repo)
	fmt.Fprintf(&sb, "Ask-branch: %s\n\n", ab.AskBranch)

	// Tasks completed in this convoy + repo.
	rows, err := db.Query(`SELECT id, payload, status FROM BountyBoard
		WHERE convoy_id = ? AND target_repo = ? AND type = 'CodeEdit'
		ORDER BY id ASC`, convoy.ID, ab.Repo)
	if err == nil {
		defer rows.Close()
		sb.WriteString("Tasks completed in this convoy/repo:\n")
		count := 0
		for rows.Next() {
			var id int
			var p, status string
			if scanErr := rows.Scan(&id, &p, &status); scanErr == nil {
				clean := p
				if strings.HasPrefix(clean, "[GOAL:") {
					if nl := strings.Index(clean, "\n"); nl > 0 {
						clean = strings.TrimSpace(clean[nl+1:])
					}
				}
				fmt.Fprintf(&sb, "- #%d (%s): %s\n",
					id, status, util.TruncateStr(strings.Split(clean, "\n")[0], 200))
				count++
			}
		}
		if rErr := rows.Err(); rErr != nil {
			// Pattern P1.1: log + continue. Best-effort context
			// builder; a partial task list is fine.
			fmt.Fprintf(&sb, "- (rows iter error: %v)\n", rErr)
		}
		if count == 0 {
			sb.WriteString("- (none recorded)\n")
		}
		sb.WriteString("\n")
	}

	// Diff file list.
	diff := igit.GetDiff(ctx, repo.LocalPath, ab.AskBranch)
	files := igit.ExtractDiffFiles(diff)
	if len(files) > 0 {
		sb.WriteString("Files changed (vs default branch):\n")
		max := 50
		for i, f := range files {
			if i >= max {
				fmt.Fprintf(&sb, "- … (%d more files elided)\n", len(files)-max)
				break
			}
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		sb.WriteString("\n")
	}

	// ConvoyReview cycles — most recent outcomes JSON. The verdict
	// is folded inside OutcomesJSON via mergeVerdict; we surface the
	// raw JSON so the LLM can quote the actual verdict string from
	// the data, not invent one.
	cycles, _ := store.ListCyclesForConvoy(db, convoy.ID)
	if len(cycles) > 0 {
		sb.WriteString("ConvoyReview cycles (newest first):\n")
		// ListCyclesForConvoy returns oldest-first; iterate in reverse.
		max := 3
		emitted := 0
		for i := len(cycles) - 1; i >= 0 && emitted < max; i-- {
			c := cycles[i]
			completed := strings.TrimSpace(c.CycleCompletedAt)
			if completed == "" {
				completed = "(in-flight)"
			}
			fmt.Fprintf(&sb, "- cycle #%d completed_at=%q outcomes=%s\n",
				c.ID, completed, util.TruncateStr(c.OutcomesJSON, 600))
			emitted++
		}
		sb.WriteString("\n")
	}

	// Council / Captain rulings — captured in TaskHistory.
	// Pull the most recent few entries with outcome containing
	// 'council' or 'captain' to surface their verdicts. Best-effort:
	// the retrieval is regex-ish, not schema-stamped, so the absence
	// of matching rows simply omits the section.
	if hist := recentRulingHistoryForConvoy(db, convoy.ID, ab.Repo, 6); hist != "" {
		sb.WriteString("Recent gate rulings (TaskHistory excerpt):\n")
		sb.WriteString(hist)
		sb.WriteString("\n")
	}

	// Senate reviews scoped to the originating Feature task. We
	// resolve the Feature by walking up parent_id from any task in
	// the convoy until we hit a Feature row (or run out of parents).
	// Best-effort — when no Feature ancestor exists (legacy convoys
	// or convoys created from a one-shot Decompose without a parent
	// Feature) the Senate section is silently omitted.
	if featureID := lookupFeatureAncestor(db, convoy.ID); featureID > 0 {
		reviews, err := store.ListSenateReviewsForFeature(db, featureID)
		if err == nil && len(reviews) > 0 {
			sb.WriteString("Senate reviews on the originating feature:\n")
			for _, r := range reviews {
				fmt.Fprintf(&sb, "- %s: %s — %s\n",
					r.Senator, r.Position,
					util.TruncateStr(r.Rationale, 300))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// lookupFeatureAncestor walks up the parent_id chain starting from
// any task in the convoy until it finds a row with type='Feature'.
// Returns the Feature task's id, or 0 when no ancestor exists.
// Best-effort: a parent_id chain that points outside BountyBoard
// terminates the search gracefully (the next iteration's lookup
// returns empty, breaking the loop).
func lookupFeatureAncestor(db *sql.DB, convoyID int) int {
	if convoyID <= 0 {
		return 0
	}
	// Pick any task in the convoy as the starting point.
	var seedID int
	if err := db.QueryRow(`SELECT id FROM BountyBoard WHERE convoy_id = ? LIMIT 1`, convoyID).Scan(&seedID); err != nil {
		return 0
	}
	// Walk up at most 8 levels — features rarely sit deeper than
	// 2-3 levels above a CodeEdit; the cap is a safety harness.
	currentID := seedID
	for i := 0; i < 8; i++ {
		var (
			parentID int
			taskType string
		)
		if err := db.QueryRow(
			`SELECT IFNULL(parent_id,0), IFNULL(type,'') FROM BountyBoard WHERE id = ?`,
			currentID,
		).Scan(&parentID, &taskType); err != nil {
			return 0
		}
		if taskType == "Feature" {
			return currentID
		}
		if parentID <= 0 {
			return 0
		}
		currentID = parentID
	}
	return 0
}

// recentRulingHistoryForConvoy returns up to `cap` recent
// TaskHistory rows whose outcome includes "council" or "captain"
// keywords, scoped to tasks in the convoy + repo. Plain text
// formatting; empty string if none.
func recentRulingHistoryForConvoy(db *sql.DB, convoyID int, repo string, cap int) string {
	rows, err := db.Query(`SELECT th.task_id, th.agent, th.outcome, th.created_at
		FROM TaskHistory th
		JOIN BountyBoard b ON b.id = th.task_id
		WHERE b.convoy_id = ? AND b.target_repo = ?
		  AND (LOWER(th.outcome) LIKE '%council%'
			OR LOWER(th.outcome) LIKE '%captain%'
			OR LOWER(th.agent) LIKE '%council%'
			OR LOWER(th.agent) LIKE '%captain%')
		ORDER BY th.id DESC
		LIMIT ?`, convoyID, repo, cap)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var taskID int
		var agent, outcome, createdAt string
		if scanErr := rows.Scan(&taskID, &agent, &outcome, &createdAt); scanErr != nil {
			continue
		}
		fmt.Fprintf(&sb, "- task #%d agent=%s outcome=%s at=%s\n",
			taskID, agent, util.TruncateStr(outcome, 200), createdAt)
	}
	if rErr := rows.Err(); rErr != nil {
		// Pattern P1.1: log into the assembled string rather than
		// abort. recentRulingHistoryForConvoy is best-effort context
		// for the LLM — a partial list is still useful.
		fmt.Fprintf(&sb, "- (rows iter error: %v)\n", rErr)
	}
	return sb.String()
}

// appendHandoffSynthesisFooter stamps the auto-generated footer.
// Reviewers see this at the bottom of the comment so there's no
// ambiguity about whether the fleet wrote it.
func appendHandoffSynthesisFooter(body, convoyName, experimentArm string) string {
	stamp := time.Now().UTC().Format("2006-01-02")
	armSuffix := ""
	if experimentArm != "" {
		armSuffix = fmt.Sprintf(" (experiment arm: `%s`)", experimentArm)
	}
	return strings.TrimRight(body, "\n") + fmt.Sprintf(
		"\n\n---\n_AUTO-GENERATED reviewer narrative for convoy `%s`%s by Diplomat (D10) on %s. Hand-edits welcome — the fleet only writes once._\n",
		convoyName, armSuffix, stamp)
}

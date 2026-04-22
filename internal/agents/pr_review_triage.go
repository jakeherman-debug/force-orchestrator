package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// ── Diplomat — PR review comment triage ─────────────────────────────────────
//
// Claimed by Diplomat alongside ShipConvoy. For each unclassified comment in
// the convoy's PRReviewComments, runs an LLM classifier that sees the full
// thread history, then dispatches:
//
//   bot / in_scope_fix    → CodeEdit on the ask-branch + reply posted
//   bot / out_of_scope    → top-level Feature task + reply posted referencing it
//   bot / not_actionable  → reply posted with LLM-drafted explanation
//   bot / conflicted_loop → Escalation with thread history; no reply, no fix
//   human / *             → classification='human'; reply_body is drafted but
//                           NEVER posted. Dashboard surfaces it for operator.
//
// The classifier is forced toward conflicted_loop when thread_depth has
// reached the configured cap (default 2) AND the new comment contradicts
// earlier directions. This prevents the fleet from flip-flopping forever
// with a bot whose feedback is internally inconsistent.

const prReviewSystemPrompt = `You are the Fleet Diplomat, triaging a review comment on an open draft PR.

You are shown:
  - the CURRENT comment (body, author, comment_type, file+line if inline)
  - author_kind: "bot" (auto-reviewer — the fleet may auto-act) or "human" (operator judgment required — the fleet drafts a response but never posts it)
  - the FULL thread history: every prior comment in this thread, including replies YOU have previously authored as the Diplomat, and "fleet-action" summaries of fixes already applied in this thread
  - thread_depth: how many fleet-authored fixes have already been applied in this thread
  - thread_depth_cap: the configured maximum before we escalate rather than fix again
  - convoy_tasks: a brief summary of the work the convoy was scoped to deliver

Classify the CURRENT comment into exactly ONE of:

in_scope_fix      — a concrete, specific code change that falls within the scope of this convoy. Choose this for bot suggestions like "rename this variable", "fix this nil check", "add a test for X", "this log is too chatty", ONLY when the suggestion is clear and bounded.
out_of_scope     — a valid suggestion, but outside what this convoy was commissioned to do. Examples: "while you're here, refactor Y", "this file has unrelated tech debt", "consider rewriting module Z".
not_actionable   — the comment is a question, a nit the fleet shouldn't act on (style preference without clear rule), a positive ack ("LGTM"), or it references a decision that was already made deliberately.
conflicted_loop  — the bot is contradicting a prior direction from the same thread AND thread_depth >= thread_depth_cap. This is a flip-flop escalation trigger. Do NOT use this classification at lower depths; at low depth, prefer in_scope_fix / not_actionable and try to address the concern one more time.

IMPORTANT rules:
  - For author_kind=human, always classify as "human". Humans require operator judgment; the fleet must not auto-act. You should still produce a high-quality reply_body as a DRAFT the operator can review, post, edit, or discard.
  - Never emit conflicted_loop when thread_depth < thread_depth_cap. Those earlier iterations should still try to resolve.
  - Keep replies concise and specific. Reference the fix task or follow-up feature number when relevant; the dispatcher will replace placeholders like {{TASK_ID}} / {{FEATURE_ID}} with the actual IDs. Do NOT fabricate IDs yourself.

Respond ONLY with valid JSON (no markdown, no preamble):
{
  "classification": "in_scope_fix|out_of_scope|not_actionable|conflicted_loop|human",
  "reasoning": "one short paragraph: why this classification, referencing the current comment and (for conflicted_loop) the prior thread direction",
  "reply_body": "the reply to post in the thread (bot) or to surface as a draft (human). Use {{TASK_ID}} or {{FEATURE_ID}} placeholder if you need to reference the spawned task/feature.",
  "fix_summary": "for in_scope_fix: specific, self-contained description of the code change the astromech should make. One paragraph. For all other classifications: empty string."
}`

// prReviewDecision is the LLM's parsed response.
type prReviewDecision struct {
	Classification string `json:"classification"`
	Reasoning      string `json:"reasoning"`
	ReplyBody      string `json:"reply_body"`
	FixSummary     string `json:"fix_summary"`
}

// prReviewTriagePayload is the JSON payload of a PRReviewTriage task.
type prReviewTriagePayload struct {
	ConvoyID int `json:"convoy_id"`
}

// runPRReviewTriage is the handler for one PRReviewTriage task. Iterates over
// every unclassified PRReviewComment in the convoy (capped per run), runs the
// classifier, and dispatches.
func runPRReviewTriage(db *sql.DB, agentName string, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload prReviewTriagePayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))
		return
	}
	if payload.ConvoyID <= 0 {
		store.FailBounty(db, bounty.ID, "payload missing convoy_id")
		return
	}

	batchCap := getIntConfig(db, "pr_review_batch_cap", 20)
	depthCap := getIntConfig(db, "pr_review_thread_depth_cap", 2)

	comments := store.ListUnclassifiedPRComments(db, payload.ConvoyID, batchCap)
	if len(comments) == 0 {
		logger.Printf("PRReviewTriage #%d: no unclassified comments for convoy %d — completing", bounty.ID, payload.ConvoyID)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}

	ghc := newGHClient()
	ghRepo := ""
	repoCfg := (*store.Repository)(nil)

	for _, c := range comments {
		// Look up repo the first time we hit each distinct repo (most convoys are single-repo).
		if repoCfg == nil || repoCfg.Name != c.Repo {
			repoCfg = store.GetRepo(db, c.Repo)
			if repoCfg != nil {
				ghRepo = deriveGHRepoFromRemoteURL(repoCfg.RemoteURL)
			}
		}

		thread := store.LoadThreadHistory(db, payload.ConvoyID, c.ReviewThreadID)
		convoyTasks := summarizeConvoyTasks(db, payload.ConvoyID)

		decision, classifyErr := classifyPRReviewComment(c, thread, convoyTasks, depthCap, logger)
		if classifyErr != nil {
			logger.Printf("PRReviewTriage: comment row %d classify failed: %v — leaving unclassified for retry", c.ID, classifyErr)
			continue
		}

		// Enforce: humans are ALWAYS classified 'human', regardless of LLM output.
		if c.AuthorKind == "human" {
			decision.Classification = "human"
		}

		logger.Printf("PRReviewTriage: comment #%d (%s/%s) classified=%s",
			c.ID, c.AuthorKind, c.Author, decision.Classification)

		if err := dispatchPRReviewDecision(db, agentName, ghc, repoCfg, ghRepo, c, decision, logger); err != nil {
			logger.Printf("PRReviewTriage: dispatch for row %d failed: %v", c.ID, err)
			// Don't FailBounty — other comments in this batch can still proceed;
			// the comment stays unclassified for next-tick retry.
		}
	}

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
}

// classifyPRReviewComment assembles the prompt, calls Claude, parses the JSON.
// The caller post-processes (e.g. forcing 'human' for human authors).
func classifyPRReviewComment(
	c store.PRReviewComment,
	thread []store.PRReviewComment,
	convoyTasks string,
	depthCap int,
	logger interface{ Printf(string, ...any) },
) (prReviewDecision, error) {
	userPrompt := buildPRReviewUserPrompt(c, thread, convoyTasks, depthCap)
	// No tools needed — the classifier is purely textual.
	raw, err := claude.AskClaudeCLI(prReviewSystemPrompt, userPrompt, "", 3)
	if err != nil {
		return prReviewDecision{}, err
	}
	jsonStr := claude.ExtractJSON(raw)
	var d prReviewDecision
	if parseErr := json.Unmarshal([]byte(jsonStr), &d); parseErr != nil {
		logger.Printf("PRReviewTriage: parse error %v; raw=%s", parseErr, util.TruncateStr(raw, 200))
		return prReviewDecision{}, parseErr
	}
	if d.Classification == "" {
		return prReviewDecision{}, fmt.Errorf("classifier returned empty classification")
	}
	return d, nil
}

// buildPRReviewUserPrompt formats the classifier input.
func buildPRReviewUserPrompt(
	c store.PRReviewComment,
	thread []store.PRReviewComment,
	convoyTasks string,
	depthCap int,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CURRENT COMMENT\n")
	fmt.Fprintf(&b, "  author: %s (author_kind=%s)\n", c.Author, c.AuthorKind)
	fmt.Fprintf(&b, "  comment_type: %s\n", c.CommentType)
	if c.Path != "" {
		fmt.Fprintf(&b, "  file: %s", c.Path)
		if c.Line > 0 {
			fmt.Fprintf(&b, ":%d", c.Line)
		}
		b.WriteString("\n")
	}
	if c.DiffHunk != "" {
		fmt.Fprintf(&b, "  diff_hunk:\n%s\n", indent(c.DiffHunk, "    "))
	}
	fmt.Fprintf(&b, "  body:\n%s\n\n", indent(c.Body, "    "))

	fmt.Fprintf(&b, "THREAD CONTEXT\n")
	fmt.Fprintf(&b, "  thread_id: %s\n", c.ReviewThreadID)
	fmt.Fprintf(&b, "  thread_depth: %d\n", c.ThreadDepth)
	fmt.Fprintf(&b, "  thread_depth_cap: %d\n\n", depthCap)

	if len(thread) > 0 {
		fmt.Fprintf(&b, "FULL THREAD HISTORY (oldest → newest, including the current comment at the end):\n")
		for _, t := range thread {
			marker := "comment"
			if t.AuthorKind == "bot" {
				marker = "bot-comment"
			} else if t.AuthorKind == "human" {
				marker = "human-comment"
			}
			fmt.Fprintf(&b, "- [%s] author=%s id=%d classification=%s\n", marker, t.Author, t.GitHubCommentID, t.Classification)
			fmt.Fprintf(&b, "    body: %s\n", indent(util.TruncateStr(t.Body, 400), "    "))
			if t.ReplyBody != "" && t.RepliedAt != "" {
				fmt.Fprintf(&b, "    fleet-reply: %s\n", indent(util.TruncateStr(t.ReplyBody, 400), "    "))
			}
			if t.SpawnedTaskID > 0 {
				fmt.Fprintf(&b, "    fleet-action: spawned task #%d\n", t.SpawnedTaskID)
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "CONVOY TASK CONTEXT\n%s\n", convoyTasks)
	return b.String()
}

// summarizeConvoyTasks returns a short, one-line-per-task summary of the
// convoy's CodeEdit tasks so the classifier can judge in-scope vs out-of-scope.
func summarizeConvoyTasks(db *sql.DB, convoyID int) string {
	rows, err := db.Query(
		`SELECT id, type, IFNULL(status, ''), payload FROM BountyBoard
		 WHERE convoy_id = ? AND type IN ('Feature', 'CodeEdit')
		 ORDER BY id ASC LIMIT 30`,
		convoyID)
	if err != nil {
		return "(unable to read tasks)"
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int
		var typ, status, payload string
		rows.Scan(&id, &typ, &status, &payload)
		fmt.Fprintf(&b, "  #%d %s [%s] — %s\n", id, typ, status, util.TruncateStr(payload, 160))
	}
	out := b.String()
	if out == "" {
		return "(convoy has no CodeEdit/Feature tasks)"
	}
	return out
}

// indent prefixes every line with the given indent.
func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// dispatchPRReviewDecision applies the classifier's decision to the DB and
// GitHub. Atomic per comment: all DB writes commit together; gh post calls
// follow the tx (so a rolled-back tx never emits a reply).
func dispatchPRReviewDecision(
	db *sql.DB,
	agentName string,
	ghc *gh.Client,
	repoCfg *store.Repository,
	ghRepo string,
	c store.PRReviewComment,
	decision prReviewDecision,
	logger interface{ Printf(string, ...any) },
) error {
	switch decision.Classification {
	case "in_scope_fix":
		if c.AuthorKind != "bot" {
			// Defensive: should already be normalized.
			return fmt.Errorf("in_scope_fix on non-bot comment; refusing to auto-fix")
		}
		return dispatchInScope(db, agentName, ghc, repoCfg, ghRepo, c, decision, logger)
	case "out_of_scope":
		if c.AuthorKind != "bot" {
			return fmt.Errorf("out_of_scope on non-bot comment")
		}
		return dispatchOutOfScope(db, agentName, ghc, repoCfg, ghRepo, c, decision, logger)
	case "not_actionable":
		if c.AuthorKind != "bot" {
			return fmt.Errorf("not_actionable on non-bot comment")
		}
		return dispatchNotActionable(db, agentName, ghc, repoCfg, ghRepo, c, decision, logger)
	case "conflicted_loop":
		return dispatchConflictedLoop(db, agentName, c, decision, logger)
	case "human":
		return dispatchHuman(db, c, decision)
	default:
		return fmt.Errorf("unknown classification %q", decision.Classification)
	}
}

// dispatchInScope: spawn CodeEdit on ask-branch (tx: classify + spawn), then
// post the reply. Reply failure is logged but doesn't fail the tx — the fix
// task is already queued and will land regardless.
func dispatchInScope(
	db *sql.DB,
	agentName string,
	ghc *gh.Client,
	repoCfg *store.Repository,
	ghRepo string,
	c store.PRReviewComment,
	decision prReviewDecision,
	logger interface{ Printf(string, ...any) },
) error {
	if repoCfg == nil {
		return fmt.Errorf("repo %s not registered", c.Repo)
	}
	ab := store.GetConvoyAskBranch(db, c.ConvoyID, c.Repo)
	if ab == nil {
		return fmt.Errorf("convoy %d repo %s has no ask-branch", c.ConvoyID, c.Repo)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Spawn the CodeEdit fix task on the ask-branch. Jedi Council's
	// completeAskBranchResolution handles the branch_name == ask_branch case:
	// force-push the ask-branch instead of opening a sub-PR.
	fixPayload := fmt.Sprintf(
		"[PR_REVIEW_FIX for comment #%d by %s on %s%s]\n\n%s\n\nOriginal comment:\n%s",
		c.GitHubCommentID, c.Author, c.Path, lineSuffix(c.Line),
		strings.TrimSpace(decision.FixSummary), util.TruncateStr(c.Body, 400),
	)
	fixTaskID, addErr := store.AddConvoyTaskTx(tx, 0, c.Repo, fixPayload, c.ConvoyID, 5, "Pending")
	if addErr != nil {
		return fmt.Errorf("add fix task: %w", addErr)
	}
	if err := store.SetBranchNameTx(tx, fixTaskID, ab.AskBranch); err != nil {
		return fmt.Errorf("set branch name on fix task: %w", err)
	}
	replyBody := strings.ReplaceAll(decision.ReplyBody, "{{TASK_ID}}", fmt.Sprintf("#%d", fixTaskID))
	if err := store.ClassifyPRCommentTx(tx, c.ID, "in_scope_fix", decision.Reasoning, replyBody, "now", fixTaskID); err != nil {
		return fmt.Errorf("classify: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	store.LogAudit(db, agentName, "pr-review-in-scope", fixTaskID,
		fmt.Sprintf("comment #%d → fix task #%d", c.GitHubCommentID, fixTaskID))

	// Post the reply; if it fails the fix still lands and the row is already
	// classified. The dashboard will surface replied_at='' so an operator can
	// retry manually.
	if err := postReplyForComment(ghc, repoCfg, ghRepo, c, replyBody); err != nil {
		logger.Printf("PRReviewTriage: reply post for comment #%d failed: %v", c.GitHubCommentID, err)
	}
	return nil
}

// dispatchOutOfScope: spawn a top-level Feature task + classify + reply.
func dispatchOutOfScope(
	db *sql.DB,
	agentName string,
	ghc *gh.Client,
	repoCfg *store.Repository,
	ghRepo string,
	c store.PRReviewComment,
	decision prReviewDecision,
	logger interface{ Printf(string, ...any) },
) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	featurePayload := fmt.Sprintf(
		"[PR_REVIEW_FOLLOWUP from convoy #%d comment #%d by %s]\n\n%s\n\nOriginal reviewer comment:\n%s",
		c.ConvoyID, c.GitHubCommentID, c.Author,
		strings.TrimSpace(decision.Reasoning), util.TruncateStr(c.Body, 600),
	)
	featureID, addErr := store.AddFeatureTaskTx(tx, c.Repo, featurePayload, 0)
	if addErr != nil {
		return fmt.Errorf("add feature task: %w", addErr)
	}
	replyBody := strings.ReplaceAll(decision.ReplyBody, "{{FEATURE_ID}}", fmt.Sprintf("#%d", featureID))
	if err := store.ClassifyPRCommentTx(tx, c.ID, "out_of_scope", decision.Reasoning, replyBody, "now", featureID); err != nil {
		return fmt.Errorf("classify: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	store.LogAudit(db, agentName, "pr-review-out-of-scope", featureID,
		fmt.Sprintf("comment #%d → feature #%d", c.GitHubCommentID, featureID))

	if err := postReplyForComment(ghc, repoCfg, ghRepo, c, replyBody); err != nil {
		logger.Printf("PRReviewTriage: reply post for comment #%d failed: %v", c.GitHubCommentID, err)
	}
	return nil
}

// dispatchNotActionable: just reply.
func dispatchNotActionable(
	db *sql.DB,
	agentName string,
	ghc *gh.Client,
	repoCfg *store.Repository,
	ghRepo string,
	c store.PRReviewComment,
	decision prReviewDecision,
	logger interface{ Printf(string, ...any) },
) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := store.ClassifyPRCommentTx(tx, c.ID, "not_actionable", decision.Reasoning, decision.ReplyBody, "now", 0); err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	store.LogAudit(db, agentName, "pr-review-not-actionable", 0,
		fmt.Sprintf("comment #%d explained: %s", c.GitHubCommentID, util.TruncateStr(decision.Reasoning, 200)))

	if err := postReplyForComment(ghc, repoCfg, ghRepo, c, decision.ReplyBody); err != nil {
		logger.Printf("PRReviewTriage: reply post for comment #%d failed: %v", c.GitHubCommentID, err)
	}
	return nil
}

// dispatchConflictedLoop: classify + create escalation with thread history.
// No reply, no resolve.
func dispatchConflictedLoop(
	db *sql.DB,
	agentName string,
	c store.PRReviewComment,
	decision prReviewDecision,
	logger interface{ Printf(string, ...any) },
) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := store.ClassifyPRCommentTx(tx, c.ID, "conflicted_loop", decision.Reasoning, decision.ReplyBody, "", 0); err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Escalation fires after commit so the webhook doesn't leak on a rolled-back tx.
	msg := fmt.Sprintf(
		"Bot review-comment loop on PR #%d (convoy %d repo %s) — %d fleet-authored fixes already applied, bot is now contradicting. Operator review required.\n\nThread id: %s\nLatest comment by %s: %s",
		c.DraftPRNumber, c.ConvoyID, c.Repo, c.ThreadDepth, c.ReviewThreadID, c.Author,
		util.TruncateStr(c.Body, 500),
	)
	CreateEscalation(db, 0, store.SeverityMedium, msg)
	store.LogAudit(db, agentName, "pr-review-conflicted-loop", 0,
		fmt.Sprintf("comment #%d thread_depth=%d", c.GitHubCommentID, c.ThreadDepth))
	logger.Printf("PRReviewTriage: comment #%d escalated — thread_depth=%d cap reached", c.GitHubCommentID, c.ThreadDepth)
	return nil
}

// dispatchHuman: classify with 'human', store the draft reply, NO gh post.
func dispatchHuman(db *sql.DB, c store.PRReviewComment, decision prReviewDecision) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// repliedAtSQL = "" — draft only; operator decides later.
	if err := store.ClassifyPRCommentTx(tx, c.ID, "human", decision.Reasoning, decision.ReplyBody, "", 0); err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	return tx.Commit()
}

// postReplyForComment routes the reply to the appropriate gh endpoint:
//
//   - issue_comment → PostIssueComment (top-level PR comment; there's no
//     "reply" concept for issue comments, so we post a new top-level comment).
//   - review_comment → PostReviewThreadReply with in_reply_to = the comment's
//     REST ID (GitHub places it in the same thread).
func postReplyForComment(ghc *gh.Client, repoCfg *store.Repository, ghRepo string, c store.PRReviewComment, body string) error {
	if repoCfg == nil {
		return fmt.Errorf("repo not registered")
	}
	if ghRepo == "" {
		ghRepo = deriveGHRepoFromRemoteURL(repoCfg.RemoteURL)
	}
	switch c.CommentType {
	case "issue_comment":
		return ghc.PostIssueComment(repoCfg.LocalPath, ghRepo, c.DraftPRNumber, body)
	case "review_comment":
		return ghc.PostReviewThreadReply(repoCfg.LocalPath, ghRepo, c.DraftPRNumber, c.GitHubCommentID, body)
	default:
		return fmt.Errorf("unknown comment_type %q", c.CommentType)
	}
}

// getIntConfig reads a SystemConfig key and parses it as int; falls back to def.
func getIntConfig(db *sql.DB, key string, def int) int {
	raw := strings.TrimSpace(store.GetConfig(db, key, ""))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func lineSuffix(line int) string {
	if line > 0 {
		return fmt.Sprintf(":%d", line)
	}
	return ""
}

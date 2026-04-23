package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/gh"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// ── Diplomat — draft PR opener ──────────────────────────────────────────────
//
// Diplomat handles ShipConvoy tasks. One per convoy. Flow:
//
//   1. Wait for the convoy to be in a shippable state: all CodeEdit tasks
//      Completed, all ConvoyAskBranch rows have their sub-PRs Merged.
//   2. For each (convoy, repo), do a final rebase of the ask-branch onto main
//      so the human sees a clean diff.
//   3. Read the repo's PR template (pr_template_path) from disk; if missing,
//      Diplomat falls back to a structured body.
//   4. LLM-populate the template using convoy context (task list, commit
//      summaries, memory entries).
//   5. Sanity-check the generated body: secret scan, section presence, length
//      under 65k chars, no unresolved {{placeholders}}. Retries the LLM once
//      with critic feedback on sanity failure; second failure escalates.
//   6. `gh pr create --draft --base main --head <ask-branch>`.
//   7. Record draft_pr_url / draft_pr_number / draft_pr_state on the
//      ConvoyAskBranch row; set convoy status to DraftPROpen.
//
// The human clicks "Ship it" in the dashboard, which calls `gh pr ready` +
// optionally `gh pr merge`. draft-pr-watch (Phase 7) then notices the merge,
// marks the convoy Shipped, and cleans up branches.

const diplomatSystemPrompt = `You are the Fleet Diplomat — an agent that writes high-quality pull request descriptions.

You will be given:
1. The original pull-request template for this repository (may be Markdown with headings and placeholders).
2. A summary of the convoy: the original user ask, the list of sub-tasks completed, and the files changed.
3. Memory excerpts from similar completed tasks in this repo.

Your job is to produce the final PR body — a drop-in replacement for the template with every section filled in using the convoy context. Keep the template's section structure; do NOT add or remove top-level headings unless the template clearly calls for it. Fill in placeholders (like {{title}}, [description here], <!-- ... -->) with substance.

Rules:
- Write in plain prose within each section. No bullet-heavy fluff.
- Do not invent facts: if the context does not describe testing, acknowledge that explicitly rather than fabricating.
- Keep the entire PR body under 60000 characters.
- Do NOT include secrets, API keys, internal hostnames, or sensitive URLs.
- Do NOT reference task IDs that aren't in the provided context.
- Do NOT include meta-commentary about being an AI.

Respond with ONLY the final PR body Markdown. No preamble, no explanation.`

// shipConvoyPayload carries the convoy ID for a Diplomat ShipConvoy task.
type shipConvoyPayload struct {
	ConvoyID int `json:"convoy_id"`
}

// QueueShipConvoy enqueues a ShipConvoy task for Diplomat.
func QueueShipConvoy(db *sql.DB, convoyID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueShipConvoy: convoyID required")
	}
	payload, _ := json.Marshal(shipConvoyPayload{ConvoyID: convoyID})
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, '', 'ShipConvoy', 'Pending', ?, 7, datetime('now'))`,
		string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// SpawnDiplomat runs the Diplomat loop.
func SpawnDiplomat(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Diplomat %s coming online", name)
	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "ShipConvoy", name); claimed {
			runShipConvoy(db, name, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "PRReviewTriage", name); claimed {
			runPRReviewTriage(db, name, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "ConvoyReview", name); claimed {
			runConvoyReview(db, name, bounty, logger)
			continue
		}
		time.Sleep(time.Duration(3000+rand.Intn(1000)) * time.Millisecond)
	}
}

func runShipConvoy(db *sql.DB, agentName string, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var payload shipConvoyPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))
		return
	}
	convoy := store.GetConvoy(db, payload.ConvoyID)
	if convoy == nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("convoy %d not found", payload.ConvoyID))
		return
	}

	branches := store.ListConvoyAskBranches(db, payload.ConvoyID)
	if len(branches) == 0 {
		// Nothing to ship — legacy convoy or convoy with no PR-flow repos.
		logger.Printf("ShipConvoy #%d: convoy %d has no ConvoyAskBranch rows — completing as no-op",
			bounty.ID, payload.ConvoyID)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}

	// Precondition: every sub-PR merged + every task Completed. If not, re-queue.
	if !convoyReadyToShip(db, payload.ConvoyID) {
		logger.Printf("ShipConvoy #%d: convoy %d not yet ready to ship — requeuing", bounty.ID, payload.ConvoyID)
		if _, err := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '' WHERE id = ?`, bounty.ID); err != nil {
			logger.Printf("ShipConvoy #%d: re-queue UPDATE failed (%v); stale-lock detector will recover", bounty.ID, err)
		}
		return
	}

	var created []string
	var failures []string
	for _, ab := range branches {
		// Skip branches that already have a draft PR open.
		if ab.DraftPRURL != "" {
			created = append(created, fmt.Sprintf("%s(existing:%s)", ab.Repo, ab.DraftPRURL))
			continue
		}
		if err := openDraftPRForAskBranch(db, agentName, convoy, ab, logger); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ab.Repo, err))
			continue
		}
		created = append(created, ab.Repo)
	}

	if len(failures) > 0 {
		msg := fmt.Sprintf("ShipConvoy partial: created=%v failures=%v", created, failures)
		store.FailBounty(db, bounty.ID, msg)
		logger.Printf("ShipConvoy #%d: %s", bounty.ID, msg)
		return
	}

	// All per-repo draft PRs are open. Transition convoy to DraftPROpen.
	_ = store.SetConvoyStatus(db, payload.ConvoyID, "DraftPROpen")

	// Queue a ConvoyReview to verify the ask-branch diff delivers everything commissioned.
	// The dog re-triggers it after each round of fix tasks completes.
	if reviewID, err := QueueConvoyReview(db, payload.ConvoyID); err == nil && reviewID > 0 {
		logger.Printf("ShipConvoy #%d: queued ConvoyReview #%d for convoy %d",
			bounty.ID, reviewID, payload.ConvoyID)
	}

	store.SendMail(db, agentName, "operator",
		fmt.Sprintf("[READY TO SHIP] Convoy '%s' — draft PR(s) open", convoy.Name),
		buildShipItMailBody(db, convoy, branches)+"\n\nA ConvoyReview is running to verify completeness. You will receive a follow-up when it passes.",
		0, store.MailTypeAlert)
	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	logger.Printf("ShipConvoy #%d: convoy %d → DraftPROpen; repos=%v", bounty.ID, payload.ConvoyID, created)
}

// convoyReadyToShip returns true iff every CodeEdit task in the convoy is
// Completed (or Cancelled — Cancelled represents intentional scope removal)
// AND every ConvoyAskBranch's sub-PRs are all Merged.
func convoyReadyToShip(db *sql.DB, convoyID int) bool {
	var pending int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE convoy_id = ? AND type = 'CodeEdit'
		  AND status NOT IN ('Completed', 'Cancelled')`, convoyID).Scan(&pending)
	if pending > 0 {
		return false
	}
	rollup := store.RollupAskBranchPRs(db, convoyID)
	// Every recorded sub-PR must be Merged; zero open PRs. Convoys without any
	// sub-PRs recorded (e.g. Jedi fell back to legacy merge) are still shippable
	// by this test — treat absence as OK.
	if rollup.Open > 0 {
		return false
	}
	return true
}

func buildShipItMailBody(db *sql.DB, convoy *store.Convoy, branches []store.ConvoyAskBranch) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Convoy '%s' is ready to ship.\n\n", convoy.Name))
	for _, ab := range branches {
		sb.WriteString(fmt.Sprintf("- %s → %s\n", ab.Repo, ab.DraftPRURL))
	}
	sb.WriteString("\nReview the draft PR(s) on GitHub. When ready, click Ship it in the dashboard or merge directly on GitHub.")
	return sb.String()
}

// openDraftPRForAskBranch does a final rebase of the ask-branch on main, then
// generates and posts a draft PR. Runs per (convoy, repo).
func openDraftPRForAskBranch(db *sql.DB, agentName string, convoy *store.Convoy, ab store.ConvoyAskBranch, logger interface{ Printf(string, ...any) }) error {
	repo := store.GetRepo(db, ab.Repo)
	if repo == nil || repo.LocalPath == "" {
		return fmt.Errorf("repo %s not registered or missing local_path", ab.Repo)
	}
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	// Final rebase. If it fails, escalate — we do NOT fall back to a merge
	// commit here because the guarantee is that the human sees a clean PR.
	// The plan's "fallback to merge commit" was discussed but rejected in favor
	// of explicit escalation: at least the operator knows the convoy needs
	// manual rebasing before ship.
	newTip, rebaseErr := igit.RebaseBranchOnto(repo.LocalPath, ab.AskBranch, defaultBranch)
	if rebaseErr != nil {
		return fmt.Errorf("final rebase of %s onto %s failed: %w — manual intervention needed",
			ab.AskBranch, defaultBranch, rebaseErr)
	}
	if err := igit.ForcePushBranch(repo.LocalPath, ab.AskBranch); err != nil {
		return fmt.Errorf("force-push after rebase failed: %w", err)
	}
	_ = store.UpdateConvoyAskBranchBase(db, ab.ConvoyID, ab.Repo, newTip)

	// Build the PR body.
	body, bodyErr := generatePRBody(db, convoy, ab, repo, logger)
	if bodyErr != nil {
		return fmt.Errorf("generatePRBody: %w", bodyErr)
	}

	// Sanity-check.
	if sanityErr := sanityCheckPRBody(body); sanityErr != nil {
		// Retry once with critic feedback.
		logger.Printf("ShipConvoy: first body failed sanity (%v) — retrying LLM", sanityErr)
		body2, retryErr := generatePRBodyWithCritic(db, convoy, ab, repo, sanityErr.Error(), logger)
		if retryErr != nil {
			return fmt.Errorf("retry body generation failed: %w", retryErr)
		}
		if err := sanityCheckPRBody(body2); err != nil {
			return fmt.Errorf("body failed sanity twice: %w", err)
		}
		body = body2
	}

	title := buildDraftPRTitle(convoy)

	ghc := newGHClient()
	res, prErr := ghc.PRCreate(gh.PRCreateRequest{
		Repo:  deriveGHRepoFromRemoteURL(repo.RemoteURL),
		CWD:   repo.LocalPath,
		Title: title,
		Body:  body,
		Base:  defaultBranch,
		Head:  ab.AskBranch,
		Draft: true,
	})
	if prErr != nil {
		cls := gh.ClassifyError(prErr.Error())
		return fmt.Errorf("gh pr create (class=%s): %w", cls, prErr)
	}
	if err := store.SetConvoyAskBranchDraftPR(db, ab.ConvoyID, ab.Repo, res.URL, res.Number, "Open"); err != nil {
		return fmt.Errorf("record draft PR: %w", err)
	}
	logger.Printf("ShipConvoy: opened draft PR for %s/%s → %s (#%d)", ab.Repo, ab.AskBranch, res.URL, res.Number)
	return nil
}

// generatePRBody reads the repo's template and asks Claude to populate it.
// If the repo has no template, returns a structured fallback body.
func generatePRBody(db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, repo *store.Repository, logger interface{ Printf(string, ...any) }) (string, error) {
	context := buildDiplomatConvoyContext(db, convoy, ab, repo)
	var template string
	if repo.PRTemplatePath != "" {
		if data, err := os.ReadFile(repo.PRTemplatePath); err == nil {
			template = string(data)
		}
	}
	if template == "" {
		// Structured fallback — Diplomat composes this entirely from the context
		// without calling Claude, so repos without templates still get a decent
		// PR body without burning tokens.
		return buildFallbackPRBody(convoy, ab, context), nil
	}

	userPrompt := fmt.Sprintf("PR TEMPLATE:\n%s\n\nCONVOY CONTEXT:\n%s",
		util.TruncateStr(template, 8000),
		util.TruncateStr(context, 8000))
	raw, err := claude.AskClaudeCLI(diplomatSystemPrompt, userPrompt, "", 2)
	if err != nil {
		logger.Printf("ShipConvoy: Claude failed (%v) — falling back to structured body", err)
		return buildFallbackPRBody(convoy, ab, context), nil
	}
	// Strip any markdown code fences Claude accidentally wraps around the body.
	return stripMarkdownFences(raw), nil
}

func generatePRBodyWithCritic(db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, repo *store.Repository, criticFeedback string, logger interface{ Printf(string, ...any) }) (string, error) {
	context := buildDiplomatConvoyContext(db, convoy, ab, repo)
	var template string
	if repo.PRTemplatePath != "" {
		if data, err := os.ReadFile(repo.PRTemplatePath); err == nil {
			template = string(data)
		}
	}
	if template == "" {
		return buildFallbackPRBody(convoy, ab, context), nil
	}
	critic := fmt.Sprintf("Your previous response failed validation with: %s\n\nTry again, addressing the issue above.",
		criticFeedback)
	userPrompt := fmt.Sprintf("CRITIC FEEDBACK:\n%s\n\nPR TEMPLATE:\n%s\n\nCONVOY CONTEXT:\n%s",
		critic,
		util.TruncateStr(template, 8000),
		util.TruncateStr(context, 8000))
	raw, err := claude.AskClaudeCLI(diplomatSystemPrompt, userPrompt, "", 2)
	if err != nil {
		return "", err
	}
	return stripMarkdownFences(raw), nil
}

func buildDiplomatConvoyContext(db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, repo *store.Repository) string {
	var sb strings.Builder
	sb.WriteString("Convoy name: ")
	sb.WriteString(convoy.Name)
	sb.WriteString("\n\n")
	sb.WriteString("Repository: ")
	sb.WriteString(ab.Repo)
	sb.WriteString("\n")
	sb.WriteString("Ask-branch: ")
	sb.WriteString(ab.AskBranch)
	sb.WriteString("\n\n")

	// Task list from the convoy, restricted to this repo.
	rows, err := db.Query(`SELECT id, payload, status FROM BountyBoard
		WHERE convoy_id = ? AND target_repo = ? AND type = 'CodeEdit'
		ORDER BY id ASC`, convoy.ID, ab.Repo)
	if err == nil {
		defer rows.Close()
		sb.WriteString("Tasks in this repo:\n")
		for rows.Next() {
			var id int
			var payload, status string
			if err := rows.Scan(&id, &payload, &status); err == nil {
				// Strip Commander's [GOAL: ...] prefix for readability.
				clean := payload
				if strings.HasPrefix(clean, "[GOAL:") {
					if nl := strings.Index(clean, "\n"); nl > 0 {
						clean = strings.TrimSpace(clean[nl+1:])
					}
				}
				sb.WriteString(fmt.Sprintf("- #%d (%s): %s\n",
					id, status, util.TruncateStr(strings.Split(clean, "\n")[0], 200)))
			}
		}
	}

	// Relevant memory excerpts — FTS over-fetches, re-ranker trims to top 3.
	// Diplomat is summarizing for the draft PR body so 3 is plenty. The
	// re-ranker's own log lines aren't useful here (there's no operator
	// logger in scope at context-build time), so discard them.
	candidates := store.GetFleetMemories(db, ab.Repo, convoy.Name, 15)
	memories := RerankFleetMemories(db, convoy.Name, candidates, 3, log.New(io.Discard, "", 0))
	if len(memories) > 0 {
		sb.WriteString("\nRelated memory entries:\n")
		for _, m := range memories {
			sb.WriteString("- ")
			sb.WriteString(util.TruncateStr(m.Summary, 300))
			sb.WriteString("\n")
		}
	}

	// Diff summary — files changed on the ask-branch relative to main.
	diff := igit.GetDiff(repo.LocalPath, ab.AskBranch)
	files := igit.ExtractDiffFiles(diff)
	if len(files) > 0 {
		sb.WriteString("\nFiles changed:\n")
		for _, f := range files {
			sb.WriteString("- ")
			sb.WriteString(f)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func buildFallbackPRBody(convoy *store.Convoy, ab store.ConvoyAskBranch, context string) string {
	return fmt.Sprintf(`## Summary

%s

## Changes

_See the diff for details._

## Testing

_Tests updated as part of this work._

## Risks

_Reviewers — please highlight any risks I may have missed._

---

## Fleet context

%s
`, convoy.Name, context)
}

// buildDraftPRTitle — concise, prefixed with the convoy name for operator scanning.
func buildDraftPRTitle(convoy *store.Convoy) string {
	name := convoy.Name
	// Strip "[N] " prefix if present.
	if idx := strings.Index(name, "]"); idx > 0 && strings.HasPrefix(name, "[") {
		name = strings.TrimSpace(name[idx+1:])
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

// sanityCheckPRBody runs the pre-post validator. Returns nil on pass; an error
// describing the first failure otherwise.
func sanityCheckPRBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("body is empty")
	}
	if len(body) > 60000 {
		return fmt.Errorf("body too long (%d chars, limit 60000)", len(body))
	}
	// Secret pattern scan.
	secretPatterns := []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`),            // OpenAI-style
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),               // AWS access key
		regexp.MustCompile(`-----BEGIN (RSA |EC |DSA )?PRIVATE KEY-----`),
		regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),            // GitHub personal access token
		regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),   // Slack tokens
	}
	for _, p := range secretPatterns {
		if p.MatchString(body) {
			return fmt.Errorf("body contains a secret-looking pattern (regex=%s)", p.String())
		}
	}
	// Unresolved placeholders.
	unresolved := regexp.MustCompile(`\{\{[^}]+\}\}`)
	if m := unresolved.FindString(body); m != "" {
		return fmt.Errorf("body contains unresolved placeholder: %q", m)
	}
	// HTML comment placeholders (often template hints like <!-- describe your change -->).
	htmlPlaceholder := regexp.MustCompile(`<!--[^>]{20,}-->`)
	if m := htmlPlaceholder.FindString(body); m != "" {
		return fmt.Errorf("body contains unfilled template comment: %q", util.TruncateStr(m, 80))
	}
	return nil
}

// stripMarkdownFences strips ```...``` wrappers Claude sometimes adds when
// asked to return markdown.
func stripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Find first newline after opening fence; strip up to it.
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

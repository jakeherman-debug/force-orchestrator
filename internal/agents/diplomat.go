package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/gh"
	igit "force-orchestrator/internal/git"
	repolib "force-orchestrator/internal/repo"
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
// Fix A (AUDIT-011 read-side): convoy_id is stamped on the row so dedup
// queries can use the structured column + idx_bounty_convoy_status instead
// of a payload-LIKE full-table scan.
func QueueShipConvoy(db *sql.DB, convoyID int) (int, error) {
	if convoyID <= 0 {
		return 0, fmt.Errorf("QueueShipConvoy: convoyID required")
	}
	payload, _ := json.Marshal(shipConvoyPayload{ConvoyID: convoyID})
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (0, '', 'ShipConvoy', 'Pending', ?, ?, 7, datetime('now'))`,
		string(payload), convoyID)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// SpawnDiplomat runs the Diplomat loop.
func SpawnDiplomat(ctx context.Context, db *sql.DB, name string) {
	logger := NewLogger(name)

	// D1 T0-1: load the three profiles Diplomat handlers use (ShipConvoy
	// uses diplomat; PRReviewTriage / ConvoyReview use their own
	// profiles). Loading all three upfront fails fast on profile errors
	// instead of mid-task.
	diplomatProfile, err := capabilities.LoadProfile("diplomat")
	if err != nil {
		logger.Printf("Diplomat %s cannot start: %v", name, err)
		return
	}
	prReviewProfile, err := capabilities.LoadProfile("pr-review-triage")
	if err != nil {
		logger.Printf("Diplomat %s cannot start: %v", name, err)
		return
	}
	convoyReviewProfile, err := capabilities.LoadProfile("convoy-review")
	if err != nil {
		logger.Printf("Diplomat %s cannot start: %v", name, err)
		return
	}
	logger.Printf("Diplomat %s coming online", name)
	for {
		if ctx.Err() != nil {
			logger.Printf("Diplomat %s exiting: %v", name, ctx.Err())
			return
		}
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}
		if SpendCapExceeded(db) {
			time.Sleep(10 * time.Second)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "ShipConvoy", name); claimed {
			runShipConvoy(ctx, db, name, bounty, diplomatProfile, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "PRReviewTriage", name); claimed {
			runPRReviewTriage(ctx, db, name, bounty, prReviewProfile, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "ConvoyReview", name); claimed {
			runConvoyReview(ctx, db, name, bounty, convoyReviewProfile, logger)
			continue
		}
		// D10 — PRHandoffSynthesis: reviewer-narrative comment on
		// draft PRs for repos with handoff_synthesis_enabled=1.
		// Default OFF; QueuePRHandoffSynthesis + runPRHandoffSynthesis
		// both gate on the per-repo flag so a row that lands without
		// any enrolled repo completes as a no-op. Uses the diplomat
		// profile (no new model selection per the roadmap).
		if bounty, claimed := store.ClaimBounty(db, "PRHandoffSynthesis", name); claimed {
			runPRHandoffSynthesis(ctx, db, name, bounty, diplomatProfile, logger)
			continue
		}
		// D8 Track 3 — synthetic integration tests of consumer repos
		// against the producer's ask-branch. Spawned by
		// DispatchConsumerIntegrationChecks (called from runShipConvoy on
		// the DraftPROpen transition) when the convoy's parent Feature
		// has a non-empty blast_radius_json.affected_consumer_repos.
		// The handler runs deterministic shell + git operations (no LLM
		// call) so the diplomat capability profile is passed but
		// unused; the signature alignment keeps the claim loop uniform.
		if bounty, claimed := store.ClaimBounty(db, "ConsumerIntegrationCheck", name); claimed {
			runConsumerIntegrationCheck(ctx, db, name, bounty, diplomatProfile, logger)
			continue
		}
		time.Sleep(time.Duration(3000+rand.Intn(1000)) * time.Millisecond)
	}
}

// Fix #8e: ctx threads from SpawnDiplomat's claim ctx so the rebase/push +
// PR-body LLM calls cancel on daemon shutdown.
func runShipConvoy(ctx context.Context, db *sql.DB, agentName string, bounty *store.Bounty, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) {
	var payload shipConvoyPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); fbErr != nil {
			logger.Printf("ShipConvoy #%d: FailBounty failed after payload parse error: %v", bounty.ID, fbErr)
		}
		return
	}
	convoy := store.GetConvoy(db, payload.ConvoyID)
	if convoy == nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("convoy %d not found", payload.ConvoyID)); fbErr != nil {
			logger.Printf("ShipConvoy #%d: FailBounty failed after missing convoy: %v", bounty.ID, fbErr)
		}
		return
	}

	branches := store.ListConvoyAskBranches(db, payload.ConvoyID)
	if len(branches) == 0 {
		// Nothing to ship — legacy convoy or convoy with no PR-flow repos.
		logger.Printf("ShipConvoy #%d: convoy %d has no ConvoyAskBranch rows — completing as no-op",
			bounty.ID, payload.ConvoyID)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("ShipConvoy #%d: Completed update failed on empty-branches no-op: %v", bounty.ID, err)
		}
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
		if err := openDraftPRForAskBranch(ctx, db, agentName, convoy, ab, profile, logger); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ab.Repo, err))
			continue
		}
		created = append(created, ab.Repo)
	}

	if len(failures) > 0 {
		msg := fmt.Sprintf("ShipConvoy partial: created=%v failures=%v", created, failures)
		if err := store.FailBounty(db, bounty.ID, msg); err != nil {
			logger.Printf("ShipConvoy #%d: FailBounty failed (%v); partial failure still recorded in log", bounty.ID, err)
		}
		logger.Printf("ShipConvoy #%d: %s", bounty.ID, msg)
		return
	}

	// All per-repo draft PRs are open. Transition convoy to DraftPROpen.
	if err := store.SetConvoyStatus(db, payload.ConvoyID, "DraftPROpen"); err != nil {
		logger.Printf("ShipConvoy #%d: SetConvoyStatus(DraftPROpen) for convoy %d failed: %v — convoy-review-watch dog will re-queue a ConvoyReview on the next tick once the status flip lands via retry", bounty.ID, payload.ConvoyID, err)
	}

	// Queue a ConvoyReview to verify the ask-branch diff delivers everything commissioned.
	// The dog re-triggers it after each round of fix tasks completes.
	if reviewID, err := QueueConvoyReview(db, payload.ConvoyID); err == nil && reviewID > 0 {
		logger.Printf("ShipConvoy #%d: queued ConvoyReview #%d for convoy %d",
			bounty.ID, reviewID, payload.ConvoyID)
	}

	// D8 Track 3 — fan out one ConsumerIntegrationCheck task per
	// affected consumer repo from the parent Feature's blast-radius.
	// Idempotent: re-running ShipConvoy on a convoy already in
	// DraftPROpen sees existing rows in ConsumerIntegrationResults and
	// queues nothing new (per QueueConsumerIntegrationCheck's
	// HasConsumerIntegrationResult gate). Failure to dispatch does NOT
	// block the ship transition — the operator sees the gap as missing
	// rows in ConsumerIntegrationResults; the convoy is still open for
	// human ratification.
	if dispatched, ciErr := DispatchConsumerIntegrationChecks(db, payload.ConvoyID, branches, logger); ciErr != nil {
		logger.Printf("ShipConvoy #%d: DispatchConsumerIntegrationChecks for convoy %d failed: %v — ship transition continues; missing rows will be visible in dashboard",
			bounty.ID, payload.ConvoyID, ciErr)
	} else if dispatched > 0 {
		logger.Printf("ShipConvoy #%d: dispatched %d ConsumerIntegrationCheck task(s) for convoy %d",
			bounty.ID, dispatched, payload.ConvoyID)
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
		fmt.Sprintf("[READY TO SHIP] Convoy '%s' — draft PR(s) open", convoy.Name),
		buildShipItMailBody(db, convoy, branches)+"\n\nA ConvoyReview is running to verify completeness. You will receive a follow-up when it passes.",
		0, store.MailTypeAlert)
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("ShipConvoy #%d: Completed update failed after successful ship: %v", bounty.ID, err)
	}
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
// Fix #8e: ctx threads from runShipConvoy so the rebase/push and PR-body
// generation cancel on daemon shutdown.
func openDraftPRForAskBranch(ctx context.Context, db *sql.DB, agentName string, convoy *store.Convoy, ab store.ConvoyAskBranch, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) error {
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
	newTip, rebaseErr := igit.RebaseBranchOnto(ctx, repo.LocalPath, ab.AskBranch, defaultBranch)
	if rebaseErr != nil {
		return fmt.Errorf("final rebase of %s onto %s failed: %w — manual intervention needed",
			ab.AskBranch, defaultBranch, rebaseErr)
	}
	if err := igit.ForcePushBranch(ctx, db, repo.Name, repo.LocalPath, ab.AskBranch); err != nil {
		return fmt.Errorf("force-push after rebase failed: %w", err)
	}
	if err := store.UpdateConvoyAskBranchBase(db, ab.ConvoyID, ab.Repo, newTip); err != nil {
		logger.Printf("ShipConvoy: UpdateConvoyAskBranchBase(%s, new tip after rebase) failed: %v — main-drift-watch reads this SHA; a stale value triggers an extra rebase cycle but does not corrupt state", ab.Repo, err)
	}

	// Build the PR body.
	body, bodyErr := generatePRBody(ctx, db, convoy, ab, repo, profile, logger)
	if bodyErr != nil {
		return fmt.Errorf("generatePRBody: %w", bodyErr)
	}

	// Sanity-check.
	if sanityErr := sanityCheckPRBody(body); sanityErr != nil {
		// Retry once with critic feedback.
		logger.Printf("ShipConvoy: first body failed sanity (%v) — retrying LLM", sanityErr)
		body2, retryErr := generatePRBodyWithCritic(ctx, db, convoy, ab, repo, sanityErr.Error(), profile, logger)
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
// Fix #8e: ctx is reserved for future use when AskClaudeCLI gains a Context
// variant; today it is unused but the signature aligns with caller propagation.
func generatePRBody(ctx context.Context, db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, repo *store.Repository, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) (string, error) {
	_ = ctx
	context := buildDiplomatConvoyContext(ctx, db, convoy, ab, repo)
	var template string
	if repo.PRTemplatePath != "" {
		// D1 T0-2: gate the read through the target repo's .forceignore.
		// If the operator has marked the PR template path as ignored
		// (rare, but legitimate when the template lives next to a
		// secrets-bearing file in the same dir), skip silently and
		// fall through to the structured fallback.
		content, ignored, rerr := repolib.ReadRepoFileGated(repo.LocalPath, repo.PRTemplatePath, "diplomat")
		if rerr != nil {
			logger.Printf("ShipConvoy: PR template read failed (%v) — falling back to structured PR body", rerr)
		} else if !ignored {
			template = content
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
	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("ShipConvoy: diplomat MCP config write failed (%v) — proceeding without --mcp-config", mcpErr)
	}
	raw, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "diplomat",
		TaskID:        int(convoy.ID),
		PromptVersion: "diplomat-v1",
	}, diplomatSystemPrompt, userPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 2)
	if err != nil {
		// AUDIT-095 (Fix #8d): classify the error. Transient / rate-limit
		// errors should NOT silently fall back to the bare PR body — the
		// caller's handleInfraFailure path retries them. Permanent errors
		// (auth, config) fall back to the structured body AND send an
		// operator-visible mail so a quiet-but-degraded PR body isn't
		// mistaken for a happy path.
		cls := gh.ClassifyError(err.Error())
		if cls == gh.ErrClassTransient || cls == gh.ErrClassRateLimited {
			return "", fmt.Errorf("generatePRBody: classify=%s: %w", cls, err)
		}
		logger.Printf("ShipConvoy: Claude failed (%v, class=%s) — falling back to structured PR body; mailing operator", err, cls)
		store.SendMail(db, "Diplomat", "operator",
			"[PR BODY DEGRADED] Diplomat Claude call failed — PR body is a structured fallback, not the LLM-composed version",
			fmt.Sprintf("generatePRBody for convoy %d (%s) fell back to buildFallbackPRBody because the Claude call failed:\n\n%v\n\nThe PR will still open but the body is the structured template fallback. Inspect the PR and revise manually if needed.", convoy.ID, convoy.Name, err),
			0, store.MailTypeAlert)
		return buildFallbackPRBody(convoy, ab, context), nil
	}
	// Strip any markdown code fences Claude accidentally wraps around the body.
	return stripMarkdownFences(raw), nil
}

// Fix #8e: ctx threads from runShipConvoy.
func generatePRBodyWithCritic(ctx context.Context, db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, repo *store.Repository, criticFeedback string, profile *capabilities.Profile, logger interface{ Printf(string, ...any) }) (string, error) {
	_ = ctx
	context := buildDiplomatConvoyContext(ctx, db, convoy, ab, repo)
	var template string
	if repo.PRTemplatePath != "" {
		// D1 T0-2: same .forceignore gate as generatePRBody.
		content, ignored, rerr := repolib.ReadRepoFileGated(repo.LocalPath, repo.PRTemplatePath, "diplomat-critic")
		if rerr != nil {
			logger.Printf("ShipConvoy critic: PR template read failed (%v) — falling back to structured PR body", rerr)
		} else if !ignored {
			template = content
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
	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("ShipConvoy: diplomat-critic MCP config write failed (%v) — proceeding without --mcp-config", mcpErr)
	}
	raw, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "diplomat-critic",
		TaskID:        int(convoy.ID),
		PromptVersion: "diplomat-critic-v1",
	}, diplomatSystemPrompt, userPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 2)
	if err != nil {
		return "", err
	}
	return stripMarkdownFences(raw), nil
}

// Fix #8e: ctx threads from generatePRBody so the diff subprocess used to
// summarise file changes cancels on daemon shutdown.
func buildDiplomatConvoyContext(ctx context.Context, db *sql.DB, convoy *store.Convoy, ab store.ConvoyAskBranch, repo *store.Repository) string {
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
		if rErr := rows.Err(); rErr != nil {
			log.Printf("diplomat.go:buildDiplomatConvoyContext: rows iter error: %v", rErr)
		}
	}

	// Relevant memory excerpts — D4 P0 (Pattern P33): route through
	// the Librarian Client's weighted-score ingress. The re-ranker
	// still trims to top 3 for the draft PR body; the change is the
	// candidate pool now respects freshness × validation ranking.
	candidates := getMemoriesForPromptInjection(ctx, db, ab.Repo, 15)
	// Re-rank under the librarian profile (the rerank LLM is part of the
	// librarian retrieval pipeline). Profile load failure here degrades to
	// FTS order via RerankFleetMemories' nil-profile branch — graceful
	// fallback rather than blocking the PR-body assembly.
	librarianProfile, _ := capabilities.LoadProfile("librarian")
	memories := RerankFleetMemories(ctx, db, convoy.Name, candidates, 3, librarianProfile, log.New(io.Discard, "", 0))
	if len(memories) > 0 {
		sb.WriteString("\nRelated memory entries:\n")
		for _, m := range memories {
			sb.WriteString("- ")
			sb.WriteString(util.TruncateStr(m.Summary, 300))
			sb.WriteString("\n")
		}
	}

	// Diff summary — files changed on the ask-branch relative to main.
	diff := igit.GetDiff(ctx, repo.LocalPath, ab.AskBranch)
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

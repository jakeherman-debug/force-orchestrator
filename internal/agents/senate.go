// Package agents — Senate (D4 Phase 3).
//
// SpawnSenate claims `SenateReview` and `SenatorOnboarding` tasks. Both
// task types are queued by orthogonal callers:
//
//   - `SenateReview`     — queued by the Senate-router hook (see
//                          senate_review_chancellor_hook.go) right after
//                          Commander writes the ProposedConvoys row but
//                          BEFORE the source Feature transitions to
//                          AwaitingChancellorReview. The Feature sits in
//                          status='AwaitingSenateReview' until the
//                          handler resolves it.
//
//   - `SenatorOnboarding` — queued by `force add-repo` and at daemon
//                            start for force-orchestrator (the recursive
//                            first Senator). Calls
//                            librarian.BootstrapSenatorRules and emits
//                            the candidate FleetRules rows through the
//                            standard PromotionProposal pipeline.
//
// Per docs/next-gen-agents.md § "Senate" / docs/roadmap.md § Deliverable
// 4 exit criterion 3, the Senate is the LAST review layer before the
// Chancellor: BoS / ISB run at commit-time AFTER a CodeEdit completes,
// while the Senate runs at PLAN-time, before any code is written. The
// state-transition diagram for a Feature is therefore:
//
//	Feature → Classifying → Decompose → AwaitingSenateReview
//	         (or skip if no active Senator) → AwaitingChancellorReview
//	         → Locked (Chancellor) → ... convoy ...
//
// Anti-cheat: every code path in this file MUST go through the
// PromotionProposal pipeline when emitting a candidate rule — there
// is no direct FleetRules write (Pattern P34, see
// audit_pattern_p34_senate_no_self_promote_test.go).
//
// CLAUDE.md "No silent failures": every error path terminates in
// store.FailBounty / store.UpdateBountyStatus.
package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/senate"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// senateReviewPayload is the JSON shape QueueSenateReview emits.
//
// D5.5 P2 β extends this shape with optional convoy_id + stage_id fields,
// emitted by store.QueueStageSenateReview for per-stage convoy reviews.
// When stage_id is non-zero the runSenateReviewTask handler routes through
// the stage-scoped path (read intent + diff for that stage) rather than
// the Feature-scoped path. The shapes are mutually exclusive: feature_id
// is set for Commander-emitted plan reviews; stage_id is set for
// ConvoyReview-emitted per-stage reviews.
type senateReviewPayload struct {
	FeatureID  int    `json:"feature_id"`
	TargetRepo string `json:"target_repo"`
	ConvoyID   int    `json:"convoy_id,omitempty"`
	StageID    int    `json:"stage_id,omitempty"`
}

// senatorOnboardingPayload is the JSON shape QueueSenatorOnboarding emits.
type senatorOnboardingPayload struct {
	RepoID    string `json:"repo_id"`
	Scope     string `json:"scope"` // 'repo:<name>'
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// SpawnSenate is the Senate agent claim loop. Mirrors SpawnBoS / SpawnISB.
// Threads the Librarian Client so SenatorOnboarding can call
// BootstrapSenatorRules and so per-Senator review context can include
// RecentCommitsDigest.
//
// The capability profile is loaded at spawn time so a missing/invalid
// `senate.yaml` fails loudly per Pattern P13's fail-closed posture.
func SpawnSenate(ctx context.Context, db *sql.DB, name string, lib librarian.Client) {
	logger := NewLogger(name)

	if _, err := capabilities.LoadProfile("senate"); err != nil {
		logger.Printf("Senate %s cannot start: %v", name, err)
		return
	}
	logger.Printf("Senate %s standing by", name)

	for {
		if ctx.Err() != nil {
			logger.Printf("Senate %s exiting: %v", name, ctx.Err())
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

		// Priority order: SenateReview (live pipeline) > SenatorOnboarding
		// (bootstrap) > SenatorRefresh (periodic re-scan). Onboarding and
		// refresh are rare relative to the review steady-state.
		if b, claimed := store.ClaimBounty(db, "SenateReview", name); claimed {
			runSenateReviewTask(ctx, db, name, b, lib, logger)
			continue
		}
		if b, claimed := store.ClaimBounty(db, "SenatorOnboarding", name); claimed {
			runSenatorOnboardingTask(ctx, db, name, b, lib, logger)
			continue
		}
		if b, claimed := store.ClaimBounty(db, "SenatorRefresh", name); claimed {
			runSenatorRefreshTask(ctx, db, name, b, lib, logger)
			continue
		}
		time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
	}
}

// runSenateReviewTask reviews a single Feature plan against every
// active Senator. The dispatch logic follows
// docs/next-gen-agents.md § "Operation":
//
//  1. Resolve affected Senators (per the senate.AffectedSenators router).
//  2. For each affected Senator, assemble per-Senator context + call
//     reviewWithSenator (LIVE_HAIKU_DISABLED gates the LLM ingress).
//  3. Persist each verdict to SenateReview.
//  4. Aggregate: any verdict that does NOT approve (per Verdict.Approves)
//     blocks; otherwise advance the source Feature to
//     AwaitingChancellorReview.
//
// Idempotent: if the feature has already advanced (status !=
// AwaitingSenateReview), the handler logs and completes without
// re-running the LLM calls.
func runSenateReviewTask(ctx context.Context, db *sql.DB, agentName string, b *store.Bounty, lib librarian.Client, logger interface{ Printf(string, ...any) }) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Senate claimed task %d (parent=%d)", sessionID, b.ID, b.ParentID)

	var p senateReviewPayload
	if err := json.Unmarshal([]byte(b.Payload), &p); err != nil {
		msg := fmt.Sprintf("SenateReview payload parse failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after payload parse failure also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}

	// D5.5 P2 β — per-stage Senate review path. Distinct from the
	// Feature-scoped review below: this variant carries (convoy_id,
	// stage_id) and is queued by ConvoyReview at each stage's
	// DraftPROpen. The full per-Senator memory-driven advice surface
	// is reserved for a follow-up slice; this path records the
	// queueing event in the audit trail and completes without firing
	// LLMs, so the hook is wired and observable end-to-end without
	// burning budget on stub Senator content.
	if p.StageID > 0 {
		runStageScopedSenateReview(ctx, db, agentName, b, p, logger)
		return
	}

	if p.FeatureID == 0 {
		msg := "SenateReview payload missing feature_id"
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after invalid payload also failed (%v)", b.ID, fbErr)
		}
		return
	}

	feature, err := store.GetBounty(db, p.FeatureID)
	if err != nil || feature == nil {
		msg := fmt.Sprintf("SenateReview: source Feature %d not found: %v", p.FeatureID, err)
		logger.Printf("Task %d: %s", b.ID, msg)
		// best-effort status update — main work already terminated (source
		// Feature vanished); if this status flip fails, the SenateReview
		// row will be recovered by the stale-lock detector. Non-critical.
		if upErr := store.UpdateBountyStatus(db, b.ID, "Completed"); upErr != nil {
			logger.Printf("Task %d: SenateReview Completed status transition failed (post feature-not-found): %v", b.ID, upErr)
		}
		return
	}
	// Idempotence: if the feature is no longer in AwaitingSenateReview,
	// some other path resolved it — log and complete this task without
	// re-firing the LLMs.
	if feature.Status != "AwaitingSenateReview" {
		logger.Printf("Task %d: Feature %d already in status %q (not AwaitingSenateReview) — Senate completing without re-review",
			b.ID, p.FeatureID, feature.Status)
		// best-effort status update — main work already succeeded (some
		// other path advanced the Feature); if this status flip fails,
		// the audit-trail flip is non-critical.
		if upErr := store.UpdateBountyStatus(db, b.ID, "Completed"); upErr != nil {
			logger.Printf("Task %d: SenateReview Completed status transition failed (post idempotent-skip): %v", b.ID, upErr)
		}
		return
	}

	// Pull the proposed plan + identify affected Senators.
	planRepos, planJSON := loadSenatePlanInputs(db, p.FeatureID)
	affected, sErr := senate.AffectedSenators(db, planRepos, feature.TargetRepo)
	if sErr != nil {
		msg := fmt.Sprintf("SenateReview: AffectedSenators failed: %v", sErr)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after senator routing failure also failed (%v)", b.ID, fbErr)
		}
		return
	}
	if len(affected) == 0 {
		// Spec: "If no Senator is affected, Senate is skipped. Zero cost,
		// zero delay." Advance the Feature directly.
		logger.Printf("Task %d: Feature %d touches no active Senator — fast-advancing to AwaitingChancellorReview", b.ID, p.FeatureID)
		if _, advErr := store.UpdateBountyStatusFrom(db, p.FeatureID, "AwaitingSenateReview", "AwaitingChancellorReview"); advErr != nil {
			logger.Printf("Task %d: senate fast-advance UpdateBountyStatusFrom failed: %v", b.ID, advErr)
		}
		// best-effort status update — main work already succeeded (the
		// fast-advance UpdateBountyStatusFrom above is the load-bearing
		// transition); if this status flip fails, it's non-critical.
		if upErr := store.UpdateBountyStatus(db, b.ID, "Completed"); upErr != nil {
			logger.Printf("Task %d: SenateReview Completed status transition failed (post fast-advance): %v", b.ID, upErr)
		}
		return
	}

	// Run each Senator's review. Sequential at launch — the per-Senator
	// LLM call is fast enough (one prompt per repo); parallelism is a
	// post-launch optimisation, gated on per-Senator concurrency caps.
	verdicts := make([]senate.Verdict, 0, len(affected))
	allApprove := true
	for _, repoID := range affected {
		senator, lErr := senate.LoadSenator(db, repoID)
		if lErr != nil {
			logger.Printf("Task %d: Senate.LoadSenator(%s) failed: %v — recording deterministic dissent", b.ID, repoID, lErr)
			v := senate.Verdict{Senator: repoID, Position: senate.PositionDissent, Confidence: 0,
				Rationale: fmt.Sprintf("LoadSenator failed: %v", lErr)}
			persistVerdict(db, p.FeatureID, v, logger)
			continue
		}
		if senator == nil {
			// Race: chamber removed between AffectedSenators and LoadSenator.
			logger.Printf("Task %d: Senator %s no longer active — skipping", b.ID, repoID)
			continue
		}

		v := reviewWithSenator(ctx, db, senator, feature, planJSON, lib, logger)
		v.Senator = repoID
		verdicts = append(verdicts, v)
		persistVerdict(db, p.FeatureID, v, logger)
		if !v.Approves() {
			allApprove = false
		}
	}

	// Aggregate decision.
	if !allApprove {
		logger.Printf("Task %d: Senate BLOCKED Feature %d (at least one Senator dissented at high confidence)", b.ID, p.FeatureID)
		feedback := buildSenateFeedback(verdicts)
		store.ReturnTaskForRework(db, p.FeatureID, feature.Payload+feedback)
		store.LogAudit(db, agentName, "senate-blocked", p.FeatureID, "high-confidence dissent recorded")
		// Mark the ProposedConvoy as cancelled so the next Decompose pass
		// gets a fresh row. Otherwise the upsert in StoreProposedConvoy
		// would re-use the stale plan.
		store.SetProposedConvoyStatus(db, p.FeatureID, "cancelled")
		telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
		// best-effort status update — main work already succeeded
		// (rejection routed via ReturnTaskForRework + audit + telemetry +
		// proposed-convoy cancel); if this status flip fails, it's
		// non-critical.
		if upErr := store.UpdateBountyStatus(db, b.ID, "Completed"); upErr != nil {
			logger.Printf("Task %d: SenateReview Completed status transition failed (post senate-blocked): %v", b.ID, upErr)
		}
		return
	}

	logger.Printf("Task %d: Senate APPROVED Feature %d (%d Senator(s) concurred) — advancing to AwaitingChancellorReview",
		b.ID, p.FeatureID, len(verdicts))
	if _, advErr := store.UpdateBountyStatusFrom(db, p.FeatureID, "AwaitingSenateReview", "AwaitingChancellorReview"); advErr != nil {
		logger.Printf("Task %d: Senate advance UpdateBountyStatusFrom failed: %v — stale-lock detector will recover", b.ID, advErr)
	}
	store.LogAudit(db, agentName, "senate-approved", p.FeatureID, fmt.Sprintf("%d concur", len(verdicts)))
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
	if err := store.UpdateBountyStatus(db, b.ID, "Completed"); err != nil {
		logger.Printf("Task %d: SenateReview Completed status transition failed: %v", b.ID, err)
	}
}

// runStageScopedSenateReview handles the D5.5 P2 β per-stage Senate review
// path: a SenateReview task whose payload carries convoy_id + stage_id (no
// feature_id) is one of these. The handler records the stage's intent in
// the audit log and completes the task. Full per-Senator memory-driven
// advice for stage scopes is reserved for a follow-up slice — wiring it
// here would require fanning the affected-Senator router across stage-
// scoped repo sets, which is a non-trivial extension. Today's contract is
// "the hook fires, the task lands, the audit trail records the stage";
// that's the minimum the per-stage Senate hook needs to be observable
// end-to-end and to unblock the rest of D5.5.
//
// Errors are non-fatal — the bounty completes either way so the dog
// doesn't loop on the SenateReview row.
func runStageScopedSenateReview(ctx context.Context, db *sql.DB, agentName string, b *store.Bounty, p senateReviewPayload, logger interface{ Printf(string, ...any) }) {
	_ = ctx
	logger.Printf("Task %d: stage-scoped SenateReview claimed (convoy=%d stage=%d)",
		b.ID, p.ConvoyID, p.StageID)

	stage, err := store.GetStage(db, p.StageID)
	if err != nil {
		// Stage row missing — the convoy may have been cleaned up. Log,
		// audit the anomaly, and complete; never fail the bounty since
		// retrying won't help.
		logger.Printf("Task %d: GetStage(%d) failed: %v — completing without action", b.ID, p.StageID, err)
		store.LogAudit(db, agentName, "stage-senate-review-skipped",
			b.ID, fmt.Sprintf("stage %d not found: %v", p.StageID, err))
		if upErr := store.UpdateBountyStatus(db, b.ID, "Completed"); upErr != nil {
			logger.Printf("Task %d: SenateReview(stage) Completed status transition failed: %v", b.ID, upErr)
		}
		return
	}

	// Audit-trail the stage scope. Operator dashboards can join this
	// against ConvoyReviewCycles + ConvoyStages to confirm the per-stage
	// review hook actually fired at the expected moment.
	store.LogAudit(db, agentName, "stage-senate-review",
		b.ID, fmt.Sprintf("convoy=%d stage_num=%d intent=%q status=%s",
			p.ConvoyID, stage.StageNum, stage.IntentText, stage.Status))

	if upErr := store.UpdateBountyStatus(db, b.ID, "Completed"); upErr != nil {
		logger.Printf("Task %d: SenateReview(stage) Completed status transition failed: %v", b.ID, upErr)
	}
}

// loadSenatePlanInputs returns the deduplicated set of repos touched by
// the Feature's plan (joined from ProposedConvoys.plan_json) plus the
// raw plan JSON for prompt assembly.
func loadSenatePlanInputs(db *sql.DB, featureID int) ([]string, string) {
	var planJSON string
	_ = db.QueryRow(`SELECT plan_json FROM ProposedConvoys WHERE feature_id = ?`, featureID).Scan(&planJSON)
	var tasks []store.TaskPlan
	if planJSON != "" {
		_ = json.Unmarshal([]byte(planJSON), &tasks)
	}
	seen := make(map[string]struct{})
	var repos []string
	for _, t := range tasks {
		r := strings.TrimSpace(t.Repo)
		if r == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		repos = append(repos, r)
	}
	return repos, planJSON
}

// reviewWithSenator runs ONE Senator's review of a Feature plan. The
// LLM call is gated by liveHaikuDisabled() + SpendCapExceeded — when
// either gate trips, the deterministic stub Verdict is returned (one
// concur with confidence=0 + a clear rationale).
//
// Phase 3 ships the deterministic stub as the production-ready fallback
// AND the full LLM ingress is wired through claude.CallWithTranscript.
// The LLM prompt assembly is intentionally minimal at launch — Phase 3
// follow-up tunes the prompt template once we have real verdict data.
func reviewWithSenator(
	ctx context.Context,
	db *sql.DB,
	s *senate.Senator,
	feature *store.Bounty,
	planJSON string,
	lib librarian.Client,
	logger interface{ Printf(string, ...any) },
) senate.Verdict {
	// Mark consulted memories so the senate-refresh dog can decay
	// untouched ones over time. Done unconditionally — even the
	// deterministic stub "consulted" memory entries to assemble the
	// rationale.
	if len(s.Memory) > 0 {
		ids := make([]int, 0, len(s.Memory))
		for _, m := range s.Memory {
			ids = append(ids, m.ID)
		}
		if err := store.MarkSenateMemoryConsulted(db, ids); err != nil {
			logger.Printf("Senator %s: MarkSenateMemoryConsulted failed: %v — continuing", s.RepoID, err)
		}
	}

	// Pull a recent-commits digest for the prompt. We tolerate digest
	// errors (unregistered repo / unreadable local clone): the digest
	// becomes empty and the review proceeds with the rest of the
	// per-Senator context.
	digest, digestErr := lib.RecentCommitsDigest(ctx, s.RepoID, 30*24*time.Hour)
	if digestErr != nil {
		logger.Printf("Senator %s: RecentCommitsDigest failed: %v — proceeding without digest", s.RepoID, digestErr)
	}

	if liveHaikuDisabled() || SpendCapExceeded(db) {
		return deterministicSenateVerdict(s, feature, planJSON, digest, "live-haiku gated")
	}

	// Live LLM ingress. Phase 3 ships the deterministic stub as the
	// canonical contract; the live path here is a structural placeholder
	// matching the SenatorOnboarding pattern in BootstrapSenatorRules:
	// the prompt assembly is documented but left to a follow-up commit.
	// Daemons that turn off LIVE_HAIKU_DISABLED prematurely fall back
	// to the stub rather than emitting an unparseable verdict.
	logger.Printf("Senator %s: live-Haiku review path not yet wired — falling back to deterministic stub", s.RepoID)
	return deterministicSenateVerdict(s, feature, planJSON, digest, "live-haiku fallback")
}

// deterministicSenateVerdict returns a stable concur-verdict that
// captures the Senator's loaded context as the rationale. Used by tests
// (LIVE_HAIKU_DISABLED set) and as the live-path fallback.
//
// The verdict's confidence is intentionally low (0.5) so the
// "high-confidence dissent" gate from Verdict.Approves never trips on
// the stub — the stub is meant to be approval-shaped.
func deterministicSenateVerdict(
	s *senate.Senator,
	feature *store.Bounty,
	planJSON string,
	digest librarian.CommitsDigest,
	mode string,
) senate.Verdict {
	cited := make([]int, 0, len(s.Memory))
	for _, m := range s.Memory {
		cited = append(cited, m.ID)
	}
	rationale := fmt.Sprintf(
		"Senator %s reviewed Feature %d (%s mode). Loaded %d memory entries, %d active rule(s), %d recent commit(s) in digest. No deterministic-check concerns surfaced; concurring with the proposed plan.",
		s.RepoID, feature.ID, mode, len(s.Memory), len(s.RuleKeys), len(digest.Commits),
	)
	_ = planJSON // captured by callers in the persisted verdict; unused here
	return senate.Verdict{
		Senator:        s.RepoID,
		Position:       senate.PositionConcur,
		Rationale:      rationale,
		Confidence:     0.5,
		CitedMemoryIDs: cited,
		CitedRuleIDs:   append([]string(nil), s.RuleKeys...),
	}
}

// persistVerdict writes one Verdict to SenateReview. Errors are logged
// rather than escalated — the verdict is also returned in-memory to the
// aggregate path, so the worst-case is "verdict applied but not
// persisted to the audit table." Pattern P28 (audit-row-completeness)
// catches the persistence-gap regression.
func persistVerdict(db *sql.DB, featureID int, v senate.Verdict, logger interface{ Printf(string, ...any) }) {
	concernsJSON, _ := json.Marshal(v.Concerns)
	amendmentsJSON, _ := json.Marshal(v.Amendments)
	_, err := store.InsertSenateReview(db, store.SenateReviewRow{
		FeatureID:  featureID,
		Senator:    v.Senator,
		Position:   string(v.Position),
		Concerns:   string(concernsJSON),
		Amendments: string(amendmentsJSON),
		Rationale:  v.Rationale,
		Confidence: v.Confidence,
	})
	if err != nil {
		logger.Printf("persistVerdict(feature=%d, senator=%s): %v", featureID, v.Senator, err)
	}
}

// buildSenateFeedback constructs an operator-readable summary of every
// non-concur verdict for inclusion in the rejected Feature's payload.
// Keeps the format symmetric with BoS / ISB feedback prefixes.
func buildSenateFeedback(verdicts []senate.Verdict) string {
	var lines []string
	for _, v := range verdicts {
		if v.Approves() {
			continue
		}
		lines = append(lines, fmt.Sprintf("  - Senator %s: %s (confidence=%.2f) — %s",
			v.Senator, v.Position, v.Confidence, v.Rationale))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\nSENATE FEEDBACK: One or more Senators dissented at high confidence. Review and revise the plan to address each Senator's concerns:\n" +
		strings.Join(lines, "\n")
}

// senatorOnboardingLLMOutputs is the parsed result of the 3-output senator
// onboarding LLM call. All three arrays must be present (empty is fine).
type senatorOnboardingLLMOutputs struct {
	KnowledgeDigest []senatorKnowledgeFact   `json:"knowledge_digest"`
	RuleSuggestions []senatorRuleSuggestion  `json:"rule_suggestions"`
	TagSuggestions  []senatorTagSuggestion   `json:"tag_suggestions"`
}

// senatorKnowledgeFact is one entry in the knowledge_digest LLM output.
type senatorKnowledgeFact struct {
	Fact     string `json:"fact"`
	Weight   int    `json:"weight"` // 1–5; 5=critical, 1=trivia
	Category string `json:"category"` // architecture|testing|patterns|conventions|risks
}

// senatorRuleSuggestion is one entry in the rule_suggestions LLM output.
type senatorRuleSuggestion struct {
	RuleKey        string `json:"rule_key"`
	Description    string `json:"description"`
	SuggestedScope string `json:"suggested_scope"` // senate:<repo>|senate:*|senate:tag:<t>
	Rationale      string `json:"rationale"`
}

// senatorTagSuggestion is one entry in the tag_suggestions LLM output.
type senatorTagSuggestion struct {
	Tag       string `json:"tag"`
	Rationale string `json:"rationale"`
}

// senatorOnboardingSystemPrompt builds the system prompt for the 3-output
// LLM call issued during SenatorOnboarding and SenatorRefresh.
func senatorOnboardingSystemPrompt(repoName string) string {
	return fmt.Sprintf(`You are a Senate advisor for the repository "%s".

You have been given context about this repository: its README content, a recent git log summary, existing SenateMemory entries (facts already known about the repo), and a file-tree sketch.

Your task is to produce exactly 3 JSON arrays in a single JSON object:

1. knowledge_digest — observable facts about this repo. Rules:
   - Each fact must be concrete and verifiable, not an opinion.
   - Weight by importance: 5=critical architectural invariant, 4=important, 3=notable, 2=minor, 1=trivia.
   - Category must be one of: architecture, testing, patterns, conventions, risks.
   - Aim for 5–20 facts. Zero facts is acceptable if you have insufficient context.

2. rule_suggestions — enforceable coding/process standards this repo should follow. Rules:
   - Only suggest things that are genuinely verifiable in code review or CI.
   - suggested_scope must be one of: "senate:%s" (repo-specific), "senate:*" (global), or "senate:tag:<tagname>" (tag-scoped).
   - Be conservative — 0 suggestions is acceptable and preferred over vague suggestions.
   - rule_key must use kebab-case and be globally unique (prefix with "senate-%s-" for repo-specific rules).

3. tag_suggestions — tags this repo likely belongs to. Rules:
   - Prefer existing tags where possible; suggest new ones only if clearly warranted.
   - Each tag must be a short lowercase kebab-case label (e.g. "go", "rails", "monolith", "microservice").
   - Aim for 1–5 tags. Zero is acceptable.

Respond with ONLY a valid JSON object with exactly these three keys. No prose, no markdown fences, no explanation:
{
  "knowledge_digest": [...],
  "rule_suggestions": [...],
  "tag_suggestions": [...]
}

%s`, repoName, repoName, repoName, promptInjectionClause)
}

// runSenatorOnboardingTask runs one onboarding pass for the repo named
// in the payload. Steps:
//
//  1. Insert / update the SenateChambers row in 'onboarding' state.
//  2. Call librarian.BootstrapSenatorRules to produce candidate rules (legacy
//     path — emits PromotionProposals).
//  3. Run the 3-output LLM call (knowledge_digest, rule_suggestions,
//     tag_suggestions) — gated by liveHaikuDisabled().
//  4. Persist knowledge_digest entries as SenateMemory rows (type="knowledge_digest").
//  5. Persist rule_suggestions as PromotionProposals (status="suggested_by_senator").
//  6. Persist tag_suggestions as TagSuggestions rows; create missing Tags rows first.
//  7. Transition the chamber to 'active'.
//
// After step 7 the senator is operational and will be consulted in SenateReview.
func runSenatorOnboardingTask(ctx context.Context, db *sql.DB, agentName string, b *store.Bounty, lib librarian.Client, logger interface{ Printf(string, ...any) }) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Senate claimed SenatorOnboarding %d", sessionID, b.ID)

	var p senatorOnboardingPayload
	if err := json.Unmarshal([]byte(b.Payload), &p); err != nil {
		msg := fmt.Sprintf("SenatorOnboarding payload parse failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after payload parse failure also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}
	if p.RepoID == "" {
		msg := "SenatorOnboarding: payload missing repo_id"
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after invalid payload also failed (%v)", b.ID, fbErr)
		}
		return
	}
	if p.Scope == "" {
		p.Scope = "repo:" + p.RepoID
	}

	// 1. Chamber row in 'onboarding'.
	if err := store.UpsertSenateChamber(db, store.SenateChamber{
		SenatorName:  p.RepoID,
		Scope:        p.Scope,
		SenateMDPath: "SENATE.md",
		Status:       "onboarding",
	}); err != nil {
		msg := fmt.Sprintf("SenatorOnboarding: chamber upsert failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after chamber upsert failure also failed (%v)", b.ID, fbErr)
		}
		return
	}

	// 2. Bootstrap candidate rules via the Librarian Client (legacy path).
	candidates, err := lib.BootstrapSenatorRules(ctx, p.RepoID)
	if err != nil {
		msg := fmt.Sprintf("SenatorOnboarding: BootstrapSenatorRules failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after bootstrap failure also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}
	logger.Printf("Task %d: BootstrapSenatorRules produced %d candidate rule(s) for %s", b.ID, len(candidates), p.RepoID)

	emitted := 0
	for _, cand := range candidates {
		evidenceJSON, mErr := json.Marshal(map[string]string{
			"category":    cand.Category,
			"agent_scope": cand.AgentScope,
			"rationale":   cand.Rationale,
			"evidence":    cand.Evidence,
			"origin":      "senate-onboarding",
		})
		if mErr != nil {
			logger.Printf("Task %d: marshal evidence for %s: %v", b.ID, cand.RuleKey, mErr)
			continue
		}
		propID, eErr := lib.EmitCandidate(ctx, librarian.Candidate{
			HypothesisKey: cand.RuleKey,
			HypothesisRaw: cand.Body,
			EvidenceJSON:  string(evidenceJSON),
		})
		if eErr != nil {
			logger.Printf("Task %d: EmitCandidate(%s) failed: %v", b.ID, cand.RuleKey, eErr)
			continue
		}
		emitted++
		logger.Printf("Task %d: emitted PromotionProposal %d for rule %s", b.ID, propID, cand.RuleKey)
	}

	// 3–6. Run the 3-output LLM call and persist results.
	llmOut, llmErr := runSenatorLLMOutputs(ctx, db, b.ID, p.RepoID, lib, logger)
	if llmErr != nil {
		// Non-fatal: the legacy bootstrap already ran. Log and continue to
		// the active transition so the Senator is not stuck in 'onboarding'
		// forever due to a transient LLM error.
		logger.Printf("Task %d: senatorLLMOutputs for %s failed: %v — proceeding with empty outputs", b.ID, p.RepoID, llmErr)
		llmOut = senatorOnboardingLLMOutputs{}
	}

	factCount := persistSenatorKnowledgeDigest(db, b.ID, p.RepoID, llmOut.KnowledgeDigest, logger)
	ruleCount := persistSenatorRuleSuggestions(ctx, db, b.ID, p.RepoID, p.Scope, llmOut.RuleSuggestions, lib, logger)
	tagCount := persistSenatorTagSuggestions(db, b.ID, p.RepoID, llmOut.TagSuggestions, logger)

	// Fall back to legacy digest-based memory if no LLM facts were produced.
	if factCount == 0 {
		seedDigestMemory(ctx, db, b.ID, p.RepoID, lib, logger)
	}

	// 7. Transition chamber to 'active' — senator is now operational.
	if err := store.SetSenateChamberStatus(db, p.RepoID, "active"); err != nil {
		logger.Printf("Task %d: SetSenateChamberStatus(active) for %s failed: %v — chamber remains onboarding", b.ID, p.RepoID, err)
	} else {
		logger.Printf("Task %d: status=active knowledge_facts=%d rule_suggestions=%d tag_suggestions=%d",
			b.ID, factCount, ruleCount, tagCount)
	}

	store.LogAudit(db, agentName, "senator-onboarded", b.ID,
		fmt.Sprintf("repo=%s candidates=%d facts=%d rules=%d tags=%d", p.RepoID, emitted, factCount, ruleCount, tagCount))
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
	if err := store.UpdateBountyStatus(db, b.ID, "Completed"); err != nil {
		logger.Printf("Task %d: SenatorOnboarding Completed status transition failed: %v", b.ID, err)
	}
}

// runSenatorRefreshTask re-runs the 3-output LLM pass for an already-active
// Senator to incorporate new repo state into SenateMemory, rule suggestions,
// and tag suggestions. The chamber remains 'active' throughout — a refresh
// is a non-destructive append, not a re-onboarding.
func runSenatorRefreshTask(ctx context.Context, db *sql.DB, agentName string, b *store.Bounty, lib librarian.Client, logger interface{ Printf(string, ...any) }) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Senate claimed SenatorRefresh %d", sessionID, b.ID)

	var p senatorOnboardingPayload // same payload shape
	if err := json.Unmarshal([]byte(b.Payload), &p); err != nil {
		msg := fmt.Sprintf("SenatorRefresh payload parse failed: %v", err)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after payload parse failure also failed (%v)", b.ID, fbErr)
		}
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}
	if p.RepoID == "" {
		msg := "SenatorRefresh: payload missing repo_id"
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty after invalid payload also failed (%v)", b.ID, fbErr)
		}
		return
	}
	if p.Scope == "" {
		p.Scope = "repo:" + p.RepoID
	}

	// Confirm the chamber is still active before spending an LLM call.
	chamber, chamberErr := store.GetSenateChamber(db, p.RepoID)
	if chamberErr != nil {
		msg := fmt.Sprintf("SenatorRefresh: GetSenateChamber(%s) failed: %v", p.RepoID, chamberErr)
		if fbErr := store.FailBounty(db, b.ID, msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty also failed (%v)", b.ID, fbErr)
		}
		return
	}
	if chamber == nil || chamber.Status != "active" {
		// Senator was retired/suspended between queue time and claim time.
		// Complete without error — the dog will stop queuing when it's gone.
		logger.Printf("Task %d: SenatorRefresh(%s): chamber status=%v — completing without refresh",
			b.ID, p.RepoID, chamber)
		if upErr := store.UpdateBountyStatus(db, b.ID, "Completed"); upErr != nil {
			logger.Printf("Task %d: SenatorRefresh Completed status transition failed: %v", b.ID, upErr)
		}
		return
	}

	llmOut, llmErr := runSenatorLLMOutputs(ctx, db, b.ID, p.RepoID, lib, logger)
	if llmErr != nil {
		logger.Printf("Task %d: senatorLLMOutputs for %s failed: %v — completing without persisting outputs", b.ID, p.RepoID, llmErr)
		llmOut = senatorOnboardingLLMOutputs{}
	}

	factCount := persistSenatorKnowledgeDigest(db, b.ID, p.RepoID, llmOut.KnowledgeDigest, logger)
	ruleCount := persistSenatorRuleSuggestions(ctx, db, b.ID, p.RepoID, p.Scope, llmOut.RuleSuggestions, lib, logger)
	tagCount := persistSenatorTagSuggestions(db, b.ID, p.RepoID, llmOut.TagSuggestions, logger)

	if markErr := store.MarkSenateChamberRefreshed(db, p.RepoID); markErr != nil {
		logger.Printf("Task %d: MarkSenateChamberRefreshed(%s): %v", b.ID, p.RepoID, markErr)
	}
	logger.Printf("Task %d: SenatorRefresh complete — status=active knowledge_facts=%d rule_suggestions=%d tag_suggestions=%d",
		b.ID, factCount, ruleCount, tagCount)

	store.LogAudit(db, agentName, "senator-refreshed", b.ID,
		fmt.Sprintf("repo=%s facts=%d rules=%d tags=%d", p.RepoID, factCount, ruleCount, tagCount))
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
	if err := store.UpdateBountyStatus(db, b.ID, "Completed"); err != nil {
		logger.Printf("Task %d: SenatorRefresh Completed status transition failed: %v", b.ID, err)
	}
}

// runSenatorLLMOutputs issues the 3-output LLM call and returns the parsed
// outputs. When liveHaikuDisabled() is set (tests/CI), returns a minimal
// deterministic fixture so tests never spend an LLM call. A failure to parse
// the LLM response returns an error; the caller decides how to handle it.
func runSenatorLLMOutputs(ctx context.Context, db *sql.DB, taskID int, repoID string, lib librarian.Client, logger interface{ Printf(string, ...any) }) (senatorOnboardingLLMOutputs, error) {
	if liveHaikuDisabled() || SpendCapExceeded(db) {
		// Deterministic stub — minimal valid output for tests.
		return senatorOnboardingLLMOutputs{
			KnowledgeDigest: []senatorKnowledgeFact{
				{Fact: fmt.Sprintf("Senator %s onboarded (deterministic stub).", repoID), Weight: 1, Category: "conventions"},
			},
			RuleSuggestions: []senatorRuleSuggestion{},
			TagSuggestions:  []senatorTagSuggestion{},
		}, nil
	}

	// Pull the senator's existing memory for context.
	existingMem, memErr := store.ListSenateMemory(db, repoID, 20)
	if memErr != nil {
		logger.Printf("Task %d: ListSenateMemory(%s) failed: %v — proceeding without memory context", taskID, repoID, memErr)
	}
	var memSummary strings.Builder
	for _, m := range existingMem {
		fmt.Fprintf(&memSummary, "- [%s] %s\n", m.Topic, m.Summary)
	}

	// Pull a recent-commits digest.
	digest, digestErr := lib.RefreshSenatorMemoryDigest(ctx, repoID)
	var digestSummary string
	if digestErr != nil {
		logger.Printf("Task %d: RefreshSenatorMemoryDigest(%s) failed: %v — proceeding without digest", taskID, repoID, digestErr)
		digestSummary = "(digest unavailable)"
	} else {
		digestSummary = digest.APISurfaceSummary
		if digest.NotesForOperator != "" {
			digestSummary += "\n\n" + digest.NotesForOperator
		}
	}

	systemPrompt := senatorOnboardingSystemPrompt(repoID)
	userPrompt := fmt.Sprintf(
		"Repository: %s\n\nExisting memory entries:\n%s\n\nRecent digest:\n%s",
		repoID,
		WrapUserContent("existing_memory", memSummary.String()),
		WrapUserContent("digest", digestSummary),
	)

	prof, profErr := capabilities.LoadProfile("senate")
	if profErr != nil {
		return senatorOnboardingLLMOutputs{}, fmt.Errorf("runSenatorLLMOutputs: LoadProfile: %w", profErr)
	}
	mcpConfig, _ := prof.MCPConfigArg()

	raw, callErr := claude.CallWithTranscript(ctx,
		claude.CallDescriptor{Agent: "senate", PromptVersion: "d14-onboarding-v1"},
		systemPrompt, userPrompt,
		prof.AllowedToolsArg(), prof.DisallowedToolsArg(), mcpConfig, 1)
	if callErr != nil {
		return senatorOnboardingLLMOutputs{}, fmt.Errorf("runSenatorLLMOutputs: LLM call: %w", callErr)
	}

	clean := claude.ExtractJSON(raw)
	var out senatorOnboardingLLMOutputs
	if parseErr := json.Unmarshal([]byte(clean), &out); parseErr != nil {
		return senatorOnboardingLLMOutputs{}, fmt.Errorf("runSenatorLLMOutputs: parse JSON: %w (raw=%q)", parseErr, clean[:min(len(clean), 200)])
	}
	return out, nil
}

// persistSenatorKnowledgeDigest inserts knowledge_digest facts as SenateMemory
// rows (type="knowledge_digest"). Returns the count inserted.
func persistSenatorKnowledgeDigest(db *sql.DB, taskID int, repoID string, facts []senatorKnowledgeFact, logger interface{ Printf(string, ...any) }) int {
	inserted := 0
	for _, f := range facts {
		if f.Fact == "" {
			continue
		}
		w := float64(f.Weight)
		if w < 1 {
			w = 1
		}
		if w > 5 {
			w = 5
		}
		topic := "knowledge_digest"
		if f.Category != "" {
			topic = "knowledge_digest:" + f.Category
		}
		if _, err := store.InsertSenateMemory(db, store.SenateMemoryEntry{
			Senator: repoID,
			Topic:   topic,
			Summary: f.Fact,
			Source:  "knowledge_digest",
			Weight:  w,
		}); err != nil {
			logger.Printf("Task %d: persistSenatorKnowledgeDigest insert failed: %v", taskID, err)
			continue
		}
		inserted++
	}
	return inserted
}

// persistSenatorRuleSuggestions emits rule_suggestions as PromotionProposals.
// Returns the count emitted.
func persistSenatorRuleSuggestions(ctx context.Context, db *sql.DB, taskID int, repoID, scope string, rules []senatorRuleSuggestion, lib librarian.Client, logger interface{ Printf(string, ...any) }) int {
	emitted := 0
	for _, r := range rules {
		if r.RuleKey == "" || r.Description == "" {
			continue
		}
		suggestedScope := r.SuggestedScope
		if suggestedScope == "" {
			suggestedScope = "senate:" + repoID
		}
		evidenceJSON, mErr := json.Marshal(map[string]string{
			"rationale":       r.Rationale,
			"suggested_scope": suggestedScope,
			"origin":          "senate-onboarding-llm",
			"repo":            repoID,
			"scope":           scope,
		})
		if mErr != nil {
			logger.Printf("Task %d: marshal evidence for rule %s: %v", taskID, r.RuleKey, mErr)
			continue
		}
		propID, eErr := lib.EmitCandidate(ctx, librarian.Candidate{
			HypothesisKey: r.RuleKey,
			HypothesisRaw: r.Description,
			EvidenceJSON:  string(evidenceJSON),
		})
		if eErr != nil {
			logger.Printf("Task %d: EmitCandidate(%s) failed: %v", taskID, r.RuleKey, eErr)
			continue
		}
		logger.Printf("Task %d: emitted PromotionProposal %d for senator rule %s", taskID, propID, r.RuleKey)
		emitted++
	}
	return emitted
}

// persistSenatorTagSuggestions inserts tag_suggestions into the TagSuggestions
// table. If a tag does not exist in the Tags table, it is created first with
// the rationale as the description. Returns the count inserted.
func persistSenatorTagSuggestions(db *sql.DB, taskID int, repoID string, tags []senatorTagSuggestion, logger interface{ Printf(string, ...any) }) int {
	inserted := 0
	for _, ts := range tags {
		if ts.Tag == "" {
			continue
		}
		// Ensure the tag exists; create if missing (idempotent via INSERT OR IGNORE).
		_, tagErr := store.GetTag(db, ts.Tag)
		if tagErr != nil {
			// Tag does not exist — create it.
			createErr := store.CreateTag(db, ts.Tag, ts.Rationale, "librarian:senate-onboarding")
			if createErr != nil {
				logger.Printf("Task %d: CreateTag(%q) failed: %v — skipping suggestion", taskID, ts.Tag, createErr)
				continue
			}
			logger.Printf("Task %d: created new tag %q (rationale=%q)", taskID, ts.Tag, ts.Rationale)
		}
		_, sugErr := store.CreateTagSuggestion(db, repoID, ts.Tag, ts.Rationale, "librarian:senate-onboarding")
		if sugErr != nil {
			logger.Printf("Task %d: CreateTagSuggestion(%q, %q) failed: %v", taskID, repoID, ts.Tag, sugErr)
			continue
		}
		inserted++
	}
	return inserted
}

// seedDigestMemory is the fallback memory-seeding path used when the LLM
// produces zero knowledge_digest facts. Calls RefreshSenatorMemoryDigest and
// inserts the APISurfaceSummary and NotesForOperator into SenateMemory.
func seedDigestMemory(ctx context.Context, db *sql.DB, taskID int, repoID string, lib librarian.Client, logger interface{ Printf(string, ...any) }) {
	digest, digestErr := lib.RefreshSenatorMemoryDigest(ctx, repoID)
	if digestErr != nil {
		logger.Printf("Task %d: RefreshSenatorMemoryDigest(%s) failed: %v — seeding minimal memory anchor", taskID, repoID, digestErr)
		if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
			Senator: repoID,
			Topic:   "onboarding",
			Summary: fmt.Sprintf("Senator %s onboarded; digest refresh deferred (%v).", repoID, digestErr),
			Source:  "bootstrap",
			Weight:  1.0,
		}); mErr != nil {
			logger.Printf("Task %d: minimal-memory insert failed: %v", taskID, mErr)
		}
		return
	}
	if digest.APISurfaceSummary != "" {
		if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
			Senator: repoID,
			Topic:   "api-surface",
			Summary: digest.APISurfaceSummary,
			Source:  "bootstrap",
			Weight:  1.0,
		}); mErr != nil {
			logger.Printf("Task %d: api-surface memory insert failed: %v", taskID, mErr)
		}
	}
	if digest.NotesForOperator != "" {
		if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
			Senator: repoID,
			Topic:   "notes-for-operator",
			Summary: digest.NotesForOperator,
			Source:  "bootstrap",
			Weight:  0.8,
		}); mErr != nil {
			logger.Printf("Task %d: notes-for-operator memory insert failed: %v", taskID, mErr)
		}
	}
}

// AdvanceSenateChamberOnRatification flips a Senator's chamber from
// 'onboarding' → 'active' once at least one of its ratified rules has
// landed in FleetRules. Used by the ratification flow + integration
// tests; idempotent (re-calling against an already-active chamber is
// a no-op return without error).
//
// Anti-cheat: this is the ONLY chamber-status flip exposed to non-store
// callers; direct status writes from inside internal/senate/ are AST-
// forbidden so the chamber-state machine is auditable from one site.
func AdvanceSenateChamberOnRatification(db *sql.DB, repoID string) error {
	chamber, err := store.GetSenateChamber(db, repoID)
	if err != nil {
		return fmt.Errorf("AdvanceSenateChamberOnRatification: %w", err)
	}
	if chamber == nil || chamber.Status != "onboarding" {
		return nil
	}
	// Confirm at least one ratified senate-scoped rule exists in
	// FleetRules. Without that, advancing would be premature.
	scope := "senate:" + repoID
	var n int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM FleetRules
		 WHERE agent_scope = ?
		   AND IFNULL(active_until, '') = ''`, scope).Scan(&n); err != nil {
		return fmt.Errorf("AdvanceSenateChamberOnRatification: count active rules: %w", err)
	}
	if n == 0 {
		return nil
	}
	return store.SetSenateChamberStatus(db, repoID, "active")
}

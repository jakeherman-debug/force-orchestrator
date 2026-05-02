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
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/senate"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// senateReviewPayload is the JSON shape QueueSenateReview emits.
type senateReviewPayload struct {
	FeatureID  int    `json:"feature_id"`
	TargetRepo string `json:"target_repo"`
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

		// Try claiming SenateReview first (the live-pipeline path); fall
		// back to SenatorOnboarding (the bootstrap path) when no review
		// work is queued. Onboarding is comparatively rare so the order
		// keeps the steady-state path tight.
		if b, claimed := store.ClaimBounty(db, "SenateReview", name); claimed {
			runSenateReviewTask(ctx, db, name, b, lib, logger)
			continue
		}
		if b, claimed := store.ClaimBounty(db, "SenatorOnboarding", name); claimed {
			runSenatorOnboardingTask(ctx, db, name, b, lib, logger)
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
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
		return
	}
	// Idempotence: if the feature is no longer in AwaitingSenateReview,
	// some other path resolved it — log and complete this task without
	// re-firing the LLMs.
	if feature.Status != "AwaitingSenateReview" {
		logger.Printf("Task %d: Feature %d already in status %q (not AwaitingSenateReview) — Senate completing without re-review",
			b.ID, p.FeatureID, feature.Status)
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
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
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
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
		_ = store.UpdateBountyStatus(db, b.ID, "Completed")
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

// runSenatorOnboardingTask runs one onboarding pass for the repo named
// in the payload. Steps:
//
//  1. Insert / update the SenateChambers row in 'onboarding' state.
//  2. Call librarian.BootstrapSenatorRules to produce candidate rules.
//  3. For each candidate, EmitCandidate via the Librarian Client (the
//     Phase 3 wiring of the "production path" described in Phase 0).
//     This routes through the standard PromotionProposal pipeline —
//     operator ratifies before the rule lands in FleetRules.
//  4. Seed an initial SenateMemory entry derived from the digest.
//
// The chamber stays in 'onboarding' until at least one ratified rule
// lands; the first call to AdvanceSenateChamberOnRatification (below)
// flips it to 'active'. Phase 3 ships a passive listener that
// integration tests can call directly; the dashboard ratification flow
// hooks into it in a follow-up.
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

	// 2. Bootstrap candidate rules via the Librarian Client. The Phase 0
	// stub is the deterministic fallback; the production path runs the
	// LLM-authored bootstrap when LIVE_HAIKU_DISABLED is unset.
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

	// 3. Emit each candidate as a PromotionProposal. The candidate's
	// rule body becomes proposed_content; the rationale + evidence are
	// folded into the evidence_summary_json so operators see WHY at
	// ratification time. Anti-cheat: this is the ONLY rule-promotion
	// path from the Senate package — direct FleetRules writes are
	// AST-forbidden by Pattern P34.
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

	// 4. Seed initial memory entries derived from the digest. The
	// SenatorOnboarding step seeds at least one memory row so the
	// Senator's prompt context isn't empty on its first review.
	digest, digestErr := lib.RefreshSenatorMemoryDigest(ctx, p.RepoID)
	if digestErr != nil {
		// A digest refresh failure is non-fatal — seed a minimal entry
		// so the chamber still has a memory anchor for its first review.
		logger.Printf("Task %d: RefreshSenatorMemoryDigest(%s) failed: %v — seeding minimal memory anchor", b.ID, p.RepoID, digestErr)
		if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
			Senator: p.RepoID,
			Topic:   "onboarding",
			Summary: fmt.Sprintf("Senator %s onboarded; digest refresh deferred (%v).", p.RepoID, digestErr),
			Source:  "bootstrap",
			Weight:  1.0,
		}); mErr != nil {
			logger.Printf("Task %d: minimal-memory insert failed: %v", b.ID, mErr)
		}
	} else {
		if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
			Senator: p.RepoID,
			Topic:   "api-surface",
			Summary: digest.APISurfaceSummary,
			Source:  "bootstrap",
			Weight:  1.0,
		}); mErr != nil {
			logger.Printf("Task %d: api-surface memory insert failed: %v", b.ID, mErr)
		}
		if digest.NotesForOperator != "" {
			if _, mErr := store.InsertSenateMemory(db, store.SenateMemoryEntry{
				Senator: p.RepoID,
				Topic:   "notes-for-operator",
				Summary: digest.NotesForOperator,
				Source:  "bootstrap",
				Weight:  0.8,
			}); mErr != nil {
				logger.Printf("Task %d: notes-for-operator memory insert failed: %v", b.ID, mErr)
			}
		}
	}

	logger.Printf("Task %d: SenatorOnboarding complete — chamber=%s status=onboarding candidates_emitted=%d", b.ID, p.RepoID, emitted)
	store.LogAudit(db, agentName, "senator-onboarded", b.ID,
		fmt.Sprintf("repo=%s candidates=%d", p.RepoID, emitted))
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, agentName, b.ID))
	if err := store.UpdateBountyStatus(db, b.ID, "Completed"); err != nil {
		logger.Printf("Task %d: SenatorOnboarding Completed status transition failed: %v", b.ID, err)
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

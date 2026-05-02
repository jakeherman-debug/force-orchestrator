// dogs_convoy_stage_watch.go — D5.5 Phase 1.
//
// `convoy-stage-watch` walks active ConvoyStages rows and advances the
// per-stage state machine. Cadence is 5 minutes (matches sub-pr-ci-watch
// / draft-pr-watch — the same heartbeat the operator already expects
// for stage-level signal).
//
// State machine the dog implements:
//
//   Open + every stage's PR merged          → AllPRsMerged (stamp all_prs_merged_at)
//   AllPRsMerged                            → AwaitingGate
//   AwaitingGate + gate evaluator passes    → GatePassed   (stamp gate_passed_at)
//   AwaitingGate + gate evaluator fails     → Failed       (stamp completed_at)
//   AwaitingGate + still pending            → no change
//   AwaitingGate + past gate_timeout_minutes→ Failed       (escalation surface)
//
// What P1 does NOT do (deferred to P2 — Commander integration):
//   - Open the next stage when stage N reports GatePassed (i.e.,
//     transition stage N → Verified and stage N+1 Pending → Open +
//     create ConvoyAskBranches rows).
//   - Astromech dispatch gating (refusing claims against Pending
//     stages). The skeletal Pattern P-StageGate audit ships in P1
//     so the regression slot is reserved.
//
// Single-stage legacy convoys (gate_type IS NULL, the forward-compat
// migration shape) are a no-op for this dog: the existing draft-pr-
// watch + stale-convoys-report machinery already owns their lifecycle,
// and we do NOT want to double-report or transition them.
//
// Registry wiring (P1 ships only the baseline gates):
//
//   stagegate.RegisterBaselineGates(reg) at daemon startup wires
//   soak_minutes / operator_confirm / null / all_of / any_of. P3 adds
//   the four advanced leaves; the dog code is unchanged.
//
// Tests build a registry per test, register baseline + any extra
// stubs they need, and call runConvoyStageWatch directly (skipping
// the cooldown machinery for determinism).

package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"force-orchestrator/internal/stagegate"
	"force-orchestrator/internal/store"
)

// ── Dependency injection ────────────────────────────────────────────────
//
// The stagegate.Registry is constructed once at daemon startup (P2
// will add this to the daemon boot path). The dog reads it via
// getStageGateRegistry; tests inject their own via the package-var
// setter. Mirrors the SupplyRecheck deps pattern so the dispatch path
// stays uniform.

var (
	stageGateRegistry *stagegate.Registry

	// gateTimeoutBudgetCheck is the indirection seam through which
	// emitGateTimeoutEscalation calls RespectNotificationBudget. Tests
	// override this to drive the allowed=true and allowed=false branches
	// without writing budget rows that StakesHigh would punch through
	// anyway. Default points at the production helper; SetGateTimeoutBudgetCheckForTest
	// returns a restore function so tests don't leak state.
	gateTimeoutBudgetCheck = store.RespectNotificationBudget

	// gateTimeoutSendMail is the indirection for store.SendMail so tests
	// can assert call vs no-call without inspecting Fleet_Mail rows.
	// Production points at store.SendMail.
	gateTimeoutSendMail = store.SendMail

	// stageTransitionNotifyFn is the seam used by stage-transition pings.
	// Default points at notifyAfterFn (the package-wide notify-after seam),
	// but a per-feature seam lets tests instrument stage pings without
	// also affecting supply-token-recheck. See onStageTransition.
	stageTransitionNotifyFn = func(ctx context.Context, label string) error {
		return notifyAfterFn(ctx, label)
	}
)

// SetStageTransitionNotifyForTest swaps the notify-after seam used by
// stage-transition pings and returns a restore closure. Tests assert
// that each transition fires (or doesn't, on debounce) by counting
// invocations on the captured stub.
func SetStageTransitionNotifyForTest(fn func(ctx context.Context, label string) error) (restore func()) {
	prev := stageTransitionNotifyFn
	stageTransitionNotifyFn = fn
	return func() { stageTransitionNotifyFn = prev }
}

// SetGateTimeoutBudgetCheckForTest swaps the budget-check seam and
// returns a restore closure. Tests use this to force allowed=false
// (the StakesHigh-punches-through real helper makes that branch
// unreachable in production today, but Wave 3 ζ tightened the call
// site to gate on allowed so the regression slot exists).
func SetGateTimeoutBudgetCheckForTest(
	fn func(ctx context.Context, db *sql.DB, operatorEmail, source, channel, payloadJSON string, stakes store.NotificationStakes) (bool, error),
) (restore func()) {
	prev := gateTimeoutBudgetCheck
	gateTimeoutBudgetCheck = fn
	return func() { gateTimeoutBudgetCheck = prev }
}

// SetGateTimeoutSendMailForTest swaps the SendMail seam and returns a
// restore closure. Tests use this to count calls without scanning
// Fleet_Mail.
func SetGateTimeoutSendMailForTest(
	fn func(db *sql.DB, from, to, subject, body string, taskID int, msgType store.MailType) int64,
) (restore func()) {
	prev := gateTimeoutSendMail
	gateTimeoutSendMail = fn
	return func() { gateTimeoutSendMail = prev }
}

// RegisterStageGateRegistry is the daemon-side seam for installing
// the Phase-1 baseline registry. P2 will call this from boot.go
// once Commander integration lands; P1 ships the seam plus tests
// that drive it directly.
//
// Idempotent — re-registering replaces. The dog tolerates a nil
// registry by logging-and-skipping rather than panicking, so a
// daemon that hasn't wired the registry yet still runs other dogs.
func RegisterStageGateRegistry(r *stagegate.Registry) {
	stageGateRegistry = r
}

// getStageGateRegistry returns the registered registry or nil.
func getStageGateRegistry() *stagegate.Registry {
	return stageGateRegistry
}

// dogConvoyStageWatch is the entry point dispatched by runDog. It
// resolves the registry from package state and delegates to
// runConvoyStageWatch (which takes the registry explicitly so tests
// can inject their own).
func dogConvoyStageWatch(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	reg := getStageGateRegistry()
	if reg == nil {
		// P1 ships the dog before P2 wires the registry — log and
		// exit quietly so the dog can be enabled in dev environments
		// without dragging in the rest of the staging stack.
		logger.Printf("Dog convoy-stage-watch: stagegate registry not registered; skipping (P2 wires this at daemon boot)")
		return nil
	}
	return runConvoyStageWatch(ctx, db, reg, logger)
}

// activeStage carries just the columns the dog needs from
// ConvoyStages for evaluation. We materialise the slice up front so
// the dog can run AdvanceStage without holding the rows cursor.
//
// intentText + convoyName are surfaced for the gate-timeout
// escalation message (D5.5 P3 ζ): operators reading the
// "[STAGE GATE TIMEOUT]" mail need to identify which convoy + which
// stage failed without clicking through to a separate dashboard.
type activeStage struct {
	id                 int
	convoyID           int
	convoyName         string // joined from Convoys.name
	stageNum           int
	intentText         string // ConvoyStages.intent_text
	status             string
	gateType           string // empty if NULL
	gateTypeIsNull     bool
	gateConfigJSON     string
	gateTimeoutMinutes int
	openedAt           string
	allPRsMergedAt     string
}

// runConvoyStageWatch is the testable entry point. Walks every
// ConvoyStages row whose status is in the set the dog mutates
// {Open, AllPRsMerged, AwaitingGate}, dispatches per-row state
// transitions, and returns nil on success. Per-stage errors are
// logged and the loop continues — a single broken gate config
// shouldn't sink the rest of the fleet.
func runConvoyStageWatch(ctx context.Context, db *sql.DB, registry *stagegate.Registry, logger interface{ Printf(string, ...any) }) error {
	if db == nil {
		return errors.New("convoy-stage-watch: db is nil")
	}
	if registry == nil {
		return errors.New("convoy-stage-watch: registry is nil")
	}

	// LEFT JOIN against Convoys for the human-readable convoy name; we
	// surface it in escalation mail (D5.5 P3 ζ) so the operator doesn't
	// have to cross-reference a numeric convoy id. Test-only convoys
	// without a Convoys row degrade gracefully: convoyName is empty and
	// the escalation message falls back to the numeric id.
	rows, err := db.Query(`
		SELECT cs.id, cs.convoy_id, IFNULL(c.name, ''),
		       cs.stage_num, IFNULL(cs.intent_text, ''), cs.status,
		       cs.gate_type, IFNULL(cs.gate_config_json, '{}'),
		       cs.gate_timeout_minutes,
		       IFNULL(cs.opened_at, ''), IFNULL(cs.all_prs_merged_at, '')
		FROM ConvoyStages cs
		LEFT JOIN Convoys c ON c.id = cs.convoy_id
		WHERE cs.status IN ('Open', 'AllPRsMerged', 'AwaitingGate')`)
	if err != nil {
		return fmt.Errorf("convoy-stage-watch: query active stages: %w", err)
	}
	defer rows.Close()

	var stages []activeStage
	for rows.Next() {
		var s activeStage
		var gateType sql.NullString
		if sErr := rows.Scan(&s.id, &s.convoyID, &s.convoyName,
			&s.stageNum, &s.intentText, &s.status,
			&gateType, &s.gateConfigJSON, &s.gateTimeoutMinutes,
			&s.openedAt, &s.allPRsMergedAt); sErr != nil {
			logger.Printf("convoy-stage-watch: scan failed: %v", sErr)
			continue
		}
		s.gateType = gateType.String
		s.gateTypeIsNull = !gateType.Valid
		stages = append(stages, s)
	}
	if rErr := rows.Err(); rErr != nil {
		return fmt.Errorf("convoy-stage-watch: rows iter: %w", rErr)
	}

	if len(stages) == 0 {
		logger.Printf("Dog convoy-stage-watch: no active stages")
		return nil
	}
	logger.Printf("Dog convoy-stage-watch: walking %d active stage(s)", len(stages))

	var transitioned int
	for _, s := range stages {
		// Legacy single-stage convoys carry gate_type=NULL and live
		// in status=Open. The dog must NOT advance them — the existing
		// stale-convoys-report + draft-pr-watch own that lifecycle.
		if s.gateTypeIsNull {
			continue
		}

		switch s.status {
		case store.StageStatusOpen:
			if openOK := stageAllPRsMerged(db, s.id); openOK {
				if aErr := store.AdvanceStage(db, s.id, store.StageStatusAllPRsMerged); aErr != nil {
					logger.Printf("convoy-stage-watch: stage %d Open→AllPRsMerged failed: %v", s.id, aErr)
					continue
				}
				transitioned++
				logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) Open→AllPRsMerged", s.id, s.convoyID, s.stageNum)
				onStageTransition(ctx, db, s, store.StageStatusOpen, store.StageStatusAllPRsMerged, "all sub-PRs merged", logger)
			}

		case store.StageStatusAllPRsMerged:
			if aErr := store.AdvanceStage(db, s.id, store.StageStatusAwaitingGate); aErr != nil {
				logger.Printf("convoy-stage-watch: stage %d AllPRsMerged→AwaitingGate failed: %v", s.id, aErr)
				continue
			}
			transitioned++
			logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) AllPRsMerged→AwaitingGate", s.id, s.convoyID, s.stageNum)
			onStageTransition(ctx, db, s, store.StageStatusAllPRsMerged, store.StageStatusAwaitingGate, "ready for gate evaluation", logger)

		case store.StageStatusAwaitingGate:
			// Pre-flight: enforce gate timeout. If the stage has been
			// AwaitingGate longer than gate_timeout_minutes, transition
			// to Failed and emit an escalation. Anti-cheat directive
			// ("No silent gate skip"): timeout sinks to Failed, never
			// to GatePassed.
			if stageGateTimedOut(s) {
				if aErr := store.AdvanceStage(db, s.id, store.StageStatusFailed); aErr != nil {
					logger.Printf("convoy-stage-watch: stage %d AwaitingGate→Failed (timeout) failed: %v", s.id, aErr)
					continue
				}
				transitioned++
				emitGateTimeoutEscalation(ctx, db, s, logger)
				logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) AwaitingGate→Failed (gate timeout %dmin exceeded)",
					s.id, s.convoyID, s.stageNum, s.gateTimeoutMinutes)
				onStageTransition(ctx, db, s, store.StageStatusAwaitingGate, store.StageStatusFailed,
					fmt.Sprintf("gate timeout %dmin exceeded", s.gateTimeoutMinutes), logger)
				continue
			}

			passed, reason, eErr := evaluateGate(ctx, db, registry, s)
			if errors.Is(eErr, stagegate.ErrPending) {
				logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) gate %q pending: %s",
					s.id, s.convoyID, s.stageNum, s.gateType, reason)
				continue
			}
			if eErr != nil {
				logger.Printf("convoy-stage-watch: stage %d gate %q evaluation error: %v",
					s.id, s.gateType, eErr)
				continue
			}
			if passed {
				if aErr := store.AdvanceStage(db, s.id, store.StageStatusGatePassed); aErr != nil {
					logger.Printf("convoy-stage-watch: stage %d AwaitingGate→GatePassed failed: %v", s.id, aErr)
					continue
				}
				transitioned++
				logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) AwaitingGate→GatePassed (gate %q: %s)",
					s.id, s.convoyID, s.stageNum, s.gateType, reason)
				onStageTransition(ctx, db, s, store.StageStatusAwaitingGate, store.StageStatusGatePassed, reason, logger)
			} else {
				if aErr := store.AdvanceStage(db, s.id, store.StageStatusFailed); aErr != nil {
					logger.Printf("convoy-stage-watch: stage %d AwaitingGate→Failed failed: %v", s.id, aErr)
					continue
				}
				transitioned++
				logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) AwaitingGate→Failed (gate %q: %s)",
					s.id, s.convoyID, s.stageNum, s.gateType, reason)
				onStageTransition(ctx, db, s, store.StageStatusAwaitingGate, store.StageStatusFailed, reason, logger)
			}
		}
	}

	logger.Printf("Dog convoy-stage-watch: %d transition(s) this tick", transitioned)
	return nil
}

// stageAllPRsMerged returns true when every AskBranchPR for the
// stage's ConvoyAskBranches has reached state='Merged'. Resolves via
// stage_id → ConvoyAskBranches → AskBranchPRs.
//
// Returns false (not merged) if there are zero PR rows for the stage
// — an Open stage with no PRs means astromechs haven't opened them
// yet; the dog stays Open until they do.
func stageAllPRsMerged(db *sql.DB, stageID int) bool {
	// Total PRs opened against ANY ConvoyAskBranches row for the stage.
	var total int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM AskBranchPRs abp
		JOIN ConvoyAskBranches cab
		  ON cab.convoy_id = abp.convoy_id AND cab.repo = abp.repo
		WHERE cab.stage_id = ?`, stageID).Scan(&total); err != nil {
		return false
	}
	if total == 0 {
		return false
	}
	var merged int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM AskBranchPRs abp
		JOIN ConvoyAskBranches cab
		  ON cab.convoy_id = abp.convoy_id AND cab.repo = abp.repo
		WHERE cab.stage_id = ?
		  AND abp.state = 'Merged'`, stageID).Scan(&merged); err != nil {
		return false
	}
	return merged == total
}

// stageGateTimedOut returns true when AwaitingGate has been the
// stage's status for longer than gate_timeout_minutes. Uses
// all_prs_merged_at as the "AwaitingGate started at" anchor since
// AwaitingGate immediately follows AllPRsMerged in the lifecycle.
//
// Returns false on any malformed timestamp (failing-open here is
// the safer default — a parse hiccup must not auto-fail a stage).
func stageGateTimedOut(s activeStage) bool {
	if s.gateTimeoutMinutes <= 0 {
		return false
	}
	if s.allPRsMergedAt == "" {
		return false
	}
	t, err := store.ParseSQLiteTime(s.allPRsMergedAt)
	if err != nil {
		return false
	}
	deadline := t.Add(time.Duration(s.gateTimeoutMinutes) * time.Minute)
	return time.Now().UTC().After(deadline)
}

// evaluateGate parses the stage's gate config + gate type and runs
// it through the registry. Builds a stagegate.StageContext from
// the activeStage row.
func evaluateGate(ctx context.Context, db *sql.DB, registry *stagegate.Registry, s activeStage) (bool, string, error) {
	openedAt, _ := store.ParseSQLiteTime(s.openedAt)
	mergedAt, _ := store.ParseSQLiteTime(s.allPRsMergedAt)
	stageCtx := stagegate.StageContext{
		StageID:        s.id,
		ConvoyID:       s.convoyID,
		StageNum:       s.stageNum,
		Status:         s.status,
		GateType:       s.gateType,
		AllPRsMergedAt: mergedAt,
		OpenedAt:       openedAt,
		GateTimeoutMin: s.gateTimeoutMinutes,
	}
	// Wrap the stored gate_config_json in the registry's expected
	// {"type":..., "config":...} envelope before dispatch — the
	// stored shape is the per-leaf config object; the registry
	// dispatcher needs the type alongside.
	wrapped := struct {
		Type   string          `json:"type"`
		Config json.RawMessage `json:"config,omitempty"`
		// Pass-through "gates" key for compound gates that store
		// their children directly in gate_config_json.
		Gates json.RawMessage `json:"gates,omitempty"`
	}{
		Type: s.gateType,
	}
	cfg := json.RawMessage(s.gateConfigJSON)
	// If the stored config is itself a compound spec (has a "gates"
	// array), we forward those rather than wrapping in "config".
	var probe struct {
		Gates json.RawMessage `json:"gates"`
	}
	_ = json.Unmarshal(cfg, &probe)
	if len(probe.Gates) > 0 {
		wrapped.Gates = probe.Gates
	} else {
		wrapped.Config = cfg
	}
	specJSON, err := json.Marshal(wrapped)
	if err != nil {
		return false, "", fmt.Errorf("convoy-stage-watch: marshal gate spec: %w", err)
	}
	return registry.EvaluateGateConfig(ctx, db, stageCtx, specJSON, 0)
}

// emitGateTimeoutEscalation surfaces the gate-timeout to the
// operator via the standard mail path. Mirrors the
// stale-convoys-report shape so dashboards filter/group these the
// same way as other auto-escalations.
//
// Pattern P27: budget-gate the operator emit through
// RespectNotificationBudget. The wave-3-ζ tightening: previously the
// `allowed` return was discarded and SendMail ran unconditionally.
// StakesHigh always punches through today, so the bug was latent —
// but a future budget-semantics change (e.g. high-stakes routed
// through digest rather than always allowed) would silently re-enable
// emission. Gating on `allowed` makes the contract self-describing
// and surfaces the regression at compile time if RespectNotificationBudget
// ever changes signature.
//
// Fail-open on err so a transient SQLite glitch never silences a
// gate-timeout alert (a stuck stage is operator-actionable; over-
// emission is preferable to under-emission for high-stakes signals).
//
// Escalation message (D5.5 P3 ζ contract):
//   - Convoy ID + name (the human-readable handle the operator stored)
//   - Stage number + intent_text (so the operator knows WHICH stage
//     failed without dashboard click-through)
//   - Gate type + gate config JSON (so the operator can re-evaluate
//     the gate spec or operator-confirm to advance)
//   - How long the stage was in AwaitingGate before timeout
//   - Recommendation pointing at `force convoy show` + the two
//     resolution paths (re-evaluate gate config or operator-confirm)
func emitGateTimeoutEscalation(ctx context.Context, db *sql.DB, s activeStage, logger interface{ Printf(string, ...any) }) {
	convoyLabel := fmt.Sprintf("%d", s.convoyID)
	if s.convoyName != "" {
		convoyLabel = fmt.Sprintf("%d (%s)", s.convoyID, s.convoyName)
	}
	intentLine := s.intentText
	if intentLine == "" {
		intentLine = "(no intent_text recorded)"
	}

	// Compute how long the stage sat in AwaitingGate before the dog
	// flipped it to Failed. Fall back to "unknown" if all_prs_merged_at
	// is missing or malformed; the operator still gets the rest of the
	// message and the timeout alert isn't blocked by a parse hiccup.
	awaitingFor := "unknown"
	if t, err := store.ParseSQLiteTime(s.allPRsMergedAt); err == nil {
		awaitingFor = time.Since(t).Round(time.Minute).String()
	}

	subject := fmt.Sprintf("[STAGE GATE TIMEOUT] convoy=%d stage=%d gate=%s",
		s.convoyID, s.stageNum, s.gateType)
	body := fmt.Sprintf(
		"Stage %d of convoy %s has been AwaitingGate longer than its configured\n"+
			"gate_timeout_minutes=%d. The convoy-stage-watch dog has transitioned\n"+
			"the stage to Failed.\n\n"+
			"Stage intent: %s\n"+
			"Gate type:    %s\n"+
			"Gate config:  %s\n"+
			"Awaiting for: %s (timeout: %d minutes)\n\n"+
			"Recommendation:\n"+
			"  Re-evaluate the gate config (the threshold may be too tight, the\n"+
			"  metric query may be stale, or the soak window may be unrealistic),\n"+
			"  or operator-confirm to advance the stage manually.\n\n"+
			"Inspect: force convoy show %d\n",
		s.stageNum, convoyLabel, s.gateTimeoutMinutes,
		intentLine, s.gateType, s.gateConfigJSON,
		awaitingFor, s.gateTimeoutMinutes, s.convoyID)

	allowed, budgetErr := gateTimeoutBudgetCheck(
		ctx, db, "operator", "inquisitor", "email", "{}",
		store.StakesHigh,
	)
	if budgetErr != nil {
		// Fail-open: a budget-table glitch must not silence a
		// high-stakes timeout alert. Log + emit anyway.
		logger.Printf("convoy-stage-watch: gate-timeout budget check failed (fail-open): %v", budgetErr)
		allowed = true
	}
	if !allowed {
		// Budget exhausted (StakesHigh punches through today, so this
		// only fires on a 0-cap config row). Log so the operator can
		// see the suppression in stdout when it matters.
		logger.Printf("convoy-stage-watch: gate-timeout escalation suppressed by notification budget (convoy=%d stage=%d)",
			s.convoyID, s.stageNum)
		return
	}
	gateTimeoutSendMail(db, "inquisitor", "operator", subject, body, 0, store.MailTypeAlert)
}

// onStageTransition is the side-effect helper invoked after every state
// flip the dog performs. Two concerns:
//
//   1. notify-after Slack ping — short, factual; debounced via SystemConfig
//      so a re-tick doesn't re-ping. The ping is best-effort: a missing
//      `notify-after` binary degrades to a no-op.
//
//   2. AuditLog row — durable record of "convoy-stage-watch-dog moved
//      stage X from Y to Z because <reason>". The dashboard's per-stage
//      history pane reads these via store.ListStageAuditLog.
//
// Debounce key shape:
//
//   stage_transition_notified_<convoy>_<stage>_<new_status>
//
// We key on the post-transition status because each transition has a
// unique `new_status` (the dog only moves forward in the linear lifecycle).
// A re-evaluation that would re-emit the same transition writes the same
// key — no second ping.
//
// The audit log itself is NOT debounced: every actual state flip records.
// Debounce only applies to the operator-facing Slack ping.
func onStageTransition(ctx context.Context, db *sql.DB, s activeStage, oldStatus, newStatus, reason string,
	logger interface{ Printf(string, ...any) }) {
	// 1. AuditLog row — always record. The dog's actor identity is the
	//    string the dashboard surfaces in the per-stage history pane.
	if alErr := store.LogStageAudit(db, "convoy-stage-watch-dog", store.AuditActionStageAutoAdvance,
		s.convoyID, s.stageNum, oldStatus, newStatus, reason, reason); alErr != nil {
		logger.Printf("convoy-stage-watch: stage audit insert failed (convoy=%d stage=%d): %v",
			s.convoyID, s.stageNum, alErr)
		// Non-fatal: the state transition itself is durable. We log and
		// proceed to the notify path so a transient AuditLog write
		// failure doesn't also silence the operator ping.
	}

	// 2. notify-after ping — debounced per (convoy, stage, new_status).
	debounceKey := fmt.Sprintf("stage_transition_notified_%d_%d_%s", s.convoyID, s.stageNum, newStatus)
	if existing := store.GetConfig(db, debounceKey, ""); existing != "" {
		// Already pinged for this transition. Skip.
		return
	}

	gateLabel := s.gateType
	if gateLabel == "" {
		gateLabel = "(none)"
	}
	intent := s.intentText
	if intent == "" {
		intent = "(no intent)"
	}
	convoyLabel := fmt.Sprintf("#%d", s.convoyID)
	if s.convoyName != "" {
		convoyLabel = fmt.Sprintf("#%d \"%s\"", s.convoyID, s.convoyName)
	}
	label := fmt.Sprintf("Convoy %s stage %d \"%s\": %s → %s (gate: %s)",
		convoyLabel, s.stageNum, intent, oldStatus, newStatus, gateLabel)

	if nErr := stageTransitionNotifyFn(ctx, label); nErr != nil {
		logger.Printf("convoy-stage-watch: notify-after stage-transition ping failed (convoy=%d stage=%d): %v",
			s.convoyID, s.stageNum, nErr)
		// Don't write the debounce key on failure — we want a retry on
		// the next tick. notify-after is best-effort but a transient
		// failure shouldn't permanently suppress the ping.
		return
	}
	store.SetConfig(db, debounceKey, time.Now().UTC().Format(time.RFC3339))
}

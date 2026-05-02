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
)

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
type activeStage struct {
	id                 int
	convoyID           int
	stageNum           int
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

	rows, err := db.Query(`
		SELECT id, convoy_id, stage_num, status,
		       gate_type, IFNULL(gate_config_json, '{}'),
		       gate_timeout_minutes,
		       IFNULL(opened_at, ''), IFNULL(all_prs_merged_at, '')
		FROM ConvoyStages
		WHERE status IN ('Open', 'AllPRsMerged', 'AwaitingGate')`)
	if err != nil {
		return fmt.Errorf("convoy-stage-watch: query active stages: %w", err)
	}
	defer rows.Close()

	var stages []activeStage
	for rows.Next() {
		var s activeStage
		var gateType sql.NullString
		if sErr := rows.Scan(&s.id, &s.convoyID, &s.stageNum, &s.status,
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
			}

		case store.StageStatusAllPRsMerged:
			if aErr := store.AdvanceStage(db, s.id, store.StageStatusAwaitingGate); aErr != nil {
				logger.Printf("convoy-stage-watch: stage %d AllPRsMerged→AwaitingGate failed: %v", s.id, aErr)
				continue
			}
			transitioned++
			logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) AllPRsMerged→AwaitingGate", s.id, s.convoyID, s.stageNum)

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
				emitGateTimeoutEscalation(db, s, logger)
				logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) AwaitingGate→Failed (gate timeout %dmin exceeded)",
					s.id, s.convoyID, s.stageNum, s.gateTimeoutMinutes)
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
			} else {
				if aErr := store.AdvanceStage(db, s.id, store.StageStatusFailed); aErr != nil {
					logger.Printf("convoy-stage-watch: stage %d AwaitingGate→Failed failed: %v", s.id, aErr)
					continue
				}
				transitioned++
				logger.Printf("convoy-stage-watch: stage %d (convoy=%d num=%d) AwaitingGate→Failed (gate %q: %s)",
					s.id, s.convoyID, s.stageNum, s.gateType, reason)
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
// Pattern P27: budget-gate the operator emit. On allowed=false the
// helper has already drop/digested per the configured budget;
// fail-open on err so a transient SQLite glitch never silences a
// gate-timeout alert.
func emitGateTimeoutEscalation(db *sql.DB, s activeStage, logger interface{ Printf(string, ...any) }) {
	subject := fmt.Sprintf("[STAGE GATE TIMEOUT] convoy=%d stage=%d", s.convoyID, s.stageNum)
	body := fmt.Sprintf(
		"Stage %d of convoy %d has been AwaitingGate longer than its configured gate_timeout_minutes=%d.\n"+
			"Gate type: %s\nThe convoy-stage-watch dog has transitioned the stage to Failed.\n\n"+
			"Inspect: force convoy show %d\n",
		s.stageNum, s.convoyID, s.gateTimeoutMinutes, s.gateType, s.convoyID)
	if allowed, _ := store.RespectNotificationBudget(
		context.Background(), db, "operator", "inquisitor", "email", "{}",
		store.StakesHigh,
	); !allowed {
		// budget exhausted (StakesHigh always punches through, so
		// this branch only fires on a real config-set 0-cap row).
	} else {
		_ = allowed
	}
	store.SendMail(db, "inquisitor", "operator", subject, body, 0, store.MailTypeAlert)
}

package agents

// D3 fix-loop-1 / γ1 — runConvoyReview produces a cycle row each pass.
//
// Tests cover:
//   - Clean pass: cycle row written, verdict="clean", outcomes mark spec results
//   - Needs_work pass: cycle row carries spawned fix-task IDs
//   - Frozen-spec invariant: spec mutated mid-pass does NOT change the cycle's
//     stored snapshot
//   - Spec failure on a clean LLM result still produces needs_work + spec
//     fix-task for the failing AT

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestRunConvoyReview_CleanPass_WritesCycleRow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Completed', 'add rate limit patterns', ?, 5, datetime('now'))`, convoyID)

	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 5001, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (5001, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	cycles, err := store.ListCyclesForConvoy(db, convoyID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle row, got %d", len(cycles))
	}
	if cycles[0].CycleNumber != 1 {
		t.Errorf("cycle_number = %d, want 1", cycles[0].CycleNumber)
	}
	if cycles[0].CycleCompletedAt == "" {
		t.Errorf("cycle_completed_at not stamped after clean pass")
	}
	if !strings.Contains(cycles[0].OutcomesJSON, `"verdict":"clean"`) {
		t.Errorf("verdict not folded into outcomes_json: %s", cycles[0].OutcomesJSON)
	}
}

func TestRunConvoyReview_NeedsWork_RecordsSpawnedTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	stubConvoyReviewLLM(t, convoyReviewResult{
		Status: "needs_work",
		Findings: []convoyReviewFinding{
			{Type: "regression", Description: "Flusher removed", Fix: "Restore Flusher in handler", Repo: "api"},
		},
	})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 5002, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (5002, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	cycles, _ := store.ListCyclesForConvoy(db, convoyID)
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(cycles))
	}
	if !strings.Contains(cycles[0].OutcomesJSON, `"verdict":"needs_work"`) {
		t.Errorf("verdict = %s, want needs_work folded into outcomes", cycles[0].OutcomesJSON)
	}

	// fix_tasks_spawned_json should have at least one ID — the test stub
	// path adds CodeEdit task rows under the bounty.
	var ids []int
	if err := json.Unmarshal([]byte(cycles[0].FixTasksSpawnedJSON), &ids); err != nil {
		t.Fatalf("parse fix_tasks_spawned_json: %v (raw=%s)", err, cycles[0].FixTasksSpawnedJSON)
	}
	if len(ids) == 0 {
		t.Errorf("fix_tasks_spawned_json empty after needs_work — expected ≥1 task id (raw=%s)",
			cycles[0].FixTasksSpawnedJSON)
	}
}

func TestRunConvoyReview_FrozenSpecAcrossMidPassMutation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	specV1 := `{"ats":[{"id":"AT-1","description":"x"}]}`
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, specV1, convoyID)

	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 5003, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (5003, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// Mutate the spec AFTER the pass completes — cycle 1 must keep V1.
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		`{"ats":[{"id":"AT-1"},{"id":"AT-2"}]}`, convoyID)

	cycles, _ := store.ListCyclesForConvoy(db, convoyID)
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(cycles))
	}
	if cycles[0].SpecVersionAtStart != specV1 {
		t.Errorf("frozen spec drifted: got %q want %q",
			cycles[0].SpecVersionAtStart, specV1)
	}
}

func TestRunConvoyReview_SpecATFailure_PromotesToNeedsWork(t *testing.T) {
	// Even when the LLM returns clean, a failing AT in the frozen spec
	// must promote the cycle verdict to needs_work and spawn a fix task
	// against the named AT.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// AT-1 has a substring evaluator that will NOT match an empty diff.
	spec := `{"ats":[{"id":"AT-1","description":"must contain marker","evaluator":"substring:RATE_LIMIT_MARKER"}]}`
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, spec, convoyID)

	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 5004, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (5004, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	cycles, _ := store.ListCyclesForConvoy(db, convoyID)
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(cycles))
	}
	verdictMatch := strings.Contains(cycles[0].OutcomesJSON, `"verdict":"needs_work"`)
	if !verdictMatch {
		t.Errorf("expected needs_work after spec AT failure (LLM was clean): outcomes=%s",
			cycles[0].OutcomesJSON)
	}
	// AT-1 result must appear in outcomes
	if !strings.Contains(cycles[0].OutcomesJSON, `"AT-1"`) {
		t.Errorf("AT-1 outcome missing from outcomes_json: %s", cycles[0].OutcomesJSON)
	}
}

func TestRunConvoyReview_DeprecatedAT_Skipped(t *testing.T) {
	// AT-1 is in deprecated[]; evaluator would fail, but the cycle should
	// skip the AT and emit a [CONVOY REVIEW] Skipped deprecated event.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	spec := `{"ats":[{"id":"AT-1","description":"x","evaluator":"substring:NEVER_MATCHES"}],"deprecated":[{"at_id":"AT-1","removed_at":"2026-01-01","removed_by_email":"op@x","rationale":"twenty chars exactly here yes","removal_kind":"mistake"}]}`
	db.Exec(`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`, spec, convoyID)

	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 5005, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (5005, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	cycles, _ := store.ListCyclesForConvoy(db, convoyID)
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(cycles))
	}
	if !strings.Contains(cycles[0].OutcomesJSON, `"AT-1":"skipped_deprecated"`) {
		t.Errorf("expected AT-1 to be skipped_deprecated; outcomes=%s", cycles[0].OutcomesJSON)
	}
	// Verdict should be clean since the deprecated AT does not block.
	if !strings.Contains(cycles[0].OutcomesJSON, `"verdict":"clean"`) {
		t.Errorf("verdict not clean despite all ATs deprecated/skipped: %s", cycles[0].OutcomesJSON)
	}
}

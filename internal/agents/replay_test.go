package agents

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestReplayDecision covers 6B.7 invariants:
//   - Happy path: ReplayResults row inserted, response synthesised,
//     no live-state mutation on the original transcript / convoy / task.
//   - Decision-changed comparison fires when the original response
//     differs from the replayed response head.
//   - LoadReplayResult round-trips a stored row + hydrates original.
//   - Bogus event kind is rejected.
//   - Idempotence: replaying the same decision twice produces two
//     ReplayResults rows (each replay is its own audit event).
func TestReplayDecision(t *testing.T) {
	t.Run("happy_path_inserts_replay_row_no_live_mutation", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()

		// Seed a Captain ruling transcript.
		res, err := db.Exec(
			`INSERT INTO LLMCallTranscripts
			   (task_id, agent, prompt_version, call_started_at, call_completed_at,
			    system_prompt, user_prompt, response_text, cost_usd)
			 VALUES (1, 'captain', 'v18', '2026-01-01 10:00:00', '2026-01-01 10:00:30',
			         'sys', 'usr', 'APPROVE: original ruling text', 0.01)`)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		evID, _ := res.LastInsertId()

		// Snapshot live state pre-replay.
		var origResp, origCompleted string
		db.QueryRow(`SELECT response_text, call_completed_at FROM LLMCallTranscripts WHERE id=?`, evID).
			Scan(&origResp, &origCompleted)

		rr, err := ReplayDecision(context.Background(), db, "captain_ruling", evID, "v19", "op@x")
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if rr.ID == 0 {
			t.Fatal("expected non-zero replay id")
		}
		if rr.OriginalEventID != evID {
			t.Errorf("orig id mismatch: %d", rr.OriginalEventID)
		}
		if rr.ReplayResponse == "" {
			t.Errorf("expected non-empty replay response")
		}

		// Verify the original row was NOT mutated.
		var afterResp, afterCompleted string
		db.QueryRow(`SELECT response_text, call_completed_at FROM LLMCallTranscripts WHERE id=?`, evID).
			Scan(&afterResp, &afterCompleted)
		if afterResp != origResp || afterCompleted != origCompleted {
			t.Errorf("REPLAY mutated original row: was (%q, %q), now (%q, %q)",
				origResp, origCompleted, afterResp, afterCompleted)
		}

		// ReplayResults row exists
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM ReplayResults WHERE id=?`, rr.ID).Scan(&n)
		if n != 1 {
			t.Errorf("ReplayResults row not persisted")
		}

		// Replay's own LLMCallTranscripts row exists, agent suffix=-replay
		var replayAgent string
		db.QueryRow(`SELECT agent FROM LLMCallTranscripts WHERE agent LIKE '%-replay' LIMIT 1`).Scan(&replayAgent)
		if !strings.HasSuffix(replayAgent, "-replay") {
			t.Errorf("expected replay transcript agent suffix; got %q", replayAgent)
		}
	})

	t.Run("decision_changed_when_responses_differ", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		res, _ := db.Exec(`INSERT INTO LLMCallTranscripts
			(task_id, agent, prompt_version, call_started_at,
			 system_prompt, user_prompt, response_text)
			VALUES (1, 'captain', 'v18', '2026-01-01 10:00:00',
			        'sys', 'usr', 'completely different original text that wont match the deterministic synth')`)
		evID, _ := res.LastInsertId()
		rr, _ := ReplayDecision(context.Background(), db, "captain_ruling", evID, "v19", "op@x")
		if !rr.DecisionChanged {
			t.Errorf("expected decision_changed=true when responses differ; got %s vs %s",
				rr.OriginalResponse, rr.ReplayResponse)
		}
	})

	t.Run("invalid_kind_rejected", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, err := ReplayDecision(context.Background(), db, "wat", 1, "v1", "op")
		if err == nil {
			t.Fatal("expected error on bogus kind")
		}
	})

	t.Run("nil_db_errors", func(t *testing.T) {
		_, err := ReplayDecision(context.Background(), nil, "captain_ruling", 1, "v1", "op")
		if err == nil {
			t.Fatal("expected error on nil db")
		}
	})

	t.Run("idempotence_each_replay_is_own_audit", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		res, _ := db.Exec(`INSERT INTO LLMCallTranscripts
			(task_id, agent, prompt_version, call_started_at,
			 system_prompt, user_prompt, response_text)
			VALUES (1, 'captain', 'v18', '2026-01-01 10:00:00', 's', 'u', 'r')`)
		evID, _ := res.LastInsertId()

		for i := 0; i < 2; i++ {
			if _, err := ReplayDecision(context.Background(), db, "captain_ruling", evID, "v19", "op"); err != nil {
				t.Fatalf("replay %d: %v", i, err)
			}
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM ReplayResults WHERE original_event_id=?`, evID).Scan(&n)
		if n != 2 {
			t.Errorf("expected 2 ReplayResults rows, got %d", n)
		}
	})

	t.Run("no_live_mutation_in_other_tables", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (1, 'c1', 'Active')`)
		_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES (1, 'F', 'p', 'Pending', 1)`)
		res, _ := db.Exec(`INSERT INTO LLMCallTranscripts
			(task_id, agent, prompt_version, call_started_at,
			 system_prompt, user_prompt, response_text)
			VALUES (1, 'captain', 'v18', '2026-01-01 10:00:00', 's', 'u', 'r')`)
		evID, _ := res.LastInsertId()

		// Snapshot all live tables that replay should NOT touch.
		var bountyStatus, convoyStatus string
		db.QueryRow(`SELECT status FROM BountyBoard WHERE id=1`).Scan(&bountyStatus)
		db.QueryRow(`SELECT status FROM Convoys WHERE id=1`).Scan(&convoyStatus)

		var preCycleCount, preEscalationCount, preBriefingCount int
		db.QueryRow(`SELECT COUNT(*) FROM ConvoyReviewCycles`).Scan(&preCycleCount)
		db.QueryRow(`SELECT COUNT(*) FROM Escalations`).Scan(&preEscalationCount)
		db.QueryRow(`SELECT COUNT(*) FROM BriefingRenders`).Scan(&preBriefingCount)

		_, err := ReplayDecision(context.Background(), db, "captain_ruling", evID, "v19", "op")
		if err != nil {
			t.Fatalf("replay: %v", err)
		}

		var bountyAfter, convoyAfter string
		db.QueryRow(`SELECT status FROM BountyBoard WHERE id=1`).Scan(&bountyAfter)
		db.QueryRow(`SELECT status FROM Convoys WHERE id=1`).Scan(&convoyAfter)
		if bountyAfter != bountyStatus {
			t.Errorf("REPLAY mutated BountyBoard.status: %q → %q", bountyStatus, bountyAfter)
		}
		if convoyAfter != convoyStatus {
			t.Errorf("REPLAY mutated Convoys.status: %q → %q", convoyStatus, convoyAfter)
		}
		var postCycleCount, postEscalationCount, postBriefingCount int
		db.QueryRow(`SELECT COUNT(*) FROM ConvoyReviewCycles`).Scan(&postCycleCount)
		db.QueryRow(`SELECT COUNT(*) FROM Escalations`).Scan(&postEscalationCount)
		db.QueryRow(`SELECT COUNT(*) FROM BriefingRenders`).Scan(&postBriefingCount)
		if postCycleCount != preCycleCount {
			t.Errorf("REPLAY inserted ConvoyReviewCycles row: %d → %d", preCycleCount, postCycleCount)
		}
		if postEscalationCount != preEscalationCount {
			t.Errorf("REPLAY inserted Escalation: %d → %d", preEscalationCount, postEscalationCount)
		}
		if postBriefingCount != preBriefingCount {
			t.Errorf("REPLAY inserted BriefingRenders: %d → %d", preBriefingCount, postBriefingCount)
		}
	})
}

func TestLoadReplayResult(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, _ := db.Exec(`INSERT INTO LLMCallTranscripts
		(task_id, agent, prompt_version, call_started_at,
		 system_prompt, user_prompt, response_text)
		VALUES (1, 'captain', 'v18', '2026-01-01 10:00:00', 's', 'u', 'r')`)
	evID, _ := res.LastInsertId()

	rr, err := ReplayDecision(context.Background(), db, "captain_ruling", evID, "v19", "op")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	got, err := LoadReplayResult(context.Background(), db, rr.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ID != rr.ID || got.OriginalEventID != evID {
		t.Errorf("load mismatch: %+v", got)
	}
	if got.OriginalResponse != "r" {
		t.Errorf("expected original 'r', got %q", got.OriginalResponse)
	}
}

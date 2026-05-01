package store

import (
	"context"
	"testing"
)

// TestDrillUnifiedEventStream covers 6B.3 invariants:
//   - Convoy event stream pulls TaskHistory + LLMCallTranscripts +
//     GitOperationLog + ConvoyReviewCycles + OperatorEventAnnotations.
//   - Events are ordered chronologically (ASC).
//   - Pagination works (limit + offset).
//   - Empty convoy returns no rows, no error.
func TestDrillUnifiedEventStream(t *testing.T) {
	t.Run("happy_path_pulls_from_all_sources", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()

		// Seed convoy + a task + LLM call + git op + cycle + annotation.
		_, err := db.Exec(
			`INSERT INTO Convoys (id, name, status) VALUES (47, 'C-47', 'Active')`)
		if err != nil {
			t.Fatalf("seed convoy: %v", err)
		}
		_, err = db.Exec(
			`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES (101, 'astromech', 'p', 'Pending', 47)`)
		if err != nil {
			t.Fatalf("seed bounty: %v", err)
		}
		_, err = db.Exec(
			`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome, created_at)
			 VALUES (101, 1, 'astromech', 's1', 'out', 'Completed', '2026-01-01 10:00:00')`)
		if err != nil {
			t.Fatalf("seed history: %v", err)
		}
		_, err = db.Exec(
			`INSERT INTO LLMCallTranscripts
			   (task_id, agent, prompt_version, call_started_at, call_completed_at,
			    system_prompt, user_prompt, response_text)
			 VALUES (101, 'captain', 'v1', '2026-01-01 10:01:00', '2026-01-01 10:01:30',
			         'sys', 'usr', 'resp')`)
		if err != nil {
			t.Fatalf("seed llm: %v", err)
		}
		_, err = db.Exec(
			`INSERT INTO GitOperationLog
			   (task_id, convoy_id, repo, operation, args_json, started_at, exit_code)
			 VALUES (101, 47, 'r', 'fetch', '["fetch","origin"]', '2026-01-01 10:02:00', 0)`)
		if err != nil {
			t.Fatalf("seed git: %v", err)
		}
		_, err = db.Exec(
			`INSERT INTO ConvoyReviewCycles
			   (convoy_id, cycle_number, spec_version_at_start, cycle_started_at)
			 VALUES (47, 1, 'v1', '2026-01-01 10:03:00')`)
		if err != nil {
			t.Fatalf("seed cycle: %v", err)
		}
		_, err = db.Exec(
			`INSERT INTO OperatorEventAnnotations
			   (operator_email, event_kind, event_ref, note_text, flag, noted_at)
			 VALUES ('op', 'llm_call', '101', 'this is interesting', 'interesting', '2026-01-01 10:04:00')`)
		if err != nil {
			t.Fatalf("seed annotation: %v", err)
		}

		evs, err := LoadConvoyDrillEvents(context.Background(), db, 47, 100, 0)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(evs) < 4 {
			t.Fatalf("expected >=4 events; got %d (%v)", len(evs), evs)
		}

		// Verify chronological order
		for i := 1; i < len(evs); i++ {
			if evs[i].Timestamp < evs[i-1].Timestamp {
				t.Errorf("events out of order: [%d]=%s < [%d]=%s", i, evs[i].Timestamp, i-1, evs[i-1].Timestamp)
			}
		}

		// Verify each kind appeared
		kinds := map[string]bool{}
		for _, e := range evs {
			kinds[e.Kind] = true
		}
		for _, want := range []string{"task_transition", "llm_call", "git_op", "cycle"} {
			if !kinds[want] {
				t.Errorf("missing event kind %q in stream; saw %v", want, kinds)
			}
		}
	})

	t.Run("invalid_convoy_id_errors", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		if _, err := LoadConvoyDrillEvents(context.Background(), db, 0, 10, 0); err == nil {
			t.Fatal("expected error on convoy_id=0")
		}
		if _, err := LoadConvoyDrillEvents(context.Background(), db, -1, 10, 0); err == nil {
			t.Fatal("expected error on negative convoy_id")
		}
	})

	t.Run("empty_convoy_returns_empty_slice", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		evs, err := LoadConvoyDrillEvents(context.Background(), db, 999, 10, 0)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(evs) != 0 {
			t.Errorf("expected 0, got %d", len(evs))
		}
	})

	t.Run("pagination", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(
			`INSERT INTO Convoys (id, name, status) VALUES (1, 'p1', 'Active')`)
		_, _ = db.Exec(
			`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES (1, 'a', 'p', 'P', 1)`)
		// Seed 5 LLM calls
		for i := 0; i < 5; i++ {
			_, err := db.Exec(
				`INSERT INTO LLMCallTranscripts
				   (task_id, agent, prompt_version, call_started_at,
				    system_prompt, user_prompt)
				 VALUES (1, 'captain', 'v1', ?, 's', 'u')`,
				"2026-01-01 1"+string(rune('0'+i))+":00:00")
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		page1, _ := LoadConvoyDrillEvents(context.Background(), db, 1, 2, 0)
		page2, _ := LoadConvoyDrillEvents(context.Background(), db, 1, 2, 2)
		if len(page1) != 2 || len(page2) != 2 {
			t.Fatalf("paginate: page1=%d page2=%d", len(page1), len(page2))
		}
		if page1[0].Timestamp == page2[0].Timestamp {
			t.Errorf("pages overlapped at first slot")
		}
	})
}

func TestDrillTaskEvents(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	_, _ = db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome, created_at)
		VALUES (33, 1, 'astromech', 's', 'o', 'Completed', '2026-01-01 09:00:00')`)
	_, _ = db.Exec(`INSERT INTO LLMCallTranscripts
		(task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt)
		VALUES (33, 'captain', 'v1', '2026-01-01 09:01:00', 's', 'u')`)
	evs, err := LoadTaskDrillEvents(context.Background(), db, 33, 100, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(evs) < 2 {
		t.Fatalf("expected >=2 events, got %d", len(evs))
	}
}

func TestDrillEventDetails(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = db.Exec(`INSERT INTO LLMCallTranscripts
		(id, task_id, agent, prompt_version, call_started_at,
		 system_prompt, user_prompt, response_text)
		VALUES (777, 1, 'captain', 'v1', '2026-01-01 10:00:00', 'sys-p', 'usr-p', 'resp')`)

	body, err := LoadEventDetails(context.Background(), db, "llm_call", 777)
	if err != nil {
		t.Fatalf("load llm: %v", err)
	}
	if body["agent"] != "captain" || body["system_prompt"] != "sys-p" {
		t.Errorf("body: %+v", body)
	}

	if _, err := LoadEventDetails(context.Background(), db, "wat", 1); err == nil {
		t.Fatal("expected error on bogus kind")
	}
}

func TestDrillSpend(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = db.Exec(`INSERT INTO Convoys (id, name) VALUES (5, 'c5')`)
	_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES (50, 'a', 'p', 'P', 5)`)
	_, _ = db.Exec(`INSERT INTO LLMCallTranscripts
		(task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt, cost_usd, input_tokens, output_tokens)
		VALUES (50, 'captain', 'v1', '2026-01-01 10:00:00', 's', 'u', 0.01, 100, 50),
		       (50, 'captain', 'v1', '2026-01-01 10:01:00', 's', 'u', 0.02, 100, 50)`)
	rollup, err := LoadConvoyDrillSpend(context.Background(), db, 5)
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if len(rollup) != 1 {
		t.Fatalf("expected 1 (task,agent) bucket, got %d", len(rollup))
	}
	if rollup[0].Calls != 2 {
		t.Errorf("expected 2 calls, got %d", rollup[0].Calls)
	}
	if rollup[0].CostUSD < 0.029 || rollup[0].CostUSD > 0.031 {
		t.Errorf("expected ~0.03, got %v", rollup[0].CostUSD)
	}
}

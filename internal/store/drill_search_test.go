package store

import (
	"context"
	"strings"
	"testing"
)

// TestDrillSearch covers 6B.6 invariants:
//   - fts5 indexes are built lazily; subsequent INSERTs into source
//     tables propagate to the fts5 indexes via triggers.
//   - SearchDrill returns hits across multiple sources for a single
//     query string.
//   - Convoy-scoped search only returns hits associated with that
//     convoy.
//   - Empty query returns no rows + no error.
//   - Bogus fts5 syntax doesn't crash the search; it returns an
//     empty (or partial) result set.
func TestDrillSearch(t *testing.T) {
	t.Run("indexes_propagate_via_triggers_global_search", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()

		// Insert source-table content; the triggers feed the fts5
		// virtual tables automatically.
		_, err := db.Exec(`INSERT INTO LLMCallTranscripts
			(task_id, agent, prompt_version, call_started_at,
			 system_prompt, user_prompt, response_text)
			VALUES (1, 'captain', 'v1', '2026-01-01 10:00:00',
			        'system', 'rate limit hit on the upstream API', 'will retry')`)
		if err != nil {
			t.Fatalf("seed llm: %v", err)
		}
		_, err = db.Exec(`INSERT INTO BountyBoard
			(id, type, payload, status, convoy_id) VALUES
			(1, 'Feature', 'investigate rate limit pattern in fleet', 'P', 5)`)
		if err != nil {
			t.Fatalf("seed bounty: %v", err)
		}
		_, err = db.Exec(`INSERT INTO BriefingRenders
			(decision_id, decision_kind, briefing_text)
			VALUES (1, 'captain_ruling', 'reviewed rate limit policy details')`)
		if err != nil {
			t.Fatalf("seed briefing: %v", err)
		}

		results, err := SearchDrill(context.Background(), db, "rate limit", "global", 0, 50)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) < 2 {
			t.Fatalf("expected hits across sources, got %d (%v)", len(results), results)
		}
		// Verify multiple kinds appeared
		kinds := map[string]bool{}
		for _, r := range results {
			kinds[r.Kind] = true
		}
		if !kinds["llm_call"] || !kinds["task"] || !kinds["briefing"] {
			t.Errorf("expected hits across llm_call+task+briefing; got %v", kinds)
		}
	})

	t.Run("convoy_scope_filters_correctly", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()

		_, _ = db.Exec(`INSERT INTO Convoys (id, name) VALUES (5, 'c5'), (6, 'c6')`)
		// Two tasks: one in convoy 5, one in convoy 6.
		_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES
			(1, 'F', 'uniquekeyword in convoy 5 task', 'P', 5),
			(2, 'F', 'uniquekeyword in convoy 6 task', 'P', 6)`)

		results, _ := SearchDrill(context.Background(), db, "uniquekeyword", "convoy", 5, 50)
		// Should only see convoy 5 task — but the task fts5 SCAN
		// may pull both rowid; the convoy join filters to convoy 5.
		var foundConvoy5, foundConvoy6 bool
		for _, r := range results {
			if r.Kind == "task" && r.RefID == 1 {
				foundConvoy5 = true
			}
			if r.Kind == "task" && r.RefID == 2 {
				foundConvoy6 = true
			}
		}
		if !foundConvoy5 {
			t.Errorf("expected convoy-5 task hit")
		}
		if foundConvoy6 {
			t.Errorf("convoy-scope leaked convoy-6 task")
		}
	})

	t.Run("empty_query_returns_nil", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		results, err := SearchDrill(context.Background(), db, "", "global", 0, 10)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0, got %d", len(results))
		}
	})

	t.Run("bogus_fts5_syntax_no_crash", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		// Unmatched paren is invalid fts5 — must not panic / error.
		results, err := SearchDrill(context.Background(), db, `"((( unmatched`, "global", 0, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = results // may be empty but must not have crashed
	})

	t.Run("idempotent_ensure", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		// EnsureDrillFTS5 should be safe to call twice
		if err := EnsureDrillFTS5(db); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if err := EnsureDrillFTS5(db); err != nil {
			t.Fatalf("re-ensure: %v", err)
		}
	})

	t.Run("index_picks_up_updates", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO LLMCallTranscripts
			(id, task_id, agent, prompt_version, call_started_at,
			 system_prompt, user_prompt, response_text)
			VALUES (1, 1, 'captain', 'v1', '2026-01-01 10:00:00',
			        'sys', 'before', 'before-resp')`)
		_, _ = db.Exec(`UPDATE LLMCallTranscripts SET user_prompt='afterkeyword' WHERE id=1`)

		results, _ := SearchDrill(context.Background(), db, "afterkeyword", "global", 0, 10)
		var found bool
		for _, r := range results {
			if r.Kind == "llm_call" && r.RefID == 1 {
				found = true
			}
		}
		if !found {
			t.Errorf("UPDATE not propagated to fts5: %v", results)
		}
	})

	t.Run("snippet_carries_match_markers", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO LLMCallTranscripts
			(task_id, agent, prompt_version, call_started_at,
			 system_prompt, user_prompt, response_text)
			VALUES (1, 'captain', 'v1', '2026-01-01 10:00:00',
			        'sys', 'this contains the magicword in context', 'r')`)
		results, _ := SearchDrill(context.Background(), db, "magicword", "global", 0, 10)
		if len(results) == 0 {
			t.Fatal("expected hit")
		}
		if !strings.Contains(results[0].Snippet, "<<") || !strings.Contains(results[0].Snippet, ">>") {
			t.Errorf("snippet missing match markers: %q", results[0].Snippet)
		}
	})
}

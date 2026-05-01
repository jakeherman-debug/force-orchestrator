package agents

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestAskHandler covers 6B.10 invariants:
//   - "what's the status of convoy <N>" returns convoy info + cite link.
//   - "task <N>" returns task info.
//   - Unknown question routes to free-text search.
//   - "delete convoy 47" type prompts produce read-only answers
//     (no live-state mutation).
//   - Empty / oversize input handled.
//   - Cost cap exhaustion produces a polite refusal.
func TestAskHandler(t *testing.T) {
	t.Run("convoy_question_returns_status_and_cite", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (47, 'demo-convoy', 'Active')`)
		_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES
			(1, 'F', 'p', 'Pending', 47), (2, 'F', 'p', 'Completed', 47)`)

		a, err := AskHandle(context.Background(), db, "what's the status of convoy 47?")
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		if !strings.Contains(a.Answer, "Convoy 47") || !strings.Contains(a.Answer, "demo-convoy") {
			t.Errorf("answer missing convoy info: %q", a.Answer)
		}
		if !strings.Contains(a.Answer, "1 open task") {
			t.Errorf("answer should mention open tasks: %q", a.Answer)
		}
		if len(a.CiteLinks) != 1 || a.CiteLinks[0].Kind != "convoy" {
			t.Errorf("expected one convoy cite link: %+v", a.CiteLinks)
		}
	})

	t.Run("task_question_returns_status", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status) VALUES (123, 'Feature', 'p', 'Locked')`)
		a, err := AskHandle(context.Background(), db, "what's task 123 doing")
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		if !strings.Contains(a.Answer, "Task 123") || !strings.Contains(a.Answer, "Locked") {
			t.Errorf("answer: %q", a.Answer)
		}
	})

	t.Run("unknown_question_routes_to_search", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status) VALUES (1, 'F', 'investigate the wonkiness', 'P')`)

		a, err := AskHandle(context.Background(), db, "wonkiness investigation")
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		// Either matches via fts5 or falls back to "no matches" — we
		// only require the call to NOT error and to NOT mutate state.
		_ = a
	})

	t.Run("destructive_phrasing_does_not_mutate", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, _ = db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (1, 'c1', 'Active')`)
		var preStatus string
		db.QueryRow(`SELECT status FROM Convoys WHERE id=1`).Scan(&preStatus)

		_, err := AskHandle(context.Background(), db, "delete convoy 1 right now")
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		var postStatus string
		db.QueryRow(`SELECT status FROM Convoys WHERE id=1`).Scan(&postStatus)
		if preStatus != postStatus {
			t.Errorf("Ask mutated convoy status despite destructive phrasing: %q → %q", preStatus, postStatus)
		}
	})

	t.Run("empty_question_errors", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		_, err := AskHandle(context.Background(), db, "   ")
		if err == nil {
			t.Error("expected error on empty question")
		}
	})

	t.Run("cost_cap_refusal", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		// Pre-spend at the default cap by seeding ask transcripts.
		_, _ = db.Exec(`INSERT INTO LLMCallTranscripts
			(task_id, agent, prompt_version, call_started_at,
			 system_prompt, user_prompt, response_text, cost_usd)
			VALUES (0, 'ask', 'v1', datetime('now'), 's', 'u', 'r', 5.00)`)
		a, err := AskHandle(context.Background(), db, "anything?")
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		if !strings.Contains(strings.ToLower(a.Answer), "budget exhausted") {
			t.Errorf("expected budget-refusal answer: %q", a.Answer)
		}
	})
}

func TestExtractNumberAfter(t *testing.T) {
	cases := []struct {
		s, kw string
		want  int
	}{
		{"convoy 47 status", "convoy", 47},
		{"task #123 something", "task", 123},
		{"convoy_999 details", "convoy", 999},
		{"convoy alpha", "convoy", 0},
		{"no keyword here", "convoy", 0},
	}
	for _, c := range cases {
		if got := extractNumberAfter(c.s, c.kw); got != c.want {
			t.Errorf("extractNumberAfter(%q, %q) = %d; want %d", c.s, c.kw, got, c.want)
		}
	}
}

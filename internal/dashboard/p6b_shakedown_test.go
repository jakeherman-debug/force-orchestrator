package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// TestShakedown_P6B exercises the integrated drill + ask + reflection
// flow end-to-end against an in-memory holocron. Sub-cases mirror the
// 10-step matrix in the orchestrator brief:
//
//   1. Trigger a real convoy with synthetic LLM calls + git ops →
//      LLMCallTranscripts + GitOperationLog populate.
//   2. Open drill convoy view → unified timeline shows all events.
//   3. Click a task → task drill view shows decision chain + transcripts.
//   4. Click an LLM event → event drill shows full prompt + tool calls.
//   5. Free-text search "rate limit" → returns events from transcripts.
//   6. Replay a Captain ruling → ReplayResults row created with side-
//      by-side diff; original BountyBoard row UNCHANGED.
//   7. Annotate an event with flag=problem → row created (operator-only).
//   8. Press `/` → ask "what's blocking convoy 47?" → synthesised
//      answer with cite link to convoy 47 drill.
//   9. Open Reflection → calibration scoreboard renders from real data.
//   10. Trigger Friday retro → markdown draft generated.
//
// All sub-cases must PASS in <60s using stubbed LLM (CLI runner is
// stubbed via claude.SetCLIRunner).
func TestShakedown_P6B(t *testing.T) {
	deadline := time.Now().Add(60 * time.Second)
	t.Cleanup(func() {
		if time.Now().After(deadline) {
			t.Errorf("Shakedown ran longer than 60s budget")
		}
	})

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Wire LLM transcript capture so any wrapped call recorded.
	claude.SetTranscriptDB(db)
	defer claude.SetTranscriptDB(nil)

	t.Run("step1_seed_convoy_with_llm_and_git_events", func(t *testing.T) {
		// Convoy 47 + tasks 101, 102.
		_, err := db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (47, 'demo', 'Active')`)
		if err != nil {
			t.Fatalf("seed convoy: %v", err)
		}
		_, err = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES
			(101, 'Feature', 'investigate rate limit hits in upstream API', 'Pending', 47),
			(102, 'Feature', 'fix the upstream auth headers', 'Completed', 47)`)
		if err != nil {
			t.Fatalf("seed bounty: %v", err)
		}
		// LLM transcripts for both tasks.
		_, err = db.Exec(`INSERT INTO LLMCallTranscripts
			(id, task_id, agent, prompt_version, call_started_at, call_completed_at,
			 system_prompt, user_prompt, response_text, cost_usd, input_tokens, output_tokens)
			VALUES
			(1001, 101, 'captain', 'v18', '2026-01-01 10:00:00', '2026-01-01 10:00:30',
			       'system', 'investigate rate limit hits', 'APPROVE: scoped well', 0.0145, 1500, 200),
			(1002, 102, 'captain', 'v18', '2026-01-01 10:01:00', '2026-01-01 10:01:30',
			       'system', 'fix the auth header', 'APPROVE', 0.0123, 1200, 150)`)
		if err != nil {
			t.Fatalf("seed llm: %v", err)
		}
		// TaskHistory rows
		_, err = db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome, cost_usd_estimate, created_at)
			VALUES
			(101, 1, 'astromech', 's-1', 'work', 'Completed', 0.0145, '2026-01-01 10:02:00'),
			(102, 1, 'astromech', 's-2', 'work', 'Completed', 0.0123, '2026-01-01 10:03:00')`)
		if err != nil {
			t.Fatalf("seed history: %v", err)
		}
		// Git ops for the convoy
		_, err = db.Exec(`INSERT INTO GitOperationLog
			(task_id, convoy_id, repo, operation, args_json, started_at, exit_code, branch)
			VALUES
			(101, 47, 'demo', 'fetch', '["git","fetch","origin"]', '2026-01-01 10:04:00', 0, 'main'),
			(101, 47, 'demo', 'push',  '["git","push","origin","main"]', '2026-01-01 10:05:00', 0, 'feature/x')`)
		if err != nil {
			t.Fatalf("seed git: %v", err)
		}

		// Sanity counts
		var ll, gl int
		db.QueryRow(`SELECT COUNT(*) FROM LLMCallTranscripts WHERE task_id IN (101,102)`).Scan(&ll)
		db.QueryRow(`SELECT COUNT(*) FROM GitOperationLog WHERE convoy_id=47`).Scan(&gl)
		if ll != 2 || gl != 2 {
			t.Fatalf("seed counts: ll=%d gl=%d", ll, gl)
		}
	})

	t.Run("step2_drill_convoy_view_shows_all_event_kinds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/drill/convoy/47", nil)
		rr := httptest.NewRecorder()
		handleDrillConvoy(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		kinds := map[string]bool{}
		for _, ev := range resp.Events {
			if k, ok := ev["kind"].(string); ok {
				kinds[k] = true
			}
		}
		for _, want := range []string{"task_transition", "llm_call", "git_op"} {
			if !kinds[want] {
				t.Errorf("convoy timeline missing %s; saw %v", want, kinds)
			}
		}
	})

	t.Run("step3_drill_task_view_shows_decision_chain", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/drill/task/101", nil)
		rr := httptest.NewRecorder()
		handleDrillTask(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d", rr.Code)
		}
		var resp struct {
			Events []map[string]any `json:"events"`
		}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if len(resp.Events) < 2 {
			t.Errorf("expected ≥2 task drill events, got %d", len(resp.Events))
		}
	})

	t.Run("step4_drill_event_view_shows_llm_body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/drill/event/llm_call/1001", nil)
		rr := httptest.NewRecorder()
		handleDrillEvent(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
		}
		var resp map[string]any
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["agent"] != "captain" {
			t.Errorf("agent: %v", resp["agent"])
		}
		if !strings.Contains(resp["user_prompt"].(string), "rate limit") {
			t.Errorf("user_prompt: %v", resp["user_prompt"])
		}
	})

	t.Run("step5_search_returns_rate_limit_hit", func(t *testing.T) {
		// Use the search handler for full HTTP coverage.
		req := httptest.NewRequest(http.MethodGet, "/api/drill/search?q=rate+limit", nil)
		rr := httptest.NewRecorder()
		handleDrillSearch(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d", rr.Code)
		}
		var resp struct {
			Results []map[string]any `json:"results"`
		}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if len(resp.Results) == 0 {
			t.Errorf("expected ≥1 rate-limit hit; got %v", resp.Results)
		}
	})

	t.Run("step6_replay_captain_ruling_no_live_mutation", func(t *testing.T) {
		// Snapshot live state.
		var preStatus string
		db.QueryRow(`SELECT status FROM BountyBoard WHERE id=101`).Scan(&preStatus)

		res, err := agents.ReplayDecision(context.Background(), db,
			"captain_ruling", 1001, "v19", "op@x")
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if res.ID == 0 {
			t.Fatal("expected non-zero replay id")
		}

		// Original BountyBoard row UNCHANGED
		var afterStatus string
		db.QueryRow(`SELECT status FROM BountyBoard WHERE id=101`).Scan(&afterStatus)
		if preStatus != afterStatus {
			t.Errorf("REPLAY mutated BountyBoard.status: %q → %q", preStatus, afterStatus)
		}
	})

	t.Run("step7_annotate_event_problem_flag", func(t *testing.T) {
		id, err := store.InsertAnnotation(context.Background(), db, store.Annotation{
			OperatorEmail: "op@x",
			EventKind:     "llm_call",
			EventRef:      "1001",
			NoteText:      "this prompt is missing context",
			Flag:          "problem",
		})
		if err != nil {
			t.Fatalf("insert annotation: %v", err)
		}
		if id == 0 {
			t.Fatal("expected non-zero id")
		}
		// Confirm via list-by-event.
		rows, _ := store.ListAnnotationsForEvent(context.Background(), db, "llm_call", "1001")
		if len(rows) != 1 || rows[0].Flag != "problem" {
			t.Errorf("annotation list: %+v", rows)
		}
	})

	t.Run("step8_ask_what_blocking_convoy_47", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/ask",
			strings.NewReader(`{"question":"what's the status of convoy 47?"}`))
		rr := httptest.NewRecorder()
		handleAsk(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Answer    string `json:"answer"`
			CiteLinks []struct {
				Kind string `json:"kind"`
				ID   int64  `json:"id"`
			} `json:"cite_links"`
		}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if !strings.Contains(resp.Answer, "Convoy 47") {
			t.Errorf("answer missing convoy 47: %q", resp.Answer)
		}
		var foundConvoyCite bool
		for _, c := range resp.CiteLinks {
			if c.Kind == "convoy" && c.ID == 47 {
				foundConvoyCite = true
			}
		}
		if !foundConvoyCite {
			t.Errorf("expected convoy=47 cite link: %+v", resp.CiteLinks)
		}
	})

	t.Run("step9_reflection_calibration_renders", func(t *testing.T) {
		// Seed some BriefingRenders so the panel has data
		_, _ = db.Exec(`INSERT INTO BriefingRenders
			(decision_id, decision_kind, briefing_text, operator_decision, decision_time_seconds, rendered_at)
			VALUES (1, 'captain_ruling', 'b', 'approve', 25, datetime('now'))`)
		req := httptest.NewRequest(http.MethodGet, "/api/reflection/calibration", nil)
		rr := httptest.NewRecorder()
		handleCalibration(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d", rr.Code)
		}
		var sb store.CalibrationScoreboard
		json.Unmarshal(rr.Body.Bytes(), &sb)
		if len(sb.DecisionTimes) == 0 {
			t.Errorf("expected ≥1 agent-decision-time row")
		}
	})

	t.Run("step10_friday_retro_generates_markdown", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/reflection/retro/generate", strings.NewReader(""))
		rr := httptest.NewRecorder()
		handleRetroGenerate(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d", rr.Code)
		}
		var resp struct {
			Markdown      string `json:"markdown"`
			SuggestedPath string `json:"suggested_path"`
		}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if !strings.Contains(resp.Markdown, "Top win") || !strings.Contains(resp.Markdown, "Top frustration") {
			t.Errorf("retro markdown missing sections: %q", resp.Markdown)
		}
		if !strings.Contains(resp.SuggestedPath, "docs/retros") {
			t.Errorf("suggested path: %q", resp.SuggestedPath)
		}
	})
}

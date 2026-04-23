package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// Fix #1 — /api/status must surface trailing-hour spend, the configured cap,
// and a pre-computed SpendCapExceeded flag so the dashboard can colour the
// burn widget without re-computing the threshold client-side.

func TestAPIStatus_ExposesHourlySpend(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var s DashboardStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// All four spend-related fields must be present. Empty DB → zero spend,
	// zero attempts, default cap, not-exceeded.
	if s.HourlySpendDollars != 0 {
		t.Errorf("empty DB: expected $0 hourly spend, got $%.2f", s.HourlySpendDollars)
	}
	if s.AttemptsLastHour != 0 {
		t.Errorf("empty DB: expected 0 attempts, got %d", s.AttemptsLastHour)
	}
	if s.HourlySpendCapUSD != agents.DefaultHourlySpendCapUSD {
		t.Errorf("empty DB: expected default cap $%.2f, got $%.2f",
			agents.DefaultHourlySpendCapUSD, s.HourlySpendCapUSD)
	}
	if s.SpendCapExceeded {
		t.Error("empty DB: expected spend_cap_exceeded=false")
	}

	// Verify JSON field names are the documented ones — clients read these.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{
		"hourly_spend_dollars",
		"hourly_spend_cap_usd",
		"attempts_last_hour",
		"spend_cap_exceeded",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing required /api/status field: %s", key)
		}
	}
}

func TestAPIStatus_SpendCapExceededFlag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed TaskHistory with 10M input tokens = $30 > $25 default cap.
	_, err := db.Exec(`INSERT INTO TaskHistory(task_id, attempt, agent, session_id, claude_output, outcome, tokens_in, tokens_out)
		VALUES (1, 1, 'astromech', 's1', '', 'Completed', 10000000, 0)`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Add a second attempt to exercise the attempts_last_hour counter.
	_, err = db.Exec(`INSERT INTO TaskHistory(task_id, attempt, agent, session_id, claude_output, outcome, tokens_in, tokens_out)
		VALUES (1, 2, 'astromech', 's2', '', 'Failed', 100, 100)`)
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)

	var s DashboardStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if s.HourlySpendDollars < 29.9 {
		t.Errorf("expected ~$30 hourly spend, got $%.2f", s.HourlySpendDollars)
	}
	if s.AttemptsLastHour != 2 {
		t.Errorf("expected 2 attempts in last hour, got %d", s.AttemptsLastHour)
	}
	if !s.SpendCapExceeded {
		t.Errorf("expected spend_cap_exceeded=true when $%.2f > $%.2f cap",
			s.HourlySpendDollars, s.HourlySpendCapUSD)
	}

	// Operator override: raise the cap and confirm the flag flips.
	store.SetConfig(db, "hourly_spend_cap_usd", "100")
	r2 := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w2 := httptest.NewRecorder()
	handleStatus(db)(w2, r2)
	var s2 DashboardStatus
	json.Unmarshal(w2.Body.Bytes(), &s2)
	if s2.HourlySpendCapUSD != 100 {
		t.Errorf("expected raised cap $100, got $%.2f", s2.HourlySpendCapUSD)
	}
	if s2.SpendCapExceeded {
		t.Errorf("cap raised above spend — expected spend_cap_exceeded=false, got true")
	}
}

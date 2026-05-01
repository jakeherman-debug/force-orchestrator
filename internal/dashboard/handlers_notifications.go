// D3 P6A.4 — Notification budget API endpoints.
package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"force-orchestrator/internal/store"
)

// handleNotificationBudgets — GET /api/notifications/budgets
// Returns every configured budget for the operator.
func handleNotificationBudgets(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		op := r.URL.Query().Get("operator")
		if op == "" {
			op = "default@operator"
		}
		budgets, err := store.ListNotificationBudgets(r.Context(), db, op)
		if err != nil {
			http.Error(w, `{"error":"list budgets failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"budgets": budgets})
	}
}

// handleNotificationBudgetUpsert — PUT /api/notifications/budgets/:source/:channel
// Body: {operator_email, max_per_period, period_minutes, digest_remainder}
func handleNotificationBudgetUpsert(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPut {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Expect /api/notifications/budgets/<source>/<channel>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/notifications/budgets/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, `{"error":"path: /api/notifications/budgets/<source>/<channel>"}`, http.StatusBadRequest)
			return
		}
		source, channel := parts[0], parts[1]

		var body struct {
			OperatorEmail   string `json:"operator_email"`
			MaxPerPeriod    int    `json:"max_per_period"`
			PeriodMinutes   int    `json:"period_minutes"`
			DigestRemainder bool   `json:"digest_remainder"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if body.OperatorEmail == "" {
			http.Error(w, `{"error":"operator_email required"}`, http.StatusBadRequest)
			return
		}
		if body.PeriodMinutes <= 0 {
			http.Error(w, `{"error":"period_minutes must be > 0"}`, http.StatusBadRequest)
			return
		}

		ctx := context.Background()
		if err := store.SetNotificationBudget(ctx, db, body.OperatorEmail, source, channel,
			body.MaxPerPeriod, body.PeriodMinutes, body.DigestRemainder); err != nil {
			http.Error(w, `{"error":"upsert failed"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

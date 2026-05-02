// D4 fix-loop-1 α2 — Per-rule precision metrics dashboard view.
//
// Endpoints:
//
//   GET /api/rule-metrics                       — list all rules' metrics
//   GET /api/rule-metrics?bureau=BoS|ISB|all    — filter by bureau
//   GET /api/rule-metrics?rule_id=BOS-001       — single-rule rollup
//
// Pure read, no operator action. Computed via store.ComputeRuleMetrics
// / store.ListAllRuleMetrics; the handler is a thin JSON encoder.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"force-orchestrator/internal/store"
)

func handleRuleMetrics(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		bureau := q.Get("bureau")
		if strings.EqualFold(bureau, "all") {
			bureau = ""
		}
		switch bureau {
		case "", "BoS", "ISB":
		default:
			http.Error(w, `{"error":"bureau must be BoS, ISB, or all"}`, http.StatusBadRequest)
			return
		}
		ruleID := strings.TrimSpace(q.Get("rule_id"))

		// Single-rule path: ?rule_id=BOS-001
		if ruleID != "" {
			m, err := store.ComputeRuleMetrics(db, bureau, ruleID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			if m == nil {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"rule_id": ruleID,
					"bureau":  bureau,
					"metrics": nil,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"rule_id": ruleID,
				"bureau":  bureau,
				"metrics": m,
			})
			return
		}

		// List path: every rule that has fired at least once.
		all, err := store.ListAllRuleMetrics(db, bureau)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		if all == nil {
			all = []store.RuleMetrics{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rules": all,
			"count": len(all),
		})
	}
}

// D3 P6A.11 — Briefing reject (counter-proposal forcing) endpoint.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"force-orchestrator/internal/agents"
)

func handleBriefingReject(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"body read"}`, http.StatusRequestEntityTooLarge)
			return
		}
		var in struct {
			BriefingID          int64  `json:"briefing_id"`
			DecisionKind        string `json:"decision_kind"`
			CounterProposalKind string `json:"counter_proposal_kind"`
			Text                string `json:"text"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if in.BriefingID == 0 {
			http.Error(w, `{"error":"briefing_id required"}`, http.StatusBadRequest)
			return
		}
		if in.CounterProposalKind == "" {
			http.Error(w, `{"error":"counter_proposal_kind required"}`, http.StatusBadRequest)
			return
		}
		newID, err := agents.RouteCounterProposal(r.Context(), db, in.BriefingID, in.DecisionKind,
			agents.CounterProposalKind(in.CounterProposalKind), in.Text)
		if err != nil {
			switch {
			case errors.Is(err, agents.ErrCounterKindUnknown),
				errors.Is(err, agents.ErrWholeThingTextTooShort),
				errors.Is(err, agents.ErrDifferentApproachTooShort),
				errors.Is(err, agents.ErrCounterKindRequired):
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			default:
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"routed_id": newID})
	}
}

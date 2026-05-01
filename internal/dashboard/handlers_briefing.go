// D3 P6A.10 — Briefing API.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// handleBriefingQueue — GET /api/briefing/queue
func handleBriefingQueue(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		q, err := agents.ListBriefingQueue(r.Context(), db)
		if err != nil {
			http.Error(w, `{"error":"queue failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"queue": q})
	}
}

// handleBriefingDecision — GET /api/briefing/decision/<kind>/<id>
func handleBriefingDecision(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		// /api/briefing/decision/<kind>/<id>
		path := strings.TrimPrefix(r.URL.Path, "/api/briefing/decision/")
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			http.Error(w, `{"error":"path: /api/briefing/decision/<kind>/<id>"}`, http.StatusBadRequest)
			return
		}
		kind := parts[0]
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			http.Error(w, `{"error":"id must be integer"}`, http.StatusBadRequest)
			return
		}
		// Look up the operator's trust dial for the agent attributed to
		// the decision. For 6A we approximate kind→agent mapping coarsely.
		dial := 70
		op := r.URL.Query().Get("operator")
		if op == "" {
			op = "default@operator"
		}
		dial, _ = store.GetCurrentTrustDial(r.Context(), db, op, briefingKindToAgent(kind))
		br, err := agents.RenderBriefing(r.Context(), db, kind, id, dial)
		if err != nil {
			http.Error(w, `{"error":"render failed"}`, http.StatusInternalServerError)
			return
		}
		// Stamp the effective stakes tier per trust dial (FrictionTierFor).
		base := "medium"
		if kind == "spec_amendment" || kind == "rule_amendment" {
			base = "high"
		}
		effective := store.FrictionTierFor(dial, base)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"briefing":              br,
			"effective_stakes_tier": effective,
			"trust_dial":            dial,
		})
	}
}

// handleBriefingDecide — POST /api/briefing/decide
// Body: {briefing_id, decision, decision_time_seconds}
// (counter-proposal data in 6A.11's POST /api/briefing/reject)
func handleBriefingDecide(db *sql.DB) http.HandlerFunc {
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
			Decision            string `json:"decision"`
			DecisionTimeSeconds int    `json:"decision_time_seconds"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if in.BriefingID == 0 || in.Decision == "" {
			http.Error(w, `{"error":"briefing_id + decision required"}`, http.StatusBadRequest)
			return
		}
		if in.Decision != "approved" && in.Decision != "rejected" && in.Decision != "deferred" {
			http.Error(w, `{"error":"decision must be approved|rejected|deferred"}`, http.StatusBadRequest)
			return
		}
		if err := agents.RecordBriefingDecision(r.Context(), db, in.BriefingID, in.Decision, in.DecisionTimeSeconds, "", "", 0); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, `{"error":"briefing not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"record failed"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// briefingKindToAgent — map a decision_kind to the agent owning it,
// for trust-dial lookup.
func briefingKindToAgent(kind string) string {
	switch kind {
	case "captain_proposal":
		return "captain"
	case "council_ratification":
		return "council"
	case "promotion_proposal":
		return "ec"
	case "spec_amendment", "convoy_review_amendment":
		return "convoy_review"
	case "investigator_finding":
		return "investigator"
	default:
		return "captain"
	}
}

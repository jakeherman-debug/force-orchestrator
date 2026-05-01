package dashboard

// D3 fix-loop-1 β2 — ProposedFeatures operator endpoints
// (concern #10 / exit criterion 14).
//
// Endpoints (registered in dashboard.go):
//
//   GET  /api/proposed-features              — list rows (status filter via ?status=)
//   POST /api/proposed-features/:id/suppress — install operator suppression rule
//   POST /api/proposed-features/:id/score    — operator override of value/complexity score
//   POST /api/proposed-features/:id/promote  — operator promotes pending → promoted
//
// Every mutating handler requires an operator email (rejected as 400
// if blank) and writes to AuditLog. Suppression rationale ≥ 20 chars
// is enforced at the store-helper boundary (schema CHECK + the helper).
//
// P23 (proposer write discipline): satisfied — these handlers are
// operator-routed, never proposer-routed. The dashboard SPA enforces
// the operator email; the handlers fail closed if it's missing.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// handleProposedFeaturesList serves GET /api/proposed-features.
//
// Optional ?status= filters: pending (default-ish, treated as "not
// archived"), promoted, archived, or empty for all-non-archived.
func handleProposedFeaturesList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		status := r.URL.Query().Get("status")
		// Treat "all" as the no-filter case.
		if status == "all" {
			status = ""
		}
		rows, err := store.ListProposedFeatures(db, status)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []store.ProposedFeatureRow{}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}
}

// proposedFeatureSubroutes dispatches the three POST verbs under
// /api/proposed-features/:id/{suppress,score,promote}. A bare
// /api/proposed-features/:id GET returns the single row.
func handleProposedFeaturesSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		path := strings.TrimPrefix(r.URL.Path, "/api/proposed-features/")
		parts := strings.Split(path, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, `{"error":"feature id required"}`, http.StatusBadRequest)
			return
		}
		idStr := parts[0]
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, `{"error":"invalid feature id"}`, http.StatusBadRequest)
			return
		}
		verb := ""
		if len(parts) > 1 {
			verb = parts[1]
		}
		switch {
		case verb == "" && r.Method == http.MethodGet:
			handleProposedFeatureSingle(w, r, db, id)
		case verb == "suppress" && r.Method == http.MethodPost:
			handleProposedFeatureSuppress(w, r, db, id)
		case verb == "score" && r.Method == http.MethodPost:
			handleProposedFeatureScoreOverride(w, r, db, id)
		case verb == "promote" && r.Method == http.MethodPost:
			handleProposedFeaturePromote(w, r, db, id)
		default:
			http.Error(w, `{"error":"unknown verb or method"}`, http.StatusNotFound)
		}
	}
}

func handleProposedFeatureSingle(w http.ResponseWriter, _ *http.Request, db *sql.DB, id int64) {
	rows, err := store.ListProposedFeatures(db, "")
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	for _, r := range rows {
		if r.ID == id {
			_ = json.NewEncoder(w).Encode(r)
			return
		}
	}
	http.Error(w, `{"error":"feature not found"}`, http.StatusNotFound)
}

type suppressRequest struct {
	Rationale         string `json:"rationale"`
	OperatorEmail     string `json:"operator_email"`
	SuppressUntilDays int    `json:"suppress_until_days"` // 0 = no expiry
}

func handleProposedFeatureSuppress(w http.ResponseWriter, r *http.Request, db *sql.DB, id int64) {
	var req suppressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OperatorEmail) == "" {
		http.Error(w, `{"error":"operator_email required"}`, http.StatusBadRequest)
		return
	}
	if len(strings.TrimSpace(req.Rationale)) < 20 {
		http.Error(w, `{"error":"rationale must be >= 20 chars"}`, http.StatusBadRequest)
		return
	}

	// Resolve the feature's fingerprint so the suppression matches.
	var fp string
	err := db.QueryRow(`SELECT IFNULL(fingerprint,'') FROM ProposedFeatures WHERE id = ?`, id).Scan(&fp)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"feature not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	if fp == "" {
		http.Error(w, `{"error":"feature has empty fingerprint"}`, http.StatusBadRequest)
		return
	}

	until := time.Time{}
	if req.SuppressUntilDays > 0 {
		until = time.Now().UTC().Add(time.Duration(req.SuppressUntilDays) * 24 * time.Hour)
	}
	suppID, err := store.SuppressProposedFeature(db, fp, req.Rationale, until, req.OperatorEmail)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"suppress: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	store.LogAudit(db, req.OperatorEmail, "proposed-feature-suppress", int(id),
		fmt.Sprintf("suppression %d installed (fingerprint=%s)", suppID, fp))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"suppression_id": suppID,
	})
}

type scoreOverrideRequest struct {
	NewValueScore      string `json:"new_value_score"`
	NewComplexityScore string `json:"new_complexity_score"`
	Rationale          string `json:"rationale"`
	OperatorEmail      string `json:"operator_email"`
}

func handleProposedFeatureScoreOverride(w http.ResponseWriter, r *http.Request, db *sql.DB, id int64) {
	var req scoreOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OperatorEmail) == "" {
		http.Error(w, `{"error":"operator_email required"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		http.Error(w, `{"error":"rationale required"}`, http.StatusBadRequest)
		return
	}
	err := store.OverrideProposedFeatureScore(db, id, req.NewValueScore, req.NewComplexityScore,
		req.Rationale, req.OperatorEmail)
	if err != nil {
		// Map "not found" → 404 so the dashboard can render the
		// right error.
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	store.LogAudit(db, req.OperatorEmail, "proposed-feature-score-override", int(id),
		fmt.Sprintf("value=%s complexity=%s rationale=%s",
			req.NewValueScore, req.NewComplexityScore, truncateForAudit(req.Rationale, 200)))
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

type promoteRequest struct {
	Deadline      string `json:"deadline"` // free-form date string per roadmap
	OperatorEmail string `json:"operator_email"`
}

func handleProposedFeaturePromote(w http.ResponseWriter, r *http.Request, db *sql.DB, id int64) {
	var req promoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.OperatorEmail) == "" {
		http.Error(w, `{"error":"operator_email required"}`, http.StatusBadRequest)
		return
	}
	err := store.PromoteProposedFeature(db, id, req.Deadline, req.OperatorEmail)
	if err != nil {
		if strings.Contains(err.Error(), "not promotable") {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	store.LogAudit(db, req.OperatorEmail, "proposed-feature-promote", int(id),
		fmt.Sprintf("promoted with deadline=%s", req.Deadline))
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func truncateForAudit(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

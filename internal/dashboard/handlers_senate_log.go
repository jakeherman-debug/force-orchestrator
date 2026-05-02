// D4 fix-loop-1 α4 — Senate review log + chambers dashboard view.
//
// Endpoints:
//
//   GET /api/senate/chambers                  — list chambers (Senators)
//   GET /api/senate/reviews?feature_id=N      — list SenateReview rows
//   GET /api/senate/reviews?senator=name      — filter by senator
//   GET /api/senate/reviews?position=concur   — filter by verdict
//   GET /api/senate/reviews/:id               — single-review detail with
//                                               cited_memory_ids resolved
//
// The /api/senate/chambers list shows every Senator and their status
// (onboarding | active | suspended | retired); the /api/senate/reviews
// list is the per-feature review log; the per-id detail surfaces the
// memories the Senator cited (so the operator can see what shaped the
// verdict).
//
// Pure read; no operator action. No CLI parity required (review writes
// are agent-internal).

package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// handleSenateChambers serves GET /api/senate/chambers.
//
// Query params:
//
//	?status=active|onboarding|suspended|retired|all   (default all)
func handleSenateChambers(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		status := r.URL.Query().Get("status")
		if strings.EqualFold(status, "all") {
			status = ""
		}
		switch status {
		case "", "active", "onboarding", "suspended", "retired":
		default:
			http.Error(w, `{"error":"status must be active, onboarding, suspended, retired, or all"}`, http.StatusBadRequest)
			return
		}

		query := `SELECT senator_name, scope, IFNULL(senate_md_path,''), status,
		                 IFNULL(onboarded_at,''), IFNULL(last_refreshed_at,''),
		                 IFNULL(retired_at,''), IFNULL(created_at,'')
		            FROM SenateChambers`
		args := []any{}
		if status != "" {
			query += " WHERE status = ?"
			args = append(args, status)
		}
		query += " ORDER BY senator_name ASC"

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []store.SenateChamber{}
		for rows.Next() {
			var c store.SenateChamber
			if scanErr := rows.Scan(&c.SenatorName, &c.Scope, &c.SenateMDPath, &c.Status,
				&c.OnboardedAt, &c.LastRefreshedAt, &c.RetiredAt, &c.CreatedAt); scanErr != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, scanErr.Error()), http.StatusInternalServerError)
				return
			}
			out = append(out, c)
		}
		if rErr := rows.Err(); rErr != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, rErr.Error()), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chambers": out,
			"count":    len(out),
		})
	}
}

// senateReviewView is the dashboard-facing shape; adds feature_title via
// a JOIN to BountyBoard.payload (best-effort) so the operator sees what
// the review was about without an extra fetch.
type senateReviewView struct {
	store.SenateReviewRow
	FeatureTitle string `json:"feature_title,omitempty"`
}

// handleSenateReviews serves GET /api/senate/reviews.
func handleSenateReviews(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		featureIDStr := strings.TrimSpace(q.Get("feature_id"))
		senator := strings.TrimSpace(q.Get("senator"))
		position := strings.TrimSpace(q.Get("position"))
		if position != "" {
			switch position {
			case "concur", "amend", "dissent":
			default:
				http.Error(w, `{"error":"position must be concur, amend, or dissent"}`, http.StatusBadRequest)
				return
			}
		}
		limit := atoiDefault(q.Get("limit"), 50)
		offset := atoiDefault(q.Get("offset"), 0)
		if limit > 500 {
			limit = 500
		}
		if limit <= 0 {
			limit = 50
		}
		if offset < 0 {
			offset = 0
		}

		query := `SELECT id, feature_id, senator, position,
		                 IFNULL(concerns,'[]'), IFNULL(amendments,'[]'),
		                 IFNULL(rationale,''), IFNULL(confidence, 0),
		                 IFNULL(created_at,'')
		            FROM SenateReview WHERE 1=1`
		args := []any{}
		if featureIDStr != "" {
			fid, err := strconv.Atoi(featureIDStr)
			if err != nil || fid <= 0 {
				http.Error(w, `{"error":"feature_id must be a positive integer"}`, http.StatusBadRequest)
				return
			}
			query += " AND feature_id = ?"
			args = append(args, fid)
		}
		if senator != "" {
			query += " AND senator = ?"
			args = append(args, senator)
		}
		if position != "" {
			query += " AND position = ?"
			args = append(args, position)
		}
		query += " ORDER BY id DESC LIMIT ? OFFSET ?"
		args = append(args, limit, offset)

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		out := []senateReviewView{}
		for rows.Next() {
			var rr senateReviewView
			if scanErr := rows.Scan(&rr.ID, &rr.FeatureID, &rr.Senator, &rr.Position,
				&rr.Concerns, &rr.Amendments, &rr.Rationale, &rr.Confidence,
				&rr.CreatedAt); scanErr != nil {
				rows.Close()
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, scanErr.Error()), http.StatusInternalServerError)
				return
			}
			out = append(out, rr)
		}
		if rErr := rows.Err(); rErr != nil {
			rows.Close()
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, rErr.Error()), http.StatusInternalServerError)
			return
		}
		// Close BEFORE issuing per-row title lookups — SQLite serialises
		// queries on the same connection, so a nested QueryRow while the
		// outer rows iterator is open would deadlock under :memory: DSN.
		rows.Close()
		// Best-effort feature-title resolution. Errors are non-fatal —
		// title is decorative metadata, not a load-bearing field.
		for i := range out {
			out[i].FeatureTitle = lookupFeatureTitle(db, out[i].FeatureID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"reviews": out,
			"count":   len(out),
			"limit":   limit,
			"offset":  offset,
		})
	}
}

// handleSenateReviewsSubroutes serves GET /api/senate/reviews/:id.
func handleSenateReviewsSubroutes(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/senate/reviews/")
		path = strings.TrimSuffix(path, "/")
		if path == "" {
			http.Error(w, `{"error":"review id required"}`, http.StatusBadRequest)
			return
		}
		id, err := strconv.Atoi(path)
		if err != nil || id <= 0 {
			http.Error(w, `{"error":"invalid review id"}`, http.StatusBadRequest)
			return
		}

		var rr senateReviewView
		err = db.QueryRow(`SELECT id, feature_id, senator, position,
		                          IFNULL(concerns,'[]'), IFNULL(amendments,'[]'),
		                          IFNULL(rationale,''), IFNULL(confidence, 0),
		                          IFNULL(created_at,'')
		                     FROM SenateReview WHERE id = ?`, id).
			Scan(&rr.ID, &rr.FeatureID, &rr.Senator, &rr.Position,
				&rr.Concerns, &rr.Amendments, &rr.Rationale, &rr.Confidence,
				&rr.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"review not found"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		rr.FeatureTitle = lookupFeatureTitle(db, rr.FeatureID)

		// Resolve the Senator's currently-cited memory list by senator
		// (top 10 by weight). The drilldown shows the operator what the
		// Senator's prompt context looked like at review time. This is a
		// best-effort approximation — we don't snapshot prompt memories
		// at review time today (a future enhancement); the current view
		// is "what memories the Senator currently has".
		mems, _ := store.ListSenateMemory(db, rr.Senator, 10)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"review":         rr,
			"cited_memories": mems,
		})
	}
}

// lookupFeatureTitle resolves a feature_id (BountyBoard row id) to a
// human-readable title, best-effort. Returns "" on miss.
func lookupFeatureTitle(db *sql.DB, featureID int) string {
	if featureID <= 0 {
		return ""
	}
	// Feature title may live in payload (free-text) or be derivable from
	// the BountyBoard row's type. We pull payload + truncate.
	var payload string
	err := db.QueryRow(`SELECT IFNULL(payload,'') FROM BountyBoard WHERE id = ?`, featureID).Scan(&payload)
	if err != nil {
		return ""
	}
	if len(payload) > 120 {
		payload = payload[:120] + "…"
	}
	return payload
}

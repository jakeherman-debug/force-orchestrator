// Package dashboard — D8 Track 2 Feature blast-radius surface.
//
// Endpoint:
//
//	GET /api/features/<id>/blast-radius
//	  → { "feature_id":N,
//	      "modified_symbols": [...],
//	      "affected_consumer_repos": [...],
//	      "auto_included_tasks": [...] }
//
// Read-only. The Chancellor blast-radius post-process
// (internal/agents/chancellor_blast_radius.go) is the only writer.
//
// Empty BlastRadiusRecord (i.e. no blast-radius computed for this
// Feature, or pre-T2 row) returns an empty arrays in every field —
// the operator-facing payload shape is stable so the dashboard SPA
// can render "no consumer impact" without a separate code path.
//
// 404 on Feature ID not found. 405 on non-GET methods.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// blastRadiusResponse is the JSON wire shape. Wraps the store record
// with the feature_id so the operator can confirm they're looking at
// the right Feature without re-reading the URL.
type blastRadiusResponse struct {
	FeatureID             int                       `json:"feature_id"`
	ModifiedSymbols       []store.BlastRadiusSymbol `json:"modified_symbols"`
	AffectedConsumerRepos []string                  `json:"affected_consumer_repos"`
	AutoIncludedTasks     []int                     `json:"auto_included_tasks"`
}

// handleFeatureBlastRadius serves GET /api/features/<id>/blast-radius.
func handleFeatureBlastRadius(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// Path: /api/features/<id>/blast-radius (or trailing slash).
		path := strings.TrimPrefix(r.URL.Path, "/api/features/")
		path = strings.TrimSuffix(path, "/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 || parts[1] != "blast-radius" {
			http.Error(w, `{"error":"expected /api/features/<id>/blast-radius"}`, http.StatusBadRequest)
			return
		}
		featureID, err := strconv.Atoi(parts[0])
		if err != nil || featureID <= 0 {
			http.Error(w, `{"error":"invalid feature id"}`, http.StatusBadRequest)
			return
		}
		rec, gErr := store.GetFeatureBlastRadius(db, featureID)
		if gErr != nil {
			if errors.Is(gErr, sql.ErrNoRows) ||
				strings.Contains(gErr.Error(), sql.ErrNoRows.Error()) {
				http.Error(w, `{"error":"feature not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		resp := blastRadiusResponse{
			FeatureID:             featureID,
			ModifiedSymbols:       rec.ModifiedSymbols,
			AffectedConsumerRepos: rec.AffectedConsumerRepos,
			AutoIncludedTasks:     rec.AutoIncludedTasks,
		}
		// Normalize nil → [] so the SPA never has to branch on null vs empty array.
		if resp.ModifiedSymbols == nil {
			resp.ModifiedSymbols = []store.BlastRadiusSymbol{}
		}
		if resp.AffectedConsumerRepos == nil {
			resp.AffectedConsumerRepos = []string{}
		}
		if resp.AutoIncludedTasks == nil {
			resp.AutoIncludedTasks = []int{}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

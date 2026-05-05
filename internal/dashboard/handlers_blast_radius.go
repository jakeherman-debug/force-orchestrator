// Package dashboard — D8 Track 2 + Track 3 Feature blast-radius surface.
//
// Endpoints:
//
//	GET /api/features/<id>/blast-radius
//	  → { "feature_id":N,
//	      "modified_symbols": [...],
//	      "affected_consumer_repos": [...],
//	      "auto_included_tasks": [...] }
//
//	GET /api/features/<id>/consumer-integ
//	  → { "feature_id":N,
//	      "any_blocking": bool,
//	      "blocking_repos": [...],
//	      "results": [
//	        {"consumer_repo_name","status","exit_code","test_command",
//	         "duration_seconds","ran_at","stdout_tail","stderr_tail"}, ...
//	      ] }
//
// Both read-only. Blast-radius is written by the Chancellor blast-radius
// post-process; consumer-integ rows are written by the Diplomat
// runConsumerIntegrationCheck handler (D8 Track 3).
//
// Empty payloads serialize as [] arrays (not null) so the SPA never has
// to branch on missing fields.
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

// handleFeatureBlastRadius serves GET /api/features/<id>/blast-radius
// AND GET /api/features/<id>/consumer-integ. Both routes share the same
// handler so the dashboard mux can dispatch on a single prefix.
func handleFeatureBlastRadius(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// Path: /api/features/<id>/<subroute> (or trailing slash).
		path := strings.TrimPrefix(r.URL.Path, "/api/features/")
		path = strings.TrimSuffix(path, "/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			http.Error(w, `{"error":"expected /api/features/<id>/<subroute>"}`, http.StatusBadRequest)
			return
		}
		featureID, err := strconv.Atoi(parts[0])
		if err != nil || featureID <= 0 {
			http.Error(w, `{"error":"invalid feature id"}`, http.StatusBadRequest)
			return
		}
		switch parts[1] {
		case "blast-radius":
			serveBlastRadius(w, db, featureID)
		case "consumer-integ":
			serveConsumerIntegResults(w, db, featureID)
		default:
			http.Error(w, `{"error":"unknown subroute; expected blast-radius or consumer-integ"}`, http.StatusBadRequest)
		}
	}
}

func serveBlastRadius(w http.ResponseWriter, db *sql.DB, featureID int) {
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

// consumerIntegResultDTO is the per-row JSON shape on the
// /consumer-integ endpoint. Mirrors store.ConsumerIntegrationResult but
// omits the row id (operator doesn't need it for rendering) and renames
// duration to seconds for SPA convention.
type consumerIntegResultDTO struct {
	ConsumerRepoName string `json:"consumer_repo_name"`
	Status           string `json:"status"`
	ExitCode         int    `json:"exit_code"`
	TestCommand      string `json:"test_command"`
	DurationSeconds  int    `json:"duration_seconds"`
	RanAt            string `json:"ran_at"`
	StdoutTail       string `json:"stdout_tail"`
	StderrTail       string `json:"stderr_tail"`
}

// consumerIntegResponse wraps the per-Feature aggregation: the array of
// rows + a precomputed any_blocking flag + the failed-repos list so the
// SPA doesn't have to walk the array twice.
type consumerIntegResponse struct {
	FeatureID     int                      `json:"feature_id"`
	AnyBlocking   bool                     `json:"any_blocking"`
	BlockingRepos []string                 `json:"blocking_repos"`
	Results       []consumerIntegResultDTO `json:"results"`
}

func serveConsumerIntegResults(w http.ResponseWriter, db *sql.DB, featureID int) {
	// Verify the Feature exists; reuse blast-radius lookup as the existence
	// check (it already returns ErrNoRows on missing row).
	if _, gErr := store.GetFeatureBlastRadius(db, featureID); gErr != nil {
		if errors.Is(gErr, sql.ErrNoRows) ||
			strings.Contains(gErr.Error(), sql.ErrNoRows.Error()) {
			http.Error(w, `{"error":"feature not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	rows, lErr := store.ListConsumerIntegrationResultsByFeature(db, featureID)
	if lErr != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	blocking, blockingRepos, _ := store.FeatureHasBlockingConsumerBreakage(db, featureID)
	out := make([]consumerIntegResultDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, consumerIntegResultDTO{
			ConsumerRepoName: r.ConsumerRepoName,
			Status:           r.Status,
			ExitCode:         r.ExitCode,
			TestCommand:      r.TestCommand,
			DurationSeconds:  r.DurationSeconds,
			RanAt:            r.RanAt,
			StdoutTail:       r.StdoutTail,
			StderrTail:       r.StderrTail,
		})
	}
	if blockingRepos == nil {
		blockingRepos = []string{}
	}
	resp := consumerIntegResponse{
		FeatureID:     featureID,
		AnyBlocking:   blocking,
		BlockingRepos: blockingRepos,
		Results:       out,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

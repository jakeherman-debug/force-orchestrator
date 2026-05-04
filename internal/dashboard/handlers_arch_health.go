// handlers_arch_health.go — D9 Phase 1: Architecture Health dashboard endpoints.
//
//	GET /api/arch-health/latest         — most recent report month + aggregates.
//	GET /api/arch-health/<YYYY-MM>      — specific month's aggregates.
//	GET /api/arch-health/months         — distinct report_months present (for picker).
//
// Response shape mirrors the other D4-style handlers (handlers_security_findings.go):
// a top-level object with `month`, `rows`, `total_violations`, `per_repo_total`.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"force-orchestrator/internal/store"
)

// archHealthRow is the JSON shape returned per ArchHealthAggregates row.
type archHealthRow struct {
	RuleID         string `json:"rule_id"`
	RepoID         int    `json:"repo_id"`
	AuthorType     string `json:"author_type"`
	ViolationCount int    `json:"violation_count"`
	ReportMonth    string `json:"report_month"`
}

// archHealthResponse is the top-level JSON for /api/arch-health/<month>.
type archHealthResponse struct {
	Month           string          `json:"month"`
	Rows            []archHealthRow `json:"rows"`
	TotalViolations int             `json:"total_violations"`
	PerRepoTotal    map[int]int     `json:"per_repo_total"`
	PerAuthorTotal  map[string]int  `json:"per_author_total"`
}

// handleArchHealthRoot dispatches GET /api/arch-health/...
//
//	/api/arch-health/latest    → most recent month aggregates
//	/api/arch-health/<YYYY-MM> → specific month aggregates
//	/api/arch-health/months    → distinct months
func handleArchHealthRoot(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/arch-health/")
		path = strings.TrimSuffix(path, "/")
		if path == "" || path == "months" {
			handleArchHealthMonthsList(db, w, r)
			return
		}
		if path == "latest" {
			handleArchHealthLatest(db, w, r)
			return
		}
		// Otherwise treat as a literal YYYY-MM token.
		writeArchHealthMonth(db, w, path)
	}
}

func handleArchHealthLatest(db *sql.DB, w http.ResponseWriter, _ *http.Request) {
	months, err := store.ListArchHealthMonths(db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(months) == 0 {
		// Empty rather than 404 — the dashboard tab can render an
		// "Awaiting first run" stub instead of a hard error.
		json.NewEncoder(w).Encode(archHealthResponse{
			Month:          "",
			Rows:           []archHealthRow{},
			PerRepoTotal:   map[int]int{},
			PerAuthorTotal: map[string]int{},
		})
		return
	}
	latest := months[len(months)-1]
	writeArchHealthMonth(db, w, latest)
}

func handleArchHealthMonthsList(db *sql.DB, w http.ResponseWriter, _ *http.Request) {
	months, err := store.ListArchHealthMonths(db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if months == nil {
		months = []string{}
	}
	json.NewEncoder(w).Encode(map[string]any{"months": months})
}

func writeArchHealthMonth(db *sql.DB, w http.ResponseWriter, month string) {
	aggs, err := store.ListArchHealthAggregatesForMonth(db, month)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]archHealthRow, 0, len(aggs))
	perRepo := map[int]int{}
	perAuthor := map[string]int{}
	total := 0
	for _, a := range aggs {
		rows = append(rows, archHealthRow{
			RuleID:         a.RuleID,
			RepoID:         a.RepoID,
			AuthorType:     a.AuthorType,
			ViolationCount: a.ViolationCount,
			ReportMonth:    a.ReportMonth,
		})
		perRepo[a.RepoID] += a.ViolationCount
		perAuthor[a.AuthorType] += a.ViolationCount
		total += a.ViolationCount
	}
	json.NewEncoder(w).Encode(archHealthResponse{
		Month:           month,
		Rows:            rows,
		TotalViolations: total,
		PerRepoTotal:    perRepo,
		PerAuthorTotal:  perAuthor,
	})
}

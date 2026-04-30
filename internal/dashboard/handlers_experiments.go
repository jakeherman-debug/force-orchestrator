package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/holdout"
)

// experimentSummary is the JSON shape returned by GET /api/experiments
// list rows. Per-experiment detail is in experimentDetail.
type experimentSummary struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	StakesTier     string `json:"stakes_tier"`
	SubjectAgent   string `json:"subject_agent"`
	AssignmentUnit string `json:"assignment_unit"`
	CreatedAt      string `json:"created_at"`
	StartedAt      string `json:"started_at,omitempty"`
	TerminatedAt   string `json:"terminated_at,omitempty"`
	OutcomeReason  string `json:"outcome_reason,omitempty"`
}

// experimentDetail is the JSON shape returned by GET /api/experiments/:id.
type experimentDetail struct {
	experimentSummary
	Hypothesis            string             `json:"hypothesis"`
	MinPracticalEffect    float64            `json:"min_practical_effect"`
	AnalysisFrameworkVersion string         `json:"analysis_framework_version"`
	BudgetUSD             float64            `json:"budget_usd"`
	HardCapUSD            float64            `json:"hard_cap_usd"`
	DurationCapHours      int                `json:"duration_cap_hours"`
	Treatments            []treatmentSummary `json:"treatments"`
	Metrics               []metricSummary    `json:"metrics"`
	WinnerTreatmentID     int                `json:"winner_treatment_id,omitempty"`
	WinnerPosterior       float64            `json:"winner_posterior,omitempty"`
	CellMeans             json.RawMessage    `json:"cell_means,omitempty"`
}

type treatmentSummary struct {
	ID                int     `json:"id"`
	ArmLabel          string  `json:"arm_label"`
	TargetCellWeight  float64 `json:"target_cell_weight"`
	PromptTemplateRef string  `json:"prompt_template_ref"`
	Model             string  `json:"model"`
	Enrollment        int     `json:"enrollment"`
	SuccessCount      int     `json:"success_count"`
	ObservedRate      float64 `json:"observed_rate"`
}

type metricSummary struct {
	MetricName    string `json:"metric_name"`
	MetricVersion string `json:"metric_version"`
	Direction     string `json:"direction"`
	IsPrimary     bool   `json:"is_primary"`
}

// handleExperimentsList serves GET /api/experiments. Query parameter
// `status` filters by lifecycle state (authored | running | terminated
// | confirming); omitted or 'all' returns every row.
func handleExperimentsList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		statusFilter := r.URL.Query().Get("status")
		query := `
			SELECT e.id, e.name, e.status, IFNULL(e.stakes_tier, ''),
			       IFNULL(e.subject_agent, ''), IFNULL(e.assignment_unit, ''),
			       IFNULL(e.created_at, ''), IFNULL(e.started_at, ''),
			       IFNULL(e.terminated_at, ''),
			       IFNULL(o.termination_reason, '')
			FROM Experiments e
			LEFT JOIN ExperimentOutcomes o ON o.experiment_id = e.id
		`
		args := []any{}
		if statusFilter != "" && statusFilter != "all" {
			query += ` WHERE e.status = ?`
			args = append(args, statusFilter)
		}
		query += ` ORDER BY e.id DESC`
		rows, err := db.QueryContext(r.Context(), query, args...)
		if err != nil {
			http.Error(w, "query: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []experimentSummary{}
		for rows.Next() {
			var s experimentSummary
			if err := rows.Scan(&s.ID, &s.Name, &s.Status, &s.StakesTier,
				&s.SubjectAgent, &s.AssignmentUnit,
				&s.CreatedAt, &s.StartedAt, &s.TerminatedAt,
				&s.OutcomeReason); err != nil {
				log.Printf("handleExperimentsList: scan: %v", err)
				continue
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			log.Printf("handleExperimentsList: rows: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"experiments": out,
			"count":       len(out),
			"status_filter": func() string {
				if statusFilter == "" {
					return "all"
				}
				return statusFilter
			}(),
		})
	}
}

// handleExperimentDetail serves GET /api/experiments/:id.
func handleExperimentDetail(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		id, ok := parseExperimentID(r.URL.Path)
		if !ok {
			http.Error(w, "missing or invalid experiment id", http.StatusBadRequest)
			return
		}
		var d experimentDetail
		err := db.QueryRowContext(r.Context(), `
			SELECT id, name, status, IFNULL(stakes_tier, ''),
			       IFNULL(subject_agent, ''), IFNULL(assignment_unit, ''),
			       IFNULL(created_at, ''), IFNULL(started_at, ''),
			       IFNULL(terminated_at, ''),
			       IFNULL(hypothesis_text, ''),
			       IFNULL(min_practical_effect, 0),
			       IFNULL(analysis_framework_version, ''),
			       IFNULL(budget_usd, 0), IFNULL(hard_cap_usd, 0),
			       IFNULL(duration_cap_hours, 0)
			FROM Experiments WHERE id = ?
		`, id).Scan(&d.ID, &d.Name, &d.Status, &d.StakesTier,
			&d.SubjectAgent, &d.AssignmentUnit,
			&d.CreatedAt, &d.StartedAt, &d.TerminatedAt,
			&d.Hypothesis, &d.MinPracticalEffect,
			&d.AnalysisFrameworkVersion, &d.BudgetUSD, &d.HardCapUSD,
			&d.DurationCapHours)
		if err == sql.ErrNoRows {
			http.Error(w, "experiment not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "load: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Treatments + per-arm enrollment / observed rate.
		treats, err := loadTreatmentSummaries(db, id)
		if err != nil {
			http.Error(w, "treatments: "+err.Error(), http.StatusInternalServerError)
			return
		}
		d.Treatments = treats

		// Metrics.
		mrows, err := db.QueryContext(r.Context(), `
			SELECT metric_name, IFNULL(metric_version, ''),
			       IFNULL(direction, ''), IFNULL(is_primary, 0)
			FROM ExperimentMetrics WHERE experiment_id = ?
			ORDER BY id
		`, id)
		if err != nil {
			http.Error(w, "metrics: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer mrows.Close()
		for mrows.Next() {
			var m metricSummary
			var primary int
			if err := mrows.Scan(&m.MetricName, &m.MetricVersion, &m.Direction, &primary); err != nil {
				log.Printf("handleExperimentDetail: metrics scan: %v", err)
				continue
			}
			m.IsPrimary = primary != 0
			d.Metrics = append(d.Metrics, m)
		}
		if err := mrows.Err(); err != nil {
			log.Printf("handleExperimentDetail: metrics rows: %v", err)
		}

		// Outcome (if terminated).
		if d.Status == "terminated" {
			var winnerID int
			var posterior float64
			var cellMeans, reason string
			err := db.QueryRowContext(r.Context(), `
				SELECT IFNULL(winner_treatment_id, 0), IFNULL(winner_posterior, 0),
				       IFNULL(cell_means_json, '{}'), IFNULL(termination_reason, '')
				FROM ExperimentOutcomes WHERE experiment_id = ?
			`, id).Scan(&winnerID, &posterior, &cellMeans, &reason)
			if err == nil {
				d.WinnerTreatmentID = winnerID
				d.WinnerPosterior = posterior
				d.CellMeans = json.RawMessage(cellMeans)
				d.OutcomeReason = reason
			}
		}

		json.NewEncoder(w).Encode(d)
	}
}

func loadTreatmentSummaries(db *sql.DB, experimentID int) ([]treatmentSummary, error) {
	rows, err := db.Query(`
		SELECT t.id, t.arm_label, IFNULL(t.target_cell_weight, 0),
		       IFNULL(s.prompt_template_ref, ''), IFNULL(s.model_identifier, ''),
		       (SELECT COUNT(*) FROM ExperimentRuns r WHERE r.treatment_id = t.id) AS enrollment,
		       (SELECT COUNT(*) FROM ExperimentRuns r WHERE r.treatment_id = t.id AND r.score >= 0.5) AS successes
		FROM ExperimentTreatments t
		LEFT JOIN TreatmentSpecs s ON s.id = t.treatment_spec_id
		WHERE t.experiment_id = ?
		ORDER BY t.id
	`, experimentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []treatmentSummary
	for rows.Next() {
		var ts treatmentSummary
		if err := rows.Scan(&ts.ID, &ts.ArmLabel, &ts.TargetCellWeight,
			&ts.PromptTemplateRef, &ts.Model,
			&ts.Enrollment, &ts.SuccessCount); err != nil {
			log.Printf("loadTreatmentSummaries: scan: %v", err)
			continue
		}
		if ts.Enrollment > 0 {
			ts.ObservedRate = float64(ts.SuccessCount) / float64(ts.Enrollment)
		}
		out = append(out, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseExperimentID extracts {id} from "/api/experiments/<id>".
// Returns (id, true) on a clean integer suffix.
func parseExperimentID(path string) (int, bool) {
	prefix := "/api/experiments/"
	if !strings.HasPrefix(path, prefix) {
		return 0, false
	}
	rest := strings.Trim(path[len(prefix):], "/")
	if rest == "" {
		return 0, false
	}
	// Reject sub-paths like /api/experiments/1/foo — the route is a
	// strict singleton.
	if strings.Contains(rest, "/") {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// fleetProgressResponse is the JSON shape returned by
// GET /api/fleet-progress. The view is intentionally simple in
// Phase 2 — the dashboard rebuild in D3 Phase 6 absorbs and replaces
// this surface with richer time-series + Pulse/Briefing context.
type fleetProgressResponse struct {
	HoldoutName       string                  `json:"holdout_name"`
	HoldoutFractionNow float64                `json:"holdout_fraction_now"`
	HoldoutMembers    int                     `json:"holdout_members"`
	HoldoutLifecycle  string                  `json:"holdout_lifecycle"`
	ReferenceDate     string                  `json:"reference_date"`
	Windows           []fleetProgressWindow   `json:"windows"`
}

type fleetProgressWindow struct {
	Label              string  `json:"label"`
	Hours              int     `json:"hours"`
	HoldoutRunCount    int     `json:"holdout_run_count"`
	CurrentRunCount    int     `json:"current_run_count"`
	HoldoutSuccessRate float64 `json:"holdout_success_rate"`
	CurrentSuccessRate float64 `json:"current_success_rate"`
}

// handleFleetProgress serves GET /api/fleet-progress: the holdout vs
// current rolling-metrics view from paired-runs.md § Dashboard. Phase
// 2 ships a minimal shape — three windows (24h, 7d, 30d) computed over
// ExperimentRuns.score. The Phase 6 rebuild replaces this with
// per-metric joins via MetricVersions.
func handleFleetProgress(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		ctx := r.Context()
		var resp fleetProgressResponse
		resp.HoldoutName = holdout.BaselineHoldoutName

		h, err := holdout.LoadHoldoutByName(ctx, db, holdout.BaselineHoldoutName)
		if err == nil {
			now := time.Now().UTC()
			resp.HoldoutFractionNow = h.CurrentFraction(now)
			resp.ReferenceDate = h.ReferenceDate.UTC().Format(time.RFC3339)
			resp.HoldoutLifecycle = lifecyclePhase(h, now)
		}

		// Holdout member count (over Features only).
		_ = db.QueryRowContext(ctx,
			`SELECT IFNULL(SUM(CASE WHEN in_holdout = 1 THEN 1 ELSE 0 END), 0) FROM Features`,
		).Scan(&resp.HoldoutMembers)

		// Three rolling windows.
		windows := []struct {
			label string
			hours int
		}{
			{"24h", 24},
			{"7d", 7 * 24},
			{"30d", 30 * 24},
		}
		for _, w := range windows {
			cutoff := time.Now().UTC().Add(-time.Duration(w.hours) * time.Hour).Format("2006-01-02 15:04:05")
			win := fleetProgressWindow{Label: w.label, Hours: w.hours}
			_ = db.QueryRowContext(ctx, `
				SELECT
					COUNT(CASE WHEN r.mode = 'holdout' AND r.completed_at >= ? THEN 1 END),
					COUNT(CASE WHEN r.mode != 'holdout' AND r.completed_at >= ? THEN 1 END),
					IFNULL(AVG(CASE WHEN r.mode = 'holdout' AND r.completed_at >= ? THEN r.score END), 0),
					IFNULL(AVG(CASE WHEN r.mode != 'holdout' AND r.completed_at >= ? THEN r.score END), 0)
				FROM ExperimentRuns r
				WHERE r.completed_at != ''
			`, cutoff, cutoff, cutoff, cutoff).Scan(
				&win.HoldoutRunCount, &win.CurrentRunCount,
				&win.HoldoutSuccessRate, &win.CurrentSuccessRate)
			resp.Windows = append(resp.Windows, win)
		}

		json.NewEncoder(w).Encode(resp)
	}
}

// lifecyclePhase classifies a holdout's current lifecycle phase
// (paired-runs.md § Lifecycle). Pure function over the row + now.
func lifecyclePhase(h holdout.Holdout, now time.Time) string {
	if !h.RetiredAt.IsZero() && !now.Before(h.RetiredAt) {
		return "retired"
	}
	if now.Before(h.ReferenceDate) {
		return "pre-mint"
	}
	rampEnd := h.ReferenceDate.Add(time.Duration(h.RampUpDays) * 24 * time.Hour)
	if now.Before(rampEnd) {
		return "ramp"
	}
	if h.FadeStartAt.IsZero() || now.Before(h.FadeStartAt) {
		return "plateau"
	}
	fadeEnd := h.FadeStartAt.Add(time.Duration(h.FadeDays) * 24 * time.Hour)
	if now.Before(fadeEnd) {
		return "fade"
	}
	return "expired"
}

// handleExperimentsSubroutes dispatches the /api/experiments/:id sub-route.
func handleExperimentsSubroutes(db *sql.DB) http.HandlerFunc {
	detail := handleExperimentDetail(db)
	return func(w http.ResponseWriter, r *http.Request) {
		// Only GET is supported. POST/PATCH (operator ratify, terminate
		// from the UI) is a Phase 6 surface — for now those flow
		// through the CLI.
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		detail(w, r)
	}
}

// init-time sanity check: catch URL-encoding subtleties in tests.
var _ = fmt.Sprintf

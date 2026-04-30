package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

// disagreementRateRow is the JSON shape for one row in
// /api/disagreement-rates. The dashboard renders one card per pair_name
// per window; the card shows the rate plus the sample/disagreement
// counts so the operator can judge confidence.
type disagreementRateRow struct {
	PairName          string  `json:"pair_name"`
	WindowStart       string  `json:"window_start"`
	WindowEnd         string  `json:"window_end"`
	WindowLengthHours int     `json:"window_length_hours"`
	SampleCount       int     `json:"sample_count"`
	DisagreementCount int     `json:"disagreement_count"`
	Rate              float64 `json:"rate"`
	ComputedAt        string  `json:"computed_at"`
}

// handleDisagreementRates serves GET /api/disagreement-rates. Returns
// the latest row per (pair_name, window-length) — the dashboard
// renders one card per pair-window combination.
//
// "Latest" is determined by computed_at DESC; the dog UPSERTs on the
// (pair, window_start, window_end) tuple so the most recently
// computed_at per pair-window is the freshest aggregate.
//
// Operator-readable: no auth required (matches the rest of /api/* —
// the dashboard binds 127.0.0.1 only).
func handleDisagreementRates(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)

		// Pull the most-recently-computed row per (pair_name, window
		// duration). Window duration is approximated from the
		// difference between window_end and window_start in hours so
		// the dashboard can render 7d/30d/90d cards distinctly even
		// though the schema doesn't store the duration directly.
		rows, err := db.Query(`
			WITH latest AS (
				SELECT
					pair_name,
					CAST(ROUND((julianday(window_end) - julianday(window_start)) * 24) AS INTEGER) AS window_hours,
					MAX(computed_at) AS max_computed_at
				FROM DisagreementPairs
				GROUP BY pair_name, window_hours
			)
			SELECT
				dp.pair_name,
				dp.window_start,
				dp.window_end,
				CAST(ROUND((julianday(dp.window_end) - julianday(dp.window_start)) * 24) AS INTEGER) AS window_length_hours,
				dp.sample_count,
				dp.disagreement_count,
				dp.rate,
				IFNULL(dp.computed_at, '')
			FROM DisagreementPairs dp
			JOIN latest l
			  ON l.pair_name = dp.pair_name
			 AND l.max_computed_at = dp.computed_at
			 AND l.window_hours = CAST(ROUND((julianday(dp.window_end) - julianday(dp.window_start)) * 24) AS INTEGER)
			ORDER BY dp.pair_name, window_length_hours
		`)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var out []disagreementRateRow
		for rows.Next() {
			var row disagreementRateRow
			if err := rows.Scan(
				&row.PairName,
				&row.WindowStart,
				&row.WindowEnd,
				&row.WindowLengthHours,
				&row.SampleCount,
				&row.DisagreementCount,
				&row.Rate,
				&row.ComputedAt,
			); err != nil {
				http.Error(w, `{"error":"scan failed"}`, http.StatusInternalServerError)
				return
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, `{"error":"rows iter failed"}`, http.StatusInternalServerError)
			return
		}
		// Always emit a JSON array (never null) so the dashboard JS
		// doesn't have to special-case empty results.
		if out == nil {
			out = []disagreementRateRow{}
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandleDisagreementRates_EmptyDB returns an empty array (NOT
// null) when no rows exist.
func TestHandleDisagreementRates_EmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/disagreement-rates", nil)
	handleDisagreementRates(db)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("empty-DB body = %q, want %q", body, "[]")
	}
}

// TestHandleDisagreementRates_ReturnsLatestPerPairWindow seeds
// DisagreementPairs with two rows for the same (pair, window-length)
// — one older, one newer — and asserts only the newer row is
// returned.
func TestHandleDisagreementRates_ReturnsLatestPerPairWindow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Older row (computed_at=2026-04-29 10:00:00) — same pair + same
	// window length (24h). Older.
	_, err := db.Exec(`
		INSERT INTO DisagreementPairs
			(pair_name, window_start, window_end, sample_count, disagreement_count, rate, computed_at)
		VALUES
			('captain-council-reject', '2026-04-28 10:00:00', '2026-04-29 10:00:00', 100, 10, 0.10, '2026-04-29 10:00:00')
	`)
	if err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	// Newer row — same pair + same 24h length, fresher computed_at.
	_, err = db.Exec(`
		INSERT INTO DisagreementPairs
			(pair_name, window_start, window_end, sample_count, disagreement_count, rate, computed_at)
		VALUES
			('captain-council-reject', '2026-04-29 11:00:00', '2026-04-30 11:00:00', 200, 30, 0.15, '2026-04-30 11:00:00')
	`)
	if err != nil {
		t.Fatalf("insert new row: %v", err)
	}
	// Distinct pair (council-ci-fail) at 168h window (7d).
	_, err = db.Exec(`
		INSERT INTO DisagreementPairs
			(pair_name, window_start, window_end, sample_count, disagreement_count, rate, computed_at)
		VALUES
			('council-ci-fail', '2026-04-23 11:00:00', '2026-04-30 11:00:00', 50, 5, 0.10, '2026-04-30 11:00:00')
	`)
	if err != nil {
		t.Fatalf("insert second-pair row: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/disagreement-rates", nil)
	handleDisagreementRates(db)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rows []disagreementRateRow
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode body: %v\nbody: %s", err, rec.Body.String())
	}
	// Expect 2 rows: latest captain-council-reject (24h) + the
	// council-ci-fail (168h).
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2 (one per (pair, window-length)): %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.PairName == "captain-council-reject" {
			if r.SampleCount != 200 || r.DisagreementCount != 30 {
				t.Errorf("captain-council-reject: returned old row, not latest: %+v", r)
			}
			if r.Rate != 0.15 {
				t.Errorf("captain-council-reject Rate = %v, want 0.15", r.Rate)
			}
			if r.WindowLengthHours != 24 {
				t.Errorf("captain-council-reject WindowLengthHours = %d, want 24", r.WindowLengthHours)
			}
		}
		if r.PairName == "council-ci-fail" {
			if r.WindowLengthHours != 168 {
				t.Errorf("council-ci-fail WindowLengthHours = %d, want 168 (7d)", r.WindowLengthHours)
			}
		}
	}
}

// TestHandleDisagreementRates_DistinctWindowsKeptSeparate seeds two
// rows for the same pair at different window lengths (24h and 168h)
// and asserts BOTH are returned (one per window-length) — the
// dashboard renders three cards per pair (7d/30d/90d), so distinct
// windows must not collapse.
func TestHandleDisagreementRates_DistinctWindowsKeptSeparate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, _ = db.Exec(`
		INSERT INTO DisagreementPairs
			(pair_name, window_start, window_end, sample_count, disagreement_count, rate, computed_at)
		VALUES
			('captain-council-reject', '2026-04-29 11:00:00', '2026-04-30 11:00:00', 100, 10, 0.10, '2026-04-30 11:00:00'),
			('captain-council-reject', '2026-04-23 11:00:00', '2026-04-30 11:00:00', 700, 70, 0.10, '2026-04-30 11:00:00')
	`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/disagreement-rates", nil)
	handleDisagreementRates(db)(rec, req)

	var rows []disagreementRateRow
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows (24h + 168h), got %d: %+v", len(rows), rows)
	}
	hours := map[int]bool{}
	for _, r := range rows {
		hours[r.WindowLengthHours] = true
	}
	if !hours[24] || !hours[168] {
		t.Errorf("missing window lengths; got %v, want both 24 and 168", hours)
	}
}

package metrics

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestMetricByPromptVersion_GroupsByVersion seeds TaskHistory with two
// distinct prompt_versions per agent and asserts the metric is grouped
// correctly.
//
// Fixture for captain_approval_rate:
//   - prompt_version "v1": 4 captain rows, 2 Completed → rate 0.5
//   - prompt_version "v2": 2 captain rows, 2 Completed → rate 1.0
func TestMetricByPromptVersion_GroupsByVersion(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := time.Now().UTC().Add(-30 * time.Minute) // place rows safely inside the 24h window
	addCaptain(t, db, "v1", "Completed", now)
	addCaptain(t, db, "v1", "Completed", now)
	addCaptain(t, db, "v1", "Rejected", now)
	addCaptain(t, db, "v1", "Rejected", now)
	addCaptain(t, db, "v2", "Completed", now)
	addCaptain(t, db, "v2", "Completed", now)

	since := time.Now().Add(-24 * time.Hour)
	got, err := MetricByPromptVersion(context.Background(), db, "captain_approval_rate", since)
	if err != nil {
		t.Fatalf("MetricByPromptVersion: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d groups, want 2: %v", len(got), got)
	}
	if !floatNear(got["v1"], 0.5, 0.0001) {
		t.Errorf("v1 rate = %v, want 0.5", got["v1"])
	}
	if !floatNear(got["v2"], 1.0, 0.0001) {
		t.Errorf("v2 rate = %v, want 1.0", got["v2"])
	}
}

// TestMetricByPromptVersion_FiltersEmptyVersion asserts rows whose
// prompt_version is empty (legacy data) are excluded from the result.
func TestMetricByPromptVersion_FiltersEmptyVersion(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := time.Now().UTC().Add(-30 * time.Minute)
	// Two rows with prompt_version, two without.
	addCaptain(t, db, "v1", "Completed", now)
	addCaptain(t, db, "v1", "Rejected", now)
	addCaptain(t, db, "", "Completed", now)
	addCaptain(t, db, "", "Completed", now)

	got, err := MetricByPromptVersion(context.Background(), db, "captain_approval_rate", time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("MetricByPromptVersion: %v", err)
	}
	if _, has := got[""]; has {
		t.Errorf("empty-version key leaked into result: %v", got)
	}
	if len(got) != 1 {
		t.Errorf("got %d groups, want 1: %v", len(got), got)
	}
	if !floatNear(got["v1"], 0.5, 0.0001) {
		t.Errorf("v1 rate = %v, want 0.5", got["v1"])
	}
}

// TestMetricByPromptVersion_ExcludesRowsBeforeSince asserts the
// `since` cutoff is honored — rows older than the cutoff don't pollute
// the rate.
func TestMetricByPromptVersion_ExcludesRowsBeforeSince(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	insideWindow := time.Now().UTC().Add(-30 * time.Minute)
	beforeWindow := time.Now().UTC().Add(-3 * time.Hour)

	// In-window: 1 Completed.
	addCaptain(t, db, "v1", "Completed", insideWindow)
	// Pre-window: 1 Rejected — must be ignored.
	addCaptain(t, db, "v1", "Rejected", beforeWindow)

	since := time.Now().Add(-1 * time.Hour)
	got, err := MetricByPromptVersion(context.Background(), db, "captain_approval_rate", since)
	if err != nil {
		t.Fatalf("MetricByPromptVersion: %v", err)
	}
	if !floatNear(got["v1"], 1.0, 0.0001) {
		t.Errorf("v1 rate = %v, want 1.0 (only the in-window row should count)", got["v1"])
	}
}

// TestMetricByPromptVersion_UnknownMetric returns ErrUnknownGroupedMetric.
func TestMetricByPromptVersion_UnknownMetric(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, err := MetricByPromptVersion(context.Background(), db, "no-such-metric", time.Now().Add(-1*time.Hour))
	if !errors.Is(err, ErrUnknownGroupedMetric) {
		t.Errorf("expected ErrUnknownGroupedMetric, got %v", err)
	}
}

// TestMetricByPromptVersion_AllAgents covers the catalog: each
// built-in metric returns sensible results given a small fixture.
func TestMetricByPromptVersion_AllAgents(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := time.Now().UTC().Add(-30 * time.Minute)
	addHistory(t, db, "Captain-1", "v1", "Completed", now)
	addHistory(t, db, "Captain-1", "v1", "Rejected", now)
	addHistory(t, db, "Council-1", "vC", "AwaitingSubPRCI", now)
	addHistory(t, db, "Council-1", "vC", "Rejected", now)
	addHistory(t, db, "Medic-1", "vM", "Completed", now)
	addHistory(t, db, "Medic-1", "vM", "Failed", now)

	since := time.Now().Add(-1 * time.Hour)

	for _, m := range []struct {
		name     string
		key      string
		want     float64
	}{
		{"captain_approval_rate", "v1", 0.5},
		{"captain_rejection_rate", "v1", 0.5},
		{"council_approval_rate", "vC", 0.5},
		{"medic_completion_rate", "vM", 0.5},
	} {
		got, err := MetricByPromptVersion(context.Background(), db, m.name, since)
		if err != nil {
			t.Errorf("%s: error: %v", m.name, err)
			continue
		}
		if !floatNear(got[m.key], m.want, 0.0001) {
			t.Errorf("%s [%s]: got %v, want %v", m.name, m.key, got[m.key], m.want)
		}
	}

	// task_count covers all rows in the window (6 rows across 3 versions).
	counts, err := MetricByPromptVersion(context.Background(), db, "task_count", since)
	if err != nil {
		t.Fatalf("task_count: %v", err)
	}
	if counts["v1"] != 2 || counts["vC"] != 2 || counts["vM"] != 2 {
		t.Errorf("task_count grouped wrong: %v", counts)
	}
}

// TestMetricByPromptVersion_RegisterGroupedMetric — extension path.
func TestMetricByPromptVersion_RegisterGroupedMetric(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	now := time.Now().UTC().Add(-30 * time.Minute)
	addCaptain(t, db, "v1", "Completed", now)

	if err := RegisterGroupedMetric(GroupedMetric{
		Name: "test_synthetic_count",
		QuerySQL: `
			SELECT prompt_version, CAST(COUNT(*) AS REAL)
			FROM TaskHistory
			WHERE created_at >= ?
			  AND IFNULL(prompt_version, '') != ''
			GROUP BY prompt_version
		`,
	}); err != nil {
		t.Fatalf("RegisterGroupedMetric: %v", err)
	}
	got, err := MetricByPromptVersion(context.Background(), db, "test_synthetic_count", time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("MetricByPromptVersion: %v", err)
	}
	if got["v1"] != 1 {
		t.Errorf("synthetic count for v1 = %v, want 1", got["v1"])
	}

	// Reject empty name / empty SQL.
	if err := RegisterGroupedMetric(GroupedMetric{Name: "", QuerySQL: "x"}); err == nil {
		t.Errorf("expected error on empty name")
	}
	if err := RegisterGroupedMetric(GroupedMetric{Name: "x", QuerySQL: ""}); err == nil {
		t.Errorf("expected error on empty SQL")
	}
}

// addCaptain inserts one Captain TaskHistory row at the given timestamp.
func addCaptain(t *testing.T, db *sql.DB, promptVersion, outcome string, ts time.Time) {
	t.Helper()
	addHistory(t, db, "Captain-1", promptVersion, outcome, ts)
}

func addHistory(t *testing.T, db *sql.DB, agent, promptVersion, outcome string, ts time.Time) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome, prompt_version, created_at)
		VALUES (?, 1, ?, 'session', '', ?, ?, ?)
	`, fakeTaskID(), agent, outcome, promptVersion, ts.Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatalf("insert TaskHistory: %v", err)
	}
}

var taskIDCounter int

func fakeTaskID() int {
	taskIDCounter++
	return 9000 + taskIDCounter
}

func floatNear(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

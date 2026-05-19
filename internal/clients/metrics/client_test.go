package metrics_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/metrics"
	"force-orchestrator/internal/store"
)

// ── In-process client (SQLite-backed) ────────────────────────────────

// TestInProcess_RegisterMetric_RoundTrip exercises the happy path:
// RegisterMetric writes a row, ListMetrics reads it back with every
// field intact (including the manifest_json-stored Units / OwningTeam).
func TestInProcess_RegisterMetric_RoundTrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	mv := metrics.MetricVersion{
		Name:        "captain-approval-rate",
		Version:     "1",
		Description: "fraction of PRs Captain approved on the first review",
		Units:       "fraction",
		Body:        "SELECT COUNT(*) FROM CaptainReviews WHERE outcome = 'approved'",
		OwningTeam:  "engineering",
	}
	if err := c.RegisterMetric(ctx, mv); err != nil {
		t.Fatalf("RegisterMetric: %v", err)
	}

	got, err := c.ListMetrics(ctx)
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListMetrics returned %d rows, want 1", len(got))
	}
	if got[0] != mv {
		t.Errorf("round-trip mismatch:\n got = %+v\nwant = %+v", got[0], mv)
	}
}

// TestInProcess_RegisterMetric_DuplicateRejected pins the immutability
// contract — re-registering (Name, Version) returns ErrMetricExists.
func TestInProcess_RegisterMetric_DuplicateRejected(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	mv := metrics.MetricVersion{Name: "m", Version: "1", Body: "SELECT 1"}
	if err := c.RegisterMetric(ctx, mv); err != nil {
		t.Fatalf("first RegisterMetric: %v", err)
	}
	if err := c.RegisterMetric(ctx, mv); !errors.Is(err, metrics.ErrMetricExists) {
		t.Errorf("second RegisterMetric: expected ErrMetricExists, got %v", err)
	}
}

// TestInProcess_RegisterMetric_ValidatesInputs ensures empty Name or
// Version is rejected before we ever touch the DB.
func TestInProcess_RegisterMetric_ValidatesInputs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	if err := c.RegisterMetric(ctx, metrics.MetricVersion{Version: "1"}); err == nil {
		t.Errorf("empty Name: expected error, got nil")
	}
	if err := c.RegisterMetric(ctx, metrics.MetricVersion{Name: "m"}); err == nil {
		t.Errorf("empty Version: expected error, got nil")
	}
}

// TestInProcess_RecordScoreAndScore_RoundTrip seeds an ExperimentRuns
// row, records a score against it, and reads it back. This is the
// load-bearing path EC's metric runners follow at scoring time.
func TestInProcess_RecordScoreAndScore_RoundTrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	runID := seedExperimentRun(t, db)

	if err := c.RecordScore(ctx, runID, "captain-approval-rate", "1", 0.85); err != nil {
		t.Fatalf("RecordScore: %v", err)
	}

	score, err := c.Score(ctx, runID, "captain-approval-rate", "1")
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score != 0.85 {
		t.Errorf("Score = %f, want 0.85", score)
	}
}

// TestInProcess_RecordScore_Idempotent verifies that recording the same
// triple twice is a no-op (CLAUDE.md testing-rule requirement).
func TestInProcess_RecordScore_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	runID := seedExperimentRun(t, db)

	for i := 0; i < 3; i++ {
		if err := c.RecordScore(ctx, runID, "m", "1", 0.5); err != nil {
			t.Fatalf("RecordScore (iteration %d): %v", i, err)
		}
	}
	score, err := c.Score(ctx, runID, "m", "1")
	if err != nil {
		t.Fatalf("Score after triple-record: %v", err)
	}
	if score != 0.5 {
		t.Errorf("Score = %f, want 0.5 (idempotence broken)", score)
	}
}

// TestInProcess_Score_MissReturnsErrNoScore covers the three distinct
// failure modes Score has to recognise: unknown run, NULL score on a
// real row, and metric_version mismatch.
func TestInProcess_Score_MissReturnsErrNoScore(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	// (a) unknown run
	if _, err := c.Score(ctx, 999, "m", "1"); !errors.Is(err, metrics.ErrNoScore) {
		t.Errorf("unknown run: expected ErrNoScore, got %v", err)
	}

	// (b) row exists, score still NULL
	runID := seedExperimentRun(t, db)
	if _, err := c.Score(ctx, runID, "m", "1"); !errors.Is(err, metrics.ErrNoScore) {
		t.Errorf("no score recorded: expected ErrNoScore, got %v", err)
	}

	// (c) row exists, score recorded against version "1", caller asks
	// for version "2".
	if err := c.RecordScore(ctx, runID, "m", "1", 0.7); err != nil {
		t.Fatalf("RecordScore: %v", err)
	}
	if _, err := c.Score(ctx, runID, "m", "2"); !errors.Is(err, metrics.ErrNoScore) {
		t.Errorf("version mismatch: expected ErrNoScore, got %v", err)
	}
}

// TestInProcess_Score_CrossMetricRejected verifies the score_source
// guard: if metric X scored run R, asking for metric Y on the same run
// returns ErrNoScore (no cross-metric leaks).
func TestInProcess_Score_CrossMetricRejected(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	runID := seedExperimentRun(t, db)
	if err := c.RecordScore(ctx, runID, "metric-A", "1", 0.9); err != nil {
		t.Fatalf("RecordScore: %v", err)
	}
	if _, err := c.Score(ctx, runID, "metric-B", "1"); !errors.Is(err, metrics.ErrNoScore) {
		t.Errorf("expected ErrNoScore on cross-metric read, got %v", err)
	}
}

// TestInProcess_RecordScore_UnknownRunRejected asserts that recording
// against a runID with no ExperimentRuns row returns an explicit error
// rather than silently inserting nothing.
func TestInProcess_RecordScore_UnknownRunRejected(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	if err := c.RecordScore(ctx, 999, "m", "1", 0.5); err == nil {
		t.Errorf("expected error on unknown run, got nil")
	}
}

// TestInProcess_RecordScore_ValidatesInputs rejects empty metricName /
// version before touching the DB; the score_source guard breaks down if
// either is the empty string.
func TestInProcess_RecordScore_ValidatesInputs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()
	runID := seedExperimentRun(t, db)

	if err := c.RecordScore(ctx, runID, "", "1", 0.5); err == nil {
		t.Errorf("empty metricName: expected error, got nil")
	}
	if err := c.RecordScore(ctx, runID, "m", "", 0.5); err == nil {
		t.Errorf("empty version: expected error, got nil")
	}
}

// TestInProcess_ListMetrics_Empty pins the "no rows" shape — returns
// empty slice (no error).
func TestInProcess_ListMetrics_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	got, err := c.ListMetrics(ctx)
	if err != nil {
		t.Fatalf("ListMetrics on empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d rows", len(got))
	}
}

// TestInProcess_ListMetrics_OrderingDeterministic ensures results are
// sorted by (metric_name, version). The dashboard depends on stable
// ordering.
func TestInProcess_ListMetrics_OrderingDeterministic(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	c := metrics.NewInProcess(db)
	ctx := context.Background()

	for _, mv := range []metrics.MetricVersion{
		{Name: "b-metric", Version: "2", Body: "SELECT 1"},
		{Name: "a-metric", Version: "1", Body: "SELECT 1"},
		{Name: "b-metric", Version: "1", Body: "SELECT 1"},
		{Name: "a-metric", Version: "2", Body: "SELECT 1"},
	} {
		if err := c.RegisterMetric(ctx, mv); err != nil {
			t.Fatalf("RegisterMetric(%s@%s): %v", mv.Name, mv.Version, err)
		}
	}
	got, err := c.ListMetrics(ctx)
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	want := []struct{ name, version string }{
		{"a-metric", "1"},
		{"a-metric", "2"},
		{"b-metric", "1"},
		{"b-metric", "2"},
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Version != w.version {
			t.Errorf("ListMetrics[%d] = (%s, %s); want (%s, %s)", i, got[i].Name, got[i].Version, w.name, w.version)
		}
	}
}

// TestInProcess_NilDB_FailsClosed makes sure every method on a client
// constructed with a nil db returns a real error rather than panicking
// on a nil pointer dereference. The constructor itself stays permissive
// (so daemon-wiring code that passes a sentinel during startup doesn't
// crash) but the methods must fail closed.
func TestInProcess_NilDB_FailsClosed(t *testing.T) {
	c := metrics.NewInProcess(nil)
	ctx := context.Background()

	if err := c.RegisterMetric(ctx, metrics.MetricVersion{Name: "m", Version: "1"}); err == nil {
		t.Errorf("RegisterMetric on nil db: expected error, got nil")
	}
	if _, err := c.Score(ctx, 1, "m", "1"); err == nil {
		t.Errorf("Score on nil db: expected error, got nil")
	}
	if err := c.RecordScore(ctx, 1, "m", "1", 0.5); err == nil {
		t.Errorf("RecordScore on nil db: expected error, got nil")
	}
	if _, err := c.ListMetrics(ctx); err == nil {
		t.Errorf("ListMetrics on nil db: expected error, got nil")
	}
}

// seedExperimentRun INSERTs a minimal ExperimentRuns row and returns its
// id. The row leaves score=NULL so tests can independently exercise the
// "no score recorded yet" path.
func seedExperimentRun(t *testing.T, db *sql.DB) int {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode)
		VALUES (1, 1, 'task', 100, 'paired_real')
	`)
	if err != nil {
		t.Fatalf("seed ExperimentRuns: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed ExperimentRuns LastInsertId: %v", err)
	}
	return int(id)
}

// ── Mock client (test-side fake) ─────────────────────────────────────

func TestMock_RegisterAndRecordRoundTrip(t *testing.T) {
	m := metrics.NewMock()
	if err := m.RegisterMetric(context.Background(), metrics.MetricVersion{
		Name: "captain-approval-rate", Version: "1",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := m.RecordScore(context.Background(), 7, "captain-approval-rate", "1", 0.85); err != nil {
		t.Fatalf("RecordScore: %v", err)
	}
	score, err := m.Score(context.Background(), 7, "captain-approval-rate", "1")
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score != 0.85 {
		t.Errorf("score = %f, want 0.85", score)
	}
}

func TestMock_RegisterDuplicateRejected(t *testing.T) {
	m := metrics.NewMock()
	mv := metrics.MetricVersion{Name: "x", Version: "1"}
	_ = m.RegisterMetric(context.Background(), mv)
	if err := m.RegisterMetric(context.Background(), mv); !errors.Is(err, metrics.ErrMetricExists) {
		t.Errorf("expected ErrMetricExists on duplicate registration, got %v", err)
	}
}

func TestMock_ScoreMiss(t *testing.T) {
	m := metrics.NewMock()
	if _, err := m.Score(context.Background(), 1, "x", "1"); !errors.Is(err, metrics.ErrNoScore) {
		t.Errorf("expected ErrNoScore on miss, got %v", err)
	}
}

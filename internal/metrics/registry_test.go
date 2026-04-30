package metrics

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestLoadManifest_SampleMetric(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "metrics", "captain_rejection_rate", "2026-04-23.manifest.yaml")
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "captain_rejection_rate" {
		t.Errorf("Name: got %q, want %q", m.Name, "captain_rejection_rate")
	}
	if m.Version != "2026-04-23" {
		t.Errorf("Version: got %q, want %q", m.Version, "2026-04-23")
	}
	if m.Direction != "lower_is_better" {
		t.Errorf("Direction: got %q, want %q", m.Direction, "lower_is_better")
	}
}

func TestLoadManifest_RejectsBadDirection(t *testing.T) {
	_, err := parseManifest("name: x\nversion: 1\ndirection: wrong\n")
	if err == nil {
		t.Fatalf("expected error on bad direction")
	}
	if !strings.Contains(err.Error(), "direction") {
		t.Errorf("error message lacks `direction` hint: %v", err)
	}
}

func TestLoadManifest_RequiresName(t *testing.T) {
	_, err := parseManifest("version: 1\ndirection: higher_is_better\n")
	if err == nil {
		t.Fatalf("expected error on missing name")
	}
}

func TestRegisterMetric_RoundTrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	m := Metric{
		Name:        "test_metric",
		Version:     "2026-04-29",
		SQLBody:     "SELECT 0 AS score;",
		TestSQL:     "-- noop",
		Description: "test",
	}
	if err := RegisterMetric(ctx, db, m); err != nil {
		t.Fatalf("RegisterMetric: %v", err)
	}
	got, err := LookupMetric(ctx, db, "test_metric")
	if err != nil {
		t.Fatalf("LookupMetric: %v", err)
	}
	if got.Name != m.Name || got.Version != m.Version {
		t.Errorf("LookupMetric round-trip mismatch: got %+v", got)
	}
	if got.SQLBody != m.SQLBody {
		t.Errorf("SQLBody not preserved: got %q, want %q", got.SQLBody, m.SQLBody)
	}
}

func TestRegisterMetric_IdempotentSameBody(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	m := Metric{Name: "x", Version: "1", SQLBody: "SELECT 1;"}
	if err := RegisterMetric(ctx, db, m); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := RegisterMetric(ctx, db, m); err != nil {
		t.Fatalf("second register: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM MetricVersions WHERE metric_name='x'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("idempotent re-register: got %d rows, want 1", n)
	}
}

func TestRegisterMetric_RejectsDifferentBodySameVersion(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	if err := RegisterMetric(ctx, db, Metric{Name: "x", Version: "1", SQLBody: "SELECT 1;"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := RegisterMetric(ctx, db, Metric{Name: "x", Version: "1", SQLBody: "SELECT 2; -- different"})
	if err == nil {
		t.Fatalf("expected ErrMetricExists for differing body at same (name, version)")
	}
}

func TestLookupMetric_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := LookupMetric(context.Background(), db, "no-such-metric"); err == nil {
		t.Fatalf("expected ErrMetricNotFound")
	}
}

func TestLoadFromDir_PicksUpSampleMetric(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	root := repoRoot(t)
	dir := filepath.Join(root, "metrics")
	n, err := LoadFromDir(ctx, db, dir)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if n < 1 {
		t.Errorf("LoadFromDir: registered %d metrics; expected ≥ 1 (sample metric)", n)
	}
	got, err := LookupMetric(ctx, db, "captain_rejection_rate")
	if err != nil {
		t.Fatalf("LookupMetric after LoadFromDir: %v", err)
	}
	if got.Version != "2026-04-23" {
		t.Errorf("loaded version: got %q, want %q", got.Version, "2026-04-23")
	}
	if !strings.Contains(got.SQLBody, "captain_rejection_rate") {
		t.Errorf("SQL body did not load: %q", got.SQLBody[:min(80, len(got.SQLBody))])
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller")
	}
	// internal/metrics/<this>.go → ../..
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

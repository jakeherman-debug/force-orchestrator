package metrics_test

import (
	"context"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/metrics"
)

func TestInProcess_StubReturnsErrNotImplemented(t *testing.T) {
	c := metrics.NewInProcess()
	ctx := context.Background()

	if err := c.RegisterMetric(ctx, metrics.MetricVersion{Name: "x", Version: "1"}); !errors.Is(err, metrics.ErrNotImplemented) {
		t.Errorf("RegisterMetric: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.Score(ctx, 1, "x", "1"); !errors.Is(err, metrics.ErrNotImplemented) {
		t.Errorf("Score: expected ErrNotImplemented, got %v", err)
	}
	if err := c.RecordScore(ctx, 1, "x", "1", 0.5); !errors.Is(err, metrics.ErrNotImplemented) {
		t.Errorf("RecordScore: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.ListMetrics(ctx); !errors.Is(err, metrics.ErrNotImplemented) {
		t.Errorf("ListMetrics: expected ErrNotImplemented, got %v", err)
	}
}

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

package metrics

import (
	"context"
	"fmt"
	"sync"
)

// MockClient is the test-side Client backing. Default behaviour: keeps
// a registry map of (name, version) → MetricVersion, and a scoreMap of
// (runID, name, version) → float64. Override hooks let tests inject
// failure modes.
type MockClient struct {
	mu sync.Mutex

	metrics  map[string]MetricVersion // key is name+":"+version
	scoreMap map[string]float64       // key is runID+":"+name+":"+version

	RegisterMetricFn func(ctx context.Context, metric MetricVersion) error
	ScoreFn          func(ctx context.Context, runID int, metricName, version string) (float64, error)
	RecordScoreFn    func(ctx context.Context, runID int, metricName, version string, score float64) error
	ListMetricsFn    func(ctx context.Context) ([]MetricVersion, error)

	RegisterCalls    []MetricVersion
	ScoreCalls       []ScoreLookup
	RecordScoreCalls []ScoreRecord
	ListCalls        int
}

// ScoreLookup records one Score call.
type ScoreLookup struct {
	RunID   int
	Metric  string
	Version string
}

// ScoreRecord records one RecordScore call.
type ScoreRecord struct {
	RunID   int
	Metric  string
	Version string
	Score   float64
}

// NewMock returns a MockClient with empty maps.
func NewMock() *MockClient {
	return &MockClient{
		metrics:  map[string]MetricVersion{},
		scoreMap: map[string]float64{},
	}
}

func metricKey(name, version string) string { return name + ":" + version }

func scoreKey(runID int, name, version string) string {
	return fmt.Sprintf("%d:%s:%s", runID, name, version)
}

func (m *MockClient) RegisterMetric(ctx context.Context, metric MetricVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RegisterCalls = append(m.RegisterCalls, metric)
	if m.RegisterMetricFn != nil {
		return m.RegisterMetricFn(ctx, metric)
	}
	k := metricKey(metric.Name, metric.Version)
	if _, ok := m.metrics[k]; ok {
		return ErrMetricExists
	}
	m.metrics[k] = metric
	return nil
}

func (m *MockClient) Score(ctx context.Context, runID int, metricName, version string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ScoreCalls = append(m.ScoreCalls, ScoreLookup{RunID: runID, Metric: metricName, Version: version})
	if m.ScoreFn != nil {
		return m.ScoreFn(ctx, runID, metricName, version)
	}
	if v, ok := m.scoreMap[scoreKey(runID, metricName, version)]; ok {
		return v, nil
	}
	return 0, ErrNoScore
}

func (m *MockClient) RecordScore(ctx context.Context, runID int, metricName, version string, score float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RecordScoreCalls = append(m.RecordScoreCalls, ScoreRecord{
		RunID: runID, Metric: metricName, Version: version, Score: score,
	})
	if m.RecordScoreFn != nil {
		return m.RecordScoreFn(ctx, runID, metricName, version, score)
	}
	m.scoreMap[scoreKey(runID, metricName, version)] = score
	return nil
}

func (m *MockClient) ListMetrics(ctx context.Context) ([]MetricVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListCalls++
	if m.ListMetricsFn != nil {
		return m.ListMetricsFn(ctx)
	}
	out := make([]MetricVersion, 0, len(m.metrics))
	for _, v := range m.metrics {
		out = append(out, v)
	}
	return out, nil
}

// Compile-time assertion: *MockClient satisfies the Client interface.
var _ Client = (*MockClient)(nil)

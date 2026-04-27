package metrics

import "context"

// inProcessClient is the placeholder D0 backing. Every method returns
// ErrNotImplemented so callers receive a real error before D3 lands.
type inProcessClient struct{}

// NewInProcess returns the placeholder Client; D3 fills in the bodies.
func NewInProcess() Client { return &inProcessClient{} }

func (*inProcessClient) RegisterMetric(ctx context.Context, metric MetricVersion) error {
	return ErrNotImplemented
}

func (*inProcessClient) Score(ctx context.Context, runID int, metricName, version string) (float64, error) {
	return 0, ErrNotImplemented
}

func (*inProcessClient) RecordScore(ctx context.Context, runID int, metricName, version string, score float64) error {
	return ErrNotImplemented
}

func (*inProcessClient) ListMetrics(ctx context.Context) ([]MetricVersion, error) {
	return nil, ErrNotImplemented
}

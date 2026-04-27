package experiments

import "context"

// inProcessClient is the placeholder D0 backing. Every method returns
// ErrNotImplemented so callers receive a real error if they reach a
// treatment-application path before D3 lands its implementation.
type inProcessClient struct{}

// NewInProcess returns the placeholder Client. D3 replaces the bodies
// in this file; the constructor signature stays the same so call sites
// don't move.
func NewInProcess() Client { return &inProcessClient{} }

func (*inProcessClient) Apply(ctx context.Context, call CallDescriptor) (CallDescriptor, []Assignment, error) {
	return call, nil, ErrNotImplemented
}

func (*inProcessClient) Outcome(ctx context.Context, experimentID int) (Outcome, error) {
	return Outcome{}, ErrNotImplemented
}

func (*inProcessClient) Register(ctx context.Context, exp ExperimentDecl) (int, error) {
	return 0, ErrNotImplemented
}

func (*inProcessClient) Cancel(ctx context.Context, experimentID int, reason string) error {
	return ErrNotImplemented
}

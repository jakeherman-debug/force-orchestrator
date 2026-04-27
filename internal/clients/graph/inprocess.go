package graph

import "context"

// inProcessClient is the placeholder D0 backing. Every method returns
// ErrNotImplemented so callers receive a real error before D8 lands.
type inProcessClient struct{}

// NewInProcess returns the placeholder Client; D8 fills in the bodies.
func NewInProcess() Client { return &inProcessClient{} }

func (*inProcessClient) Consumers(ctx context.Context, symbol Symbol) ([]Consumer, error) {
	return nil, ErrNotImplemented
}

func (*inProcessClient) Definers(ctx context.Context, symbol Symbol) ([]Symbol, error) {
	return nil, ErrNotImplemented
}

func (*inProcessClient) BlastRadius(ctx context.Context, modifiedSymbol Symbol) (BlastRadius, error) {
	return BlastRadius{}, ErrNotImplemented
}

func (*inProcessClient) IndexHealth(ctx context.Context) (Health, error) {
	return Health{}, ErrNotImplemented
}

package rules

import "context"

// inProcessClient is the placeholder D0 backing. Every method returns
// ErrNotImplemented so callers receive a real error before D3 lands.
type inProcessClient struct{}

// NewInProcess returns the placeholder Client; D3 fills in the bodies.
func NewInProcess() Client { return &inProcessClient{} }

func (*inProcessClient) ActiveRules(ctx context.Context, agent, category string) ([]Rule, error) {
	return nil, ErrNotImplemented
}

func (*inProcessClient) RuleByKey(ctx context.Context, ruleKey string) (Rule, error) {
	return Rule{}, ErrNotImplemented
}

func (*inProcessClient) PromoteFromExperiment(ctx context.Context, experimentID int, p PromotionRequest) (Rule, error) {
	return Rule{}, ErrNotImplemented
}

func (*inProcessClient) Retire(ctx context.Context, ruleKey, reason string) error {
	return ErrNotImplemented
}

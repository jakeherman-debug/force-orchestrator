package rules

import (
	"context"
	"sync"
)

// MockClient is the test-side Client backing. Tests fixture rules
// directly via the Rules slice (one per agent/category combo); the
// default lookup is "filter Rules by agent + category."
type MockClient struct {
	mu sync.Mutex

	Rules []Rule

	ActiveRulesFn           func(ctx context.Context, agent, category string) ([]Rule, error)
	RuleByKeyFn             func(ctx context.Context, ruleKey string) (Rule, error)
	PromoteFromExperimentFn func(ctx context.Context, experimentID int, p PromotionRequest) (Rule, error)
	RetireFn                func(ctx context.Context, ruleKey, reason string) error

	ActiveCalls   []struct{ Agent, Category string }
	ByKeyCalls    []string
	PromoteCalls  []PromotionRequest
	RetireCalls   []string

	NextPromoteID int
}

// NewMock returns a MockClient with an empty rule list.
func NewMock() *MockClient { return &MockClient{NextPromoteID: 1} }

func (m *MockClient) ActiveRules(ctx context.Context, agent, category string) ([]Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ActiveCalls = append(m.ActiveCalls, struct{ Agent, Category string }{agent, category})
	if m.ActiveRulesFn != nil {
		return m.ActiveRulesFn(ctx, agent, category)
	}
	out := []Rule{}
	for _, r := range m.Rules {
		if r.Agent == agent && r.Category == category && r.RetiredAt == "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *MockClient) RuleByKey(ctx context.Context, ruleKey string) (Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ByKeyCalls = append(m.ByKeyCalls, ruleKey)
	if m.RuleByKeyFn != nil {
		return m.RuleByKeyFn(ctx, ruleKey)
	}
	for _, r := range m.Rules {
		if r.Key == ruleKey {
			return r, nil
		}
	}
	return Rule{}, ErrRuleNotFound
}

func (m *MockClient) PromoteFromExperiment(ctx context.Context, experimentID int, p PromotionRequest) (Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PromoteCalls = append(m.PromoteCalls, p)
	if m.PromoteFromExperimentFn != nil {
		return m.PromoteFromExperimentFn(ctx, experimentID, p)
	}
	r := Rule{
		ID:           m.NextPromoteID,
		Key:          p.Key,
		Agent:        p.Agent,
		Category:     p.Category,
		Body:         p.Body,
		PromotedFrom: experimentID,
		ActivatedAt:  "now",
	}
	m.NextPromoteID++
	m.Rules = append(m.Rules, r)
	return r, nil
}

func (m *MockClient) Retire(ctx context.Context, ruleKey, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RetireCalls = append(m.RetireCalls, ruleKey)
	if m.RetireFn != nil {
		return m.RetireFn(ctx, ruleKey, reason)
	}
	for i := range m.Rules {
		if m.Rules[i].Key == ruleKey {
			m.Rules[i].RetiredAt = "now"
			m.Rules[i].RetireReason = reason
			return nil
		}
	}
	return ErrRuleNotFound
}

// Compile-time assertion: *MockClient satisfies the Client interface.
var _ Client = (*MockClient)(nil)

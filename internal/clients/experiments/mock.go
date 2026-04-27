package experiments

import (
	"context"
	"sync"
)

// MockClient is the test-side Client backing. Tests assign behaviour
// via the *Fn hooks; default behaviour is "no treatment, no error" —
// Apply passes the call through unchanged with no assignments, and
// Outcome / Register / Cancel return zero-value successes. The default
// is permissive so legacy tests that don't care about experiments
// don't break when the dependency is added.
type MockClient struct {
	mu sync.Mutex

	ApplyFn    func(ctx context.Context, call CallDescriptor) (CallDescriptor, []Assignment, error)
	OutcomeFn  func(ctx context.Context, experimentID int) (Outcome, error)
	RegisterFn func(ctx context.Context, exp ExperimentDecl) (int, error)
	CancelFn   func(ctx context.Context, experimentID int, reason string) error

	ApplyCalls    []CallDescriptor
	OutcomeCalls  []int
	RegisterCalls []ExperimentDecl
	CancelCalls   []int

	// NextRegisterID auto-increments and is returned from Register
	// when RegisterFn is unset.
	NextRegisterID int
}

// NewMock returns a permissive MockClient — Apply is a no-op, Register
// hands out auto-incrementing IDs starting at 1.
func NewMock() *MockClient { return &MockClient{NextRegisterID: 1} }

func (m *MockClient) Apply(ctx context.Context, call CallDescriptor) (CallDescriptor, []Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ApplyCalls = append(m.ApplyCalls, call)
	if m.ApplyFn != nil {
		return m.ApplyFn(ctx, call)
	}
	return call, nil, nil
}

func (m *MockClient) Outcome(ctx context.Context, experimentID int) (Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.OutcomeCalls = append(m.OutcomeCalls, experimentID)
	if m.OutcomeFn != nil {
		return m.OutcomeFn(ctx, experimentID)
	}
	return Outcome{ExperimentID: experimentID}, nil
}

func (m *MockClient) Register(ctx context.Context, exp ExperimentDecl) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RegisterCalls = append(m.RegisterCalls, exp)
	if m.RegisterFn != nil {
		return m.RegisterFn(ctx, exp)
	}
	id := m.NextRegisterID
	m.NextRegisterID++
	return id, nil
}

func (m *MockClient) Cancel(ctx context.Context, experimentID int, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CancelCalls = append(m.CancelCalls, experimentID)
	if m.CancelFn != nil {
		return m.CancelFn(ctx, experimentID, reason)
	}
	return nil
}

// Compile-time assertion: *MockClient satisfies the Client interface.
var _ Client = (*MockClient)(nil)

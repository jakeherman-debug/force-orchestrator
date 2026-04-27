package capabilities

import (
	"context"
	"sync"
)

// MockClient is the test-side Client backing. Tests fixture profiles
// directly via the Profiles map; lookups return ErrProfileNotFound for
// agents not in the map. Behaviour hooks let tests override individual
// methods without rewriting the whole mock.
type MockClient struct {
	mu sync.Mutex

	Profiles map[string]*Profile

	LoadProfileFn      func(ctx context.Context, agentName string) (*Profile, error)
	AllowedToolsFn     func(ctx context.Context, agentName string) ([]string, error)
	DisallowedToolsFn  func(ctx context.Context, agentName string) ([]string, error)
	MCPConfigPathFn    func(ctx context.Context, agentName string) (string, error)

	LoadCalls          []string
	AllowedCalls       []string
	DisallowedCalls    []string
	MCPCalls           []string
}

// NewMock returns a MockClient with an empty fixture map. Tests assign
// to .Profiles before invoking the agent under test.
func NewMock() *MockClient { return &MockClient{Profiles: map[string]*Profile{}} }

func (m *MockClient) LoadProfile(ctx context.Context, agentName string) (*Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LoadCalls = append(m.LoadCalls, agentName)
	if m.LoadProfileFn != nil {
		return m.LoadProfileFn(ctx, agentName)
	}
	if p, ok := m.Profiles[agentName]; ok {
		return p, nil
	}
	return nil, ErrProfileNotFound
}

func (m *MockClient) AllowedTools(ctx context.Context, agentName string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AllowedCalls = append(m.AllowedCalls, agentName)
	if m.AllowedToolsFn != nil {
		return m.AllowedToolsFn(ctx, agentName)
	}
	if p, ok := m.Profiles[agentName]; ok {
		return p.AllowedTools, nil
	}
	return nil, ErrProfileNotFound
}

func (m *MockClient) DisallowedTools(ctx context.Context, agentName string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DisallowedCalls = append(m.DisallowedCalls, agentName)
	if m.DisallowedToolsFn != nil {
		return m.DisallowedToolsFn(ctx, agentName)
	}
	if p, ok := m.Profiles[agentName]; ok {
		return p.DisallowedTools, nil
	}
	return nil, ErrProfileNotFound
}

func (m *MockClient) MCPConfigPath(ctx context.Context, agentName string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MCPCalls = append(m.MCPCalls, agentName)
	if m.MCPConfigPathFn != nil {
		return m.MCPConfigPathFn(ctx, agentName)
	}
	if p, ok := m.Profiles[agentName]; ok {
		return p.MCPConfigPath, nil
	}
	return "", ErrProfileNotFound
}

// Compile-time assertion: *MockClient satisfies the Client interface.
var _ Client = (*MockClient)(nil)

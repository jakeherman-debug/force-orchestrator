package graph

import (
	"context"
	"sync"
)

// MockClient is the test-side Client backing. Tests fixture the index
// state directly via the Consumers / Definers maps; the default
// behaviour is "look up the requested symbol in the map." Override
// hooks let tests inject specific failure modes.
type MockClient struct {
	mu sync.Mutex

	// Indexed state. Keys are Symbol.Name (test-fixture convenience).
	ConsumersByName map[string][]Consumer
	DefinersByName  map[string][]Symbol
	BlastByName     map[string]BlastRadius
	HealthFixture   Health

	// BlastByModifications keys on the SymbolPath of the FIRST modification
	// in the batch (test convenience). For richer fixtures use the *Fn hook.
	BlastByModifications map[string]BlastRadius

	ConsumersFn                  func(ctx context.Context, symbol Symbol) ([]Consumer, error)
	DefinersFn                   func(ctx context.Context, symbol Symbol) ([]Symbol, error)
	BlastRadiusFn                func(ctx context.Context, modifiedSymbol Symbol) (BlastRadius, error)
	BlastRadiusForModificationsFn func(ctx context.Context, mods []SymbolModification) (BlastRadius, error)
	IndexHealthFn                func(ctx context.Context) (Health, error)

	ConsumersCalls                  []Symbol
	DefinersCalls                   []Symbol
	BlastRadiusCalls                []Symbol
	BlastRadiusForModificationsCalls [][]SymbolModification
	HealthCalls                     int
}

// NewMock returns a MockClient with empty fixture maps.
func NewMock() *MockClient {
	return &MockClient{
		ConsumersByName:      map[string][]Consumer{},
		DefinersByName:       map[string][]Symbol{},
		BlastByName:          map[string]BlastRadius{},
		BlastByModifications: map[string]BlastRadius{},
	}
}

func (m *MockClient) Consumers(ctx context.Context, symbol Symbol) ([]Consumer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConsumersCalls = append(m.ConsumersCalls, symbol)
	if m.ConsumersFn != nil {
		return m.ConsumersFn(ctx, symbol)
	}
	if c, ok := m.ConsumersByName[symbol.Name]; ok {
		return c, nil
	}
	return nil, ErrSymbolNotFound
}

func (m *MockClient) Definers(ctx context.Context, symbol Symbol) ([]Symbol, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DefinersCalls = append(m.DefinersCalls, symbol)
	if m.DefinersFn != nil {
		return m.DefinersFn(ctx, symbol)
	}
	if d, ok := m.DefinersByName[symbol.Name]; ok {
		return d, nil
	}
	return nil, ErrSymbolNotFound
}

func (m *MockClient) BlastRadius(ctx context.Context, modifiedSymbol Symbol) (BlastRadius, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BlastRadiusCalls = append(m.BlastRadiusCalls, modifiedSymbol)
	if m.BlastRadiusFn != nil {
		return m.BlastRadiusFn(ctx, modifiedSymbol)
	}
	if b, ok := m.BlastByName[modifiedSymbol.Name]; ok {
		return b, nil
	}
	return BlastRadius{}, ErrSymbolNotFound
}

func (m *MockClient) BlastRadiusForModifications(ctx context.Context, mods []SymbolModification) (BlastRadius, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Defensive copy so test assertions on the recorded slice aren't
	// mutated by a caller that retains the input.
	cp := append([]SymbolModification(nil), mods...)
	m.BlastRadiusForModificationsCalls = append(m.BlastRadiusForModificationsCalls, cp)
	if m.BlastRadiusForModificationsFn != nil {
		return m.BlastRadiusForModificationsFn(ctx, mods)
	}
	if len(mods) > 0 {
		if b, ok := m.BlastByModifications[mods[0].SymbolPath]; ok {
			return b, nil
		}
	}
	return BlastRadius{ConsumersBySymbol: map[string][]ConsumerSite{}}, nil
}

func (m *MockClient) IndexHealth(ctx context.Context) (Health, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HealthCalls++
	if m.IndexHealthFn != nil {
		return m.IndexHealthFn(ctx)
	}
	return m.HealthFixture, nil
}

// Compile-time assertion: *MockClient satisfies the Client interface.
var _ Client = (*MockClient)(nil)

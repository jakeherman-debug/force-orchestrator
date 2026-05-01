package bos

import "sync"

// registry holds every rule that has called Register(). It is
// populated by init() functions in internal/bos/rules/*.go. Access is
// guarded by registryMu so concurrent test cases that import the rules
// package alongside the BoS reviewer don't race on registration.
var (
	registryMu sync.RWMutex
	registry   = map[string]Rule{}
	regOrder   []string // insertion order, for deterministic All() iteration
)

// Register adds r to the rule registry. Panics on duplicate ID — that
// would indicate two rule implementations claiming the same ID, which
// is always a bug. A second call to Register for the same Rule
// instance (e.g., a test that re-registers via init) is also a panic;
// tests should construct rules directly without going through the
// registry when they want isolation.
func Register(r Rule) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if r == nil {
		panic("bos.Register: nil rule")
	}
	id := r.ID()
	if id == "" {
		panic("bos.Register: rule has empty ID")
	}
	if _, dup := registry[id]; dup {
		panic("bos.Register: duplicate rule ID " + id)
	}
	registry[id] = r
	regOrder = append(regOrder, id)
}

// All returns every registered rule, in insertion order.
func All() []Rule {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Rule, 0, len(regOrder))
	for _, id := range regOrder {
		out = append(out, registry[id])
	}
	return out
}

// Get returns the rule with the given ID, or nil if no such rule is
// registered. Used by tests that want to exercise a single rule.
func Get(id string) Rule {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[id]
}

// resetForTest clears the registry. Test-only — exported only via
// internal/bos/registry_test.go's same-package access. Production code
// must never call this; the rules package's init() is the only
// legitimate writer of registry.
func resetForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Rule{}
	regOrder = nil
}

// Compile-time fence so the resetForTest helper can't be silently
// dropped — the test file references it.
var _ = resetForTest

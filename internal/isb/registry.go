package isb

import "sync"

// Design choice: this registry mirrors internal/bos/registry.go rather
// than sharing a common abstraction in internal/security/rule.go. The
// rule sets are independent (different ID prefixes, different anchors,
// different bypass directive text), and the SecurityFindings table is
// the shared substrate — not the Go-side rule interface. A future
// extraction is cheap if a third bureau lands; today, two parallel
// shapes are easier to read than one abstracted one.
var (
	registryMu sync.RWMutex
	registry   = map[string]Rule{}
	regOrder   []string // insertion order, for deterministic All() iteration
)

// Register adds r to the rule registry. Panics on duplicate ID — that
// would indicate two rule implementations claiming the same ID, which
// is always a bug.
func Register(r Rule) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if r == nil {
		panic("isb.Register: nil rule")
	}
	id := r.ID()
	if id == "" {
		panic("isb.Register: rule has empty ID")
	}
	if _, dup := registry[id]; dup {
		panic("isb.Register: duplicate rule ID " + id)
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

// resetForTest clears the registry. Test-only.
func resetForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Rule{}
	regOrder = nil
}

// Compile-time fence so the resetForTest helper can't be silently
// dropped — the test file references it.
var _ = resetForTest

package bos

import (
	"go/ast"
	"go/types"
	"testing"
)

// snapshotRegistry captures the current registry state so a hermetic
// test can reset for its own assertions and then restore — necessary
// because review_test.go (in the bos_test package) imports
// internal/bos/rules which registers BOS-001..BOS-011 at package init.
// Without restore, registry-mutating tests would leave the global
// state empty and review_test.go would see zero rules.
func snapshotRegistry() (map[string]Rule, []string) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	cpy := make(map[string]Rule, len(registry))
	for k, v := range registry {
		cpy[k] = v
	}
	order := append([]string{}, regOrder...)
	return cpy, order
}

func restoreRegistry(snap map[string]Rule, order []string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = snap
	regOrder = order
}

// stubRule is a minimal Rule used only to exercise the registry — the
// actual rule library lives under internal/bos/rules/ and registers via
// init(). This file uses the package-internal resetForTest hook to
// keep its assertions hermetic.
type stubRule struct {
	id, anchor string
	sev        Severity
}

func (s stubRule) ID() string             { return s.id }
func (s stubRule) CLAUDEMDAnchor() string { return s.anchor }
func (s stubRule) Severity() Severity     { return s.sev }
func (s stubRule) Check(_ *ast.File, _ string, _ *types.Info) []Finding {
	return nil
}

func TestRegister_HappyPath(t *testing.T) {
	snap, order := snapshotRegistry()
	resetForTest()
	t.Cleanup(func() { restoreRegistry(snap, order) })

	Register(stubRule{id: "BOS-TEST-001", anchor: "Test", sev: SeverityAdvise})
	if got := len(All()); got != 1 {
		t.Fatalf("All(): got %d, want 1", got)
	}
	if Get("BOS-TEST-001") == nil {
		t.Fatal("Get(BOS-TEST-001): nil")
	}
}

func TestRegister_DuplicateIDPanics(t *testing.T) {
	snap, order := snapshotRegistry()
	resetForTest()
	t.Cleanup(func() { restoreRegistry(snap, order) })
	Register(stubRule{id: "DUP", sev: SeverityAdvise})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register: duplicate ID expected panic")
		}
	}()
	Register(stubRule{id: "DUP", sev: SeverityAdvise})
}

func TestRegister_EmptyIDPanics(t *testing.T) {
	snap, order := snapshotRegistry()
	resetForTest()
	t.Cleanup(func() { restoreRegistry(snap, order) })
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register: empty ID expected panic")
		}
	}()
	Register(stubRule{id: ""})
}

func TestAll_DeterministicOrder(t *testing.T) {
	snap, order := snapshotRegistry()
	resetForTest()
	t.Cleanup(func() { restoreRegistry(snap, order) })

	Register(stubRule{id: "Z"})
	Register(stubRule{id: "A"})
	Register(stubRule{id: "M"})
	got := All()
	if len(got) != 3 || got[0].ID() != "Z" || got[1].ID() != "A" || got[2].ID() != "M" {
		t.Fatalf("All(): expected insertion-order [Z, A, M], got %v", []string{got[0].ID(), got[1].ID(), got[2].ID()})
	}
}

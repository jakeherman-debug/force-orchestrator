package isb

import (
	"go/ast"
	"go/types"
	"testing"
)

// stubRule is a minimal Rule for registry exercise.
type stubRule struct{ id string }

func (s stubRule) ID() string                                                  { return s.id }
func (s stubRule) CLAUDEMDAnchor() string                                      { return "" }
func (s stubRule) Severity() Severity                                          { return SeverityAdvise }
func (s stubRule) Check(_ *ast.File, _, _ string, _ *types.Info) []Finding { return nil }

func TestRegistry_RegisterAndAll(t *testing.T) {
	resetForTest()
	defer resetForTest()
	Register(stubRule{id: "ISB-A"})
	Register(stubRule{id: "ISB-B"})
	rs := All()
	if len(rs) != 2 {
		t.Fatalf("All(): got %d, want 2", len(rs))
	}
	if rs[0].ID() != "ISB-A" || rs[1].ID() != "ISB-B" {
		t.Fatalf("insertion order: got [%s, %s]", rs[0].ID(), rs[1].ID())
	}
	if Get("ISB-A") == nil || Get("ISB-Missing") != nil {
		t.Fatal("Get() returned wrong shape")
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	resetForTest()
	defer resetForTest()
	Register(stubRule{id: "ISB-DUP"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate ID")
		}
	}()
	Register(stubRule{id: "ISB-DUP"})
}

func TestRegistry_NilOrEmptyPanics(t *testing.T) {
	resetForTest()
	defer resetForTest()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty ID")
		}
	}()
	Register(stubRule{id: ""})
}

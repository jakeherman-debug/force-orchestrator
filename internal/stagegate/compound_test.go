package stagegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// passGate / failGate / pendingGate are tiny stubGate-backed
// helpers we register inside compound tests so we can drive
// pass/fail/pending outcomes deterministically without depending on
// real time-based gates. stubGate is defined in gate_test.go.

func passGate(typeName string) *stubGate {
	return &stubGate{typeName: typeName, passed: true, reason: "stub-pass"}
}
func failGate(typeName string) *stubGate {
	return &stubGate{typeName: typeName, passed: false, reason: "stub-fail"}
}
func pendingGate(typeName string) *stubGate {
	return &stubGate{typeName: typeName, passed: false, reason: "stub-pending", err: ErrPending}
}

func compoundSpec(compoundType string, children ...json.RawMessage) json.RawMessage {
	out, _ := json.Marshal(map[string]any{
		"type":  compoundType,
		"gates": children,
	})
	return out
}

func leafSpec(typeName string) json.RawMessage {
	out, _ := json.Marshal(map[string]any{"type": typeName, "config": map[string]any{}})
	return out
}

// ── all_of ──────────────────────────────────────────────────────────────

func TestAllOf_AllPass_Passes(t *testing.T) {
	r := NewRegistry()
	r.Register(AllOf{})
	r.Register(passGate("a"))
	r.Register(passGate("b"))

	spec := compoundSpec("all_of", leafSpec("a"), leafSpec("b"))
	passed, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if !passed {
		t.Error("expected all_of to pass when all children pass")
	}
}

func TestAllOf_OneFails_FailsShortCircuit(t *testing.T) {
	r := NewRegistry()
	r.Register(AllOf{})
	a := passGate("a")
	b := failGate("b")
	c := passGate("c") // should NOT be evaluated due to short-circuit
	r.Register(a)
	r.Register(b)
	r.Register(c)

	spec := compoundSpec("all_of", leafSpec("a"), leafSpec("b"), leafSpec("c"))
	passed, reason, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if passed {
		t.Error("expected all_of to fail when one child fails")
	}
	if !strings.Contains(reason, "child 1 failed") {
		t.Errorf("expected reason to mention child 1, got %q", reason)
	}
	if c.calls != 0 {
		t.Errorf("expected short-circuit: gate c should not have been evaluated, got %d calls", c.calls)
	}
}

func TestAllOf_AnyPending_Pending(t *testing.T) {
	r := NewRegistry()
	r.Register(AllOf{})
	r.Register(passGate("a"))
	r.Register(pendingGate("b"))

	spec := compoundSpec("all_of", leafSpec("a"), leafSpec("b"))
	passed, reason, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending, got %v", err)
	}
	if passed {
		t.Error("expected passed=false")
	}
	if !strings.Contains(reason, "pending") {
		t.Errorf("expected reason to mention pending, got %q", reason)
	}
}

func TestAllOf_EmptyChildren_Errors(t *testing.T) {
	r := NewRegistry()
	r.Register(AllOf{})
	spec, _ := json.Marshal(map[string]any{
		"type":  "all_of",
		"gates": []json.RawMessage{},
	})
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("expected non-Pending error on empty children, got %v", err)
	}
}

func TestAllOf_SingleChild_WarnsAndDelegates(t *testing.T) {
	r := NewRegistry()
	logger := &captureLogger{}
	r.SetLogger(logger)
	r.Register(AllOf{})
	r.Register(passGate("a"))

	spec := compoundSpec("all_of", leafSpec("a"))
	passed, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{StageID: 5}, spec, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if !passed {
		t.Error("expected single-child all_of to delegate to its child")
	}
	hit := false
	for _, line := range logger.lines {
		if strings.Contains(line, "single child") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("expected single-child warning, got: %v", logger.lines)
	}
}

func TestAllOf_DirectEvaluate_Errors(t *testing.T) {
	// Calling AllOf.Evaluate directly (not via the registry) should
	// error — compound gates require recursion machinery.
	_, _, err := AllOf{}.Evaluate(context.Background(), nil, StageContext{})
	if err == nil {
		t.Fatal("expected AllOf.Evaluate without registry to error")
	}
}

// ── any_of ──────────────────────────────────────────────────────────────

func TestAnyOf_OnePass_Passes(t *testing.T) {
	r := NewRegistry()
	r.Register(AnyOf{})
	r.Register(failGate("a"))
	b := passGate("b")
	r.Register(b)
	c := passGate("c") // any_of short-circuits — should not be touched if b passes first
	r.Register(c)

	spec := compoundSpec("any_of", leafSpec("a"), leafSpec("b"), leafSpec("c"))
	passed, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if !passed {
		t.Error("expected any_of to pass when at least one child passes")
	}
	if c.calls != 0 {
		t.Errorf("expected any_of to short-circuit; gate c had %d calls", c.calls)
	}
}

func TestAnyOf_AllFail_Fails(t *testing.T) {
	r := NewRegistry()
	r.Register(AnyOf{})
	r.Register(failGate("a"))
	r.Register(failGate("b"))

	spec := compoundSpec("any_of", leafSpec("a"), leafSpec("b"))
	passed, reason, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if passed {
		t.Error("expected any_of to fail when all children fail")
	}
	if !strings.Contains(reason, "all") {
		t.Errorf("expected reason to mention all-children-failed, got %q", reason)
	}
}

func TestAnyOf_AllPending_Pending(t *testing.T) {
	r := NewRegistry()
	r.Register(AnyOf{})
	r.Register(pendingGate("a"))
	r.Register(pendingGate("b"))

	spec := compoundSpec("any_of", leafSpec("a"), leafSpec("b"))
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending, got %v", err)
	}
}

func TestAnyOf_PendingBlocksFail(t *testing.T) {
	// any_of with [fail, pending]: even though one fails, the other
	// is pending — we must stay AwaitingGate, not fail.
	r := NewRegistry()
	r.Register(AnyOf{})
	r.Register(failGate("a"))
	r.Register(pendingGate("b"))

	spec := compoundSpec("any_of", leafSpec("a"), leafSpec("b"))
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending while one child still pending, got %v", err)
	}
}

func TestAnyOf_DirectEvaluate_Errors(t *testing.T) {
	_, _, err := AnyOf{}.Evaluate(context.Background(), nil, StageContext{})
	if err == nil {
		t.Fatal("expected AnyOf.Evaluate without registry to error")
	}
}

// ── nested compounds ────────────────────────────────────────────────────

func TestAnyOf_NestedCompound_Recursive(t *testing.T) {
	// 3-level nest:
	//   any_of(
	//     all_of(
	//       all_of(
	//         soak_minutes,
	//       ),
	//     ),
	//     fail,
	//   )
	// soak elapsed → all_of inner passes → all_of middle passes →
	// any_of passes.
	r := NewRegistry()
	RegisterBaselineGates(r)
	r.Register(failGate("fail_stub"))

	soak := json.RawMessage(`{"type":"soak_minutes","config":{"minutes":1}}`)
	innerAll, _ := json.Marshal(map[string]any{
		"type":  "all_of",
		"gates": []json.RawMessage{soak},
	})
	middleAll, _ := json.Marshal(map[string]any{
		"type":  "all_of",
		"gates": []json.RawMessage{innerAll},
	})
	outerAny, _ := json.Marshal(map[string]any{
		"type":  "any_of",
		"gates": []json.RawMessage{middleAll, leafSpec("fail_stub")},
	})

	stage := StageContext{
		AllPRsMergedAt: time.Now().Add(-2 * time.Minute), // soak should have elapsed
	}
	passed, reason, err := r.EvaluateGateConfig(context.Background(), nil, stage, outerAny, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v (reason=%s)", err, reason)
	}
	if !passed {
		t.Errorf("expected outer any_of to pass via soak path, got reason=%q", reason)
	}
}

func TestNested_DepthCapEnforcedAtRecursion(t *testing.T) {
	// Build a compound chain of depth = MaxNestingDepth + 2 (so
	// recursion will exceed the cap before reaching the leaf).
	r := NewRegistry()
	RegisterBaselineGates(r)

	leaf := json.RawMessage(`{"type":"null","config":{}}`)
	cur := leaf
	for i := 0; i < MaxNestingDepth+2; i++ {
		nested, _ := json.Marshal(map[string]any{
			"type":  "all_of",
			"gates": []json.RawMessage{cur},
		})
		cur = nested
	}
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, cur, 0)
	if err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("expected nesting depth error, got %v", err)
	}
}

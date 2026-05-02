package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// stubGate is a minimal Gate implementation tests use to inject
// pre-decided pass/fail/pending outcomes into the registry.
type stubGate struct {
	typeName string
	passed   bool
	reason   string
	err      error
	calls    int
}

func (s *stubGate) Type() string { return s.typeName }
func (s *stubGate) Evaluate(_ context.Context, _ *sql.DB, _ StageContext) (bool, string, error) {
	s.calls++
	return s.passed, s.reason, s.err
}

// captureLogger records Printf calls so tests can assert warnings fire.
type captureLogger struct {
	lines []string
}

func (c *captureLogger) Printf(f string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(f, args...))
}

// ── Registry surface ────────────────────────────────────────────────────────

func TestRegistry_Register_DuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubGate{typeName: "stub"})
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r.Register(&stubGate{typeName: "stub"})
}

func TestRegistry_Register_NilPanics(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic on nil registration")
		}
	}()
	r.Register(nil)
}

func TestRegistry_Register_EmptyTypePanics(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic when Gate.Type() returns empty string")
		}
	}()
	r.Register(&stubGate{typeName: ""})
}

func TestRegistry_Lookup_HitMiss(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubGate{typeName: "stub"})
	if _, ok := r.Lookup("stub"); !ok {
		t.Error("expected stub to be registered")
	}
	if _, ok := r.Lookup("does-not-exist"); ok {
		t.Error("expected unknown lookup to miss")
	}
}

func TestRegistry_RegisterBaselineGates_AllFive(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	for _, want := range []string{"soak_minutes", "operator_confirm", "null", "all_of", "any_of"} {
		if _, ok := r.Lookup(want); !ok {
			t.Errorf("baseline registry missing %q", want)
		}
	}
}

// ── EvaluateGateConfig validation ──────────────────────────────────────────

func TestEvaluateGateConfig_NestingDepthExceeded_Errors(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	// Build a deeply nested all_of(all_of(...(soak)..)) config — depth
	// = MaxNestingDepth + 1 levels of compound + 1 leaf.
	leaf := json.RawMessage(`{"type":"soak_minutes","config":{"minutes":1}}`)
	cur := leaf
	for i := 0; i <= MaxNestingDepth; i++ {
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

func TestEvaluateGateConfig_EmptyChildren_Errors(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	for _, gateType := range []string{"all_of", "any_of"} {
		spec, _ := json.Marshal(map[string]any{
			"type":  gateType,
			"gates": []json.RawMessage{},
		})
		_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
		if err == nil || !strings.Contains(err.Error(), "empty children") {
			t.Errorf("%s with empty children: expected error, got %v", gateType, err)
		}
	}
}

func TestEvaluateGateConfig_SingleChild_PassesWithWarning(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	logger := &captureLogger{}
	r.SetLogger(logger)

	// Build all_of with a single null-gate child. Should pass + emit
	// a warning line.
	spec, _ := json.Marshal(map[string]any{
		"type":  "all_of",
		"gates": []json.RawMessage{json.RawMessage(`{"type":"null","config":{}}`)},
	})
	passed, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{StageID: 7}, spec, 0)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !passed {
		t.Error("expected passed=true for all_of with single null child")
	}
	if len(logger.lines) == 0 {
		t.Error("expected single-child warning to fire")
	}
	hit := false
	for _, line := range logger.lines {
		if strings.Contains(line, "single child") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("expected warning about single child, got: %v", logger.lines)
	}
}

func TestEvaluateGateConfig_UnknownGateType_Errors(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	spec := json.RawMessage(`{"type":"definitely_not_a_gate","config":{}}`)
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, spec, 0)
	if err == nil || !strings.Contains(err.Error(), "unknown gate type") {
		t.Fatalf("expected unknown-gate-type error, got %v", err)
	}
}

func TestEvaluateGateConfig_MissingType_Errors(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, json.RawMessage(`{"config":{}}`), 0)
	if err == nil || !strings.Contains(err.Error(), "missing type") {
		t.Fatalf("expected missing-type error, got %v", err)
	}
}

func TestEvaluateGateConfig_EmptySpec_Errors(t *testing.T) {
	r := NewRegistry()
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, nil, 0)
	if err == nil {
		t.Fatalf("expected error on empty spec, got nil")
	}
}

func TestEvaluateGateConfig_MalformedJSON_Errors(t *testing.T) {
	r := NewRegistry()
	RegisterBaselineGates(r)
	_, _, err := r.EvaluateGateConfig(context.Background(), nil, StageContext{}, json.RawMessage(`{not json`), 0)
	if err == nil {
		t.Fatalf("expected error on malformed JSON")
	}
}

// Sanity: ErrPending is the documented sentinel.
func TestErrPending_IsSentinel(t *testing.T) {
	wrapped := errors.New("wrap: " + ErrPending.Error())
	if errors.Is(wrapped, ErrPending) {
		t.Fatal("plain string-wrapped errors should NOT match ErrPending — must use %w")
	}
	wrapped2 := wrapErr(ErrPending)
	if !errors.Is(wrapped2, ErrPending) {
		t.Fatal("expected errors.Is(wrap %w of ErrPending, ErrPending) to be true")
	}
}

// wrapErr is a tiny helper that wraps with %w semantics.
func wrapErr(err error) error {
	return wrappedErr{inner: err}
}

type wrappedErr struct{ inner error }

func (w wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrappedErr) Unwrap() error { return w.inner }

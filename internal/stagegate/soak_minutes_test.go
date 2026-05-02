package stagegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSoakMinutes_Type(t *testing.T) {
	g := SoakMinutes{}
	if g.Type() != "soak_minutes" {
		t.Errorf("Type() = %q, want soak_minutes", g.Type())
	}
}

func TestSoakMinutes_NotYetMerged_Pending(t *testing.T) {
	g := SoakMinutes{}
	stage := StageContext{
		GateConfig:     json.RawMessage(`{"minutes":1}`),
		AllPRsMergedAt: time.Time{}, // zero — stage hasn't reached AllPRsMerged
	}
	passed, _, err := g.Evaluate(context.Background(), nil, stage)
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending, got err=%v", err)
	}
	if passed {
		t.Error("expected passed=false while pending")
	}
}

func TestSoakMinutes_RemainingTime_Pending(t *testing.T) {
	g := SoakMinutes{}
	// Merged 30 seconds ago, soak is 1 minute → still pending.
	stage := StageContext{
		GateConfig:     json.RawMessage(`{"minutes":1}`),
		AllPRsMergedAt: time.Now().Add(-30 * time.Second),
	}
	passed, reason, err := g.Evaluate(context.Background(), nil, stage)
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending, got err=%v", err)
	}
	if passed {
		t.Error("expected passed=false")
	}
	if !strings.Contains(reason, "remaining") {
		t.Errorf("expected reason to mention remaining time, got %q", reason)
	}
}

func TestSoakMinutes_Elapsed_Passed(t *testing.T) {
	g := SoakMinutes{}
	// Merged 2 minutes ago, soak is 1 minute → passed.
	stage := StageContext{
		GateConfig:     json.RawMessage(`{"minutes":1}`),
		AllPRsMergedAt: time.Now().Add(-2 * time.Minute),
	}
	passed, reason, err := g.Evaluate(context.Background(), nil, stage)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if !passed {
		t.Error("expected passed=true after soak elapsed")
	}
	if !strings.Contains(reason, "elapsed") {
		t.Errorf("expected reason to mention elapsed, got %q", reason)
	}
}

func TestSoakMinutes_InvalidConfig_Errors(t *testing.T) {
	g := SoakMinutes{}
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"non-positive minutes", `{"minutes":0}`, "must be positive"},
		{"negative minutes", `{"minutes":-5}`, "must be positive"},
		{"malformed json", `{minutes:`, "parse config"},
		{"wrong type", `{"minutes":"sixty"}`, "parse config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage := StageContext{
				GateConfig:     json.RawMessage(tc.config),
				AllPRsMergedAt: time.Now(),
			}
			_, _, err := g.Evaluate(context.Background(), nil, stage)
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, ErrPending) {
				t.Errorf("expected non-Pending error for invalid config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected err to contain %q, got %v", tc.want, err)
			}
		})
	}
}

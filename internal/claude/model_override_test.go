// Package claude — D7 model-override plumbing tests.
//
// These tests guard the swap-point that lets a paired-runs experiment
// downgrade an agent to Haiku per-arm. The contract:
//
//  1. buildClaudeArgs emits `--model <id>` exactly when modelOverride is non-empty.
//  2. RequestedModel(ctx) round-trips through withRequestedModel.
//  3. The TreatmentApplyHook can stash a model on the ctx; downstream
//     readers (defaultCLIRunner, RunCLIStreamingContext exec branch)
//     pull it back via RequestedModel.
//  4. When no hook is installed OR the hook returns "", argv carries no --model.
//
// Anti-cheat: the model swap is the load-bearing seam for cost/quality
// experiments. A regression that silently drops the override would
// invalidate every D7 experiment's data — these tests are the AST-level
// guard.

package claude

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBuildClaudeArgs_ModelOverrideEmpty(t *testing.T) {
	args := buildClaudeArgs("p", "", "", "", 1, "json", "")
	for i, a := range args {
		if a == "--model" {
			t.Errorf("args[%d]=--model emitted with empty modelOverride; argv=%v", i, args)
		}
	}
}

func TestBuildClaudeArgs_ModelOverrideSet(t *testing.T) {
	args := buildClaudeArgs("p", "", "", "", 1, "json", "claude-haiku-4-5-20251001")
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--model" && args[i+1] == "claude-haiku-4-5-20251001" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--model claude-haiku-4-5-20251001 not in argv; argv=%v", args)
	}
}

func TestRequestedModel_RoundTrip(t *testing.T) {
	ctx := withRequestedModel(context.Background(), "claude-haiku-4-5-20251001")
	got := RequestedModel(ctx)
	if got != "claude-haiku-4-5-20251001" {
		t.Errorf("RequestedModel: got %q want claude-haiku-4-5-20251001", got)
	}
}

func TestRequestedModel_EmptyOnUnsetCtx(t *testing.T) {
	if got := RequestedModel(context.Background()); got != "" {
		t.Errorf("RequestedModel on unset ctx: got %q want ''", got)
	}
	if got := RequestedModel(nil); got != "" {
		t.Errorf("RequestedModel(nil): got %q want ''", got)
	}
}

func TestTreatmentApplyHook_StashesModelOnCtx(t *testing.T) {
	// Install a stub hook that returns a model id; the downstream runner
	// must see it via RequestedModel(ctx).
	prev := activeTreatmentApplyHook
	t.Cleanup(func() { activeTreatmentApplyHook = prev })

	SetTreatmentApplyHook(func(ctx context.Context, agent string, taskID int) (string, error) {
		return "claude-haiku-4-5-20251001", nil
	})

	hookedCtx, err := invokeTreatmentApplyHook(context.Background())
	if err != nil {
		t.Fatalf("invokeTreatmentApplyHook: %v", err)
	}
	got := RequestedModel(hookedCtx)
	if got != "claude-haiku-4-5-20251001" {
		t.Errorf("RequestedModel(hooked): got %q want claude-haiku-4-5-20251001", got)
	}
}

func TestTreatmentApplyHook_EmptyModelLeavesCtxClean(t *testing.T) {
	prev := activeTreatmentApplyHook
	t.Cleanup(func() { activeTreatmentApplyHook = prev })

	SetTreatmentApplyHook(func(ctx context.Context, agent string, taskID int) (string, error) {
		return "", nil
	})
	hookedCtx, err := invokeTreatmentApplyHook(context.Background())
	if err != nil {
		t.Fatalf("invokeTreatmentApplyHook: %v", err)
	}
	if got := RequestedModel(hookedCtx); got != "" {
		t.Errorf("RequestedModel after empty-hook: got %q want ''", got)
	}
}

func TestTreatmentApplyHook_NoneInstalled(t *testing.T) {
	prev := activeTreatmentApplyHook
	activeTreatmentApplyHook = nil
	t.Cleanup(func() { activeTreatmentApplyHook = prev })

	hookedCtx, err := invokeTreatmentApplyHook(context.Background())
	if err != nil {
		t.Fatalf("invokeTreatmentApplyHook: %v", err)
	}
	if got := RequestedModel(hookedCtx); got != "" {
		t.Errorf("no hook → ctx should not carry model; got %q", got)
	}
}

// TestAskClaudeCLIContext_HookSwapPropagatesToRunner asserts the
// end-to-end seam: hook installs a model → runner observes it via
// RequestedModel(ctx). Uses a stub CLI runner that captures the ctx
// it received so the assertion happens without spawning a real claude
// subprocess.
func TestAskClaudeCLIContext_HookSwapPropagatesToRunner(t *testing.T) {
	prevHook := activeTreatmentApplyHook
	t.Cleanup(func() { activeTreatmentApplyHook = prevHook })

	SetTreatmentApplyHook(func(ctx context.Context, agent string, taskID int) (string, error) {
		return "claude-haiku-4-5-20251001", nil
	})

	var observedModel string
	SetCLIRunner(func(ctx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
		observedModel = RequestedModel(ctx)
		return "ok response", nil
	})
	t.Cleanup(ResetCLIRunner)

	_, err := AskClaudeCLIContext(context.Background(), "sys", "user", "", "", "", 1)
	if err != nil {
		t.Fatalf("AskClaudeCLIContext: %v", err)
	}
	if observedModel != "claude-haiku-4-5-20251001" {
		t.Errorf("runner observed model=%q; want claude-haiku-4-5-20251001 (hook stashed it via withRequestedModel)", observedModel)
	}
}

// TestAskClaudeCLIContext_NoHookKeepsArgvClean asserts the symmetric
// case: no installed hook → defaultCLIRunner emits no --model. We
// can't easily exec the real claude binary here, so we assert via
// buildClaudeArgs(modelOverride="") — the same code path
// defaultCLIRunner takes when RequestedModel returns "".
func TestAskClaudeCLIContext_NoHookKeepsArgvClean(t *testing.T) {
	args := buildClaudeArgs("p", "", "", "", 1, "json", RequestedModel(context.Background()))
	for _, a := range args {
		if strings.HasPrefix(a, "claude-") || a == "--model" {
			t.Errorf("argv leaked model flag with no hook installed: %v", args)
		}
	}
}

// TestCallWithTranscript_StampsAgentOnCtx asserts the D7 wiring fix:
// CallWithTranscript auto-stamps ctx with the descriptor's Agent +
// TaskID via ensureCallCtx so the treatments.Apply hook sees the
// subject agent. Pre-D7 the only agent threading WithClaudeCallContext
// was Captain; without this stamp, every subject agent's hook
// invocation would receive agent="" and never match an experiment
// by subject_agent.
func TestCallWithTranscript_StampsAgentOnCtx(t *testing.T) {
	prevHook := activeTreatmentApplyHook
	t.Cleanup(func() { activeTreatmentApplyHook = prevHook })

	var observedAgent string
	var observedTask int
	SetTreatmentApplyHook(func(ctx context.Context, agent string, taskID int) (string, error) {
		observedAgent = agent
		observedTask = taskID
		return "", nil
	})
	SetCLIRunner(func(ctx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
		return "ok response", nil
	})
	t.Cleanup(ResetCLIRunner)

	_, err := CallWithTranscript(context.Background(),
		CallDescriptor{Agent: "boot", TaskID: 42, PromptVersion: "boot-triage-v1"},
		"sys", "usr", "", "", "", 1)
	if err != nil {
		t.Fatalf("CallWithTranscript: %v", err)
	}
	if observedAgent != "boot" {
		t.Errorf("hook saw agent=%q, want 'boot' (D7 wiring: descriptor must auto-stamp ctx)", observedAgent)
	}
	if observedTask != 42 {
		t.Errorf("hook saw taskID=%d, want 42", observedTask)
	}
}

// TestCallWithTranscript_ExistingStampWins guards the no-clobber rule:
// when the caller already stamped a non-empty agent (e.g. Captain
// wires its own with byte-attribution contributions), ensureCallCtx
// must NOT overwrite it. Otherwise Captain's prompt-byte attribution
// rows would land under the wrong agent.
func TestCallWithTranscript_ExistingStampWins(t *testing.T) {
	prevHook := activeTreatmentApplyHook
	t.Cleanup(func() { activeTreatmentApplyHook = prevHook })

	var observedAgent string
	SetTreatmentApplyHook(func(ctx context.Context, agent string, taskID int) (string, error) {
		observedAgent = agent
		return "", nil
	})
	SetCLIRunner(func(ctx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
		return "ok response", nil
	})
	t.Cleanup(ResetCLIRunner)

	parent := WithClaudeCallContext(context.Background(), "captain", 99, nil)
	// Caller passes a different desc.Agent — ensureCallCtx must
	// preserve the parent stamp, not clobber it.
	_, err := CallWithTranscript(parent,
		CallDescriptor{Agent: "boot", TaskID: 42},
		"sys", "usr", "", "", "", 1)
	if err != nil {
		t.Fatalf("CallWithTranscript: %v", err)
	}
	if observedAgent != "captain" {
		t.Errorf("inner stamp clobbered outer: hook saw %q want 'captain'", observedAgent)
	}
}

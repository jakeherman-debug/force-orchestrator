package agents

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// TestRunChancellorReview_RespectsContextCancellation is the D3 P1 follow-up B
// regression: pre-fix, runChancellorReview + synthesizeMergedPlan internally
// used context.Background(), so a SIGINT / e-stop on the daemon could not
// interrupt an in-flight Claude CLI call. The fix threads the daemon-
// cancellable ctx from SpawnChancellor → runChancellorReview → claude.
// AskClaudeCLIContext.
//
// Anti-cheat: this test exercises a REAL ctx cancellation. The Claude stub
// blocks on the ctx it receives (the same one runChancellorReview passes to
// AskClaudeCLIContext); the test cancels the outer ctx; the stub returns
// ctx.Err(). Pre-fix this would deadlock until the per-call timeout fired
// (or the test's own timeout) because runChancellorReview's
// context.Background() was opaque to the caller.
//
// Pattern P11's "fabricated context" defense applies — context.WithTimeout
// (context.Background(), …) would let this test pass cosmetically while
// detaching the subprocess. The check that proves the fix is real: the
// stub's ctx MUST cancel within the test's bounded wait window after we
// cancel the outer ctx.
func TestRunChancellorReview_RespectsContextCancellation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")
	featureID := store.AddBounty(db, 0, "Feature", "ctx-cancellation feature")
	feature, _ := store.GetBounty(db, featureID)

	// stubCalled fires the moment the runner is invoked — the test uses
	// this to guarantee the LLM call is in flight before we cancel the
	// outer ctx (otherwise we'd race the cancellation against the stub's
	// own scheduling).
	stubCalled := make(chan struct{})
	// stubCtxErr captures whatever ctx.Err() the stub observed when its
	// ctx was cancelled. The test asserts this is context.Canceled (the
	// real cancellation), not something fabricated.
	stubCtxErrCh := make(chan error, 1)

	claude.SetCLIRunner(func(runnerCtx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
		close(stubCalled)
		// Block on the runner's ctx — this is the threaded ctx from
		// runChancellorReview. If the fix is real, cancelling the outer
		// ctx propagates here and Done() fires. Pre-fix, runChancellor-
		// Review used context.Background() internally so this would
		// block until the test's own deadline.
		select {
		case <-runnerCtx.Done():
			stubCtxErrCh <- runnerCtx.Err()
			return "", runnerCtx.Err()
		case <-time.After(5 * time.Second):
			// Bounded fallback so a regressed test fails fast rather
			// than hangs the suite.
			stubCtxErrCh <- errors.New("stub ctx not cancelled within 5s — ctx threading regressed")
			return "", errors.New("ctx not cancelled")
		}
	})
	t.Cleanup(claude.ResetCLIRunner)

	ctx, cancel := context.WithCancel(context.Background())
	tasks := []store.TaskPlan{{TempID: 1, Repo: "myrepo", Task: "do the work"}}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// Run the Chancellor review on a goroutine so we can cancel ctx
	// while it's blocked inside the stubbed CLI runner.
	done := make(chan struct{})
	go func() {
		runChancellorReview(ctx, db, feature, tasks, mustLoadCapProfile(t, "chancellor"), logger)
		close(done)
	}()

	// Wait for the stub to be entered before we cancel — this ensures
	// the cancellation actually races the in-flight LLM call rather
	// than landing before the runner is even called.
	select {
	case <-stubCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("Claude stub never invoked — runChancellorReview did not reach the LLM call site")
	}

	cancel()

	// The stub's ctx must observe the cancellation.
	select {
	case stubErr := <-stubCtxErrCh:
		if !errors.Is(stubErr, context.Canceled) {
			t.Fatalf("expected stub ctx to observe context.Canceled (proving caller-supplied ctx threaded through), got %v", stubErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stub ctx error never reported — ctx cancellation did not propagate to the runner")
	}

	// runChancellorReview should now complete (its claude call returned
	// an error, which routes to the fail-closed branch).
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runChancellorReview did not return after ctx cancellation — ctx threading regressed")
	}

	// Sanity check: the fail-closed branch ran on Claude error, so the
	// Feature should be Failed (not silently auto-approved). This is the
	// orthogonal AUDIT-116 contract; we restate it here so a regression
	// that loses ctx-cancellation but still hits some other terminator
	// is caught explicitly.
	after, _ := store.GetBounty(db, featureID)
	if after.Status != "Failed" {
		t.Errorf("after ctx cancellation, expected Feature status Failed (fail-closed on Claude error), got %q", after.Status)
	}
}

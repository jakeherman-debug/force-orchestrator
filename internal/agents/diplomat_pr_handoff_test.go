package agents

// D10 — PRHandoffSynthesis tests.
//
// Coverage:
//   - Anti-cheat #1 (default OFF): a fresh repo has
//     handoff_synthesis_enabled=0; QueuePRHandoffSynthesis is a no-op.
//   - Flag-on enqueues the task; runPRHandoffSynthesis posts a
//     comment via gh and lands a PRHandoffSyntheses row.
//   - Flag-off at run time short-circuits to Completed (the no-op
//     transition path).
//   - Idempotency: a second QueuePRHandoffSynthesis for the same
//     convoy returns 0, nil (already queued).
//   - Convoy with no enabled repos completes as a no-op (multi-
//     repo convoy with one enrolled, one not — the enrolled gets
//     a comment, the not-enrolled is silently skipped).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// stubGHRunnerForHandoff captures calls to `gh pr comment ... --body-file -`
// and records the body bytes for assertions.
type stubGHRunnerForHandoff struct {
	calls    []stubGHCall
	failBody bool
}

type stubGHCall struct {
	cwd  string
	args []string
	body []byte
}

func (s *stubGHRunnerForHandoff) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	s.calls = append(s.calls, stubGHCall{cwd: cwd, args: append([]string(nil), args...), body: append([]byte(nil), stdin...)})
	if s.failBody {
		return nil, []byte("synthetic gh failure"), errors.New("gh: forced test failure")
	}
	return []byte("{}"), nil, nil
}

func diplomatHandoffSynthesisProfile(t *testing.T) *capabilities.Profile {
	t.Helper()
	prof, err := capabilities.LoadProfile("diplomat")
	if err != nil {
		t.Fatalf("LoadProfile(diplomat): %v", err)
	}
	return prof
}

// seedConvoyForHandoffSynthesis registers a repo, registers a convoy +
// ConvoyAskBranch with a draft PR open. Returns convoyID + repo name.
func seedConvoyForHandoffSynthesis(t *testing.T, db *sql.DB, repoName string, enabled bool) (int, string) {
	t.Helper()
	store.AddRepo(db, repoName, t.TempDir(), "test repo")
	if err := store.SetRepoRemoteInfo(db, repoName, "git@github.com:acme/"+repoName+".git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo: %v", err)
	}
	if enabled {
		if err := store.SetHandoffSynthesisEnabled(db, repoName, true); err != nil {
			t.Fatalf("SetHandoffSynthesisEnabled: %v", err)
		}
	}
	cid, err := store.CreateConvoy(db, "test handoff convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	if cid == 0 {
		t.Fatalf("CreateConvoy returned 0")
	}
	if err := store.UpsertConvoyAskBranch(db, cid, repoName, "convoy/handoff/ask", "0000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("UpsertConvoyAskBranch: %v", err)
	}
	if err := store.SetConvoyAskBranchDraftPR(db, cid, repoName,
		"https://github.com/acme/"+repoName+"/pull/42", 42, "Open"); err != nil {
		t.Fatalf("SetConvoyAskBranchDraftPR: %v", err)
	}
	return cid, repoName
}

// TestPRHandoffSynthesis_DefaultOffDoesNotEnqueue covers anti-cheat
// directive #1: a fresh repo has handoff_synthesis_enabled=0, and
// QueuePRHandoffSynthesis returns (0, nil) — no row lands on the
// BountyBoard.
func TestPRHandoffSynthesis_DefaultOffDoesNotEnqueue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := seedConvoyForHandoffSynthesis(t, db, "default-off-repo", false)

	id, err := QueuePRHandoffSynthesis(db, cid, "")
	if err != nil {
		t.Fatalf("QueuePRHandoffSynthesis: %v", err)
	}
	if id != 0 {
		t.Errorf("expected (0, nil) when handoff_synthesis_enabled=0, got id=%d", id)
	}

	// Verify nothing landed on BountyBoard.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'PRHandoffSynthesis' AND convoy_id = ?`, cid).Scan(&n)
	if n != 0 {
		t.Errorf("expected zero PRHandoffSynthesis rows when flag is off; got %d", n)
	}
}

// TestPRHandoffSynthesis_FlagOnEnqueues covers anti-cheat #1's
// converse: enabling the flag results in a real BountyBoard row.
func TestPRHandoffSynthesis_FlagOnEnqueues(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := seedConvoyForHandoffSynthesis(t, db, "flag-on-repo", true)

	id, err := QueuePRHandoffSynthesis(db, cid, "treatment_on")
	if err != nil {
		t.Fatalf("QueuePRHandoffSynthesis: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive task id when flag enabled; got %d", id)
	}

	// Idempotent re-queue is a no-op (returns 0).
	id2, err := QueuePRHandoffSynthesis(db, cid, "treatment_on")
	if err != nil {
		t.Fatalf("QueuePRHandoffSynthesis (re-queue): %v", err)
	}
	if id2 != 0 {
		t.Errorf("expected re-queue to return 0; got %d", id2)
	}
}

// TestPRHandoffSynthesis_PostsComment_Smoke runs the claim-loop
// handler end-to-end with stubbed Claude + gh: convoy enabled, draft
// PR open, runPRHandoffSynthesis lands a comment + PRHandoffSyntheses
// row.
func TestPRHandoffSynthesis_PostsComment_Smoke(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := seedConvoyForHandoffSynthesis(t, db, "smoke-repo", true)

	// Stub Claude to return a fixed reviewer narrative.
	stub := withStubCLIRunner(t, "## What changed\n\nA test convoy ran.\n", nil)
	_ = stub

	// Stub gh runner.
	ghStub := &stubGHRunnerForHandoff{}
	restoreGH := SetGHClientFactory(func() *gh.Client { return gh.NewClientWithRunner(ghStub) })
	defer restoreGH()

	// Enqueue + claim + run.
	taskID, err := QueuePRHandoffSynthesis(db, cid, "treatment_on")
	if err != nil {
		t.Fatalf("QueuePRHandoffSynthesis: %v", err)
	}
	if taskID == 0 {
		t.Fatalf("expected non-zero task id with flag enabled")
	}
	bounty, claimed := store.ClaimBounty(db, "PRHandoffSynthesis", "diplomat-test")
	if !claimed {
		t.Fatalf("expected to claim PRHandoffSynthesis bounty")
	}
	prof := diplomatHandoffSynthesisProfile(t)
	logger := testLogger{}
	runPRHandoffSynthesis(context.Background(), db, "diplomat-test", bounty, prof, logger)

	// Verify the bounty completed.
	finalBounty, _ := store.GetBounty(db, taskID)
	if finalBounty.Status != "Completed" {
		t.Errorf("expected status=Completed; got %q", finalBounty.Status)
	}

	// Verify the gh runner saw a `pr comment` invocation.
	if len(ghStub.calls) == 0 {
		t.Fatalf("expected at least one gh call; got 0")
	}
	sawComment := false
	for _, c := range ghStub.calls {
		if len(c.args) > 0 && c.args[0] == "pr" && len(c.args) > 1 && c.args[1] == "comment" {
			sawComment = true
			if !strings.Contains(string(c.body), "What changed") {
				t.Errorf("expected stubbed Claude body to flow into gh body; got %q", string(c.body))
			}
			if !strings.Contains(string(c.body), "AUTO-GENERATED") {
				t.Errorf("expected appended footer with AUTO-GENERATED marker; body=%q", string(c.body))
			}
		}
	}
	if !sawComment {
		t.Errorf("expected `gh pr comment` call; saw %v", ghStub.calls)
	}

	// Verify a PRHandoffSyntheses row landed.
	rows, err := store.ListPRHandoffSynthesesForConvoy(db, cid)
	if err != nil {
		t.Fatalf("ListPRHandoffSynthesesForConvoy: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 PRHandoffSyntheses row; got %d", len(rows))
	}
	if rows[0].ExperimentArm != "treatment_on" {
		t.Errorf("expected experiment_arm=treatment_on; got %q", rows[0].ExperimentArm)
	}
	if rows[0].PRURL == "" || rows[0].PostedAt == "" {
		t.Errorf("expected non-empty pr_url + posted_at; got %+v", rows[0])
	}
}

// TestPRHandoffSynthesis_RuntimeFlagOffNoOps covers the run-time
// gate: even if the task somehow landed (e.g. a queued row from
// before the operator turned the flag off), the runtime path
// completes as a no-op rather than firing the LLM call.
func TestPRHandoffSynthesis_RuntimeFlagOffNoOps(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, repo := seedConvoyForHandoffSynthesis(t, db, "runtime-off-repo", true)
	taskID, err := QueuePRHandoffSynthesis(db, cid, "")
	if err != nil {
		t.Fatalf("QueuePRHandoffSynthesis: %v", err)
	}
	if taskID == 0 {
		t.Fatalf("expected non-zero task id while flag enabled")
	}

	// Now flip the flag back off (operator opt-out mid-flight).
	if err := store.SetHandoffSynthesisEnabled(db, repo, false); err != nil {
		t.Fatalf("SetHandoffSynthesisEnabled(false): %v", err)
	}

	// Stub Claude to fail loudly if it gets called — anti-cheat #1
	// states "flag off → does not fire".
	stub := withStubCLIRunnerFn(t, func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		t.Errorf("Claude must NOT be called when flag is off at run-time")
		return "", errors.New("must not be called")
	})
	_ = stub

	bounty, claimed := store.ClaimBounty(db, "PRHandoffSynthesis", "diplomat-test")
	if !claimed {
		t.Fatalf("expected to claim PRHandoffSynthesis bounty")
	}
	prof := diplomatHandoffSynthesisProfile(t)
	runPRHandoffSynthesis(context.Background(), db, "diplomat-test", bounty, prof, testLogger{})

	finalBounty, _ := store.GetBounty(db, taskID)
	if finalBounty.Status != "Completed" {
		t.Errorf("expected runtime-flag-off → Completed (no-op); got %q", finalBounty.Status)
	}
	rows, _ := store.ListPRHandoffSynthesesForConvoy(db, cid)
	if len(rows) != 0 {
		t.Errorf("expected no PRHandoffSyntheses rows when runtime gate said no; got %d", len(rows))
	}
}

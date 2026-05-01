package agents

import (
	"context"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"force-orchestrator/internal/agents/adversarial"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// TestHotPathAdversarialPair_RateZeroSkips confirms an explicit
// operator-authored rate=0 is a clean no-op fast path. No goroutine,
// no DB writes. (Distinct from the absent-key path: see
// TestHotPathAdversarialPair_DefaultRampWhenKeyAbsent.)
func TestHotPathAdversarialPair_RateZeroSkips(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "0")

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    101,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		Reasoning:     "small diff",
		PromptVersion: "council-v1",
	}
	if _, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); scheduled {
		t.Errorf("rate=0: pair must NOT be scheduled; got scheduled=true")
	}
	// And no AdversarialPairings row should appear.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM AdversarialPairings`).Scan(&n)
	if n != 0 {
		t.Errorf("rate=0: AdversarialPairings rows expected=0; got %d", n)
	}
}

// TestHotPathAdversarialPair_DefaultRampWhenKeyAbsent — fresh-deploy
// path: no SystemConfig row → adversarialPairingRate falls back to
// the default ramp (0.1), so a primary decision rolling under 0.1
// schedules. Iter1 left this branch returning 0.0, which silenced
// fresh deploys; iter2 ramps it up so AdversarialPairings rows
// accumulate per exit criterion 10.
func TestHotPathAdversarialPair_DefaultRampWhenKeyAbsent(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Fresh DB — no SystemConfig row for adversarial_pairing_rate.
	got := adversarialPairingRate(db)
	if got != adversarialPairingRateDefault {
		t.Errorf("adversarialPairingRate fresh DB: got %f, want %f (default ramp)", got, adversarialPairingRateDefault)
	}

	// Pin the dice below the default (any value < 0.1 should sample).
	restore := setAdversarialDiceRollForTest(0.05)
	defer restore()

	// Register a stub critic so the goroutine doesn't crash on the
	// no-critic path.
	var ran sync.WaitGroup
	ran.Add(1)
	adversarial.RegisterCritic(adversarial.AgentCouncil,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			defer ran.Done()
			return adversarial.CriticOutcome{Outcome: `{"approved":true}`, PromptVersion: "council-critic-v1"}, nil
		})
	defer EnableAdversarialPairing(context.Background())

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    707,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v1",
	}
	handle, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger)
	if !scheduled {
		t.Fatalf("default ramp + dice=0.05: pair should be scheduled")
	}
	if !waitWGTimeout(&ran, 3*time.Second) {
		t.Fatalf("critic goroutine did not run within 3s under default ramp")
	}
	// Drain the join handle so the goroutine doesn't outlive the test
	// and race t.Cleanup-installed CLI runner resets in sibling tests.
	_ = handle.Wait(context.Background())
}

// TestHotPathAdversarialPair_ExplicitEmptyTreatedAsSilence — the
// distinction between "key missing" (default ramp) and "key set to
// empty string" (operator silenced explicitly) MUST not blur. An
// operator who authored an empty value gets silence.
func TestHotPathAdversarialPair_ExplicitEmptyTreatedAsSilence(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "")

	got := adversarialPairingRate(db)
	if got != 0.0 {
		t.Errorf("explicit empty rate: got %f, want 0.0 (silence)", got)
	}

	restore := setAdversarialDiceRollForTest(0.0)
	defer restore()

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    808,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v1",
	}
	if _, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); scheduled {
		t.Errorf("explicit empty rate: pair should NOT be scheduled")
	}
}

// TestHotPathAdversarialPair_RateOneSchedulesAndPersists pins rate=1
// (always sample) and registers a stub critic that disagrees. The
// helper schedules, the goroutine runs, and we observe the persisted
// AdversarialPairings row + Fleet_Mail surfacing.
func TestHotPathAdversarialPair_RateOneSchedulesAndPersists(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "1.0")

	// Force the dice to deterministically fall under any positive rate.
	restore := setAdversarialDiceRollForTest(0.0)
	defer restore()

	// Stub critic that always disagrees with Council's "approved=true".
	var pairWrittenWG sync.WaitGroup
	pairWrittenWG.Add(1)
	adversarial.RegisterCritic(adversarial.AgentCouncil,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			defer pairWrittenWG.Done() // signal: critic ran
			return adversarial.CriticOutcome{
				Outcome:       `{"approved":false,"feedback":"missed boundary"}`,
				PromptVersion: "council-critic-v1",
			}, nil
		})
	defer EnableAdversarialPairing(context.Background())

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    202,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		Reasoning:     "diff is fine",
		PromptVersion: "council-v1",
	}
	handle, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger)
	if !scheduled {
		t.Fatalf("rate=1.0 dice=0: pair should be scheduled")
	}

	// Wait for the critic to run (goroutine).
	if !waitWGTimeout(&pairWrittenWG, 3*time.Second) {
		t.Fatalf("critic goroutine did not run within 3s")
	}
	// Drain the join handle so the goroutine fully exits before the
	// surrounding teardown rolls (Pre-D4 race baseline fix).
	defer func() { _ = handle.Wait(context.Background()) }()
	// The pair persistence + surfacing happens inline in the
	// goroutine's runHotPathPair; give it a beat to drain.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var pn, mn int
		db.QueryRow(`SELECT COUNT(*) FROM AdversarialPairings`).Scan(&pn)
		db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent='adversarial-pairing'`).Scan(&mn)
		if pn == 1 && mn == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	var pn, mn int
	db.QueryRow(`SELECT COUNT(*) FROM AdversarialPairings`).Scan(&pn)
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent='adversarial-pairing'`).Scan(&mn)
	t.Errorf("after wait: AdversarialPairings=%d (want 1), surfacing mail=%d (want 1)", pn, mn)
}

// TestHotPathAdversarialPair_DiceAboveRateSkips confirms the dice
// gate works: when the dice rolls ABOVE the configured rate, no
// pair is scheduled.
func TestHotPathAdversarialPair_DiceAboveRateSkips(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "0.1")
	restore := setAdversarialDiceRollForTest(0.5) // above 0.1 → skip
	defer restore()

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    303,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v1",
	}
	if _, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); scheduled {
		t.Errorf("dice=0.5 rate=0.1: pair should NOT be scheduled")
	}
}

// TestHotPathAdversarialPair_MalformedRateValue confirms parse failures
// fail closed (rate=0 → no sampling).
func TestHotPathAdversarialPair_MalformedRateValue(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "not-a-float")
	restore := setAdversarialDiceRollForTest(0.0)
	defer restore()

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    404,
		Agent:         adversarial.AgentMedic,
		Outcome:       `{"decision":"requeue"}`,
		PromptVersion: "medic-v1",
	}
	if _, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); scheduled {
		t.Errorf("malformed rate: pair should NOT be scheduled (fail-closed)")
	}
}

// TestHotPathAdversarialPair_OutOfRangeRateValue confirms negative /
// >1 rate values are also fail-closed.
func TestHotPathAdversarialPair_OutOfRangeRateValue(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for _, v := range []string{"-0.5", "1.5", "999"} {
		store.SetConfig(db, adversarialPairingRateKey, v)
		restore := setAdversarialDiceRollForTest(0.0)
		logger := log.New(os.Stderr, "[test] ", 0)
		primary := adversarial.PrimaryDecision{
			DecisionID:    505,
			Agent:         adversarial.AgentMedic,
			Outcome:       `{"decision":"requeue"}`,
			PromptVersion: "medic-v1",
		}
		_, got := WrapHotPathAdversarialPair(context.Background(), db, primary, logger)
		restore()
		if got {
			t.Errorf("rate=%q: pair should NOT be scheduled (out-of-range fail-closed)", v)
		}
	}
}

// TestHotPathAdversarialPair_NilDBNoOp ensures a missing DB doesn't
// panic — covers the early-return defensive path.
func TestHotPathAdversarialPair_NilDBNoOp(t *testing.T) {
	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    606,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{}`,
		PromptVersion: "council-v1",
	}
	if _, scheduled := WrapHotPathAdversarialPair(context.Background(), nil, primary, logger); scheduled {
		t.Errorf("nil db: pair should NOT be scheduled (defensive no-op)")
	}
}

// TestHotPathAdversarialPair_AllThreeAgentsCovered exercises Council,
// Medic, and ConvoyReview each through the helper with rate=1, stub
// critics, and confirms one AdversarialPairings row per agent — that
// the hot path is wired for all three (exit criterion 10).
func TestHotPathAdversarialPair_AllThreeAgentsCovered(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "1.0")
	restore := setAdversarialDiceRollForTest(0.0)
	defer restore()

	var counter sync.WaitGroup
	counter.Add(3)
	adversarial.RegisterCritic(adversarial.AgentCouncil,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			defer counter.Done()
			return adversarial.CriticOutcome{Outcome: `{"approved":false}`, PromptVersion: "council-critic-v1"}, nil
		})
	adversarial.RegisterCritic(adversarial.AgentMedic,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			defer counter.Done()
			return adversarial.CriticOutcome{Outcome: `{"decision":"escalate"}`, PromptVersion: "medic-critic-v1"}, nil
		})
	adversarial.RegisterCritic(adversarial.AgentConvoyReview,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			defer counter.Done()
			return adversarial.CriticOutcome{Outcome: `{"status":"findings","findings":[]}`, PromptVersion: "convoy-review-critic-v1"}, nil
		})
	defer EnableAdversarialPairing(context.Background())

	logger := log.New(os.Stderr, "[test] ", 0)

	handles := make([]*HotPathPairHandle, 0, 3)
	for _, c := range []struct {
		agent   adversarial.Agent
		out     string
		primaryVer string
		decID   int64
	}{
		{adversarial.AgentCouncil, `{"approved":true}`, "council-v1", 1001},
		{adversarial.AgentMedic, `{"decision":"requeue"}`, "medic-v1", 1002},
		{adversarial.AgentConvoyReview, `{"status":"clean","findings":[]}`, "convoy-review-v1", 1003},
	} {
		h, _ := WrapHotPathAdversarialPair(context.Background(), db, adversarial.PrimaryDecision{
			DecisionID:    c.decID,
			Agent:         c.agent,
			Outcome:       c.out,
			PromptVersion: c.primaryVer,
		}, logger)
		handles = append(handles, h)
	}
	if !waitWGTimeout(&counter, 5*time.Second) {
		t.Fatalf("not all three critics ran within 5s")
	}
	// Drain all join handles so the goroutines fully exit before
	// teardown (Pre-D4 race baseline fix).
	for _, h := range handles {
		_ = h.Wait(context.Background())
	}
	// Drain inserts.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM AdversarialPairings`).Scan(&n)
		if n == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rows, err := db.Query(`SELECT agent FROM AdversarialPairings ORDER BY decision_id ASC`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		got = append(got, a)
	}
	want := []string{"council", "medic", "convoy_review"}
	if len(got) != len(want) {
		t.Fatalf("AdversarialPairings rows: got %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// waitWGTimeout returns true if the WG counts to zero before the
// timeout, false otherwise.
func waitWGTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestHotPathPairHandle_NilWaitIsNoOp guards the contract that a nil
// handle (returned when no pair was scheduled) is safe to Wait /
// Cancel without crashing. Production callers `defer
// pairHandle.Wait(ctx)` unconditionally; if rate=0 the handle is nil
// and the deferred call must do nothing.
func TestHotPathPairHandle_NilWaitIsNoOp(t *testing.T) {
	var h *HotPathPairHandle
	if err := h.Wait(context.Background()); err != nil {
		t.Errorf("nil handle Wait: got err=%v, want nil", err)
	}
	// Cancel on nil must also not panic.
	h.Cancel()
}

// TestHotPathPairHandle_WaitDrainsGoroutine confirms Wait actually
// blocks until the background goroutine finishes. We register a
// stub critic that holds a small explicit barrier and then signals
// via a chan that the critic's body has run. Wait must return only
// after the critic returns — not before.
func TestHotPathPairHandle_WaitDrainsGoroutine(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "1.0")
	restore := setAdversarialDiceRollForTest(0.0)
	defer restore()

	criticReturned := make(chan struct{})
	release := make(chan struct{})
	adversarial.RegisterCritic(adversarial.AgentCouncil,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			<-release // hold here until the test releases us
			close(criticReturned)
			return adversarial.CriticOutcome{Outcome: `{"approved":true}`, PromptVersion: "council-critic-v1"}, nil
		})
	defer EnableAdversarialPairing(context.Background())

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    909,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v1",
	}
	handle, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger)
	if !scheduled {
		t.Fatalf("rate=1.0 dice=0: pair should be scheduled")
	}

	// Wait must NOT return while the critic is still blocked.
	waitDone := make(chan struct{})
	go func() {
		_ = handle.Wait(context.Background())
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatalf("Wait returned before the goroutine finished — handle is not draining correctly")
	case <-time.After(50 * time.Millisecond):
		// Expected: wait is still blocked.
	}

	// Release the critic. Wait should now return promptly.
	close(release)
	select {
	case <-criticReturned:
	case <-time.After(2 * time.Second):
		t.Fatalf("critic did not return within 2s after release")
	}
	select {
	case <-waitDone:
		// Good — Wait returned after the goroutine finished.
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return within 2s after the goroutine finished")
	}
}

// TestHotPathPairHandle_CtxCancelUnblocksWait confirms that a Wait
// blocked on a still-running goroutine returns ctx.Err() when the
// supplied ctx is cancelled — without requiring the goroutine to
// have finished. This is the SIGINT shape: when the daemon
// cancels its root ctx, every Wait site exits promptly, even if
// the underlying claude subprocess hasn't finished its WaitDelay
// drain yet. Note: after asserting Wait returned via ctx-cancel,
// the test releases the critic and drains the goroutine on a
// fresh Wait so teardown doesn't race the still-running goroutine.
func TestHotPathPairHandle_CtxCancelUnblocksWait(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "1.0")
	restore := setAdversarialDiceRollForTest(0.0)
	defer restore()

	// Critic blocks until the test releases it. The Wait should
	// return via ctx-cancel before the critic ever finishes.
	release := make(chan struct{})
	adversarial.RegisterCritic(adversarial.AgentCouncil,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			<-release
			return adversarial.CriticOutcome{Outcome: `{"approved":true}`, PromptVersion: "council-critic-v1"}, nil
		})

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    910,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v1",
	}
	handle, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger)
	if !scheduled {
		t.Fatalf("rate=1.0 dice=0: pair should be scheduled")
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() { waitDone <- handle.Wait(waitCtx) }()

	// Confirm Wait is blocked, then cancel the wait ctx.
	select {
	case <-waitDone:
		t.Fatalf("Wait returned before ctx-cancel — Wait is not respecting ctx")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-waitDone:
		if err == nil {
			t.Errorf("Wait after ctx-cancel: got nil err, want ctx.Err()")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return within 2s after ctx-cancel")
	}

	// Drain the still-running goroutine before teardown — release
	// the critic, then Wait on a fresh ctx so the goroutine is
	// fully done before EnableAdversarialPairing's RegisterCritic
	// rewrites the critics map. Without this, the running goroutine
	// races teardown — exactly the shape this whole closure fixes.
	close(release)
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatalf("drain Wait: %v", err)
	}
	EnableAdversarialPairing(context.Background())
}

// TestHotPathPairHandle_RaceRegression_CouncilCleanup is the targeted
// regression for the pre-D4 race baseline finding. It mirrors the
// Council shape that surfaced the data race: a stub CLI runner is
// installed via t.Cleanup, runCouncilTask schedules a sampled pair
// goroutine, and the test exits while the goroutine is still in
// flight. The fix (the join handle waited on by runCouncilTask's
// defer) means the goroutine MUST have drained before the deferred
// ResetCLIRunner in withStubCLIRunner fires. Run with -race to
// confirm no warning is emitted; this test relies on the runner
// atomic guard plus the join handle wait, both at once.
func TestHotPathPairHandle_RaceRegression_CouncilCleanup(t *testing.T) {
	defer clearAdversarialPairingRateOverrideForTest()()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, adversarialPairingRateKey, "1.0")
	restore := setAdversarialDiceRollForTest(0.0)
	defer restore()

	// Register a stub critic that calls through claude.AskClaudeCLIContext —
	// the same path the production critics use. The withStubCLIRunner
	// helper sets the CLI runner; t.Cleanup reverts it. Without the
	// fix, the goroutine could still be inside cliRunner when
	// ResetCLIRunner ran.
	adversarial.RegisterCritic(adversarial.AgentCouncil,
		func(ctx context.Context, p adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
			// Touch the runner read site once to force a Load.
			_, _ = claude.AskClaudeCLIContext(ctx, "sys", "usr", "", "", "", 1)
			return adversarial.CriticOutcome{Outcome: `{"approved":true}`, PromptVersion: "council-critic-v1"}, nil
		})
	defer EnableAdversarialPairing(context.Background())

	withStubCLIRunner(t, `{"approved":true,"feedback":""}`, nil)

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    1234,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v1",
	}
	handle, scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger)
	if !scheduled {
		t.Fatalf("expected pair scheduled at rate=1.0")
	}
	// Wait drains the goroutine before the test returns. Pre-fix this
	// was the missing piece — the goroutine outlived the test and
	// raced t.Cleanup-installed ResetCLIRunner.
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := handle.Wait(waitCtx); err != nil {
		t.Fatalf("handle.Wait: %v", err)
	}
}

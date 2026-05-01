package agents

import (
	"context"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"force-orchestrator/internal/agents/adversarial"
	"force-orchestrator/internal/store"
)

// TestHotPathAdversarialPair_RateZeroSkips confirms the default
// configuration (no SystemConfig key, rate=0) is a clean no-op fast
// path. No goroutine, no DB writes.
func TestHotPathAdversarialPair_RateZeroSkips(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(os.Stderr, "[test] ", 0)
	primary := adversarial.PrimaryDecision{
		DecisionID:    101,
		Agent:         adversarial.AgentCouncil,
		Outcome:       `{"approved":true}`,
		Reasoning:     "small diff",
		PromptVersion: "council-v1",
	}
	if scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); scheduled {
		t.Errorf("rate=0: pair must NOT be scheduled; got scheduled=true")
	}
	// And no AdversarialPairings row should appear.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM AdversarialPairings`).Scan(&n)
	if n != 0 {
		t.Errorf("rate=0: AdversarialPairings rows expected=0; got %d", n)
	}
}

// TestHotPathAdversarialPair_RateOneSchedulesAndPersists pins rate=1
// (always sample) and registers a stub critic that disagrees. The
// helper schedules, the goroutine runs, and we observe the persisted
// AdversarialPairings row + Fleet_Mail surfacing.
func TestHotPathAdversarialPair_RateOneSchedulesAndPersists(t *testing.T) {
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
	if scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); !scheduled {
		t.Fatalf("rate=1.0 dice=0: pair should be scheduled")
	}

	// Wait for the critic to run (goroutine).
	if !waitWGTimeout(&pairWrittenWG, 3*time.Second) {
		t.Fatalf("critic goroutine did not run within 3s")
	}
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
	if scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); scheduled {
		t.Errorf("dice=0.5 rate=0.1: pair should NOT be scheduled")
	}
}

// TestHotPathAdversarialPair_MalformedRateValue confirms parse failures
// fail closed (rate=0 → no sampling).
func TestHotPathAdversarialPair_MalformedRateValue(t *testing.T) {
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
	if scheduled := WrapHotPathAdversarialPair(context.Background(), db, primary, logger); scheduled {
		t.Errorf("malformed rate: pair should NOT be scheduled (fail-closed)")
	}
}

// TestHotPathAdversarialPair_OutOfRangeRateValue confirms negative /
// >1 rate values are also fail-closed.
func TestHotPathAdversarialPair_OutOfRangeRateValue(t *testing.T) {
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
		got := WrapHotPathAdversarialPair(context.Background(), db, primary, logger)
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
	if scheduled := WrapHotPathAdversarialPair(context.Background(), nil, primary, logger); scheduled {
		t.Errorf("nil db: pair should NOT be scheduled (defensive no-op)")
	}
}

// TestHotPathAdversarialPair_AllThreeAgentsCovered exercises Council,
// Medic, and ConvoyReview each through the helper with rate=1, stub
// critics, and confirms one AdversarialPairings row per agent — that
// the hot path is wired for all three (exit criterion 10).
func TestHotPathAdversarialPair_AllThreeAgentsCovered(t *testing.T) {
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
		WrapHotPathAdversarialPair(context.Background(), db, adversarial.PrimaryDecision{
			DecisionID:    c.decID,
			Agent:         c.agent,
			Outcome:       c.out,
			PromptVersion: c.primaryVer,
		}, logger)
	}
	if !waitWGTimeout(&counter, 5*time.Second) {
		t.Fatalf("not all three critics ran within 5s")
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

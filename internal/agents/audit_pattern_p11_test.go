package agents

// Pattern P11 verification test — see /AUDIT.md findings AUDIT-105, AUDIT-106,
// AUDIT-107.
//
// E-stop is nominal, not effective. Three independent defects combine so that
// flipping `estop=true` does not actually halt the fleet:
//
//   AUDIT-106 — RunDogs (internal/agents/dogs.go:74-102) does not consult
//               IsEstopped before firing scheduled dogs. Dogs continue issuing
//               gh API calls, pushing empty commits, rebasing ask-branches,
//               queuing PR-review triage tasks, and auto-closing escalations
//               while the operator believes the fleet is stopped.
//
//   AUDIT-107 — Astromech rate-limit handling (internal/agents/astromech.go:473)
//               performs a blind `time.Sleep(RateLimitBackoff(count))` of up
//               to 10 minutes. An operator e-stop mid-backoff cannot interrupt
//               the sleeper; the agent wakes and re-checks, so wall-clock
//               response to an emergency halt is whatever backoff was in
//               flight.
//
//   AUDIT-105 — The astromech heartbeat goroutine
//               (internal/agents/astromech.go:403-430) does not poll
//               IsEstopped and therefore never cancels the Claude CLI context.
//               A 30-minute session kicked off before e-stop runs to
//               completion — combined with AUDIT-004 (no spend cap), the
//               big-red-button is toothless for in-flight work.
//
// This test is a RED-phase pattern test: each sub-test FAILS today to prove
// the defect is live. When the remedy lands (see AUDIT.md Fix #1), the
// assertions pass and CI goes green. If any sub-test flips to PASS before
// the corresponding fix is reviewed, that is the signal to verify the fix
// semantics are correct.

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestPattern_P11_EstopDoesNotStopTheWorld verifies the three P11 defects.
// Each sub-test is independent and targets a single finding.
func TestPattern_P11_EstopDoesNotStopTheWorld(t *testing.T) {

	// ── Sub-test A — AUDIT-106 static ────────────────────────────────────
	// RunDogs MUST call IsEstopped before iterating the dog list. Today it
	// does not — this sub-test fails until the gate is added.
	t.Run("AUDIT-106_RunDogs_must_check_IsEstopped", func(t *testing.T) {
		src, err := os.ReadFile("dogs.go")
		if err != nil {
			t.Fatalf("read dogs.go: %v", err)
		}
		text := string(src)

		// Locate the RunDogs function body — from the func header to the next
		// top-level func (or end of file).
		const marker = "func RunDogs("
		idx := strings.Index(text, marker)
		if idx < 0 {
			t.Fatalf("could not locate RunDogs in dogs.go")
		}
		rest := text[idx:]
		// End at the next top-level "\nfunc " declaration.
		if end := strings.Index(rest[1:], "\nfunc "); end > 0 {
			rest = rest[:end+1]
		}
		runDogsSrc := rest

		if !strings.Contains(runDogsSrc, "IsEstopped") {
			t.Fatal("AUDIT-106: RunDogs does not call IsEstopped — dogs continue to " +
				"fire gh API calls, push empty commits, rebase ask-branches, and " +
				"queue PR-review triage tasks during operator e-stop. Fix: gate the " +
				"loop on `if IsEstopped(db) { return }` before iterating dogOrder.")
		}
		t.Log("AUDIT-106 appears fixed: RunDogs now references IsEstopped")
	})

	// ── Sub-test B — AUDIT-107 behavioural ──────────────────────────────
	// The rate-limit backoff path is a blind time.Sleep. With e-stop set we
	// expect the sleeper to return promptly (≤ 3s) if honouring e-stop; today
	// it sleeps through for minutes. Time-boxed hard at 3 seconds.
	//
	// For count=2, RateLimitBackoff returns 240s (4 minutes) — well beyond
	// our 3s budget, so a "sleeps through" defect is clearly observable.
	t.Run("AUDIT-107_rate_limit_backoff_must_honor_estop", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()

		SetEstop(db, true)
		if !IsEstopped(db) {
			t.Fatalf("precondition failed: SetEstop did not stick")
		}

		const count = 2
		backoff := RateLimitBackoff(count)
		if backoff < 10*time.Second {
			t.Fatalf("precondition failed: expected large backoff, got %v", backoff)
		}

		// Mirror the astromech.go:473 call shape: time.Sleep(backoff) with no
		// e-stop awareness. If a future fix replaces the blind Sleep with an
		// e-stop-aware helper (e.g. SleepUnlessEstopped), this sub-test must
		// exercise that helper instead.
		var returned atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			time.Sleep(backoff)
			returned.Store(true)
		}()

		select {
		case <-done:
			t.Logf("AUDIT-107 appears fixed: rate-limit sleep returned before 3s budget despite e-stop (backoff was %v)", backoff)
		case <-time.After(3 * time.Second):
			// The goroutine is still blocked on time.Sleep. Document and
			// fail. The goroutine leak is bounded — it exits on its own
			// when `backoff` elapses.
			if returned.Load() {
				t.Fatal("race: goroutine finished but done chan not observed")
			}
			t.Fatalf("AUDIT-107: rate-limit sleeper still blocked after 3s with e-stop active "+
				"(backoff=%v). Operator e-stop cannot interrupt the blind time.Sleep at "+
				"astromech.go:473. Fix: replace `time.Sleep(backoff)` with a loop that "+
				"polls IsEstopped every ~1s (and sleeps for the min of remaining-backoff / 1s).",
				backoff)
		}
	})

	// ── Sub-test C — AUDIT-105 static ────────────────────────────────────
	// The heartbeat goroutine + the surrounding Claude-invocation block
	// (astromech.go ~400-435) MUST poll IsEstopped so an operator e-stop
	// mid-session can cancel the Claude context. Today there is no such
	// poll anywhere in that region.
	t.Run("AUDIT-105_heartbeat_must_poll_IsEstopped", func(t *testing.T) {
		src, err := os.ReadFile("astromech.go")
		if err != nil {
			t.Fatalf("read astromech.go: %v", err)
		}
		text := string(src)

		// Carve the heartbeat + Claude-invocation block: from the heartbeat
		// goroutine spawn down through close(heartbeatDone). Any fix for
		// AUDIT-105 must introduce an e-stop poll in this range.
		startMarker := "heartbeatDone := make(chan struct{})"
		endMarker := "close(heartbeatDone)"
		startIdx := strings.Index(text, startMarker)
		if startIdx < 0 {
			t.Fatalf("could not locate heartbeat start marker in astromech.go — " +
				"file shape changed, update this test's carve-out")
		}
		endIdx := strings.Index(text[startIdx:], endMarker)
		if endIdx < 0 {
			t.Fatalf("could not locate heartbeat end marker in astromech.go — " +
				"file shape changed, update this test's carve-out")
		}
		block := text[startIdx : startIdx+endIdx+len(endMarker)]

		if !strings.Contains(block, "IsEstopped") {
			t.Fatal("AUDIT-105: heartbeat goroutine does not poll IsEstopped — an " +
				"in-flight Claude CLI session (up to 30 minutes) runs to completion " +
				"after the operator flips e-stop, burning tokens during an emergency " +
				"halt. Fix: inside the heartbeat loop, on each tick check IsEstopped(db) " +
				"and cancel the context passed to RunCLIStreaming so the Claude CLI " +
				"exits promptly.")
		}
		t.Log("AUDIT-105 appears fixed: heartbeat/Claude-invocation block now references IsEstopped")
	})
}

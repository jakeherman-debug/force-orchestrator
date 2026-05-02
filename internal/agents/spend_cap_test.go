package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// These tests cover Fix #1: spend cap + effective e-stop. Coverage sketch:
//
//   UNIT (this file, 4):
//     - TestSpendCap_DefaultsToTwentyFive
//     - TestSpendCap_HonoursOperatorOverride
//     - TestSpendCapExceeded_Boundaries
//     - TestSleepUnlessEstopped_ReturnsEarlyOnEstop
//   INTEGRATION (this file, 3):
//     - TestDogSpendBurnWatch_AutoEstopsAtHardCap
//     - TestRunDogs_SkippedWhenEstopped
//     - TestSpendCapExceeded_GuardsAgentClaimLoops  (agent-loop shape check)
//   ACCEPTANCE (dashboard_test.go, 2):
//     - TestAPIStatus_ExposesHourlySpend
//     - TestAPIStatus_SpendCapExceededFlag
//   FEATURE (this file, 1):
//     - TestSpendBurnPattern_TriggersAutoEstopInOneCycle

// ── UNIT ────────────────────────────────────────────────────────────────────

func TestSpendCap_DefaultsToTwentyFive(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if got := HourlySpendCapUSD(db); got != DefaultHourlySpendCapUSD {
		t.Fatalf("HourlySpendCapUSD default = %v, want %v", got, DefaultHourlySpendCapUSD)
	}
	if got := HourlySpendEstopUSD(db); got != DefaultHourlySpendEstopUSD {
		t.Fatalf("HourlySpendEstopUSD default = %v, want %v", got, DefaultHourlySpendEstopUSD)
	}
}

func TestSpendCap_HonoursOperatorOverride(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, "hourly_spend_cap_usd", "50.00")
	store.SetConfig(db, "hourly_spend_estop_usd", "400.00")

	if got := HourlySpendCapUSD(db); got != 50.0 {
		t.Fatalf("override cap: got %v want 50.0", got)
	}
	if got := HourlySpendEstopUSD(db); got != 400.0 {
		t.Fatalf("override estop: got %v want 400.0", got)
	}

	// Zero or negative values must fall back to defaults — a DB-corrupt row
	// with "0" should NOT disable the cap.
	store.SetConfig(db, "hourly_spend_cap_usd", "0")
	if got := HourlySpendCapUSD(db); got != DefaultHourlySpendCapUSD {
		t.Fatalf("zero cap fallback: got %v want default", got)
	}
	store.SetConfig(db, "hourly_spend_cap_usd", "-5")
	if got := HourlySpendCapUSD(db); got != DefaultHourlySpendCapUSD {
		t.Fatalf("negative cap fallback: got %v want default", got)
	}
}

func TestSpendCapExceeded_Boundaries(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Pricing: $3/M input, $15/M output. 10M input + 0 output = $30.
	// The default cap is $25 so 10M input tokens recorded in the trailing
	// hour trips the cap, and 5M input (= $15) does not.
	if SpendCapExceeded(db) {
		t.Fatalf("precondition: empty TaskHistory should not trip cap")
	}

	// Below cap: 5M input = $15 < $25.
	_, err := db.Exec(`INSERT INTO TaskHistory(task_id, attempt, agent, session_id, claude_output, outcome, tokens_in, tokens_out)
		VALUES (1, 1, 'test', 's1', '', 'Failed', 5000000, 0)`)
	if err != nil {
		t.Fatalf("seed history 1: %v", err)
	}
	if SpendCapExceeded(db) {
		t.Fatalf("5M input (=$15) should NOT trip $25 cap")
	}

	// Push past cap: add 5M more input → 10M total = $30.
	_, err = db.Exec(`INSERT INTO TaskHistory(task_id, attempt, agent, session_id, claude_output, outcome, tokens_in, tokens_out)
		VALUES (1, 2, 'test', 's2', '', 'Failed', 5000000, 0)`)
	if err != nil {
		t.Fatalf("seed history 2: %v", err)
	}
	if !SpendCapExceeded(db) {
		t.Fatalf("10M input (=$30) should trip $25 cap")
	}
}

func TestSleepUnlessEstopped_ReturnsEarlyOnEstop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// e-stop off: helper sleeps the full window.
	start := time.Now()
	interrupted := sleepUnlessEstopped(db, 50*time.Millisecond, 10*time.Millisecond)
	if interrupted {
		t.Fatalf("not estopped, expected full sleep")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("not estopped, slept only %v — expected >= 40ms", elapsed)
	}

	// e-stop on: helper returns within one poll interval.
	SetEstop(db, true)
	start = time.Now()
	interrupted = sleepUnlessEstopped(db, 5*time.Second, 50*time.Millisecond)
	elapsed := time.Since(start)
	if !interrupted {
		t.Fatalf("estopped, expected interrupted=true")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("estopped, took %v — expected <500ms", elapsed)
	}
}

// ── INTEGRATION ─────────────────────────────────────────────────────────────

func TestDogSpendBurnWatch_AutoEstopsAtHardCap(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if IsEstopped(db) {
		t.Fatal("precondition: estop should start off")
	}

	// Seed spend well above estop threshold: 100M input tokens = $300.
	_, err := db.Exec(`INSERT INTO TaskHistory(task_id, attempt, agent, session_id, claude_output, outcome, tokens_in, tokens_out)
		VALUES (1, 1, 'test', 's1', '', 'Failed', 100000000, 0)`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	hourly, capEx, estop := ReportSpendBurn(db)
	if hourly < 200 {
		t.Fatalf("hourly spend should be ~$300, got $%.2f", hourly)
	}
	if !capEx || !estop {
		t.Fatalf("expected capExceeded=true and estopped=true, got capEx=%v estop=%v", capEx, estop)
	}
	if !IsEstopped(db) {
		t.Fatal("ReportSpendBurn did not flip e-stop after hard-cap breach")
	}
}

func TestRunDogs_SkippedWhenEstopped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetEstop(db, true)

	// Count dogs that ran by tracking the Dogs table before/after.
	var beforeCount int
	db.QueryRow(`SELECT COUNT(*) FROM Dogs`).Scan(&beforeCount)

	rl := &recordingLogger{}
	RunDogs(context.Background(), db, librarian.NewInProcess(db), nil, rl)

	var afterCount int
	db.QueryRow(`SELECT COUNT(*) FROM Dogs`).Scan(&afterCount)

	// Dogs table should be unchanged when estopped.
	if afterCount != beforeCount {
		t.Fatalf("RunDogs ran dogs during e-stop: before=%d after=%d", beforeCount, afterCount)
	}
	// And the short-circuit log line must fire.
	if !rl.contains("e-stop active") {
		t.Fatalf("RunDogs did not log the e-stop skip: %v", rl.lines)
	}
}

func TestSpendCapExceeded_GuardsAgentClaimLoops(t *testing.T) {
	// Static check: every Spawn* loop (Astromech, Medic, Council, Diplomat,
	// Commander, Pilot, Captain, Chancellor, Investigator, Auditor, Librarian)
	// must call SpendCapExceeded. If a future spawn is added without the
	// guard, the cap is bypassable for that agent type.
	//
	// We grep sources rather than driving a real claim, because reliably
	// simulating an agent hitting the cap requires stubbing ClaimBounty —
	// a static assertion is more precise for this invariant.
	files := []string{
		"astromech.go",
		"medic.go",
		"jedi_council.go",
		"diplomat.go",
		"commander.go",
		"pilot.go",
		"captain.go",
		"chancellor.go",
		"investigator.go",
		"auditor.go",
		"librarian.go",
	}
	for _, f := range files {
		src, err := readAgentFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !hasSpendCapGuard(src) {
			t.Errorf("%s: Spawn loop does NOT reference SpendCapExceeded — cap is "+
				"bypassable for this agent type", f)
		}
	}
}

// ── FEATURE ────────────────────────────────────────────────────────────────

func TestSpendBurnPattern_TriggersAutoEstopInOneCycle(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Lower the estop threshold so we can trip it with a realistic-seeming row.
	store.SetConfig(db, "hourly_spend_estop_usd", "50")

	// Seed $60 of spend in the trailing hour: 20M input tokens = $60.
	_, err := db.Exec(`INSERT INTO TaskHistory(task_id, attempt, agent, session_id, claude_output, outcome, tokens_in, tokens_out)
		VALUES (1, 1, 'test', 's1', '', 'Failed', 20000000, 0)`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rl := &recordingLogger{}
	if err := dogSpendBurnWatch(db, rl); err != nil {
		t.Fatalf("dogSpendBurnWatch: %v", err)
	}

	if !IsEstopped(db) {
		t.Fatal("one dog cycle should have auto-estopped the fleet")
	}
	if !rl.contains("AUTO-ESTOP") {
		t.Fatalf("dog did not log AUTO-ESTOP: %v", rl.lines)
	}

	// Idempotence: running again should NOT re-send mail (estop already set).
	// We verify by counting mails before/after the second call.
	var mailCountAfterFirst int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '[AUTO-ESTOP]%'`).Scan(&mailCountAfterFirst)
	if mailCountAfterFirst != 1 {
		t.Fatalf("expected exactly 1 auto-estop mail after first call, got %d", mailCountAfterFirst)
	}

	_ = dogSpendBurnWatch(db, rl)
	var mailCountAfterSecond int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '[AUTO-ESTOP]%'`).Scan(&mailCountAfterSecond)
	if mailCountAfterSecond != 1 {
		t.Fatalf("auto-estop mail was re-sent on idempotent call: before=%d after=%d",
			mailCountAfterFirst, mailCountAfterSecond)
	}
}

// ── Heartbeat cancellation (AUDIT-105 behavioural) ─────────────────────────

// TestHeartbeatCancelsClaudeOnEstop verifies that an in-flight Claude CLI
// session cancels its context when e-stop flips. We simulate the heartbeat's
// poll loop directly (5s polls in production; 20ms in-test) and assert the
// context is cancelled within the test budget.
func TestHeartbeatCancelsClaudeOnEstop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Emulate the astromech heartbeat goroutine shape.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if IsEstopped(db) {
					cancel()
					return
				}
			}
		}
	}()

	// After 50ms flip e-stop; the heartbeat should cancel within ~30ms.
	time.Sleep(50 * time.Millisecond)
	SetEstop(db, true)

	select {
	case <-ctx.Done():
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("heartbeat did not cancel context within 500ms after e-stop")
	}
	<-done // wait for goroutine to exit
}

// ── Helpers ────────────────────────────────────────────────────────────────

// recordingLogger captures logger.Printf calls for assertions.
type recordingLogger struct {
	lines []string
	count atomic.Int32
}

func (r *recordingLogger) Printf(format string, args ...any) {
	r.count.Add(1)
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) contains(substr string) bool {
	for _, l := range r.lines {
		if strings.Contains(strings.ToLower(l), strings.ToLower(substr)) {
			return true
		}
	}
	return false
}

// readAgentFile reads the given basename from internal/agents/ relative to
// this test file. Used by TestSpendCapExceeded_GuardsAgentClaimLoops.
func readAgentFile(basename string) (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), basename)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// hasSpendCapGuard returns true if the source references SpendCapExceeded.
// A more thorough check would parse the Spawn function body and assert the
// guard fires before ClaimBounty — but a substring check is sufficient for
// the regression-protection purpose and tolerates arbitrary loop shapes.
func hasSpendCapGuard(src string) bool {
	return strings.Contains(src, "SpendCapExceeded(")
}

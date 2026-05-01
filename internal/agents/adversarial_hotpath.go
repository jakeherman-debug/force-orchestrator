package agents

// adversarial_hotpath.go — D3 fix-loop-1 (slice δ).
//
// Hot-path wiring for adversarial pairing. Exit criterion 10 of D3
// requires production Council / Medic / ConvoyReview decision handlers
// to actually run the pair against a fraction of decisions in flight,
// so that AdversarialPairings rows accumulate disagreement evidence
// the operator can review.
//
// Shape:
//
//   1. The hot-path helper reads SystemConfig key
//      "adversarial_pairing_rate" (a fraction in [0,1]).
//   2. Rolls a per-decision dice; if the dice falls under the rate,
//      invokes adversarial.RunAdversarialPair via the wired
//      CriticFn (council-critic / medic-critic / convoy-review-critic
//      registered by EnableAdversarialPairing).
//   3. On disagreement, calls SurfaceDisagreementToOperator so a
//      Fleet_Mail row + dashboard banner accumulates.
//
// Important contract: the helper is BACKGROUND — it never blocks the
// primary's decision flow. The primary returns the original outcome
// to the caller; the pair runs after, persists, and (on disagreement)
// surfaces. This is intentional: pair runs cost extra LLM tokens, and
// blocking the primary would double-charge latency on every sampled
// decision. The pair's output is for analysis, not gating.
//
// Tests inject deterministic critics via adversarial.RegisterCritic
// and pin the dice via adversarialDiceRoll (the package-level seam).
// Default production: a real-random 0..1 from math/rand/v2.

import (
	"context"
	"database/sql"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"force-orchestrator/internal/agents/adversarial"
	"force-orchestrator/internal/store"
)

// adversarialDiceRoll is the package-level seam for tests. Returns a
// uniform random in [0, 1). Tests overwrite via the helper at the
// bottom of this file.
var adversarialDiceRoll = func() float64 { return rand.Float64() }

// adversarialPairingRateOverride is an OPTIONAL package-level test
// seam that, when non-nil, takes precedence over the DB-resolved
// rate. TestMain pins it to 0 across the unit-test binary so the
// default 10% ramp doesn't introduce non-determinism into tests
// that read stub prompts (the pair's critic call would otherwise
// land in the stub's prompts buffer ~10% of the time, depending on
// dice). Tests that specifically want to exercise the rate logic
// unset the override via the helper at the bottom of this file.
//
// Production code never sets this — it stays nil and the DB-backed
// adversarialPairingRate path runs unchanged.
var adversarialPairingRateOverride atomic.Pointer[float64]

// adversarialPairingRateKey is the SystemConfig key the operator
// twiddles to flip pair sampling on/off. Parse errors and out-of-
// range values fail closed (rate=0 → no sampling) so a malformed
// config row never accidentally turns the spigot on. Absence of the
// key, by contrast, falls back to the default ramp rate — fresh
// deploys need to sample SOMETHING for AdversarialPairings rows to
// accumulate per exit criterion 10. Iter1 closed with a 0-default
// that left fresh deploys mute; iter2 ramps it up.
const adversarialPairingRateKey = "adversarial_pairing_rate"

// adversarialPairingRateDefault is the ramp rate applied when the
// SystemConfig key is missing entirely. 10% sampling is enough that a
// busy convoy surfaces a disagreement within a day or two while still
// being cheap (one extra LLM critic call per ten primary decisions).
// Operators twiddle the SystemConfig key to opt down to 0 (silence)
// or up to 1.0 (always-pair while debugging a surface).
const adversarialPairingRateDefault = 0.1

// adversarialPairingRate reads the configured sampling rate. Returns
// the default ramp (0.1) when the key is absent — fresh deploys need
// to sample some traffic so AdversarialPairings rows accumulate per
// exit criterion 10. Returns 0.0 on parse / range error so a
// malformed config row never accidentally turns the spigot on.
//
// Test-only seam: if adversarialPairingRateOverride is non-nil
// (set by TestMain or per-test helpers), the override takes
// precedence so tests that don't care about pairing get
// deterministic rate=0 behaviour.
func adversarialPairingRate(db *sql.DB) float64 {
	if p := adversarialPairingRateOverride.Load(); p != nil {
		return *p
	}
	// Distinguish "key absent" (fall back to default) from "key set to
	// empty string" (operator-authored silence): GetConfig returns the
	// passed default-string only on the absent path. We pin a sentinel
	// here so we can detect the absence-vs-empty distinction.
	const sentinel = "__force_default__"
	v := store.GetConfig(db, adversarialPairingRateKey, sentinel)
	if v == sentinel {
		return adversarialPairingRateDefault
	}
	v = strings.TrimSpace(v)
	if v == "" {
		// Operator authored an empty string — treat as silence.
		return 0.0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0.0
	}
	if f < 0 || f > 1 {
		return 0.0
	}
	return f
}

// HotPathPairHandle is a join handle returned by
// WrapHotPathAdversarialPair when a pair is scheduled. Callers (the
// production wiring sites in Council, Medic, ConvoyReview) and tests
// use it to wait for the background goroutine to drain before
// tearing down resources the goroutine still depends on. Without the
// handle, a goroutine spawned by a test that exits while the
// goroutine is still inside claude.AskClaudeCLIContext races the
// test's ResetCLIRunner cleanup — the pre-D4 -race baseline finding
// surfaced exactly that shape with TestRunCouncilTask_Rejected_MaxRetries.
//
// The handle is intentionally minimal: Wait(ctx) blocks until the
// goroutine exits or the supplied ctx is cancelled. Cancel() asks
// the goroutine to stop early via its derived ctx — the underlying
// claude exec is already ctx-aware, so cancelling the pair ctx kills
// any in-flight Claude subprocess. Both methods are safe to call
// zero-or-more times and on a nil receiver.
type HotPathPairHandle struct {
	done   chan struct{}
	cancel context.CancelFunc
}

// Wait blocks until the background pair goroutine finishes or the
// supplied ctx is cancelled. Returns nil when the goroutine drained
// cleanly; returns ctx.Err() if the ctx was cancelled first. A nil
// handle is a no-op (returns nil immediately) so callers don't need
// to nil-check.
func (h *HotPathPairHandle) Wait(ctx context.Context) error {
	if h == nil {
		return nil
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel asks the background goroutine to stop early via its derived
// ctx. The goroutine still writes its done signal once it returns,
// so a subsequent Wait still functions normally.
func (h *HotPathPairHandle) Cancel() {
	if h == nil || h.cancel == nil {
		return
	}
	h.cancel()
}

// WrapHotPathAdversarialPair is the hot-path helper Council, Medic,
// and ConvoyReview each call right after their primary decision is
// formed. It does NOT block — when it decides to run the pair, the
// pair is dispatched on a goroutine and the helper returns
// immediately with a join handle the caller can Wait on at an
// appropriate teardown / cleanup point. handle is nil when no pair
// was scheduled.
//
// On scheduling: the helper takes its OWN cancellable derived ctx
// (with a generous 5-minute timeout) so a daemon SIGINT cancels the
// pair's LLM call cleanly even though the primary's flow has already
// exited.
//
// Pre-D4 race baseline fix: the join handle replaces the prior
// fire-and-forget shape so callers (and tests via t.Cleanup) can
// wait for the goroutine to drain before tearing down the CLI
// runner stub. Without the handle, a goroutine spawned by a test
// that exits while the goroutine is still in claude.AskClaudeCLIContext
// races the test's ResetCLIRunner cleanup.
//
// Honest caveat: when adversarial_pairing_rate is 0 (default), this
// is a no-op fast path — one DB read, no goroutine, nil handle. The
// cost of having Council/Medic/ConvoyReview call this on every
// decision is effectively zero until the operator opts in.
func WrapHotPathAdversarialPair(
	parentCtx context.Context,
	db *sql.DB,
	primary adversarial.PrimaryDecision,
	logger interface{ Printf(string, ...any) },
) (handle *HotPathPairHandle, scheduled bool) {
	if db == nil {
		return nil, false
	}
	rate := adversarialPairingRate(db)
	if rate <= 0 {
		return nil, false
	}
	if adversarialDiceRoll() >= rate {
		return nil, false
	}

	// Sampled. Dispatch the pair on a goroutine so the primary's
	// flow returns to its caller immediately. The goroutine takes a
	// derived ctx so SIGINT (and explicit handle.Cancel) propagates.
	pairCtx, cancel := contextDerivedForBackground(parentCtx)
	h := &HotPathPairHandle{
		done:   make(chan struct{}),
		cancel: cancel,
	}
	go func() {
		defer close(h.done)
		defer cancel()
		runHotPathPair(pairCtx, db, primary, logger)
	}()
	return h, true
}

// contextDerivedForBackground returns a new ctx that inherits
// cancellation from the parent but has its own cancel func, so the
// goroutine can be cancelled independently. We deliberately don't
// add a timeout here — the per-CriticFn call inside RunAdversarialPair
// already routes through claude.CallWithTranscript which has its own
// per-call timeout. Adding a second one here would be redundant and
// would mask the real failure shape.
//
// Pattern P11: parent ctx MUST be non-nil — callers thread the
// daemon ctx in via Spawn{Council,Medic,Diplomat}. If a caller ever
// passes nil it's a misuse bug; we panic rather than silently
// fabricate a context.Background root, since that would mask SIGINT
// propagation.
func contextDerivedForBackground(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		// Should be unreachable — every Spawn site threads ctx
		// through. A nil here means a programmer error in a new
		// call site; failing fast surfaces it during local dev.
		panic("WrapHotPathAdversarialPair: parent ctx is nil — Pattern P11 violation")
	}
	return context.WithCancel(parent)
}

// runHotPathPair is the goroutine body. Errors are logged but never
// propagated — a failed pair is operational signal, not a primary-
// path failure. The primary already completed; we are here to record
// adversarial evidence, not gate.
func runHotPathPair(
	ctx context.Context,
	db *sql.DB,
	primary adversarial.PrimaryDecision,
	logger interface{ Printf(string, ...any) },
) {
	if logger == nil {
		logger = log.Default()
	}
	pair, err := adversarial.RunAdversarialPair(ctx, db, primary)
	if err != nil {
		logger.Printf("adversarial pair (%s decision %d): pair runner failed: %v",
			primary.Agent, primary.DecisionID, err)
		return
	}
	if pair == nil {
		// Defensive: RunAdversarialPair returns (nil, err) on the
		// no-critic path; adversarial code already returns an error
		// in that case so this is unreachable. Bail safely.
		return
	}
	if pair.Agreement {
		logger.Printf("adversarial pair (%s decision %d): agreement", primary.Agent, primary.DecisionID)
		return
	}
	logger.Printf("adversarial pair (%s decision %d): DISAGREEMENT — surfacing pair %d to operator",
		primary.Agent, primary.DecisionID, pair.ID)
	if surfErr := adversarial.SurfaceDisagreementToOperator(ctx, db, pair.ID); surfErr != nil {
		logger.Printf("adversarial pair (%s decision %d): SurfaceDisagreementToOperator failed: %v",
			primary.Agent, primary.DecisionID, surfErr)
	}
}

// hotPathPairTestHookMu serialises test mutations of the dice seam
// so concurrent table-driven subtests don't race the package-level
// var.
var hotPathPairTestHookMu sync.Mutex

// SetAdversarialDiceRollForTest pins the dice to a fixed value for
// tests. Returns a restore func; tests should defer it.
//
// This is exported in spirit (lower-case but inside-package only —
// hot-path callers don't touch it); kept un-exported because the
// only legitimate caller is adversarial_hotpath_test.go.
func setAdversarialDiceRollForTest(v float64) func() {
	hotPathPairTestHookMu.Lock()
	prior := adversarialDiceRoll
	adversarialDiceRoll = func() float64 { return v }
	return func() {
		adversarialDiceRoll = prior
		hotPathPairTestHookMu.Unlock()
	}
}

// setAdversarialPairingRateOverrideForTest pins the package-level
// rate override to v and returns a restore func. Used by TestMain
// to default tests to rate=0 (no pairing) so tests reading stub
// prompts get deterministic results, and by individual hot-path
// tests that want to clear the override and exercise the real
// DB-backed rate-resolution path.
func setAdversarialPairingRateOverrideForTest(v float64) func() {
	prior := adversarialPairingRateOverride.Load()
	cp := v
	adversarialPairingRateOverride.Store(&cp)
	return func() {
		adversarialPairingRateOverride.Store(prior)
	}
}

// clearAdversarialPairingRateOverrideForTest removes the override
// so the DB-backed adversarialPairingRate path runs. Used by the
// hot-path tests that explicitly exercise rate-resolution
// (TestHotPathAdversarialPair_DefaultRampWhenKeyAbsent etc.).
// Returns a restore func so the test can re-pin the override on
// teardown.
func clearAdversarialPairingRateOverrideForTest() func() {
	prior := adversarialPairingRateOverride.Load()
	adversarialPairingRateOverride.Store(nil)
	return func() {
		adversarialPairingRateOverride.Store(prior)
	}
}

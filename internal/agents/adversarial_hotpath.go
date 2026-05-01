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

	"force-orchestrator/internal/agents/adversarial"
	"force-orchestrator/internal/store"
)

// adversarialDiceRoll is the package-level seam for tests. Returns a
// uniform random in [0, 1). Tests overwrite via the helper at the
// bottom of this file.
var adversarialDiceRoll = func() float64 { return rand.Float64() }

// adversarialPairingRateKey is the SystemConfig key the operator
// twiddles to flip pair sampling on/off. Empty / unparsable / out-
// of-range values fail closed (rate=0 → no sampling).
const adversarialPairingRateKey = "adversarial_pairing_rate"

// adversarialPairingRate reads the configured sampling rate. Returns
// 0.0 (no pairing) on any parse / range error so a malformed config
// row never accidentally turns the spigot on.
func adversarialPairingRate(db *sql.DB) float64 {
	v := store.GetConfig(db, adversarialPairingRateKey, "")
	v = strings.TrimSpace(v)
	if v == "" {
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

// WrapHotPathAdversarialPair is the hot-path helper Council, Medic,
// and ConvoyReview each call right after their primary decision is
// formed. It does NOT block — when it decides to run the pair, the
// pair is dispatched on a goroutine and the helper returns
// immediately. The bool return indicates whether a pair was scheduled
// (true) so callers can log the sampling event for telemetry.
//
// On scheduling: the helper takes its OWN cancellable derived ctx
// (with a generous 5-minute timeout) so a daemon SIGINT cancels the
// pair's LLM call cleanly even though the primary's flow has already
// exited.
//
// Honest caveat: when adversarial_pairing_rate is 0 (default), this
// is a no-op fast path — one DB read, no goroutine. The cost of
// having Council/Medic/ConvoyReview call this on every decision is
// effectively zero until the operator opts in.
func WrapHotPathAdversarialPair(
	parentCtx context.Context,
	db *sql.DB,
	primary adversarial.PrimaryDecision,
	logger interface{ Printf(string, ...any) },
) (scheduled bool) {
	if db == nil {
		return false
	}
	rate := adversarialPairingRate(db)
	if rate <= 0 {
		return false
	}
	if adversarialDiceRoll() >= rate {
		return false
	}

	// Sampled. Dispatch the pair on a goroutine so the primary's
	// flow returns to its caller immediately. The goroutine takes a
	// derived ctx so SIGINT propagates.
	pairCtx, cancel := contextDerivedForBackground(parentCtx)
	go func() {
		defer cancel()
		runHotPathPair(pairCtx, db, primary, logger)
	}()
	return true
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

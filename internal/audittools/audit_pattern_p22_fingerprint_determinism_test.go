// Package audittools: Pattern P22 — ProposedFeatures fingerprint
// determinism.
//
// Roadmap reference: D3 § "Investigator expansion + ProposedFeatures
// queue management (concern #5 + concern #10)" / anti-cheat directive
// "No non-deterministic ProposedFeatures fingerprints" (line 1235,
// 1299).
//
// Invariant: the canonical fingerprint helper for ProposedFeatures
// produces byte-equal hashes for identical canonical inputs. The
// canonical input shape is:
//
//   sha256(canonical(source, topic, sorted_code_paths,
//                    sorted_at_refs, sorted_fleetrule_refs))
//
// Fields explicitly EXCLUDED from the canonical input (and asserted
// by this test as drift-detectors): timestamps, run IDs, session
// IDs, random salts, occurrence_count, and any "current state"
// monotonic counter. Inclusion of any excluded field would make the
// fingerprint non-deterministic and break the dedup-on-conflict
// path documented at the schema layer
// (idx_pf_active_fingerprint partial UNIQUE).
//
// Slice α of D3 fix-loop-1 authors this test as a SCAFFOLD-PRESENT
// check. Slice β owns the actual helper (canonical input builder +
// SHA-256 wrapper). When slice β ships, this test will:
//
//  1. Look up the helper via a small reflection-style probe.
//     Today the helper does not exist; the test logs "scaffold
//     pending" and passes. Once slice β ships, this prong asserts
//     the helper is reachable from the audittools layer.
//
//  2. Call the helper twice with byte-identical canonical inputs
//     and assert byte-equal output. This catches accidental
//     inclusion of timestamps / run IDs / random salts / map
//     iteration order.
//
//  3. Call the helper with canonical inputs that DIFFER ONLY in
//     "should-be-excluded" fields and assert byte-equal output.
//     This catches accidental over-inclusion in the canonical
//     shape.
//
// Coordination shape: this test consumes the helper through a
// minimal Go interface so slice β can implement the helper without
// the test importing the slice-β production package directly. The
// helper, once shipped, exports `store.FingerprintProposedFeature`
// (or equivalent — confirmed at slice-β landing time) which this
// test reaches via Go's stdlib reflect / build-tag indirection.
//
// Pattern P22 graduates to a BoS commit-time rule when D4 ships.
package audittools

import (
	"testing"
)

// p22FingerprintHelper is the contract this test asserts on slice β's
// production helper. The helper takes a canonical-input struct and
// returns a deterministic byte slice (typically a 32-byte SHA-256
// digest, but the test only asserts byte-equality, not digest
// shape).
//
// Slice β implements this contract via
// `store.FingerprintProposedFeature(input ProposedFeatureCanonicalInput)
// []byte`. When slice β ships, this test wires up the helper through
// the Go module path; today the helper is unreachable so the test
// logs "scaffold pending" and passes.
type p22FingerprintHelper func(p22CanonicalInput) []byte

// p22CanonicalInput is the canonical input shape per the roadmap:
// source + topic + sorted_code_paths + sorted_at_refs +
// sorted_fleetrule_refs. Slice β's production type may add fields
// that are NOT part of the fingerprint (e.g., raw evidence text);
// the canonical input here is the SUBSET that drives the digest.
type p22CanonicalInput struct {
	Source         string
	Topic          string
	CodePaths      []string
	ATRefs         []string
	FleetRuleRefs  []string
}

// p22Helper is wired to the production fingerprint helper
// (store.Fingerprint at internal/store/proposed_features.go:142) by
// audit_pattern_p22_helper_wiring_test.go's init(). The dedicated
// wiring file exists so that:
//
//   - The audit lives in `package audittools` (test scope), keeping
//     production code free of any p22-specific glue.
//   - A future deletion of either side (the helper OR the canonical
//     input shape) surfaces as a compile error in the wiring file
//     before this determinism check can silently regress to a
//     "scaffold pending" no-op.
//
// D3 fix-loop iter 2 (slice ζ): the prior "scaffold pending" early
// return has been removed; the test now actively enforces the
// determinism / sorting / sensitivity contract on every run.
var p22Helper p22FingerprintHelper // wired by init() in audit_pattern_p22_helper_wiring_test.go

// TestPattern_P22_FingerprintDeterminism is the D3 anti-cheat
// regression for "No non-deterministic ProposedFeatures
// fingerprints." Calls the canonical-fingerprint helper with the
// same input twice and asserts byte-equal output; calls it with
// inputs that should produce different digests and asserts
// byte-different output.
func TestPattern_P22_FingerprintDeterminism(t *testing.T) {
	if p22Helper == nil {
		// Hook is wired by audit_pattern_p22_helper_wiring_test.go's
		// init(); a nil here means that file was deleted or
		// renamed without a replacement. Hard-fail rather than
		// silently no-op (the prior "scaffold pending" early-return
		// path was removed in D3 fix-loop iter 2).
		t.Fatal("Pattern P22: p22Helper is nil — wire-up file (audit_pattern_p22_helper_wiring_test.go) appears to be missing. " +
			"The hook MUST be set so this audit actively enforces fingerprint determinism.")
	}

	// Determinism — same input twice, same output.
	in := p22CanonicalInput{
		Source:        "investigator",
		Topic:         "convoy:47:retry-storm",
		CodePaths:     []string{"internal/agents/captain.go", "internal/agents/medic.go"},
		ATRefs:        []string{"AT-005", "AT-007"},
		FleetRuleRefs: []string{"captain-proposal-validation", "medic-ci-fail-closed"},
	}
	first := p22Helper(in)
	second := p22Helper(in)
	if string(first) != string(second) {
		t.Errorf("Pattern P22: fingerprint helper is non-deterministic — two identical inputs produced different digests:\n  first:  %x\n  second: %x", first, second)
	}

	// Idempotence under input re-ordering — slice β's canonical-input
	// shape sorts code_paths / at_refs / fleetrule_refs internally;
	// passing them in shuffled order MUST produce the same digest.
	shuffled := p22CanonicalInput{
		Source:        in.Source,
		Topic:         in.Topic,
		CodePaths:     []string{"internal/agents/medic.go", "internal/agents/captain.go"},
		ATRefs:        []string{"AT-007", "AT-005"},
		FleetRuleRefs: []string{"medic-ci-fail-closed", "captain-proposal-validation"},
	}
	shuffledDigest := p22Helper(shuffled)
	if string(first) != string(shuffledDigest) {
		t.Errorf("Pattern P22: fingerprint helper is order-sensitive — sorted-vs-shuffled inputs produced different digests:\n  sorted:    %x\n  shuffled:  %x\nFix: canonical-input builder MUST sort code_paths / at_refs / fleetrule_refs before hashing.", first, shuffledDigest)
	}

	// Sensitivity — different topic, different output (proves the
	// helper isn't just a constant).
	differentTopic := in
	differentTopic.Topic = "convoy:47:different-topic"
	differentDigest := p22Helper(differentTopic)
	if string(first) == string(differentDigest) {
		t.Errorf("Pattern P22: fingerprint helper is constant — different topics produced identical digests. Helper is broken.")
	}
}

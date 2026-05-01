// Pattern P22 helper wire-up — D3 fix-loop iter 2 (slice ζ).
//
// Slice α authored audit_pattern_p22_fingerprint_determinism_test.go
// with a `p22Helper p22FingerprintHelper` hook left nil; slice β
// shipped the production helper (`store.Fingerprint`) at
// internal/store/proposed_features.go:142 but did not wire it into
// the audit hook. Today (slice ζ) connects them.
//
// The wiring lives in its own _test.go file so:
//   - It compiles only under `go test`, not in the production binary.
//   - Slice ζ touches one new file rather than mutating slice α's
//     scaffold or slice β's production code.
//   - Future deletions of either side surface as a build-error here
//     before the determinism check silently lapses back to "scaffold
//     pending".
//
// The hook adapts P22's `p22CanonicalInput` shape to the production
// helper's positional argv. Adapter is intentionally trivial: one
// call site, no transformation other than fanning struct fields out
// to positionals.

package audittools

import (
	"force-orchestrator/internal/store"
)

func init() {
	p22Helper = func(in p22CanonicalInput) []byte {
		// store.Fingerprint returns a hex string; we return its bytes
		// so the determinism test only cares about byte-equality (its
		// declared contract). The hex digest is a stable encoding of
		// the underlying SHA-256 sum, so byte-equality on the hex
		// representation is equivalent to byte-equality on the digest.
		fp := store.Fingerprint(in.Source, in.Topic, in.CodePaths, in.ATRefs, in.FleetRuleRefs)
		return []byte(fp)
	}
}

// Package rules: SUPPLY-005 — Known vulnerability blocking via
// vendored osv-scanner (D5 Phase 3, slice α).
//
// Per docs/roadmap.md § "Deliverable 5 — Supply Chain Hygiene" →
// SUPPLY-005:
//
//	On each manifest change, invoke the vendored osv-scanner Go
//	library on the lock file. osv-scanner already supports Ruby,
//	Python, npm, Maven, and Go (via go.mod). High or Critical
//	severity → reject; Medium / Low → advise. Independent of
//	CodeArtifact / AWS auth; works during token-expired windows.
//
// What this rule does NOT do (anti-cheat + scope discipline):
//
//   - It does NOT call CodeArtifact. SUPPLY-005's whole point is that
//     it works offline (against OSV.dev's public API) when the AWS
//     SSO token has expired. There is therefore NO deferral path —
//     the auth-error class doesn't exist for this rule. Pattern
//     P-SupplyDeferral exempts files that don't import the
//     codeartifact client.
//
//   - It does NOT compute its own CVE database. The vendored
//     osv-scanner library does the lookup; we only project the
//     resulting vulnerabilities into Force-shaped findings.
//
//   - It does NOT auto-block at launch. Per the SUPPLY-* anti-cheat
//     directive ("no block-default for new rules"), every SUPPLY rule
//     ships at advise. The rule still computes severity correctly so
//     the dashboard sees CRITICAL / HIGH labels; the BLOCKING
//     behaviour is a future FleetRules promotion (paired-run
//     mechanism, same as the other rules). Reconciles with the spec's
//     "High/Critical → reject" line: that's the future state once
//     operators promote.
//
// Lock-file mapping. SUPPLY-005 wants the LOCK file (Gemfile.lock,
// package-lock.json, go.sum, etc.), not the top-level manifest. The
// manifest-gating dispatcher already populates ChangedManifests for
// both the manifest and any companion lock file when both are
// touched. We feed each ChangedManifest's path + AfterBytes straight
// to the osv-scanner wrapper; if the wrapper doesn't recognise the
// basename (e.g. plain `Gemfile`), it returns
// osv.ErrUnsupportedLockfile and we silently skip — the same commit
// will normally also touch `Gemfile.lock`, which the wrapper does
// understand.
//
// Outcome map:
//
//	scanner returns N findings → emit N isb.Findings, all advise-mode
//	                              at launch (severity bucket included
//	                              in the message).
//	osv.ErrUnsupportedLockfile → silent skip for that ChangedManifest.
//	other scanner error        → wrap into errs slice; partial results
//	                              from sibling manifests still flow
//	                              back via errors.Join.
//
// Discipline (per CLAUDE.md "No silent failures"): every error path
// either (a) returns a wrapped error from Run, (b) emits a Finding,
// or (c) silently skips an out-of-scope condition (unsupported
// lockfile basename) with a comment.
package rules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/scanners/osv"
)

// supply005 is the SUPPLY-005 rule. The osv.Client is injected at
// construction time so tests can stub the scanner at the interface
// boundary (mirrors SUPPLY-001's codeartifact.Client injection
// shape).
type supply005 struct {
	scanner osv.Client
}

// NewSUPPLY005 constructs a SUPPLY-005 rule bound to scanner. The
// scanner is the cross-package service interface (per CLAUDE.md
// "Cross-agent service interfaces" — never a concrete struct).
func NewSUPPLY005(scanner osv.Client) *supply005 {
	return &supply005{scanner: scanner}
}

// ID implements isb.ManifestGatedRule.
func (r *supply005) ID() string { return "SUPPLY-005" }

// Ecosystems implements isb.ManifestGatedRule. SUPPLY-005 covers all
// five D5 ecosystems including Go — osv-scanner's lockfile parser
// supports go.sum / go.mod natively (unlike CodeArtifact, which has
// no Go format).
func (r *supply005) Ecosystems() []manifests.Ecosystem {
	return []manifests.Ecosystem{
		manifests.EcosystemRubyGems,
		manifests.EcosystemPyPI,
		manifests.EcosystemNPM,
		manifests.EcosystemMaven,
		manifests.EcosystemGo,
	}
}

// Run implements isb.ManifestGatedRule. See package comment for the
// per-outcome map.
func (r *supply005) Run(ctx context.Context, _ *sql.DB, input isb.ManifestGatedInput) ([]isb.Finding, error) {
	if r.scanner == nil {
		return nil, errors.New("SUPPLY-005: nil osv scanner")
	}

	declared := map[manifests.Ecosystem]bool{}
	for _, e := range r.Ecosystems() {
		declared[e] = true
	}

	var (
		findings []isb.Finding
		errs     []error
	)

	for _, cm := range input.ChangedManifests {
		// Defensive: only scan ecosystems the rule declared. The
		// dispatcher already filters this way, but if a future
		// caller bypasses dispatch we still skip cleanly.
		if !declared[cm.Ecosystem] {
			continue
		}

		// We feed the AFTER bytes — the post-commit content. If the
		// lock file was deleted in this commit (AfterBytes == nil),
		// the wrapper returns (nil, nil) and we move on.
		scanFindings, err := r.scanner.ScanLockfile(ctx, cm.Path, cm.AfterBytes)
		switch {
		case err == nil:
			// fall through to projection below
		case errors.Is(err, osv.ErrUnsupportedLockfile):
			// Common path: the dispatcher gives us BOTH the manifest
			// (e.g. Gemfile) and the lock file (Gemfile.lock); the
			// manifest is unparseable by osv-scanner, the lock file
			// isn't. Silent skip on this side; the lock-file pass
			// produces the findings.
			continue
		default:
			errs = append(errs, fmt.Errorf("SUPPLY-005: scan %s: %w", cm.Path, err))
			continue
		}

		for _, sf := range scanFindings {
			// Severity at launch: every SUPPLY rule ships at advise
			// per the anti-cheat directive in the package comment.
			// scanner severity (CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN) is
			// surfaced in the message so the operator + dashboard
			// can see it; the actual block-vs-advise decision is
			// FleetRules-gated for future promotion.
			findings = append(findings, isb.Finding{
				RuleID:   "SUPPLY-005",
				Severity: isb.SeverityAdvise,
				Path:     cm.Path,
				Message: fmt.Sprintf(
					"SUPPLY-005: [%s] %s %s@%s — %s (%s severity, see %s)",
					sf.OSVID, sf.Ecosystem, sf.PackageName, sf.PackageVersion,
					sf.Summary, sf.Severity, sf.URL,
				),
			})
		}
	}

	if len(errs) > 0 {
		return findings, errors.Join(errs...)
	}
	return findings, nil
}

// Compile-time guard: supply005 satisfies isb.ManifestGatedRule.
var _ isb.ManifestGatedRule = (*supply005)(nil)

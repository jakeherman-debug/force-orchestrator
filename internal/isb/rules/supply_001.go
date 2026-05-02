// Package rules: SUPPLY-001 — Hallucinated package rejection.
//
// SUPPLY-001 is the first manifest-gated rule (D5 P1). It runs only
// when the commit's diff includes a recognised manifest file (Gemfile,
// package.json, pom.xml, requirements.txt, go.mod, etc.) — the
// manifest-gating dispatcher in internal/isb/manifest_gated.go does
// the file-touch filter; this rule's Run only sees commits that
// already qualified.
//
// For every dep in input.ChangedManifests[*].DepsAdded the rule
// queries CodeArtifact's DescribePackageVersion. The mapping per
// outcome:
//
//	200 (package exists)        → cache positive (≤24h), no finding.
//	ErrPackageNotFound (404)    → emit advise-severity Finding. NEVER
//	                              cache 404 — anti-cheat per
//	                              docs/roadmap.md § D5: "a package that
//	                              404'd today might exist tomorrow."
//	ErrTokenExpired             → record deferral (SecurityFindings row
//	                              with disposition='token_expired') via
//	                              supplydeferral.RecordDeferral. Advise-
//	                              mode through; do NOT block. Other
//	                              deps in the same input are still
//	                              processed (cache hits / 404s might
//	                              not need network at all).
//	ErrTransient (throttle/5xx) → retry once after a small backoff;
//	                              if the second call also fails,
//	                              advise-mode + log + continue. No
//	                              finding (we don't know what the
//	                              correct answer is).
//	ErrUnsupportedEcosystem     → silent skip. Go modules are declared
//	                              as a SUPPLY-001 ecosystem (per the
//	                              roadmap exit criteria
//	                              TestSupply001_HallucinatedGoModule_*)
//	                              but CodeArtifact has no Go format —
//	                              future Go-specific resolution lives
//	                              in SUPPLY-005 via osv-scanner. For
//	                              this rule, Go deps are no-ops.
//	other err                   → collect into a per-rule error and
//	                              continue. Errors do NOT short-circuit:
//	                              partial findings still flow back to
//	                              the dispatcher, joined via errors.Join.
//
// Discipline (per CLAUDE.md "No silent failures"): every error path
// either (a) returns a wrapped error from Run, (b) emits a Finding,
// or (c) records a SecurityFindings deferral row. The "log and
// continue" pattern is reserved for ErrTransient retry-fallback and
// the explicit token_expired log line — and even those leave a row
// behind in either the findings slice or the SecurityFindings table.
//
// Registration is intentionally OUT of scope for this file: D5 P1 α
// ships the rule + tests; daemon-side wiring (where the
// codeartifact.Client is constructed and injected) lands later. Do
// NOT add an init() that calls isb.RegisterManifestGated — there's no
// client to bind it to at package init time.
package rules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/supplydeferral"
)

// supplyCachePositiveTTL is how long a 200 response stays cached.
// Anti-cheat: 404 responses are NEVER cached.
const supplyCachePositiveTTL = 24 * time.Hour

// supplyTransientRetryDelay is the backoff between the first and the
// retry attempt for ErrTransient errors. Small enough that a single
// burst of throttling resolves; large enough that a runaway loop
// can't burn through quotas.
var supplyTransientRetryDelay = 500 * time.Millisecond

type supplyCacheKey struct {
	Ecosystem string
	Name      string
	Version   string
}

type supplyCacheEntry struct {
	ExpiresAt time.Time
}

// supply001 is the SUPPLY-001 rule. The codeartifact.Client is
// injected at construction time so tests can stub the registry layer
// at the interface boundary.
type supply001 struct {
	client codeartifact.Client

	mu    sync.Mutex
	cache map[supplyCacheKey]supplyCacheEntry
}

// NewSUPPLY001 constructs a SUPPLY-001 rule bound to client. The
// client is the cross-agent service interface (per CLAUDE.md "Cross-
// agent service interfaces") — never a concrete struct.
func NewSUPPLY001(client codeartifact.Client) *supply001 {
	return &supply001{
		client: client,
		cache:  map[supplyCacheKey]supplyCacheEntry{},
	}
}

// ID implements isb.ManifestGatedRule.
func (r *supply001) ID() string { return "SUPPLY-001" }

// Ecosystems implements isb.ManifestGatedRule. Declares every
// ecosystem the manifest parsers cover. Go is included even though
// CodeArtifact has no Go format — Run silently skips Go deps via the
// ErrUnsupportedEcosystem branch.
func (r *supply001) Ecosystems() []manifests.Ecosystem {
	return []manifests.Ecosystem{
		manifests.EcosystemRubyGems,
		manifests.EcosystemPyPI,
		manifests.EcosystemNPM,
		manifests.EcosystemMaven,
		manifests.EcosystemGo,
	}
}

// Run implements isb.ManifestGatedRule. See package comment for the
// per-outcome mapping. The dispatcher already filtered the input to
// commits that touched at least one recognised manifest; here we walk
// every ChangedManifest whose ecosystem we declare and process its
// DepsAdded.
func (r *supply001) Run(ctx context.Context, db *sql.DB, input isb.ManifestGatedInput) ([]isb.Finding, error) {
	if r.client == nil {
		return nil, errors.New("SUPPLY-001: nil codeartifact client")
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
		if !declared[cm.Ecosystem] {
			continue
		}
		caEco, ok := manifestsToCodeArtifact(cm.Ecosystem)
		// !ok means an ecosystem we declare but CodeArtifact does not
		// support (Go). Silent skip — see package comment.
		if !ok {
			continue
		}

		for _, dep := range cm.DepsAdded {
			// Skip deps with no version pin — DescribePackageVersion
			// requires both name and version. Range / VCS-ref / unset
			// version is the parser's "best-effort empty" shape; this
			// rule can't validate it. (Future: SUPPLY-005 / osv-scanner
			// covers ranges.)
			if dep.Name == "" || dep.Version == "" {
				continue
			}

			key := supplyCacheKey{
				Ecosystem: string(cm.Ecosystem),
				Name:      dep.Name,
				Version:   dep.Version,
			}
			if r.cacheHit(key) {
				continue
			}

			result, err := r.lookup(ctx, caEco, dep.Name, dep.Version)
			switch {
			case err == nil:
				r.cachePositive(key)
				_ = result
			case errors.Is(err, codeartifact.ErrPackageNotFound):
				findings = append(findings, isb.Finding{
					RuleID:   "SUPPLY-001",
					Severity: isb.SeverityAdvise,
					Path:     cm.Path,
					Message: fmt.Sprintf(
						"SUPPLY-001: %s package %s@%s does not exist in registry — possible hallucination",
						cm.Ecosystem, dep.Name, dep.Version,
					),
				})
			case errors.Is(err, codeartifact.ErrTokenExpired):
				if defErr := r.recordDeferral(db, input, cm, dep); defErr != nil {
					errs = append(errs, fmt.Errorf("SUPPLY-001: deferral for %s@%s: %w", dep.Name, dep.Version, defErr))
					continue
				}
				log.Printf("SUPPLY-001: deferred registry check for %s package %s@%s on branch %s (token expired) — manifest=%s commit=%s",
					cm.Ecosystem, dep.Name, dep.Version, input.Branch, cm.Path, input.CommitSHA)
			case errors.Is(err, codeartifact.ErrTransient):
				// Already retried inside lookup. Fall back to advise-
				// mode + log; emit no finding (we don't know the
				// correct answer). The operator sees the log line.
				log.Printf("SUPPLY-001: transient error after retry for %s package %s@%s on branch %s — proceeding in advise-mode: %v",
					cm.Ecosystem, dep.Name, dep.Version, input.Branch, err)
			case errors.Is(err, codeartifact.ErrUnsupportedEcosystem):
				// Defensive: shouldn't reach here because we filter on
				// manifestsToCodeArtifact above. Treat as silent skip
				// rather than wrapping into errs.
			default:
				// Unknown class. Collect for errors.Join — partial
				// findings from sibling deps still flow back.
				errs = append(errs, fmt.Errorf("SUPPLY-001: lookup %s package %s@%s: %w", cm.Ecosystem, dep.Name, dep.Version, err))
			}
		}
	}

	if len(errs) > 0 {
		return findings, errors.Join(errs...)
	}
	return findings, nil
}

// lookup wraps DescribePackageVersion with the one-shot retry on
// ErrTransient. Returns the underlying error class unchanged so the
// caller can branch on errors.Is.
func (r *supply001) lookup(ctx context.Context, eco codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error) {
	info, err := r.client.DescribePackageVersion(ctx, eco, name, version)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, codeartifact.ErrTransient) {
		return info, err
	}
	// Retry once after a small backoff. Honour ctx cancellation so a
	// shutting-down daemon doesn't block here.
	select {
	case <-ctx.Done():
		return info, ctx.Err()
	case <-time.After(supplyTransientRetryDelay):
	}
	return r.client.DescribePackageVersion(ctx, eco, name, version)
}

// recordDeferral inserts a SUPPLY-001 deferral row for one dep. The
// payload carries only the single dep that triggered the deferral so
// the recovery dog (P4) can re-resolve at fine granularity.
func (r *supply001) recordDeferral(db *sql.DB, input isb.ManifestGatedInput, cm isb.ChangedManifest, dep manifests.Dependency) error {
	if db == nil {
		return errors.New("recordDeferral: nil db")
	}
	payload := supplydeferral.DeferralPayload{
		RuleKey:      "SUPPLY-001",
		ManifestPath: cm.Path,
		DepsAdded:    []manifests.Dependency{dep},
		Branch:       input.Branch,
		CommitSHA:    input.CommitSHA,
		DeferredAt:   time.Now().UTC(),
	}
	_, err := supplydeferral.RecordDeferral(db, input.SourceTaskID, payload)
	return err
}

// cacheHit returns true if a positive cached entry for key is still
// within the TTL. Expired entries are evicted opportunistically on
// read so the map doesn't grow unboundedly.
func (r *supply001) cacheHit(key supplyCacheKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok {
		return false
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(r.cache, key)
		return false
	}
	return true
}

// cachePositive records key as a positive (200) hit valid for
// supplyCachePositiveTTL.
func (r *supply001) cachePositive(key supplyCacheKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = supplyCacheEntry{ExpiresAt: time.Now().Add(supplyCachePositiveTTL)}
}

// manifestsToCodeArtifact maps the manifests.Ecosystem enum to the
// codeartifact.Ecosystem enum. Returns ok=false for ecosystems that
// CodeArtifact does not support (Go); SUPPLY-001 silently skips
// those deps.
func manifestsToCodeArtifact(e manifests.Ecosystem) (codeartifact.Ecosystem, bool) {
	switch e {
	case manifests.EcosystemPyPI:
		return codeartifact.EcosystemPyPI, true
	case manifests.EcosystemNPM:
		return codeartifact.EcosystemNPM, true
	case manifests.EcosystemRubyGems:
		return codeartifact.EcosystemRubyGems, true
	case manifests.EcosystemMaven:
		return codeartifact.EcosystemMaven, true
	}
	return "", false
}

// Compile-time guard: supply001 satisfies isb.ManifestGatedRule.
var _ isb.ManifestGatedRule = (*supply001)(nil)

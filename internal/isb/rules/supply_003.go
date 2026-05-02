// Package rules: SUPPLY-003 — Stale package detection.
//
// SUPPLY-003 is the second registry-hit manifest-gated rule (D5 P2,
// slice α). It runs only when the commit's diff includes a recognised
// manifest file — the manifest-gating dispatcher in
// internal/isb/manifest_gated.go does the file-touch filter; this
// rule's Run only sees commits that already qualified.
//
// For every dep in input.ChangedManifests[*].DepsAdded the rule
// queries CodeArtifact's DescribePackageVersion and compares the
// returned PublishedAt against now() − threshold. The threshold is
// read at Run time from SystemConfig key `supply_stale_threshold_days`
// (default 730 ≈ 2 years per docs/roadmap.md § D5 SUPPLY-003).
//
// Per-outcome mapping:
//
//	200 + PublishedAt fresh    → cache positive (≤24h), no finding.
//	200 + PublishedAt < cutoff → emit advise-severity Finding with the
//	                             ecosystem + name@version + published
//	                             date + threshold_days. Do NOT cache —
//	                             a stale package today might receive a
//	                             new release tomorrow (anti-cheat
//	                             mirror of SUPPLY-001's 404 rule).
//	200 + PublishedAt zero     → no finding (CodeArtifact didn't
//	                             surface a publish time; silent skip
//	                             rather than guess).
//	ErrPackageNotFound (404)   → not our job — SUPPLY-001 owns that
//	                             outcome. Silent skip.
//	ErrTokenExpired            → record deferral via
//	                             supplydeferral.RecordDeferral; advise-
//	                             mode through (no finding emitted).
//	                             Other deps in the same input still
//	                             processed.
//	ErrTransient               → retry once after small backoff; if the
//	                             second call also fails, advise-mode +
//	                             log + continue. No finding (we don't
//	                             know the publish date).
//	ErrUnsupportedEcosystem    → silent skip (Go modules — declared as
//	                             a SUPPLY-* ecosystem but CodeArtifact
//	                             has no Go format).
//	other err                  → collect into a per-rule error and
//	                             continue. Errors do NOT short-circuit:
//	                             partial findings still flow back via
//	                             errors.Join.
//
// Discipline (per CLAUDE.md "No silent failures"): every error path
// either (a) returns a wrapped error from Run, (b) emits a Finding,
// or (c) records a SecurityFindings deferral row. The "log and
// continue" pattern is reserved for ErrTransient retry-fallback and
// the explicit token_expired log line — and even those leave a row
// behind in either the findings slice or the SecurityFindings table.
//
// Cache shape (anti-cheat per docs/roadmap.md § D5):
//   - Positive cache TTL ≤ 24h, keyed on (ecosystem, name, version).
//     Cached entries mean "we recently confirmed this package is fresh,"
//     so a second Run within 24h doesn't re-hit the registry.
//   - NO negative cache for stale findings — re-evaluate every Run so
//     a newly-released version flips the rule the next time the
//     scanner runs.
//
// Registration is intentionally OUT of scope for this file: D5 P2 α
// ships the rule + tests; daemon-side wiring lands later (slice γ /
// δ). Do NOT add an init() that calls isb.RegisterManifestGated.
package rules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/supplydeferral"
)

// supplyStaleDefaultDays is the default value of
// `supply_stale_threshold_days`. 730 days ≈ 2 years — the spec's
// "noticeably abandoned" boundary per docs/roadmap.md § D5
// SUPPLY-003.
const supplyStaleDefaultDays = 730

// supplyStaleConfigKey is the SystemConfig key the rule reads at
// Run time. Operator-tunable; missing / unparseable values fall back
// to supplyStaleDefaultDays.
const supplyStaleConfigKey = "supply_stale_threshold_days"

// supply003 is the SUPPLY-003 rule. The codeartifact.Client is
// injected at construction time so tests can stub the registry layer
// at the interface boundary (Pattern P16).
type supply003 struct {
	client codeartifact.Client

	mu    sync.Mutex
	cache map[supplyCacheKey]supplyCacheEntry
}

// NewSUPPLY003 constructs a SUPPLY-003 rule bound to client. The
// client is the cross-agent service interface (per CLAUDE.md "Cross-
// agent service interfaces") — never a concrete struct.
func NewSUPPLY003(client codeartifact.Client) *supply003 {
	return &supply003{
		client: client,
		cache:  map[supplyCacheKey]supplyCacheEntry{},
	}
}

// ID implements isb.ManifestGatedRule.
func (r *supply003) ID() string { return "SUPPLY-003" }

// Ecosystems implements isb.ManifestGatedRule. Mirrors SUPPLY-001's
// declared set: PyPI / npm / RubyGems / Maven / Go. Go is declared
// for parity but silently skipped at Run time (CodeArtifact has no
// Go format — future Go-specific staleness lives in SUPPLY-005 via
// osv-scanner).
func (r *supply003) Ecosystems() []manifests.Ecosystem {
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
// commits that touched at least one recognised manifest; here we
// walk every ChangedManifest whose ecosystem we declare and process
// its DepsAdded.
func (r *supply003) Run(ctx context.Context, db *sql.DB, input isb.ManifestGatedInput) ([]isb.Finding, error) {
	if r.client == nil {
		return nil, errors.New("SUPPLY-003: nil codeartifact client")
	}

	thresholdDays := loadStaleThresholdDays(db)
	threshold := time.Duration(thresholdDays) * 24 * time.Hour
	cutoff := time.Now().Add(-threshold)

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
		// support (Go). Silent skip.
		if !ok {
			continue
		}

		for _, dep := range cm.DepsAdded {
			// Skip deps with no version pin — DescribePackageVersion
			// requires both name and version. Range / VCS-ref / unset
			// is the parser's "best-effort empty" shape; SUPPLY-003
			// can't validate it.
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

			info, err := r.lookup(ctx, caEco, dep.Name, dep.Version)
			switch {
			case err == nil:
				// Fresh metadata. Decide stale vs. fresh based on
				// PublishedAt. Zero-value PublishedAt means
				// CodeArtifact didn't surface a publish time; treat as
				// inconclusive (no finding) rather than guessing.
				if info.PublishedAt.IsZero() {
					// Inconclusive — silent skip, no cache (we don't
					// know whether it'll be answered next time).
					continue
				}
				if info.PublishedAt.Before(cutoff) {
					findings = append(findings, isb.Finding{
						RuleID:   "SUPPLY-003",
						Severity: isb.SeverityAdvise,
						Path:     cm.Path,
						Message: fmt.Sprintf(
							"SUPPLY-003: %s package %s@%s last published %s — older than %d-day staleness threshold; check for a maintained alternative",
							cm.Ecosystem, dep.Name, dep.Version,
							info.PublishedAt.UTC().Format("2006-01-02"),
							thresholdDays,
						),
					})
					log.Printf("SUPPLY-003: stale %s package %s@%s on branch %s — published=%s threshold_days=%d manifest=%s commit=%s",
						cm.Ecosystem, dep.Name, dep.Version, input.Branch,
						info.PublishedAt.UTC().Format("2006-01-02"),
						thresholdDays, cm.Path, input.CommitSHA)
					// Anti-cheat: do NOT cache a stale result. A new
					// release tomorrow flips the rule.
					continue
				}
				// Fresh — cache positive (≤24h).
				r.cachePositive(key)
			case errors.Is(err, codeartifact.ErrPackageNotFound):
				// SUPPLY-001's job, not ours. Silent skip — no
				// finding, no cache.
			case errors.Is(err, codeartifact.ErrTokenExpired):
				if defErr := r.recordDeferral(db, input, cm, dep); defErr != nil {
					errs = append(errs, fmt.Errorf("SUPPLY-003: deferral for %s@%s: %w", dep.Name, dep.Version, defErr))
					continue
				}
				log.Printf("SUPPLY-003: deferred registry check for %s package %s@%s on branch %s (token expired) — manifest=%s commit=%s",
					cm.Ecosystem, dep.Name, dep.Version, input.Branch, cm.Path, input.CommitSHA)
			case errors.Is(err, codeartifact.ErrTransient):
				// Already retried inside lookup. Advise-mode + log;
				// emit no finding (we don't know the publish date).
				log.Printf("SUPPLY-003: transient error after retry for %s package %s@%s on branch %s — proceeding in advise-mode: %v",
					cm.Ecosystem, dep.Name, dep.Version, input.Branch, err)
			case errors.Is(err, codeartifact.ErrUnsupportedEcosystem):
				// Defensive: shouldn't reach here because we filter on
				// manifestsToCodeArtifact above. Treat as silent skip.
			default:
				// Unknown class. Collect for errors.Join — partial
				// findings from sibling deps still flow back.
				errs = append(errs, fmt.Errorf("SUPPLY-003: lookup %s package %s@%s: %w", cm.Ecosystem, dep.Name, dep.Version, err))
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
// caller can branch on errors.Is. Reuses the package-level
// supplyTransientRetryDelay so SUPPLY-001 and SUPPLY-003 honour the
// same backoff knob.
func (r *supply003) lookup(ctx context.Context, eco codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error) {
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

// recordDeferral inserts a SUPPLY-003 deferral row for one dep. The
// payload carries only the single dep that triggered the deferral so
// the recovery dog (P4) can re-resolve at fine granularity.
func (r *supply003) recordDeferral(db *sql.DB, input isb.ManifestGatedInput, cm isb.ChangedManifest, dep manifests.Dependency) error {
	if db == nil {
		return errors.New("recordDeferral: nil db")
	}
	payload := supplydeferral.DeferralPayload{
		RuleKey:      "SUPPLY-003",
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
// read so the map doesn't grow unboundedly. (Mirrors supply001's
// per-rule cache — kept separate so SUPPLY-001 and SUPPLY-003 don't
// share state across rule lifetimes.)
func (r *supply003) cacheHit(key supplyCacheKey) bool {
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

// cachePositive records key as a positive (fresh) hit valid for
// supplyCachePositiveTTL.
func (r *supply003) cachePositive(key supplyCacheKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = supplyCacheEntry{ExpiresAt: time.Now().Add(supplyCachePositiveTTL)}
}

// loadStaleThresholdDays reads the operator-tunable threshold from
// SystemConfig. Missing or unparseable values fall back to
// supplyStaleDefaultDays. A non-positive value is treated as the
// default — a zero-day threshold would flag every dep as stale,
// which is almost certainly an operator typo.
func loadStaleThresholdDays(db *sql.DB) int {
	if db == nil {
		return supplyStaleDefaultDays
	}
	raw := getConfigViaStore(db, supplyStaleConfigKey)
	if raw == "" {
		return supplyStaleDefaultDays
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SUPPLY-003: ignoring invalid %s=%q — falling back to default %d days",
			supplyStaleConfigKey, raw, supplyStaleDefaultDays)
		return supplyStaleDefaultDays
	}
	return n
}

// Compile-time guard: supply003 satisfies isb.ManifestGatedRule.
var _ isb.ManifestGatedRule = (*supply003)(nil)

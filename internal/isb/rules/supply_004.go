// Package rules: SUPPLY-004 — License compatibility detection (D5
// Phase 2, slice β).
//
// Per docs/roadmap.md § "Deliverable 5 — Supply Chain Hygiene" →
// SUPPLY-004:
//
//   - Repo's declared license lives in `Repositories.license` (D5 P0
//     SPDX detector backfill). Looked up by `input.TargetRepo`.
//   - For each added dep, license metadata comes from the
//     CodeArtifact `DescribePackageVersion` call's `License` field.
//   - Static compatibility matrix at internal/isb/rules/license_matrix.yaml
//     keyed by SPDX IDs. PR-reviewable when the matrix changes.
//   - Pairs absent from the matrix → advise-mode + operator review.
//
// Anti-cheat (per § "Anti-cheat directives"):
//   - **NO LLM decides license compatibility.** The matrix is the
//     only authority. Pairs not in the matrix land in advise-mode for
//     human review — never auto-allow, never auto-deny.
//   - Positive cache ≤24h on (ecosystem, name, version) → license.
//     NO negative cache: an empty license today might be populated
//     tomorrow if the package metadata is corrected upstream.
//   - Empty repo license OR empty dep license → advise-mode (operator
//     review). Never auto-allow.
//
// Outcome map (matches SUPPLY-001's deferral shape for consistency):
//
//	200 + matrix allow         → no finding, cache positive license.
//	200 + matrix deny          → advise-mode finding.
//	200 + pair not in matrix   → advise-mode finding (operator review).
//	200 + dep license empty    → advise-mode finding (cannot check).
//	repo license empty         → advise-mode finding once per Run
//	                             (cannot check anything).
//	ErrTokenExpired            → supplydeferral.RecordDeferral, no
//	                             finding for this dep.
//	ErrTransient (after retry) → log + advise-mode through (no
//	                             finding — we don't know the answer).
//	ErrPackageNotFound         → silent skip (SUPPLY-001's domain;
//	                             SUPPLY-004 only checks license
//	                             compatibility, not existence).
//	ErrUnsupportedEcosystem    → silent skip (Go).
//	other err                  → wrapped via errors.Join.
//
// Discipline (per CLAUDE.md "No silent failures"): every error path
// either (a) returns a wrapped error from Run, (b) emits a Finding,
// (c) records a SecurityFindings deferral row, or (d) silently skips
// an out-of-scope condition (Go, ErrPackageNotFound — both
// intentionally) with a comment.
//
// Registration is OUT of scope for slice β. Slice γ adds the
// FleetRules seed; daemon-side wire happens in a later phase.
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
	"force-orchestrator/internal/store"
)

// supply004CachePositiveTTL mirrors SUPPLY-001's TTL: a positive
// (license discovered) entry is good for 24h. Anti-cheat: there is
// NO negative cache.
const supply004CachePositiveTTL = 24 * time.Hour

// supply004TransientRetryDelay is overridable in tests for fast
// retry scheduling, mirroring SUPPLY-001.
var supply004TransientRetryDelay = 500 * time.Millisecond

// supply004CacheKey keys the per-package license cache.
type supply004CacheKey struct {
	Ecosystem string
	Name      string
	Version   string
}

// supply004CacheEntry holds the discovered license + its expiry. We
// cache the resolved license string (never the absence of a license)
// to honour the "no negative cache" anti-cheat directive.
type supply004CacheEntry struct {
	License   string
	ExpiresAt time.Time
}

// supply004 is the SUPPLY-004 rule. The codeartifact.Client is
// injected at construction time (cross-agent service interface — per
// CLAUDE.md, never a concrete struct). The matrix is loaded once at
// construction; failure to load is a fatal construction error
// (fails-closed — the rule cannot meaningfully run without the
// matrix).
type supply004 struct {
	client codeartifact.Client
	matrix map[string]licenseEntry

	mu    sync.Mutex
	cache map[supply004CacheKey]supply004CacheEntry
}

// NewSUPPLY004 constructs a SUPPLY-004 rule bound to client. Returns
// an error if the embedded license_matrix.yaml fails to parse — the
// rule cannot run without the matrix (fail-closed). The matrix is
// loaded once at construction so per-Run calls don't repeat the YAML
// parse.
func NewSUPPLY004(client codeartifact.Client) (*supply004, error) {
	matrix, err := LoadLicenseMatrix()
	if err != nil {
		return nil, fmt.Errorf("NewSUPPLY004: %w", err)
	}
	return &supply004{
		client: client,
		matrix: matrix,
		cache:  map[supply004CacheKey]supply004CacheEntry{},
	}, nil
}

// ID implements isb.ManifestGatedRule.
func (r *supply004) ID() string { return "SUPPLY-004" }

// Ecosystems implements isb.ManifestGatedRule. Declares the same five
// ecosystems as SUPPLY-001/002. Go is included even though
// CodeArtifact has no Go format; Run silently skips Go deps via the
// ErrUnsupportedEcosystem branch (mirrors SUPPLY-001).
func (r *supply004) Ecosystems() []manifests.Ecosystem {
	return []manifests.Ecosystem{
		manifests.EcosystemRubyGems,
		manifests.EcosystemPyPI,
		manifests.EcosystemNPM,
		manifests.EcosystemMaven,
		manifests.EcosystemGo,
	}
}

// Run implements isb.ManifestGatedRule. See package comment for the
// full per-outcome map.
func (r *supply004) Run(ctx context.Context, db *sql.DB, input isb.ManifestGatedInput) ([]isb.Finding, error) {
	if r.client == nil {
		return nil, errors.New("SUPPLY-004: nil codeartifact client")
	}
	if r.matrix == nil {
		// Defensive: NewSUPPLY004 fails on parse error so this should
		// never be hit. Returning a wrapped error rather than panicking
		// honours the "no silent failures" invariant.
		return nil, errors.New("SUPPLY-004: nil license matrix (constructor bypassed?)")
	}

	repoLicense, repoErr := store.GetRepositoryLicense(db, input.TargetRepo)
	if repoErr != nil {
		// DB-level failure looking up the repo. Wrap and return — the
		// dispatcher records this; partial findings still flow back.
		return nil, fmt.Errorf("SUPPLY-004: GetRepositoryLicense(%s): %w", input.TargetRepo, repoErr)
	}

	declared := map[manifests.Ecosystem]bool{}
	for _, e := range r.Ecosystems() {
		declared[e] = true
	}

	var (
		findings  []isb.Finding
		errs      []error
		repoWarn  bool // log the empty-repo-license warning at most once per Run
	)

	for _, cm := range input.ChangedManifests {
		if !declared[cm.Ecosystem] {
			continue
		}
		caEco, ok := manifestsToCodeArtifact(cm.Ecosystem)
		// !ok = ecosystem we declare but CodeArtifact does not handle
		// (Go). Silent skip — license check delegated to SUPPLY-005's
		// osv-scanner path in a later phase.
		if !ok {
			continue
		}

		for _, dep := range cm.DepsAdded {
			if dep.Name == "" || dep.Version == "" {
				// No version pin → can't query DescribePackageVersion.
				// Same posture as SUPPLY-001.
				continue
			}

			// If the repo's declared license is unknown we can't make
			// any compatibility judgement. Emit advise-mode for each
			// dep so the operator sees the gap; log once per Run to
			// avoid log-spam on big diffs.
			if repoLicense == "" {
				if !repoWarn {
					log.Printf("SUPPLY-004: repo %q has no declared license — license-compatibility check disabled until Repositories.license is populated", input.TargetRepo)
					repoWarn = true
				}
				findings = append(findings, isb.Finding{
					RuleID:   "SUPPLY-004",
					Severity: isb.SeverityAdvise,
					Path:     cm.Path,
					Message: fmt.Sprintf(
						"SUPPLY-004: %s dep %s@%s — repo license not declared, cannot check compatibility (operator review)",
						cm.Ecosystem, dep.Name, dep.Version,
					),
				})
				continue
			}

			key := supply004CacheKey{
				Ecosystem: string(cm.Ecosystem),
				Name:      dep.Name,
				Version:   dep.Version,
			}
			depLicense, cached := r.cacheGet(key)

			if !cached {
				info, err := r.lookup(ctx, caEco, dep.Name, dep.Version)
				switch {
				case err == nil:
					depLicense = info.License
					if depLicense != "" {
						r.cachePositive(key, depLicense)
					}
					// fall through to compatibility check below
				case errors.Is(err, codeartifact.ErrPackageNotFound):
					// Not SUPPLY-004's concern (SUPPLY-001 owns
					// existence). Silent skip for this dep.
					continue
				case errors.Is(err, codeartifact.ErrTokenExpired):
					if defErr := r.recordDeferral(db, input, cm, dep); defErr != nil {
						errs = append(errs, fmt.Errorf("SUPPLY-004: deferral for %s@%s: %w", dep.Name, dep.Version, defErr))
						continue
					}
					log.Printf("SUPPLY-004: deferred license check for %s package %s@%s on branch %s (token expired) — manifest=%s commit=%s",
						cm.Ecosystem, dep.Name, dep.Version, input.Branch, cm.Path, input.CommitSHA)
					continue
				case errors.Is(err, codeartifact.ErrTransient):
					// Already retried inside lookup. Log + advise-mode
					// (no finding) — we don't know the answer; the
					// operator sees the log line.
					log.Printf("SUPPLY-004: transient error after retry for %s package %s@%s on branch %s — proceeding in advise-mode: %v",
						cm.Ecosystem, dep.Name, dep.Version, input.Branch, err)
					continue
				case errors.Is(err, codeartifact.ErrUnsupportedEcosystem):
					// Defensive: filtered above via manifestsToCodeArtifact.
					continue
				default:
					errs = append(errs, fmt.Errorf("SUPPLY-004: lookup %s package %s@%s: %w", cm.Ecosystem, dep.Name, dep.Version, err))
					continue
				}
			}

			// Empty dep license → can't decide compatibility. Emit
			// advise-mode finding so operator sees the gap. Some
			// ecosystems (PyPI, RubyGems) reliably surface license
			// metadata; others (Maven, raw npm) don't. We do NOT
			// cache the empty result — anti-cheat (no negative cache).
			if depLicense == "" {
				findings = append(findings, isb.Finding{
					RuleID:   "SUPPLY-004",
					Severity: isb.SeverityAdvise,
					Path:     cm.Path,
					Message: fmt.Sprintf(
						"SUPPLY-004: %s dep %s@%s — license unknown, cannot check compatibility against repo license %s (operator review)",
						cm.Ecosystem, dep.Name, dep.Version, repoLicense,
					),
				})
				continue
			}

			allowed, denied := CheckLicenseCompatibility(r.matrix, repoLicense, depLicense)
			switch {
			case allowed:
				// Compatible — no finding.
			case denied:
				findings = append(findings, isb.Finding{
					RuleID:   "SUPPLY-004",
					Severity: isb.SeverityAdvise,
					Path:     cm.Path,
					Message: fmt.Sprintf(
						"SUPPLY-004: %s dep %s@%s license %s is incompatible with repo license %s (denied by license_matrix.yaml)",
						cm.Ecosystem, dep.Name, dep.Version, depLicense, repoLicense,
					),
				})
			default:
				// Pair not in matrix → advise-mode + operator review.
				// NEVER auto-allow.
				findings = append(findings, isb.Finding{
					RuleID:   "SUPPLY-004",
					Severity: isb.SeverityAdvise,
					Path:     cm.Path,
					Message: fmt.Sprintf(
						"SUPPLY-004: %s dep %s@%s license %s vs repo license %s — pair not in license_matrix.yaml (operator review)",
						cm.Ecosystem, dep.Name, dep.Version, depLicense, repoLicense,
					),
				})
			}
		}
	}

	if len(errs) > 0 {
		return findings, errors.Join(errs...)
	}
	return findings, nil
}

// lookup wraps DescribePackageVersion with the one-shot retry on
// ErrTransient (mirrors SUPPLY-001).
func (r *supply004) lookup(ctx context.Context, eco codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error) {
	info, err := r.client.DescribePackageVersion(ctx, eco, name, version)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, codeartifact.ErrTransient) {
		return info, err
	}
	select {
	case <-ctx.Done():
		return info, ctx.Err()
	case <-time.After(supply004TransientRetryDelay):
	}
	return r.client.DescribePackageVersion(ctx, eco, name, version)
}

// recordDeferral inserts a SUPPLY-004 deferral row for one dep.
// Mirrors SUPPLY-001's shape; the recovery dog (P4) replays per-dep.
func (r *supply004) recordDeferral(db *sql.DB, input isb.ManifestGatedInput, cm isb.ChangedManifest, dep manifests.Dependency) error {
	if db == nil {
		return errors.New("recordDeferral: nil db")
	}
	payload := supplydeferral.DeferralPayload{
		RuleKey:      "SUPPLY-004",
		ManifestPath: cm.Path,
		DepsAdded:    []manifests.Dependency{dep},
		Branch:       input.Branch,
		CommitSHA:    input.CommitSHA,
		DeferredAt:   time.Now().UTC(),
	}
	_, err := supplydeferral.RecordDeferral(db, input.SourceTaskID, payload)
	return err
}

// cacheGet returns (license, true) if a non-expired positive entry
// exists; ("", false) otherwise. Expired entries are evicted on read
// so the map doesn't grow unboundedly.
func (r *supply004) cacheGet(key supply004CacheKey) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(r.cache, key)
		return "", false
	}
	return entry.License, true
}

// cachePositive records a discovered license for `supply004CachePositiveTTL`.
// Empty licenses are never cached (anti-cheat: no negative cache).
func (r *supply004) cachePositive(key supply004CacheKey, license string) {
	if license == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = supply004CacheEntry{
		License:   license,
		ExpiresAt: time.Now().Add(supply004CachePositiveTTL),
	}
}

// Compile-time guard: supply004 satisfies isb.ManifestGatedRule.
var _ isb.ManifestGatedRule = (*supply004)(nil)

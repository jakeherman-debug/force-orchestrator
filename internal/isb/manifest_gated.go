// Package isb: manifest-gating dispatch for SUPPLY-* rules
// (D5 Phase 0).
//
// Existing ISB-001..010 rules are AST checks against changed `.go`
// files — they ALWAYS fire when the rule has a FleetRules row.
// SUPPLY-001..005 (P1+) need a different gate: they only fire when
// the commit's diff actually touches a recognised manifest file
// (Gemfile, package.json, pom.xml, etc.). Per-commit network calls
// would otherwise burn through the AWS SSO token TTL and trigger far
// more often than the threat model warrants.
//
// This file defines the ManifestGatedRule interface + a separate
// registry for them. P0 ships the dispatch path + a stub registration
// surface for tests; P1 lands the first SUPPLY-001 implementation
// against this contract. The dispatcher matches the docs/roadmap.md
// § D5 "Manifest-gating + deferral path" design.
//
// Anti-cheat (per docs/roadmap.md § D5): manifest-gated rules MUST
// route their auth-error deferral through internal/isb/rules
// (RecordDeferral). The deferral helper itself is the choke-point;
// this package only owns dispatch.
package isb

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"force-orchestrator/internal/isb/scanners/manifests"
)

// ManifestGatedRule is the contract for rules that should only fire
// when the commit's diff includes a manifest file from one of the
// rule's declared ecosystems.
//
// Implementations are stateless (no per-rule fields beyond
// configuration). The Run method receives the resolved set of
// changed manifests already grouped by ecosystem so a rule can scan
// only the files relevant to its ecosystem(s).
type ManifestGatedRule interface {
	// ID returns the stable rule identifier (e.g. "SUPPLY-001").
	ID() string

	// Ecosystems returns the ecosystems this rule cares about. The
	// dispatcher fires Run only when at least one of these
	// ecosystems is represented in the changed-manifest set.
	Ecosystems() []manifests.Ecosystem

	// Run executes the rule against the supplied per-manifest
	// changeset. Returns Findings and an optional deferral marker
	// (true = the rule deferred via the token-expired path; the
	// caller treats this as advisory and does NOT block).
	Run(ctx context.Context, db *sql.DB, in ManifestGatedInput) ([]Finding, error)
}

// ManifestGatedInput is the per-rule call shape. Carries the source
// task / branch / commit metadata SUPPLY-* needs for the deferral
// payload, plus the resolved changed-manifest set.
type ManifestGatedInput struct {
	SourceTaskID    int
	TargetRepo      string
	Branch          string
	CommitSHA       string
	ChangedManifests []ChangedManifest
}

// ChangedManifest describes one manifest file that was modified in
// this commit, plus the dep-set delta extracted by the per-ecosystem
// parser. Direct-only or transitive-only filters are the rule's
// concern.
type ChangedManifest struct {
	Path         string
	Ecosystem    manifests.Ecosystem
	DepsAdded    []manifests.Dependency
	DepsRemoved  []manifests.Dependency
	BeforeBytes  []byte // pre-commit content, may be nil if newly added
	AfterBytes   []byte // current content, may be nil if deleted
}

// EcosystemSet returns the unique set of ecosystems represented in
// the input. Used by the dispatcher to skip rules whose ecosystems
// don't intersect.
func (in ManifestGatedInput) EcosystemSet() map[manifests.Ecosystem]bool {
	out := map[manifests.Ecosystem]bool{}
	for _, c := range in.ChangedManifests {
		out[c.Ecosystem] = true
	}
	return out
}

// ── manifest-gated rule registry ─────────────────────────────────────────

var (
	mgRegMu    sync.RWMutex
	mgRegistry = map[string]ManifestGatedRule{}
	mgOrder    []string
)

// RegisterManifestGated adds r to the manifest-gated rule registry.
// Panics on duplicate ID (same shape as Register for AST rules).
func RegisterManifestGated(r ManifestGatedRule) {
	mgRegMu.Lock()
	defer mgRegMu.Unlock()
	if r == nil {
		panic("isb.RegisterManifestGated: nil rule")
	}
	id := r.ID()
	if id == "" {
		panic("isb.RegisterManifestGated: rule has empty ID")
	}
	if _, dup := mgRegistry[id]; dup {
		panic("isb.RegisterManifestGated: duplicate rule ID " + id)
	}
	mgRegistry[id] = r
	mgOrder = append(mgOrder, id)
}

// AllManifestGated returns every registered manifest-gated rule, in
// insertion order. Used by the dispatcher.
func AllManifestGated() []ManifestGatedRule {
	mgRegMu.RLock()
	defer mgRegMu.RUnlock()
	out := make([]ManifestGatedRule, 0, len(mgOrder))
	for _, id := range mgOrder {
		out = append(out, mgRegistry[id])
	}
	return out
}

// ResetManifestGatedForTest clears the manifest-gated registry. Tests
// that exercise the dispatcher in isolation use this to drop any
// production registrations the init() chain pulled in.
func ResetManifestGatedForTest() {
	mgRegMu.Lock()
	defer mgRegMu.Unlock()
	mgRegistry = map[string]ManifestGatedRule{}
	mgOrder = nil
}

// DispatchManifestGated runs every registered manifest-gated rule
// whose ecosystems intersect the supplied input. Returns aggregated
// findings + a per-rule error map (rule-level errors do NOT short-
// circuit the dispatch; one rule's failure must not silence another's
// finding). The gate function decides which rules are active —
// inactive rules are skipped entirely (no scan, no findings).
//
// Returns an empty result + nil error when ChangedManifests is
// empty: that's the manifest-gating point — source-only commits make
// zero rule calls. The caller (ISBReview) checks this via
// `len(input.ChangedManifests)` before bothering to call.
func DispatchManifestGated(ctx context.Context, db *sql.DB, gate FleetRulesGate, input ManifestGatedInput) ([]Finding, map[string]error) {
	var (
		out   []Finding
		errs  = map[string]error{}
		ecoIn = input.EcosystemSet()
	)
	if len(ecoIn) == 0 {
		return nil, nil
	}
	for _, r := range AllManifestGated() {
		active, _, ok := gate(r.ID())
		if !ok || !active {
			continue
		}
		// Ecosystem intersection: skip rule if none of its declared
		// ecosystems appear in the changed-manifest set.
		hit := false
		for _, e := range r.Ecosystems() {
			if ecoIn[e] {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		findings, err := r.Run(ctx, db, input)
		if err != nil {
			errs[r.ID()] = err
		}
		// SUPPLY-BYPASS application: walk each changed manifest's AfterBytes
		// for SUPPLY-BYPASS markers and apply them to findings whose Path
		// matches the manifest's path. The marker mutates Severity →
		// SeverityAdvise and prefixes the Message with the BYPASSED shape
		// so the agents-side persistence (dispositionFromMessage /
		// extractAuditFromBypassed / extractReasonFromBypassed) transparently
		// records disposition='overridden' + bypass_audit_id + bypass_reason
		// on the SecurityFindings row. Per-rule scoping (RuleKey) means a
		// SUPPLY-001 bypass does NOT silence a SUPPLY-002 finding —
		// anti-cheat: bypasses must be targeted by default. An empty RuleKey
		// applies to all SUPPLY rules (operator-wide override).
		findings = applySupplyBypasses(findings, input.ChangedManifests)
		out = append(out, findings...)
	}
	return out, errs
}

// applySupplyBypasses walks the changed manifests for SUPPLY-BYPASS markers
// and downgrades any matching findings. Findings without a path match
// against any manifest pass through untouched — a finding's Path must
// equal a ChangedManifest.Path for that manifest's bypass markers to
// apply. This keeps cross-file bypass leakage impossible (a bypass in
// Gemfile cannot suppress a finding in package.json).
//
// Returns the (possibly-mutated) findings slice; the underlying Finding
// values are copied before mutation so callers don't see action-at-a-
// distance.
func applySupplyBypasses(findings []Finding, manifestSet []ChangedManifest) []Finding {
	if len(findings) == 0 || len(manifestSet) == 0 {
		return findings
	}
	// Index markers by manifest path. Parse each manifest's AfterBytes
	// once per dispatch (cheap regex; manifests are small).
	byPath := make(map[string][]SupplyBypassMarker, len(manifestSet))
	for _, m := range manifestSet {
		if len(m.AfterBytes) == 0 {
			continue
		}
		markers := ParseSupplyBypasses(m.AfterBytes)
		if len(markers) > 0 {
			byPath[m.Path] = markers
		}
	}
	if len(byPath) == 0 {
		return findings
	}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		markers, ok := byPath[f.Path]
		if !ok {
			out = append(out, f)
			continue
		}
		bp := MatchSupplyBypass(markers, f.RuleID)
		if bp == nil {
			out = append(out, f)
			continue
		}
		// Apply: downgrade severity and prefix message with the
		// BYPASSED shape for agents-side disposition extraction.
		f.Severity = SeverityAdvise
		f.Message = fmt.Sprintf("[BYPASSED %s: %s] %s", bp.AuditID, bp.Reason, f.Message)
		out = append(out, f)
	}
	return out
}

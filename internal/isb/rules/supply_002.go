// Package rules: SUPPLY-002 — Typosquat detection (D5 Phase 1, slice β).
//
// Per docs/roadmap.md § "Deliverable 5 — Supply Chain Hygiene":
//
//   - Per-ecosystem allowlist source: `aws codeartifact list-packages
//     --domain code-artifacts-prod --domain-owner 801997600626
//     --repository <ecosystem>-prod`. The result is the set of
//     packages the org has ever pulled — better signal than external
//     popularity.
//   - For each added dep not already in the allowlist, compute
//     Damerau–Levenshtein distance to every allowlist entry.
//   - If `distance <= 2` AND the added dep is not in
//     `SystemConfig.supply_typosquat_preapproved`, raise
//     `[SUPPLY-002]` (advise-mode at launch) with suspected-original
//     suggestion.
//   - Operator pre-approval lands in the preapproved set via durable
//     audit (force CLI: `force supply preapprove <ecosystem> <name>`,
//     Phase 4 work).
//
// Allowlist storage shape: SystemConfig keys
// `supply_allowlist_<ecosystem>` and `supply_typosquat_preapproved`,
// values are newline-separated package names. The
// `supply-allowlist-refresh` daemon dog (Phase 4) populates the
// per-ecosystem allowlist daily; until that lands the allowlist is
// empty and the rule logs+returns no findings (gracefully disabled,
// not an error — it's normal during P1..P3 bring-up).
//
// Anti-cheat (per § "Anti-cheat directives"):
//   - **No hardcoded allowlists for popular packages.** Allowlist
//     comes from CodeArtifact (Phase 4 dog). This file ships zero
//     baked-in package names.
//   - Severity is advise at launch (no block-default for new rules).
//   - This rule does NOT call CodeArtifact at run-time, so no
//     deferral path is required (the dog handles auth errors when
//     refreshing the allowlist).
package rules

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
)

// supply002 is the manifest-gated typosquat detector.
type supply002 struct{}

// NewSUPPLY002 constructs a SUPPLY-002 rule instance. Stateless — one
// shared instance is fine across the fleet.
func NewSUPPLY002() *supply002 { return &supply002{} }

// ID implements isb.ManifestGatedRule.
func (r *supply002) ID() string { return "SUPPLY-002" }

// Ecosystems returns every ecosystem D5 covers. SUPPLY-002 is
// applicable across all five — typosquats are an ecosystem-agnostic
// threat model.
func (r *supply002) Ecosystems() []manifests.Ecosystem {
	return []manifests.Ecosystem{
		manifests.EcosystemRubyGems,
		manifests.EcosystemPyPI,
		manifests.EcosystemNPM,
		manifests.EcosystemMaven,
		manifests.EcosystemGo,
	}
}

// Run scans the input's added deps against the per-ecosystem allowlist
// and emits a SUPPLY-002 advise-mode finding for each suspected
// typosquat. The empty-allowlist case is logged and returns no
// findings (the Phase 4 supply-allowlist-refresh dog populates the
// allowlist; until then the rule is intentionally inert).
func (r *supply002) Run(ctx context.Context, db *sql.DB, in isb.ManifestGatedInput) ([]isb.Finding, error) {
	// Build the preapproved set once per Run — covers every ecosystem
	// (the operator-allowlist is global, not per-ecosystem, by spec
	// §1572: "operator pre-approval lands in the preapproved set").
	preapproved := loadAllowlistEntries(db, "supply_typosquat_preapproved")
	preapprovedSet := makeLowerSet(preapproved)

	// Cache per-ecosystem allowlists across multi-manifest inputs so
	// we don't re-query SystemConfig once per ChangedManifest.
	allowlistCache := map[manifests.Ecosystem][]string{}
	emptyWarned := map[manifests.Ecosystem]bool{}

	var out []isb.Finding

	for _, cm := range in.ChangedManifests {
		eco := cm.Ecosystem

		allow, cached := allowlistCache[eco]
		if !cached {
			key := fmt.Sprintf("supply_allowlist_%s", eco)
			allow = loadAllowlistEntries(db, key)
			allowlistCache[eco] = allow
		}

		// Empty-allowlist gate: log once per ecosystem per Run and
		// skip — the Phase 4 dog hasn't populated this yet.
		if len(allow) == 0 {
			if !emptyWarned[eco] {
				log.Printf("[SUPPLY-002] %s allowlist empty — typosquat detection disabled until supply-allowlist-refresh dog runs (P4)", eco)
				emptyWarned[eco] = true
			}
			continue
		}

		allowSet := makeLowerSet(allow)

		for _, dep := range cm.DepsAdded {
			lower := strings.ToLower(dep.Name)

			// Already in the allowlist → it's a known popular package
			// for this org. No finding.
			if _, ok := allowSet[lower]; ok {
				continue
			}

			// Operator-preapproved exception → no finding even if it
			// looks like a typosquat.
			if _, ok := preapprovedSet[lower]; ok {
				continue
			}

			closest, distance := closestAllowlistEntry(lower, allow)
			// distance==0 is a case-insensitive exact match handled
			// above (allowSet hit); guard anyway.
			if distance <= 0 || distance > 2 {
				continue
			}

			out = append(out, isb.Finding{
				RuleID:   "SUPPLY-002",
				Severity: isb.SeverityAdvise,
				Path:     cm.Path,
				Line:     0,
				Message: fmt.Sprintf(
					"SUPPLY-002: %s dep %s@%s may be a typo of %s (distance=%d)",
					eco, dep.Name, dep.Version, closest, distance,
				),
			})
		}
	}

	return out, nil
}

// loadAllowlistEntries reads a newline-separated SystemConfig value
// and returns the list of non-empty entries. Returns an empty slice
// when the key is missing or the value is whitespace-only — both are
// "no allowlist entries", indistinguishable for downstream logic.
func loadAllowlistEntries(db *sql.DB, key string) []string {
	// store.GetConfig returns "" for missing keys (its default
	// argument is "") — we treat that the same as an empty list.
	raw := getConfigViaStore(db, key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// makeLowerSet builds a lowercase-keyed set from a list of names.
// Typosquats often differ only in case (e.g. "Express" vs "express"),
// so the allowlist comparison is case-insensitive per spec §
// "anti-cheat" and field experience.
func makeLowerSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = struct{}{}
	}
	return out
}

// closestAllowlistEntry returns (closestName, distance) for the
// allowlist entry with the smallest Damerau-Levenshtein distance to
// the lowercase candidate name. Returns ("", -1) on empty allowlist
// (defensive — caller already checks).
func closestAllowlistEntry(candidateLower string, allow []string) (string, int) {
	if len(allow) == 0 {
		return "", -1
	}
	bestName := ""
	bestDist := -1
	for _, entry := range allow {
		lower := strings.ToLower(entry)
		// Trivial early-exit: if exact case-insensitive match, distance
		// is 0 and we're done.
		if lower == candidateLower {
			return entry, 0
		}
		d := damerauLevenshtein(candidateLower, lower)
		if bestDist < 0 || d < bestDist {
			bestDist = d
			bestName = entry
			// distance 1 is the lowest meaningful typosquat signal;
			// no point continuing to scan further once we've hit it.
			if d == 1 {
				break
			}
		}
	}
	return bestName, bestDist
}

// damerauLevenshtein returns the Damerau-Levenshtein edit distance
// between s and t, supporting insert / delete / substitute / adjacent
// transposition (each op = cost 1). Implementation: classical DP
// table with O(|s|*|t|) time and space, restricted to the
// "Optimal-String-Alignment" form (transposition only across two
// adjacent characters with no further edits in between). For the
// SUPPLY-002 allowlist sizes (~10K entries) this is cheap.
//
// We accept the OSA restriction over the unrestricted Damerau-
// Levenshtein because (a) the unrestricted form needs an alphabet-
// indexed extra row that's overkill for distance ≤ 2, and (b) for
// pairs at distance ≤ 2 the two metrics agree.
func damerauLevenshtein(s, t string) int {
	// Use rune slices so multibyte characters (rare in dep names but
	// possible in custom internal packages) count correctly.
	a := []rune(s)
	b := []rune(t)
	la := len(a)
	lb := len(b)

	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// d is a (la+1) x (lb+1) matrix; flatten as 1-D for cache locality.
	w := lb + 1
	d := make([]int, (la+1)*w)
	for i := 0; i <= la; i++ {
		d[i*w+0] = i
	}
	for j := 0; j <= lb; j++ {
		d[0*w+j] = j
	}

	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := d[(i-1)*w+j] + 1
			ins := d[i*w+(j-1)] + 1
			sub := d[(i-1)*w+(j-1)] + cost
			best := del
			if ins < best {
				best = ins
			}
			if sub < best {
				best = sub
			}
			// Transposition (Damerau): when a[i-2..i-1] swap matches
			// b[j-2..j-1] in reversed order, allow a single-cost
			// transposition.
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				trans := d[(i-2)*w+(j-2)] + 1
				if trans < best {
					best = trans
				}
			}
			d[i*w+j] = best
		}
	}
	return d[la*w+lb]
}

// getConfigViaStore is a tiny indirection that calls store.GetConfig
// with an empty default. It exists so the rule-side test harness can
// stub the SystemConfig reader without owning a full *sql.DB,
// although the production tests use a real :memory: DB so this is
// just a code-style affordance.
//
// Defined as a package var to keep the call site simple and to allow
// supply_002_test.go to override it in unit tests of the
// damerauLevenshtein helper without seeding the DB. Production code
// must NOT reassign this — the audittools P-SupplyDeferral pattern
// rejects override sites outside _test.go files.
var getConfigViaStore = func(db *sql.DB, key string) string {
	// Inline implementation to avoid importing store from within the
	// rules package (which already imports store transitively only
	// via the helpers; we keep this leaf-isolated).
	if db == nil {
		return ""
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM SystemConfig WHERE key = ?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// Static-typecheck assertion: supply002 satisfies the manifest-gated
// rule contract. Failing this fails compilation — keeps the contract
// drift-proof.
var _ isb.ManifestGatedRule = (*supply002)(nil)

// (No init() registration here — the rule is wired into the
// manifest-gated registry by the daemon-side wire site, which is
// out of scope for slice β. Slice γ adds the FleetRules seed row;
// post-integration commits the daemon registration.)

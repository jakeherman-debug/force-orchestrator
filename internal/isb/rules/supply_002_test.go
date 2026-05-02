package rules

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/store"
)

// ── Test helpers ─────────────────────────────────────────────────────────────

// seedAllowlist writes the per-ecosystem allowlist into SystemConfig
// using the same key shape the supply-allowlist-refresh dog (P4) will
// use: `supply_allowlist_<ecosystem>`. Newline-separated entries.
func seedAllowlist(t *testing.T, db *sql.DB, eco manifests.Ecosystem, names []string) {
	t.Helper()
	store.SetConfig(db, "supply_allowlist_"+string(eco), strings.Join(names, "\n"))
}

// seedPreapproved writes the operator-preapproved typosquat exception
// list (e.g. "expres" deliberately allowed alongside "express").
func seedPreapproved(t *testing.T, db *sql.DB, names []string) {
	t.Helper()
	store.SetConfig(db, "supply_typosquat_preapproved", strings.Join(names, "\n"))
}

// inputWith builds a minimal ManifestGatedInput with one ChangedManifest
// entry for the supplied ecosystem + path + dep set.
func inputWith(eco manifests.Ecosystem, path string, deps ...manifests.Dependency) isb.ManifestGatedInput {
	return isb.ManifestGatedInput{
		SourceTaskID: 1,
		TargetRepo:   "example",
		Branch:       "feature/test",
		CommitSHA:    "deadbeefcafebabe",
		ChangedManifests: []isb.ChangedManifest{{
			Path:      path,
			Ecosystem: eco,
			DepsAdded: deps,
		}},
	}
}

// mkDep is a tiny constructor for the manifests.Dependency shape used
// throughout these tests. (Named mkDep to avoid colliding with the
// `dep` helper in supply_001_test.go — both files share the rules
// package namespace.)
func mkDep(eco manifests.Ecosystem, name, version string) manifests.Dependency {
	return manifests.Dependency{
		Ecosystem: eco,
		Name:      name,
		Version:   version,
		Source:    manifests.SourceDirect,
	}
}

// runSupply002 executes the rule against an input, defaulting to a
// background context, and returns the findings (failing the test on
// any non-nil error so individual cases stay terse).
func runSupply002(t *testing.T, db *sql.DB, in isb.ManifestGatedInput) []isb.Finding {
	t.Helper()
	r := NewSUPPLY002()
	out, err := r.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("SUPPLY-002 Run: unexpected error: %v", err)
	}
	return out
}

// ── End-to-end rule tests ────────────────────────────────────────────────────

func TestSUPPLY002_DepInAllowlist_NoFinding(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"express", "react"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "express", "4.18.0"))

	got := runSupply002(t, db, in)
	if len(got) != 0 {
		t.Fatalf("expected zero findings for allowlisted dep; got %d: %+v", len(got), got)
	}
}

func TestSUPPLY002_ExactMatch_NoFinding(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"react"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "react", "18.2.0"))

	got := runSupply002(t, db, in)
	if len(got) != 0 {
		t.Fatalf("expected zero findings for exact match; got %+v", got)
	}
}

func TestSUPPLY002_Distance1_AdviseFinding(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"express"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "expres", "1.0.0"))

	got := runSupply002(t, db, in)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding; got %d: %+v", len(got), got)
	}
	if got[0].RuleID != "SUPPLY-002" {
		t.Errorf("rule id mismatch: %q", got[0].RuleID)
	}
	if got[0].Severity != isb.SeverityAdvise {
		t.Errorf("severity mismatch: %q", got[0].Severity)
	}
	if !strings.Contains(got[0].Message, "express") {
		t.Errorf("message missing closest match 'express': %q", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "distance=1") {
		t.Errorf("message missing distance=1: %q", got[0].Message)
	}
	if got[0].Path != "package.json" {
		t.Errorf("path mismatch: %q", got[0].Path)
	}
}

func TestSUPPLY002_Distance2_AdviseFinding(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"express"})

	// "exqresx" vs "express": p→q (pos 3) and s→x (pos 7) — two
	// substitutions = distance 2.
	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "exqresx", "1.0.0"))

	got := runSupply002(t, db, in)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding; got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "distance=2") {
		t.Errorf("message missing distance=2: %q", got[0].Message)
	}
}

func TestSUPPLY002_Distance3_NoFinding(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// "exprezz" vs "express" is distance=3 (two substitutions s→z s→z
	// plus one removal — actually substitutions only: s→z s→z, len(7)
	// vs len(7), so distance=2). Use a clearly-distance-3 candidate
	// instead so the test is unambiguous: "exqrezz" vs "express" is
	// distance=3 (q→p, z→s, z→s after first match).
	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"express"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "exqrezz", "1.0.0"))

	got := runSupply002(t, db, in)
	if len(got) != 0 {
		t.Fatalf("expected zero findings for distance-3; got %+v", got)
	}
}

func TestSUPPLY002_Transposition_AdviseFinding(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// "lodahs" vs "lodash" is a single adjacent transposition (sh ↔
	// hs), so Damerau-Levenshtein distance = 1.
	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"lodash"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "lodahs", "1.0.0"))

	got := runSupply002(t, db, in)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding; got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "distance=1") {
		t.Errorf("expected distance=1 (Damerau transposition): %q", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "lodash") {
		t.Errorf("expected closest=lodash: %q", got[0].Message)
	}
}

func TestSUPPLY002_Preapproved_NoFinding(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"express"})
	seedPreapproved(t, db, []string{"expres"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "expres", "1.0.0"))

	got := runSupply002(t, db, in)
	if len(got) != 0 {
		t.Fatalf("expected zero findings for preapproved typosquat; got %+v", got)
	}
}

func TestSUPPLY002_EmptyAllowlist_NoFindingsWarn(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Deliberately do NOT seed an allowlist — simulates the pre-P4
	// state where the supply-allowlist-refresh dog hasn't run yet.
	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "anything", "1.0.0"))

	r := NewSUPPLY002()
	got, err := r.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("expected nil error on empty allowlist; got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero findings when allowlist empty; got %+v", got)
	}
	// The log line is verified-by-eye via TestSUPPLY002_EmptyAllowlist
	// running with `-v`; we don't capture log output here because
	// log.Printf goes to stderr by default and snooping on it would
	// over-couple the test to an implementation detail.
}

func TestSUPPLY002_CaseInsensitive(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Allowlist has "Express" (capitalized); dep is "express" — the
	// case-insensitive comparator should treat this as an exact match
	// (distance=0) and emit no finding.
	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"Express"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "express", "4.18.0"))

	got := runSupply002(t, db, in)
	if len(got) != 0 {
		t.Fatalf("expected zero findings (case-insensitive match); got %+v", got)
	}
}

func TestSUPPLY002_MultipleDeps_FindingsCollected(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedAllowlist(t, db, manifests.EcosystemNPM, []string{"express", "lodash"})

	in := inputWith(manifests.EcosystemNPM, "package.json",
		mkDep(manifests.EcosystemNPM, "express", "4.18.0"),    // exact match → no finding
		mkDep(manifests.EcosystemNPM, "lodahs", "1.0.0"),      // transposition → finding
		mkDep(manifests.EcosystemNPM, "totally-unrelated", "1.0.0"), // far away → no finding
	)

	got := runSupply002(t, db, in)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 finding (only the typosquat); got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "lodahs") {
		t.Errorf("expected the lodahs finding: %q", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "lodash") {
		t.Errorf("expected closest=lodash: %q", got[0].Message)
	}
}

// ── Damerau-Levenshtein helper unit tests ────────────────────────────────────

func TestDamerauLevenshtein_KnownCases(t *testing.T) {
	cases := []struct {
		s, t string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "a", 1},
		{"a", "a", 0},
		{"a", "b", 1},
		{"ab", "ba", 1},      // adjacent transposition
		{"ab", "ab", 0},
		{"abc", "abd", 1},    // single substitution
		{"abc", "abcd", 1},   // single insert
		{"abcd", "abc", 1},   // single delete
		{"kitten", "sitting", 3},
		{"lodash", "lodahs", 1}, // transposition
		{"express", "expres", 1},
		{"express", "exqresx", 2},
		{"express", "exqrezz", 3},
	}
	for _, c := range cases {
		got := damerauLevenshtein(c.s, c.t)
		if got != c.want {
			t.Errorf("damerauLevenshtein(%q,%q)=%d, want %d", c.s, c.t, got, c.want)
		}
	}
}

// closestAllowlistEntry is exercised indirectly by every rule test
// above, but here's a focused check that the early-exit on exact
// case-insensitive match returns 0.
func TestClosestAllowlistEntry_ExactCaseInsensitive(t *testing.T) {
	closest, dist := closestAllowlistEntry("express", []string{"React", "Express", "Lodash"})
	if dist != 0 {
		t.Errorf("expected distance=0 for case-insensitive match; got %d (closest=%q)", dist, closest)
	}
	if closest != "Express" {
		t.Errorf("expected closest='Express'; got %q", closest)
	}
}

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBootstrapFleetRules_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	ctx := context.Background()
	// First run inserts every seed.
	n1, err := BootstrapFleetRules(ctx, db, "")
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if n1 == 0 {
		t.Fatalf("first bootstrap inserted 0 rows; expected len(audit) > 0")
	}
	// Second run no-ops.
	n2, err := BootstrapFleetRules(ctx, db, "")
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second bootstrap inserted %d rows; expected 0 (idempotent)", n2)
	}
	// Row count matches seed length.
	got := countFleetRules(t, db)
	if got != len(BootstrapAudit()) {
		t.Fatalf("FleetRules row count: got %d, want %d (one per audit seed)", got, len(BootstrapAudit()))
	}
}

func TestBootstrapFleetRules_AllSectionsCategorized(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	root := repoRoot(t)
	mdPath := filepath.Join(root, "CLAUDE.md")
	if _, err := BootstrapFleetRules(ctx, db, mdPath); err != nil {
		t.Fatalf("BootstrapFleetRules with all-sections check: %v", err)
	}
}

func TestBootstrapFleetRules_RenderToJustified(t *testing.T) {
	for _, s := range BootstrapAudit() {
		if s.RenderTo == "claude-md-file" {
			if strings.TrimSpace(s.Justification) == "" {
				t.Errorf("audit seed %q (render_to='claude-md-file') has empty Justification — explain the universal-load fit", s.RuleKey)
			}
		}
	}
}

func TestBootstrapFleetRules_RenderToEnumValid(t *testing.T) {
	allowed := map[string]bool{
		"claude-md-file":         true,
		"agent-prompt":           true,
		"fix-log":                true,
		"pattern-test-docstring": true,
		"discard":                true,
	}
	for _, s := range BootstrapAudit() {
		if allowed[s.RenderTo] {
			continue
		}
		if strings.HasPrefix(s.RenderTo, "per-domain-doc:") {
			continue
		}
		t.Errorf("audit seed %q has invalid render_to %q", s.RuleKey, s.RenderTo)
	}
}

func TestBootstrapFleetRules_RuleKeyUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range BootstrapAudit() {
		if seen[s.RuleKey] {
			t.Errorf("duplicate rule_key %q in BootstrapAudit", s.RuleKey)
		}
		seen[s.RuleKey] = true
	}
}

func TestBootstrapFleetRules_BreakdownReport(t *testing.T) {
	// Report-only test: prints the render_to breakdown so the gate-2
	// verification has a single source for the operator-reviewable
	// summary. Always passes; the count assertion lives in
	// TestBootstrapFleetRules_UniversalLoadCountIsPlausible.
	counts := map[string]int{}
	for _, s := range BootstrapAudit() {
		key := s.RenderTo
		if strings.HasPrefix(key, "per-domain-doc:") {
			key = "per-domain-doc:*"
		}
		counts[key]++
	}
	t.Logf("FleetRules audit render_to breakdown:")
	for k, v := range counts {
		t.Logf("  %-25s %d", k, v)
	}
	t.Logf("  TOTAL                     %d", len(BootstrapAudit()))
}

func TestBootstrapFleetRules_UniversalLoadCountIsPlausible(t *testing.T) {
	// The 10 KB target for CLAUDE.md is achievable only if the
	// universal-load count stays small. The cap here mirrors the
	// roadmap's anti-cheat directive on "no claude-md-file as default".
	// If this trips, either the audit drifted or the operator
	// explicitly wants more universal content — bumping the cap
	// requires touching this constant deliberately.
	const universalLoadCap = 15
	got := 0
	for _, s := range BootstrapAudit() {
		if s.RenderTo == "claude-md-file" {
			got++
		}
	}
	if got > universalLoadCap {
		t.Errorf("FleetRules audit: %d entries with render_to='claude-md-file' exceeds plausible cap (%d). Either narrow the audit or bump the constant deliberately.", got, universalLoadCap)
	}
}

func countFleetRules(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM FleetRules`).Scan(&n); err != nil {
		t.Fatalf("count FleetRules: %v", err)
	}
	return n
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller: cannot locate self")
	}
	// internal/store/<this>.go → ../../
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

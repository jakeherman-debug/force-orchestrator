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

	// Run the check against the pre-Phase-3 CLAUDE.md snapshot. The live
	// CLAUDE.md becomes auto-generated content with category-labeled
	// section headings after Phase 3's render — those section names do
	// not match the audit's Section fields by design (the Section field
	// captures the ORIGINAL hand-authored heading the rule came from).
	// The snapshot is the audit-time witness; if a future audit
	// extension references a section not in the snapshot, the snapshot
	// gets refreshed in lockstep.
	root := repoRoot(t)
	fixturePath := filepath.Join(root, "internal", "store", "testdata", "claude_md_pre_p3.md")
	if _, err := BootstrapFleetRules(ctx, db, fixturePath); err != nil {
		t.Fatalf("BootstrapFleetRules with all-sections check against pre-P3 fixture: %v", err)
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

// TestBootstrapFleetRules_ConvergentOnContentChange asserts that when a
// bootstrap-managed row's persisted content drifts from the audit slice,
// a second BootstrapFleetRules call refreshes the row in place — same
// id, same created_at, refreshed content + content_hash.
//
// This is the systemic fix for the recurring "stale persistent DB"
// false-drift signal: editing fleet_rules_audit.go content and
// re-bootstrapping now converges DB state on the audit slice instead of
// silently no-opping.
func TestBootstrapFleetRules_ConvergentOnContentChange(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	if _, err := BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// Pick a known bootstrap-managed seed and corrupt its persisted
	// content + content_hash to simulate stale-DB drift.
	seed := BootstrapAudit()[0]
	const stale = "STALE CONTENT — should be overwritten by convergent bootstrap"
	staleHash := sha256Hex(stale)
	res, err := db.ExecContext(ctx, `
		UPDATE FleetRules SET content = ?, content_hash = ?
		WHERE rule_key = ? AND version = 1
	`, stale, staleHash, seed.RuleKey)
	if err != nil {
		t.Fatalf("inject stale content: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("inject stale: expected 1 row updated, got %d", n)
	}

	// Snapshot id + created_at BEFORE second bootstrap so we can
	// prove the convergent UPDATE preserves both.
	var idBefore int
	var createdAtBefore string
	if err := db.QueryRowContext(ctx, `
		SELECT id, created_at FROM FleetRules WHERE rule_key = ? AND version = 1
	`, seed.RuleKey).Scan(&idBefore, &createdAtBefore); err != nil {
		t.Fatalf("snapshot pre-second-bootstrap: %v", err)
	}

	n2, err := BootstrapFleetRules(ctx, db, "")
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("second bootstrap touched %d rows; expected exactly 1 (the stale-content row)", n2)
	}

	var (
		idAfter        int
		createdAtAfter string
		createdByAfter string
		contentAfter   string
		hashAfter      string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT id, created_at, created_by, content, content_hash
		FROM FleetRules WHERE rule_key = ? AND version = 1
	`, seed.RuleKey).Scan(&idAfter, &createdAtAfter, &createdByAfter, &contentAfter, &hashAfter); err != nil {
		t.Fatalf("read post-second-bootstrap: %v", err)
	}

	if idAfter != idBefore {
		t.Errorf("convergent bootstrap re-keyed the row: id %d → %d (expected stable id)", idBefore, idAfter)
	}
	if createdAtAfter != createdAtBefore {
		t.Errorf("convergent bootstrap rewrote created_at: %q → %q (expected preservation)", createdAtBefore, createdAtAfter)
	}
	if createdByAfter != "bootstrap" {
		t.Errorf("convergent bootstrap rewrote created_by: %q (expected 'bootstrap')", createdByAfter)
	}
	if contentAfter != seed.Content {
		t.Errorf("convergent bootstrap did not refresh content; still drifted")
	}
	if hashAfter != sha256Hex(seed.Content) {
		t.Errorf("convergent bootstrap did not refresh content_hash; still drifted")
	}
}

// TestBootstrapFleetRules_DoesNotClobberOperatorDirectWriteRules asserts
// the convergence scope is bootstrap-managed rows only. An
// operator-routed row at the same (rule_key, version) coordinate
// — created_by='operator:<email>' per docs/paired-runs.md §
// Direct-write rules — must survive bootstrap re-runs byte-identical.
func TestBootstrapFleetRules_DoesNotClobberOperatorDirectWriteRules(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	if _, err := BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// Insert an operator-direct-write row at a fresh rule_key not in
	// the audit slice (so it cannot conflict with any seed).
	const operatorKey = "operator-direct-test/ad-hoc-rule"
	const operatorContent = "Operator-authored rule body — must not be touched by bootstrap convergence."
	const operatorBy = "operator:test@example.com"
	operatorHash := sha256Hex(operatorContent)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO FleetRules (
			rule_key, category, agent_scope, render_to, enforced_by,
			content, content_hash, version, active_from, created_by
		) VALUES (?, 'operator-direct', 'all', 'discard', 'trust-only', ?, ?, 1, datetime('now'), ?)
	`, operatorKey, operatorContent, operatorHash, operatorBy); err != nil {
		t.Fatalf("insert operator-direct row: %v", err)
	}

	// Snapshot every column on the operator row so a single byte-level
	// drift surfaces.
	type opRow struct {
		id          int
		category    string
		agentScope  string
		renderTo    string
		enforcedBy  string
		content     string
		contentHash string
		createdBy   string
		createdAt   string
	}
	read := func() opRow {
		t.Helper()
		var r opRow
		if err := db.QueryRowContext(ctx, `
			SELECT id, category, agent_scope, render_to, enforced_by,
				content, content_hash, created_by, created_at
			FROM FleetRules WHERE rule_key = ? AND version = 1
		`, operatorKey).Scan(&r.id, &r.category, &r.agentScope, &r.renderTo, &r.enforcedBy,
			&r.content, &r.contentHash, &r.createdBy, &r.createdAt); err != nil {
			t.Fatalf("read operator row: %v", err)
		}
		return r
	}
	before := read()

	// Re-run bootstrap. The operator row is at a key the audit slice
	// doesn't claim, so no UPSERT fires for it; this asserts that
	// bootstrap-key rows AND non-bootstrap-key rows both stay safe.
	if _, err := BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if got := read(); got != before {
		t.Errorf("bootstrap clobbered operator-direct-write row.\n  before: %+v\n  after:  %+v", before, got)
	}

	// Now collide on a key the audit slice DOES claim — simulate the
	// theoretical case where an operator-direct-write row replaces a
	// bootstrap row at the same coordinate. Bootstrap should refuse
	// to touch it.
	collisionSeed := BootstrapAudit()[0]
	if _, err := db.ExecContext(ctx, `
		UPDATE FleetRules SET created_by = ? WHERE rule_key = ? AND version = 1
	`, operatorBy, collisionSeed.RuleKey); err != nil {
		t.Fatalf("convert audit row to operator row: %v", err)
	}
	// Drift the content too — convergent bootstrap would refresh this
	// for a 'bootstrap' row but must SKIP it for the operator row.
	const operatorOverride = "Operator override of an audit-managed key — must persist."
	if _, err := db.ExecContext(ctx, `
		UPDATE FleetRules SET content = ?, content_hash = ?
		WHERE rule_key = ? AND version = 1
	`, operatorOverride, sha256Hex(operatorOverride), collisionSeed.RuleKey); err != nil {
		t.Fatalf("override audit row content: %v", err)
	}

	if _, err := BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}

	var (
		gotContent, gotBy string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT content, created_by FROM FleetRules WHERE rule_key = ? AND version = 1
	`, collisionSeed.RuleKey).Scan(&gotContent, &gotBy); err != nil {
		t.Fatalf("read collision row: %v", err)
	}
	if gotContent != operatorOverride {
		t.Errorf("bootstrap clobbered operator-overridden audit-key row content: got %q, want %q", gotContent, operatorOverride)
	}
	if gotBy != operatorBy {
		t.Errorf("bootstrap rewrote created_by: got %q, want %q", gotBy, operatorBy)
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

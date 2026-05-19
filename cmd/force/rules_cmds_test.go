package main

// rules_cmds_test.go — D14 Phase 3 tests for force rules.
//
// Tests:
//   - rules list with --repo calls ResolveRulesForRepo
//   - rules list with --scope filters by scope
//   - rules upgrade validates scope syntax
//   - rules upgrade updates agent_scope for active rule

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedFleetRule inserts a FleetRules row with active_until='' (active).
// Uses direct SQL to avoid dependency on a non-existent store helper.
func seedFleetRule(t *testing.T, db interface {
	Exec(string, ...any) (interface{ LastInsertId() (int64, error) }, error)
	QueryRow(string, ...any) interface{ Scan(...any) error }
}, ruleKey, agentScope, category, renderTo, content string) {
	t.Helper()
	// No-op stub — real seeding is done by calling db.Exec directly in tests.
}

// ── rules list ────────────────────────────────────────────────────────────────

func TestRulesList_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		code := cmdRules(db, []string{"list"})
		if code != 0 {
			t.Errorf("rules list empty: exit %d", code)
		}
	})
	if !strings.Contains(out, "no active FleetRules") {
		t.Errorf("expected empty message; out=%q", out)
	}
}

func TestRulesList_WithRepo_CallsResolveRulesForRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a global rule (senate:*) and a tag-scoped rule (senate:tag:pay).
	// Use direct Exec to seed rows — no high-level store helper exists for
	// arbitrary FleetRules inserts outside the bootstrap path.
	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('global-rule', 'test', 'senate:*', 'claude-md-file', '', 'global content', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed global-rule: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('repo-rule', 'test', 'senate:myrepo', 'claude-md-file', '', 'repo content', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed repo-rule: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('other-repo-rule', 'test', 'senate:otherrepo', 'claude-md-file', '', 'other content', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed other-repo-rule: %v", err)
	}

	// force rules list --repo myrepo: should show global-rule + repo-rule, not other-repo-rule.
	out := captureOutput(func() {
		code := cmdRules(db, []string{"list", "--repo", "myrepo"})
		if code != 0 {
			t.Errorf("rules list --repo: exit %d", code)
		}
	})

	if !strings.Contains(out, "global-rule") {
		t.Errorf("missing global-rule for myrepo; out=%q", out)
	}
	if !strings.Contains(out, "repo-rule") {
		t.Errorf("missing repo-rule for myrepo; out=%q", out)
	}
	if strings.Contains(out, "other-repo-rule") {
		t.Errorf("other-repo-rule should not appear for myrepo; out=%q", out)
	}
}

func TestRulesList_WithScopeGlobal(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('g-rule', 'test', 'senate:*', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('r-rule', 'test', 'senate:somerepo', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := captureOutput(func() {
		code := cmdRules(db, []string{"list", "--scope", "global"})
		if code != 0 {
			t.Errorf("rules list --scope global: exit %d", code)
		}
	})
	if !strings.Contains(out, "g-rule") {
		t.Errorf("missing g-rule with --scope global; out=%q", out)
	}
	if strings.Contains(out, "r-rule") {
		t.Errorf("r-rule should not appear with --scope global; out=%q", out)
	}
}

func TestRulesList_WithScopeTag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('tag-rule', 'test', 'senate:tag:payments', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := captureOutput(func() {
		code := cmdRules(db, []string{"list", "--scope", "tag:payments"})
		if code != 0 {
			t.Errorf("rules list --scope tag:payments: exit %d", code)
		}
	})
	if !strings.Contains(out, "tag-rule") {
		t.Errorf("missing tag-rule; out=%q", out)
	}
}

func TestRulesList_WithScopeRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('my-repo-rule', 'test', 'senate:myrepo', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := captureOutput(func() {
		code := cmdRules(db, []string{"list", "--scope", "repo:myrepo"})
		if code != 0 {
			t.Errorf("rules list --scope repo:myrepo: exit %d", code)
		}
	})
	if !strings.Contains(out, "my-repo-rule") {
		t.Errorf("missing my-repo-rule; out=%q", out)
	}
}

func TestRulesList_InactiveRulesExcluded(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed one active and one inactive (active_until set).
	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('active-rule', 'test', 'senate:*', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed active: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('inactive-rule', 'test', 'senate:*', 'claude-md-file', '', 'c', 1, '2024-01-01')
	`)
	if err != nil {
		t.Fatalf("seed inactive: %v", err)
	}

	out := captureOutput(func() {
		code := cmdRules(db, []string{"list"})
		if code != 0 {
			t.Errorf("rules list: exit %d", code)
		}
	})
	if !strings.Contains(out, "active-rule") {
		t.Errorf("missing active-rule; out=%q", out)
	}
	if strings.Contains(out, "inactive-rule") {
		t.Errorf("inactive-rule should not appear in list; out=%q", out)
	}
}

func TestRulesList_WithRepoAndTagScoped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a tag-scoped rule, a global rule, and a repo-specific rule.
	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('global-r', 'test', 'senate:*', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed global: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('tag-pay-r', 'test', 'senate:tag:payments', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	// Add myrepo to the payments tag.
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "r")
	if err2 := store.CreateTag(db, "payments", "", "op"); err2 != nil {
		t.Fatalf("create tag: %v", err2)
	}
	if err2 := store.AddRepoTag(db, "myrepo", "payments", "op", "test"); err2 != nil {
		t.Fatalf("add repotag: %v", err2)
	}

	// ResolveRulesForRepo should return both global-r and tag-pay-r.
	out := captureOutput(func() {
		code := cmdRules(db, []string{"list", "--repo", "myrepo"})
		if code != 0 {
			t.Errorf("rules list --repo: exit %d", code)
		}
	})
	if !strings.Contains(out, "global-r") {
		t.Errorf("missing global-r; out=%q", out)
	}
	if !strings.Contains(out, "tag-pay-r") {
		t.Errorf("missing tag-pay-r (should apply via tag:payments); out=%q", out)
	}
}

// ── rules upgrade ─────────────────────────────────────────────────────────────

func TestRulesUpgrade_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('my-rule', 'test', 'senate:oldrepo', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := captureOutput(func() {
		code := cmdRules(db, []string{"upgrade", "my-rule", "--to-scope", "senate:*"})
		if code != 0 {
			t.Errorf("rules upgrade: exit %d", code)
		}
	})
	if !strings.Contains(out, "senate:*") {
		t.Errorf("missing scope confirmation; out=%q", out)
	}

	// Verify the DB was updated.
	var scope string
	if err := db.QueryRow(`SELECT agent_scope FROM FleetRules WHERE rule_key = 'my-rule'`).Scan(&scope); err != nil {
		t.Fatalf("query: %v", err)
	}
	if scope != "senate:*" {
		t.Errorf("agent_scope: got %q want senate:*", scope)
	}
}

func TestRulesUpgrade_ToTagScope(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('tag-rule', 'test', 'senate:*', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	code := cmdRules(db, []string{"upgrade", "tag-rule", "--to-scope", "senate:tag:payments"})
	if code != 0 {
		t.Errorf("rules upgrade to tag scope: exit %d", code)
	}

	var scope string
	if err := db.QueryRow(`SELECT agent_scope FROM FleetRules WHERE rule_key = 'tag-rule'`).Scan(&scope); err != nil {
		t.Fatalf("query: %v", err)
	}
	if scope != "senate:tag:payments" {
		t.Errorf("agent_scope: got %q want senate:tag:payments", scope)
	}
}

func TestRulesUpgrade_InvalidScope_Rejects(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('my-rule', 'test', 'senate:*', 'claude-md-file', '', 'c', 1, '')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Missing "senate:" prefix.
	code := cmdRules(db, []string{"upgrade", "my-rule", "--to-scope", "bogus-scope"})
	if code == 0 {
		t.Error("invalid scope should exit non-zero")
	}

	// Verify agent_scope was NOT changed.
	var scope string
	if err := db.QueryRow(`SELECT agent_scope FROM FleetRules WHERE rule_key = 'my-rule'`).Scan(&scope); err != nil {
		t.Fatalf("query: %v", err)
	}
	if scope != "senate:*" {
		t.Errorf("agent_scope should be unchanged, got %q", scope)
	}
}

func TestRulesUpgrade_MissingToScope_Rejects(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	code := cmdRules(db, []string{"upgrade", "some-rule"})
	if code == 0 {
		t.Error("missing --to-scope should exit non-zero")
	}
}

func TestRulesUpgrade_NotFound_Rejects(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	code := cmdRules(db, []string{"upgrade", "nonexistent-rule", "--to-scope", "senate:*"})
	if code == 0 {
		t.Error("upgrading non-existent rule should exit non-zero")
	}
}

func TestRulesUpgrade_DoesNotUpdateInactiveRule(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert an inactive rule.
	_, err := db.Exec(`
		INSERT INTO FleetRules (rule_key, category, agent_scope, render_to, enforced_by, content, version, active_until)
		VALUES ('old-rule', 'test', 'senate:oldrepo', 'claude-md-file', '', 'c', 1, '2024-01-01')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Should fail (no active row with that key).
	code := cmdRules(db, []string{"upgrade", "old-rule", "--to-scope", "senate:*"})
	if code == 0 {
		t.Error("upgrading inactive rule should exit non-zero (no active rows matched)")
	}
}

// ── validateRuleScope ─────────────────────────────────────────────────────────

func TestValidateRuleScope_ValidForms(t *testing.T) {
	cases := []string{
		"senate:*",
		"senate:tag:payments",
		"senate:tag:my-team",
		"senate:myrepo",
		"senate:some-long-repo-name",
	}
	for _, scope := range cases {
		if err := validateRuleScope(scope); err != nil {
			t.Errorf("validateRuleScope(%q) unexpected error: %v", scope, err)
		}
	}
}

func TestValidateRuleScope_InvalidForms(t *testing.T) {
	cases := []string{
		"",
		"global",
		"tag:payments",
		"repo:myrepo",
		"senate:",         // empty repo
		"senate:tag:",     // empty tag name
		"notsenate:*",
	}
	for _, scope := range cases {
		if err := validateRuleScope(scope); err == nil {
			t.Errorf("validateRuleScope(%q) should return error, got nil", scope)
		}
	}
}

// ── unknown subcommand / help ─────────────────────────────────────────────────

func TestCmdRules_UnknownSubcommand(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdRules(db, []string{"bogus"})
	if code == 0 {
		t.Error("unknown subcommand should exit non-zero")
	}
}

func TestCmdRules_HelpFlag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdRules(db, []string{"--help"})
	if code != 0 {
		t.Errorf("--help should exit 0, got %d", code)
	}
}

func TestCmdRules_NoSubcommand(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdRules(db, []string{})
	if code == 0 {
		t.Error("no subcommand should exit non-zero")
	}
}

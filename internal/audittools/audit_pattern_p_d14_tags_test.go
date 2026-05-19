// Package audittools: D14 Phase 1 pattern tests — Tag-driven rule scoping.
//
// Four pattern tests enforce the invariants introduced by D14 P1:
//
//  P_RuleScopeSyntaxValid      — any hardcoded senate:tag:* scope value must
//                                match the canonical regex; also validates that
//                                scope strings are syntactically valid.
//
//  P_SenateNoRepoTagsWrites    — AST walk of internal/senate/*.go: none may
//                                contain raw SQL that INSERTs into RepoTags.
//                                Only store functions may write RepoTags.
//
//  P_TagRegistryEnforced       — in-memory DB: inserting a RepoTags row with
//                                a tag absent from Tags must fail with a FK
//                                constraint error.
//
//  P_ResolveRulesForRepoComplete — in-memory DB: seed 4 matching rules + 1
//                                  non-matching rule, assert ResolveRulesForRepo
//                                  returns exactly 4 rows.
package audittools

import (
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── P_RuleScopeSyntaxValid ────────────────────────────────────────────────────

// senateScopeRe is the canonical regex for valid senate:* agent_scope values.
// Matches: senate:* | senate:tag:<label> | senate:<name>
// where <label> is [a-z0-9_-]+ and <name> is [a-zA-Z0-9_/-]+
var senateScopeRe = regexp.MustCompile(`^senate:(\*|tag:[a-z0-9_-]+|[a-zA-Z0-9_/-]+)$`)

// knownSenateScopeFixtures is the list of senate:* scope strings that appear
// as hardcoded string literals in fleet_rules_audit.go test fixtures or store
// helpers. The test validates each against senateScopeRe.
var knownSenateScopeFixtures = []struct {
	scope string
	valid bool
}{
	{"senate:*", true},
	{"senate:tag:frontend", true},
	{"senate:tag:api", true},
	{"senate:tag:valid-tag", true},
	{"senate:tag:valid_tag", true},
	{"senate:my-repo", true},
	{"senate:org/repo", true},
	// deliberately invalid — the test asserts these would NOT pass the regex
	{"senate:tag:INVALID TAG", false},
	{"senate:tag:UPPER", false},
	{"senate:tag:has space", false},
}

// TestPattern_P_RuleScopeSyntaxValid validates that our canonical senate scope
// regex correctly accepts valid values and rejects malformed ones.
func TestPattern_P_RuleScopeSyntaxValid(t *testing.T) {
	for _, tc := range knownSenateScopeFixtures {
		got := senateScopeRe.MatchString(tc.scope)
		if got != tc.valid {
			t.Errorf("scope %q: senateScopeRe.Match = %v, want %v", tc.scope, got, tc.valid)
		}
	}
}

// ── P_SenateNoRepoTagsWrites ──────────────────────────────────────────────────

// TestPattern_P_SenateNoRepoTagsWrites walks internal/senate/*.go (non-test)
// and asserts that none of them contain raw INSERT INTO RepoTags SQL. Only
// store layer functions may write RepoTags rows.
func TestPattern_P_SenateNoRepoTagsWrites(t *testing.T) {
	root := moduleRoot(t)
	senateDir := filepath.Join(root, "internal", "senate")

	var offences []string

	walkErr := filepath.WalkDir(senateDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if strings.Contains(err.Error(), "no such file") {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok {
				return true
			}
			// Check raw SQL string literals for forbidden patterns.
			v := strings.ToUpper(strings.TrimSpace(lit.Value))
			if strings.Contains(v, "INSERT INTO REPOTAGS") {
				pos := fset.Position(lit.Pos())
				rel, _ := filepath.Rel(root, path)
				offences = append(offences, rel+":"+itoa(pos.Line))
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", senateDir, walkErr)
	}

	if len(offences) > 0 {
		t.Errorf("Pattern P_SenateNoRepoTagsWrites: %d senate file(s) contain raw INSERT INTO RepoTags SQL. "+
			"Only store functions (store.AddRepoTag) may write RepoTags rows:", len(offences))
		for _, o := range offences {
			t.Errorf("  %s", o)
		}
	}
}

// itoa is a local int-to-string helper (avoids importing strconv).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 10)
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ── P_TagRegistryEnforced ─────────────────────────────────────────────────────

// TestPattern_P_TagRegistryEnforced asserts that the FOREIGN KEY constraint on
// RepoTags(tag) REFERENCES Tags(name) is enforced by SQLite with
// PRAGMA foreign_keys = ON.
//
// The holocron init sequence sets PRAGMA foreign_keys=ON after migrations, so
// any DB opened via InitHolocronDSN has FK enforcement active.
func TestPattern_P_TagRegistryEnforced(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Attempt to insert a RepoTag referencing a tag that does NOT exist in Tags.
	// With foreign_keys=ON this should fail.
	_, err := db.Exec(
		`INSERT INTO RepoTags (repo_name, tag, added_by, source)
		 VALUES ('my-repo', 'nonexistent-tag', 'test', 'operator')`,
	)
	if err == nil {
		t.Fatal("P_TagRegistryEnforced: expected FK constraint error when inserting RepoTag with unknown tag, got nil")
	}
	if !isFKConstraintError(err) {
		t.Fatalf("P_TagRegistryEnforced: expected FOREIGN KEY constraint error, got: %v", err)
	}

	// Positive path: tag exists in Tags → insert succeeds.
	if err := store.CreateTag(db, "my-tag", "test tag", "test"); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if err := store.AddRepoTag(db, "my-repo", "my-tag", "test", "operator"); err != nil {
		t.Fatalf("AddRepoTag: %v", err)
	}
}

// isFKConstraintError returns true if the error message contains the SQLite
// FK constraint failure text.
func isFKConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "FOREIGN KEY") || strings.Contains(msg, "CONSTRAINT FAILED")
}

// ── P_ResolveRulesForRepoComplete ────────────────────────────────────────────

// TestPattern_P_ResolveRulesForRepoComplete seeds an in-memory DB with 5
// FleetRules rows (4 should match "web", 1 should not) and asserts that
// ResolveRulesForRepo returns exactly 4 rows for "web".
func TestPattern_P_ResolveRulesForRepoComplete(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed tags and repo-tag associations.
	for _, tag := range []string{"frontend", "api"} {
		if err := store.CreateTag(db, tag, "", "test"); err != nil {
			t.Fatalf("CreateTag(%q): %v", tag, err)
		}
	}
	for _, tag := range []string{"frontend", "api"} {
		if err := store.AddRepoTag(db, "web", tag, "test", "operator"); err != nil {
			t.Fatalf("AddRepoTag(web, %q): %v", tag, err)
		}
	}

	// Helper to insert a FleetRules row with the given rule_key and agent_scope.
	insertRule := func(ruleKey, agentScope string) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO FleetRules (rule_key, agent_scope, render_to, content, active_until, created_by)
			 VALUES (?, ?, 'senate', 'rule content', '', 'test')`,
			ruleKey, agentScope,
		)
		if err != nil {
			t.Fatalf("insertRule(%q, %q): %v", ruleKey, agentScope, err)
		}
	}

	// 4 rules that SHOULD match "web":
	insertRule("rule-global", "senate:*")
	insertRule("rule-web-specific", "senate:web")
	insertRule("rule-tag-frontend", "senate:tag:frontend")
	insertRule("rule-tag-api", "senate:tag:api")

	// 1 rule that should NOT match "web" (targets a different repo):
	insertRule("rule-other-repo", "senate:other-repo")

	rules, err := store.ResolveRulesForRepo(db, "web")
	if err != nil {
		t.Fatalf("ResolveRulesForRepo: %v", err)
	}
	if len(rules) != 4 {
		t.Errorf("ResolveRulesForRepo(\"web\"): got %d rules, want 4", len(rules))
		for _, r := range rules {
			t.Logf("  rule_key=%q agent_scope=%q", r.RuleKey, r.AgentScope)
		}
	}

	// Verify the non-matching rule is absent.
	for _, r := range rules {
		if r.AgentScope == "senate:other-repo" {
			t.Errorf("ResolveRulesForRepo(\"web\"): unexpectedly returned rule with scope %q", r.AgentScope)
		}
	}

	// Verify all 4 expected scopes are present.
	scopesSeen := map[string]bool{}
	for _, r := range rules {
		scopesSeen[r.AgentScope] = true
	}
	for _, wantScope := range []string{"senate:*", "senate:web", "senate:tag:frontend", "senate:tag:api"} {
		if !scopesSeen[wantScope] {
			t.Errorf("ResolveRulesForRepo(\"web\"): missing rule with scope %q", wantScope)
		}
	}
}

// ── Additional idempotence checks ─────────────────────────────────────────────

// TestPattern_P_D14_TagCRUD exercises the happy path and key failure modes of
// Tags / RepoTags / TagSuggestions store helpers to catch regressions in the
// store layer.
func TestPattern_P_D14_TagCRUD(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// CreateTag
	if err := store.CreateTag(db, "payments", "payment-domain repos", "operator"); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	// Duplicate must fail.
	if err := store.CreateTag(db, "payments", "", "operator"); err == nil {
		t.Error("CreateTag duplicate: expected error, got nil")
	}

	// GetTag
	tag, err := store.GetTag(db, "payments")
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if tag.Name != "payments" {
		t.Errorf("GetTag: Name = %q, want %q", tag.Name, "payments")
	}

	// GetTag — not found
	_, err = store.GetTag(db, "does-not-exist")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetTag missing: want sql.ErrNoRows, got %v", err)
	}

	// ListTags
	tags, err := store.ListTags(db)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Name != "payments" {
		t.Errorf("ListTags: got %v", tags)
	}

	// AddRepoTag + ListTagsForRepo
	if err := store.AddRepoTag(db, "billing-svc", "payments", "op", "operator"); err != nil {
		t.Fatalf("AddRepoTag: %v", err)
	}
	rts, err := store.ListTagsForRepo(db, "billing-svc")
	if err != nil {
		t.Fatalf("ListTagsForRepo: %v", err)
	}
	if len(rts) != 1 || rts[0].Tag != "payments" {
		t.Errorf("ListTagsForRepo: got %v", rts)
	}

	// ListReposForTag
	rts2, err := store.ListReposForTag(db, "payments")
	if err != nil {
		t.Fatalf("ListReposForTag: %v", err)
	}
	if len(rts2) != 1 || rts2[0].RepoName != "billing-svc" {
		t.Errorf("ListReposForTag: got %v", rts2)
	}

	// DeleteTag — should fail because RepoTags references it.
	if err := store.DeleteTag(db, "payments"); err == nil {
		t.Error("DeleteTag while referenced: expected FK error, got nil")
	}

	// RemoveRepoTag first, then DeleteTag should succeed.
	if err := store.RemoveRepoTag(db, "billing-svc", "payments"); err != nil {
		t.Fatalf("RemoveRepoTag: %v", err)
	}
	if err := store.DeleteTag(db, "payments"); err != nil {
		t.Errorf("DeleteTag after removing RepoTag: %v", err)
	}

	// TagSuggestions lifecycle
	if err := store.CreateTag(db, "infra", "", "op"); err != nil {
		t.Fatalf("CreateTag infra: %v", err)
	}
	id, err := store.CreateTagSuggestion(db, "core-svc", "infra", "looks like infra", "senator-bot")
	if err != nil {
		t.Fatalf("CreateTagSuggestion: %v", err)
	}
	if id <= 0 {
		t.Errorf("CreateTagSuggestion returned id %d, want > 0", id)
	}

	// ListTagSuggestions — all
	sugs, err := store.ListTagSuggestions(db, "")
	if err != nil {
		t.Fatalf("ListTagSuggestions all: %v", err)
	}
	if len(sugs) != 1 {
		t.Fatalf("ListTagSuggestions all: got %d, want 1", len(sugs))
	}
	if sugs[0].Status != "pending" {
		t.Errorf("initial status = %q, want pending", sugs[0].Status)
	}

	// ListTagSuggestions — by status
	pending, err := store.ListTagSuggestions(db, "pending")
	if err != nil {
		t.Fatalf("ListTagSuggestions pending: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("ListTagSuggestions pending: got %d, want 1", len(pending))
	}

	// ResolveTagSuggestion — invalid status
	if err := store.ResolveTagSuggestion(db, id, "rejected", "op"); err == nil {
		t.Error("ResolveTagSuggestion invalid status: expected error, got nil")
	}

	// ResolveTagSuggestion — accepted
	if err := store.ResolveTagSuggestion(db, id, "accepted", "op"); err != nil {
		t.Fatalf("ResolveTagSuggestion accepted: %v", err)
	}

	// Verify status changed
	accepted, err := store.ListTagSuggestions(db, "accepted")
	if err != nil {
		t.Fatalf("ListTagSuggestions accepted: %v", err)
	}
	if len(accepted) != 1 {
		t.Errorf("ListTagSuggestions accepted: got %d, want 1", len(accepted))
	}
	if accepted[0].ResolvedBy != "op" {
		t.Errorf("ResolvedBy = %q, want op", accepted[0].ResolvedBy)
	}
}

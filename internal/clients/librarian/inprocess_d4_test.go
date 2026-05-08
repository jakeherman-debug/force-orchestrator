package librarian

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestGetWeightedMemories_ScoreOrdering seeds three memories with
// distinct freshness/validation tuples and asserts the composite-
// score ordering returns the high-quality one first.
func TestGetWeightedMemories_ScoreOrdering(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.StoreFleetMemory(db, "repoA", 1, "success", "Memory A.", "a.go", "a")
	store.StoreFleetMemory(db, "repoA", 2, "success", "Memory B.", "b.go", "b")
	store.StoreFleetMemory(db, "repoA", 3, "success", "Memory C.", "c.go", "c")

	// Row 2 is the strongest (high validation, full freshness); row 1 is
	// fresh but un-validated; row 3 is fresh but penalised.
	if _, err := db.Exec(`UPDATE FleetMemory SET freshness_score = 1.0, validation_score = 0.0 WHERE id = 1`); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := db.Exec(`UPDATE FleetMemory SET freshness_score = 1.0, validation_score = 0.8 WHERE id = 2`); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if _, err := db.Exec(`UPDATE FleetMemory SET freshness_score = 1.0, validation_score = -0.5 WHERE id = 3`); err != nil {
		t.Fatalf("seed 3: %v", err)
	}

	c := NewInProcess(db)
	got, err := c.GetWeightedMemories(context.Background(), Scope{Repo: "repoA"}, 3)
	if err != nil {
		t.Fatalf("GetWeightedMemories: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 memories, got %d", len(got))
	}
	if got[0].ID != 2 {
		t.Errorf("expected highest-score memory (id=2) first, got %d", got[0].ID)
	}
	if got[2].ID != 3 {
		t.Errorf("expected lowest-score memory (id=3) last, got %d", got[2].ID)
	}
}

// TestGetWeightedMemories_ExcludesMerged asserts canonical_id != 0 rows
// are not returned.
func TestGetWeightedMemories_ExcludesMerged(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.StoreFleetMemory(db, "repoA", 1, "success", "Survivor.", "a.go", "a")
	store.StoreFleetMemory(db, "repoA", 2, "success", "Merged.", "a.go", "a")
	if _, err := db.Exec(`UPDATE FleetMemory SET canonical_id = 1 WHERE id = 2`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := NewInProcess(db)
	got, err := c.GetWeightedMemories(context.Background(), Scope{Repo: "repoA"}, 10)
	if err != nil {
		t.Fatalf("GetWeightedMemories: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 memory (merged excluded), got %d", len(got))
	}
	if got[0].ID != 1 {
		t.Errorf("expected survivor id=1, got %d", got[0].ID)
	}
}

// TestGetWeightedMemories_EmptyScope is the input-validation regression.
func TestGetWeightedMemories_EmptyScope(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)
	_, err := c.GetWeightedMemories(context.Background(), Scope{}, 5)
	if err != ErrEmptyScope {
		t.Errorf("expected ErrEmptyScope, got %v", err)
	}
}

// TestRecentCommitsDigest_StubRepo init's a temp git repo, creates two
// commits, registers it with the orchestrator DB, and calls the digest.
func TestRecentCommitsDigest_StubRepo(t *testing.T) {
	repoDir := t.TempDir()

	// Init real git repo.
	mustRunGit(t, repoDir, "init", "-q")
	mustRunGit(t, repoDir, "config", "user.email", "test@example.com")
	mustRunGit(t, repoDir, "config", "user.name", "Test")
	mustRunGit(t, repoDir, "config", "commit.gpgsign", "false")

	// Two commits.
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("alpha\n"), 0644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	mustRunGit(t, repoDir, "add", "a.txt")
	mustRunGit(t, repoDir, "commit", "-q", "-m", "Initial commit")

	if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("beta\n"), 0644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	mustRunGit(t, repoDir, "add", "b.txt")
	mustRunGit(t, repoDir, "commit", "-q", "-m", "Add beta file")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "stubrepo", repoDir, "test repo")

	c := NewInProcess(db)
	digest, err := c.RecentCommitsDigest(context.Background(), "stubrepo", 24*time.Hour)
	if err != nil {
		t.Fatalf("RecentCommitsDigest: %v", err)
	}
	if digest.Repo != "stubrepo" {
		t.Errorf("expected Repo=stubrepo, got %q", digest.Repo)
	}
	if len(digest.Commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(digest.Commits))
	}
	// Newest first — "Add beta file" must lead.
	if !strings.Contains(digest.Commits[0].Subject, "beta") {
		t.Errorf("expected newest commit subject to mention 'beta', got %q", digest.Commits[0].Subject)
	}
	if digest.Commits[0].SHA == "" {
		t.Errorf("expected SHA populated")
	}
}

// TestRecentCommitsDigest_MissingRepo asserts an unregistered repo
// returns an error rather than panicking.
func TestRecentCommitsDigest_MissingRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)
	_, err := c.RecentCommitsDigest(context.Background(), "nope", 24*time.Hour)
	if err == nil {
		t.Errorf("expected error for missing repo, got nil")
	}
}

// TestParseCommitsDigestOutput exercises the line-parser without
// shelling out.
func TestParseCommitsDigestOutput(t *testing.T) {
	const fieldSep = "\x1f"
	out := "abc123" + fieldSep + "Add x" + fieldSep + "alice" + fieldSep + "2024-01-02T03:04:05Z\n" +
		" 1 file changed, 2 insertions(+), 0 deletions(-)\n" +
		"\n" +
		"def456" + fieldSep + "Fix y" + fieldSep + "bob" + fieldSep + "2024-01-03T03:04:05Z\n" +
		" 3 files changed, 5 insertions(+), 2 deletions(-)\n"
	got := parseCommitsDigestOutput(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 commits parsed, got %d", len(got))
	}
	if got[0].SHA != "abc123" {
		t.Errorf("expected SHA abc123, got %q", got[0].SHA)
	}
	if !strings.Contains(got[0].Diffstat, "1 file changed") {
		t.Errorf("expected diffstat populated for first commit, got %q", got[0].Diffstat)
	}
	if got[1].Author != "bob" {
		t.Errorf("expected second author=bob, got %q", got[1].Author)
	}
}

// TestBootstrapSenatorRules_DeterministicStub ensures the stub fixture
// returns a sane shape under LIVE_HAIKU_DISABLED.
func TestBootstrapSenatorRules_DeterministicStub(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", "/nonexistent/path", "stub")
	c := NewInProcess(db)

	rules, err := c.BootstrapSenatorRules(context.Background(), "myrepo")
	if err != nil {
		t.Fatalf("BootstrapSenatorRules: %v", err)
	}
	if len(rules) < 1 {
		t.Fatalf("expected ≥1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Category != "senate" {
		t.Errorf("expected category=senate, got %q", r.Category)
	}
	if !strings.HasPrefix(r.AgentScope, "senate:") {
		t.Errorf("expected agent_scope prefixed senate:, got %q", r.AgentScope)
	}
	if r.RuleKey == "" || r.Body == "" || r.Rationale == "" {
		t.Errorf("expected RuleKey/Body/Rationale populated, got %+v", r)
	}
}

// TestBootstrapSenatorRules_RequiresRepo proves the input-validation
// regression.
func TestBootstrapSenatorRules_RequiresRepo(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)
	_, err := c.BootstrapSenatorRules(context.Background(), "")
	if err == nil {
		t.Errorf("expected error for empty repo, got nil")
	}
}

// TestRefreshSenatorMemoryDigest_DeterministicStub fires the digest
// path against an unreadable repo path; the digest should still come
// back (with empty commits) under LIVE_HAIKU_DISABLED.
func TestRefreshSenatorMemoryDigest_DeterministicStub(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", "/nonexistent/path", "stub")
	c := NewInProcess(db)

	digest, err := c.RefreshSenatorMemoryDigest(context.Background(), "myrepo")
	if err != nil {
		t.Fatalf("RefreshSenatorMemoryDigest: %v", err)
	}
	if digest.Repo != "myrepo" {
		t.Errorf("expected repo=myrepo, got %q", digest.Repo)
	}
	if digest.GeneratedAt == "" {
		t.Errorf("expected GeneratedAt populated")
	}
	if digest.APISurfaceSummary == "" {
		t.Errorf("expected APISurfaceSummary populated under deterministic stub")
	}
}

// mustRunGit runs git in dir and t.Fatalf's on failure.
func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (output: %s)", args, err, string(out))
	}
}

// TestParseBootstrapSenatorRulesResponse_SnakeCaseKeysUnmarshal pins
// the JSON-tag contract on CandidateRule. The
// bootstrapSenatorRulesSystemPrompt asks the LLM to emit snake_case
// keys (rule_key, agent_scope); the parser unmarshals into
// CandidateRule. Without explicit `json:"rule_key"` tags, Go's
// case-insensitive matcher does NOT cross underscores, so the
// snake_case keys would silently unmarshal to empty strings and the
// parser would reject every candidate as "missing rule_key or body".
//
// This is a regression test for the live-Haiku SenatorOnboarding bug
// observed against teamupstart/force-orchestrator on 2026-05-08.
func TestParseBootstrapSenatorRulesResponse_SnakeCaseKeysUnmarshal(t *testing.T) {
	// Mirrors exactly the shape requested by the system prompt.
	raw := `{
  "candidates": [
    {
      "rule_key":   "senate-myrepo-cap-profiles",
      "category":   "senate",
      "agent_scope": "senate:myrepo",
      "body":       "Every agent that shells out to the Claude CLI MUST source its tool restrictions from a static YAML capability profile.",
      "rationale":  "Capability profiles are the only audited path; ad-hoc flags drift.",
      "evidence":   "agents/capabilities/REGISTRY.yaml"
    }
  ]
}`
	candidates, err := parseBootstrapSenatorRulesResponse(raw, "myrepo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	if c.RuleKey != "senate-myrepo-cap-profiles" {
		t.Errorf("RuleKey: got %q, want %q (snake_case json tag missing on RuleKey?)", c.RuleKey, "senate-myrepo-cap-profiles")
	}
	if c.AgentScope != "senate:myrepo" {
		t.Errorf("AgentScope: got %q, want %q (snake_case json tag missing on AgentScope?)", c.AgentScope, "senate:myrepo")
	}
	if c.Category != "senate" {
		t.Errorf("Category: got %q, want senate", c.Category)
	}
	if !strings.HasPrefix(c.Body, "Every agent that shells out") {
		t.Errorf("Body: did not unmarshal correctly; got %q", c.Body)
	}
	if !strings.HasPrefix(c.Rationale, "Capability profiles") {
		t.Errorf("Rationale: did not unmarshal correctly; got %q", c.Rationale)
	}
	if c.Evidence != "agents/capabilities/REGISTRY.yaml" {
		t.Errorf("Evidence: got %q, want %q", c.Evidence, "agents/capabilities/REGISTRY.yaml")
	}
}

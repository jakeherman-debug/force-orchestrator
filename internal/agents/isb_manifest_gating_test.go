// Package agents — D5 Phase 0 manifest-gating dispatch test.
//
// Confirms the ISBReview task path correctly buckets rules into:
//   - always-fire (existing ISB-001..010 AST checks)
//   - manifest-gated (P1+ SUPPLY-001..005)
//
// Manifest-gated rules MUST only be invoked when the commit's diff
// includes a recognised manifest file. Source-only commits skip
// manifest-gated rules entirely (no AWS network calls, no deferral
// rows).
package agents

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/store"
)

// stubMGRule satisfies isb.ManifestGatedRule. The dispatcher routes
// here when the rule's ecosystems intersect the changed-manifest set.
type stubMGRule struct {
	id    string
	ecos  []manifests.Ecosystem
	calls *int32
}

func (s *stubMGRule) ID() string                           { return s.id }
func (s *stubMGRule) Ecosystems() []manifests.Ecosystem    { return s.ecos }
func (s *stubMGRule) Run(_ context.Context, _ *sql.DB, _ isb.ManifestGatedInput) ([]isb.Finding, error) {
	if s.calls != nil {
		atomic.AddInt32(s.calls, 1)
	}
	return nil, nil
}

// makeRepoCommit creates a small git repo with two commits — base
// commit on `main` followed by a feature-branch commit that touches
// the supplied files. Returns the repo path + the feature branch
// name.
func makeRepoCommit(t *testing.T, files map[string]string, mainFiles map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()

	mustGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	mustGit("init", "--initial-branch=main")
	mustGit("config", "user.email", "test@example.com")
	mustGit("config", "user.name", "test")
	mustGit("config", "commit.gpgsign", "false")

	// Base content.
	for rel, body := range mainFiles {
		full := filepath.Join(dir, rel)
		if err := writeFile(full, body); err != nil {
			t.Fatalf("write base %s: %v", rel, err)
		}
	}
	mustGit("add", "-A")
	mustGit("commit", "-m", "base")

	// Set origin/HEAD-equivalent so GetDefaultBranch resolves to main.
	mustGit("symbolic-ref", "HEAD", "refs/heads/main")

	mustGit("checkout", "-b", "feature/x")
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := writeFile(full, body); err != nil {
			t.Fatalf("write feature %s: %v", rel, err)
		}
	}
	mustGit("add", "-A")
	mustGit("commit", "-m", "feature work")

	return dir, "feature/x"
}

func writeFile(path, body string) error {
	if err := mkdirParent(path); err != nil {
		return err
	}
	return writeAll(path, []byte(body), 0o644)
}

func mkdirParent(path string) error {
	dir := filepath.Dir(path)
	return mkdirAll(dir, 0o755)
}

// Tiny stdlib wrappers so the test file's intent stays readable.
var (
	mkdirAll = osMkdirAll
	writeAll = osWriteFile
)

// TestISBManifestGating_SourceOnlyCommit_DoesNotFire — a commit
// touching only `.go` files must NOT invoke any manifest-gated rule.
func TestISBManifestGating_SourceOnlyCommit_DoesNotFire(t *testing.T) {
	isb.ResetManifestGatedForTest()
	defer isb.ResetManifestGatedForTest()

	var calls int32
	isb.RegisterManifestGated(&stubMGRule{
		id:    "TEST-MG",
		ecos:  []manifests.Ecosystem{manifests.EcosystemNPM, manifests.EcosystemPyPI, manifests.EcosystemRubyGems, manifests.EcosystemMaven, manifests.EcosystemGo},
		calls: &calls,
	})

	// Need a FleetRules row so the gate considers TEST-MG active.
	db, repoDir := setupISBManifestTestDB(t)
	seedTestRule(t, db, "TEST-MG")

	// Repo: source-only commit (no manifests).
	mainFiles := map[string]string{"README.md": "hello"}
	featureFiles := map[string]string{"foo.go": "package foo\nfunc F() {}\n"}
	overwriteRepo(t, repoDir, mainFiles, featureFiles)

	taskID := insertSourceTask(t, db, "demo")
	bountyID := queueISBReview(t, db, taskID, "demo", "feature/x", "deadbeef")

	// Run the reviewer.
	logger := &isbTestLogger{}
	bounty, err := store.GetBounty(db, bountyID)
	if err != nil || bounty == nil {
		t.Fatalf("GetBounty: %v", err)
	}
	runISBReviewTask(context.Background(), db, "isb-test", bounty, logger)

	if calls != 0 {
		t.Errorf("manifest-gated rule should NOT fire on source-only commit, got %d calls", calls)
	}
}

// TestISBManifestGating_ManifestCommit_FiresMatchingRules — a commit
// touching package.json must invoke the manifest-gated rule scoped
// to npm.
func TestISBManifestGating_ManifestCommit_FiresMatchingRules(t *testing.T) {
	isb.ResetManifestGatedForTest()
	defer isb.ResetManifestGatedForTest()

	var npmCalls, pypiCalls int32
	isb.RegisterManifestGated(&stubMGRule{
		id:    "TEST-MG-NPM",
		ecos:  []manifests.Ecosystem{manifests.EcosystemNPM},
		calls: &npmCalls,
	})
	isb.RegisterManifestGated(&stubMGRule{
		id:    "TEST-MG-PYPI",
		ecos:  []manifests.Ecosystem{manifests.EcosystemPyPI},
		calls: &pypiCalls,
	})

	db, repoDir := setupISBManifestTestDB(t)
	seedTestRule(t, db, "TEST-MG-NPM")
	seedTestRule(t, db, "TEST-MG-PYPI")

	// Repo: feature commit modifies package.json (npm) only.
	mainFiles := map[string]string{
		"package.json": `{"name":"app","dependencies":{"react":"18.0.0"}}`,
	}
	featureFiles := map[string]string{
		"package.json": `{"name":"app","dependencies":{"react":"18.0.0","lodash":"4.17.21"}}`,
	}
	overwriteRepo(t, repoDir, mainFiles, featureFiles)

	taskID := insertSourceTask(t, db, "demo")
	bountyID := queueISBReview(t, db, taskID, "demo", "feature/x", "deadbeef")

	logger := &isbTestLogger{}
	bounty, _ := store.GetBounty(db, bountyID)
	runISBReviewTask(context.Background(), db, "isb-test", bounty, logger)

	if npmCalls != 1 {
		t.Errorf("npm rule should have been invoked exactly once, got %d", npmCalls)
	}
	if pypiCalls != 0 {
		t.Errorf("pypi rule should NOT have been invoked when only package.json changed, got %d", pypiCalls)
	}
}

// ── helpers ────────────────────────────────────────────────────────────

func setupISBManifestTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	t.Cleanup(func() { db.Close() })
	if _, err := store.BootstrapFleetRules(context.Background(), db, ""); err != nil {
		t.Fatalf("BootstrapFleetRules: %v", err)
	}
	repoDir := t.TempDir()
	if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES (?, ?, 'write')`, "demo", repoDir); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return db, repoDir
}

// seedTestRule inserts a synthetic FleetRules row so the gate
// considers `ruleKey` active. Test-only — production rules go
// through fleet_rules_audit.go.
func seedTestRule(t *testing.T, db *sql.DB, ruleKey string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO FleetRules (rule_key, category, agent_scope, content, render_to, enforced_by, version, active_from, active_until)
		VALUES (?, 'isb', 'all', 'test rule', 'discard', 'test', 1, datetime('now'), '')`, ruleKey)
	if err != nil {
		t.Fatalf("seed rule %s: %v", ruleKey, err)
	}
}

func insertSourceTask(t *testing.T, db *sql.DB, repo string) int {
	t.Helper()
	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload) VALUES (0, ?, 'astromech', 'CommittedReadyForReview', 'src')`, repo)
	if err != nil {
		t.Fatalf("insert src task: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

func queueISBReview(t *testing.T, db *sql.DB, srcTaskID int, repo, branch, sha string) int {
	t.Helper()
	payload := `{"source_task_id":` + mgItoa(srcTaskID) + `,"branch":"` + branch + `","commit_sha":"` + sha + `","target_repo":"` + repo + `"}`
	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload) VALUES (?, ?, 'ISBReview', 'Pending', ?)`, srcTaskID, repo, payload)
	if err != nil {
		t.Fatalf("queue ISBReview: %v", err)
	}
	id, _ := res.LastInsertId()
	// The reviewer expects to claim a row; simplest path is to flag
	// it as already claimed by our test caller.
	if _, err := db.Exec(`UPDATE BountyBoard SET status='Claimed', owner='isb-test' WHERE id=?`, id); err != nil {
		t.Fatalf("claim ISBReview: %v", err)
	}
	return int(id)
}

func mgItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// overwriteRepo wipes repoDir and re-creates it with the supplied
// base+feature commits. Used by the manifest-gating tests since they
// share a single t.TempDir per setupISBManifestTestDB.
func overwriteRepo(t *testing.T, repoDir string, mainFiles, featureFiles map[string]string) {
	t.Helper()
	mustRun := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Clean if there's an existing .git
	_ = removeAll(filepath.Join(repoDir, ".git"))
	mustRun("init", "--initial-branch=main")
	mustRun("config", "user.email", "test@example.com")
	mustRun("config", "user.name", "test")
	mustRun("config", "commit.gpgsign", "false")

	for rel, body := range mainFiles {
		if err := writeFile(filepath.Join(repoDir, rel), body); err != nil {
			t.Fatalf("write base %s: %v", rel, err)
		}
	}
	mustRun("add", "-A")
	mustRun("commit", "-m", "base")

	mustRun("checkout", "-b", "feature/x")
	for rel, body := range featureFiles {
		if err := writeFile(filepath.Join(repoDir, rel), body); err != nil {
			t.Fatalf("write feature %s: %v", rel, err)
		}
	}
	mustRun("add", "-A")
	mustRun("commit", "-m", "feature")
}

var removeAll = osRemoveAll

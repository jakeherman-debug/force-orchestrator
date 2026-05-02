package agents

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"

	// Force-register the gemfile parser for branch-tip parsing.
	_ "force-orchestrator/internal/isb/scanners/manifests/gemfile"
)

// ── Test fixtures ────────────────────────────────────────────────────────

// stubCA implements codeartifact.Client. Health and the package-level
// methods are gated on per-call channels so tests can simulate
// success/failure transitions.
type stubCA struct {
	healthErr   error
	healthCalls int
	mu          sync.Mutex
}

func (s *stubCA) DescribePackageVersion(_ context.Context, _ codeartifact.Ecosystem, _, _ string) (codeartifact.PackageVersionInfo, error) {
	return codeartifact.PackageVersionInfo{}, nil
}
func (s *stubCA) ListPackages(_ context.Context, _ codeartifact.Ecosystem) ([]codeartifact.Package, error) {
	return nil, nil
}
func (s *stubCA) Health(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthCalls++
	return s.healthErr
}
func (s *stubCA) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthCalls
}
func (s *stubCA) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthErr = err
}

var _ codeartifact.Client = (*stubCA)(nil)

// stubReplayRule satisfies supplydeferral.ReplayableRule. By default
// returns no findings → resolved_late.
type stubReplayRule struct {
	id       string
	findings []supplydeferral.ReplayFinding
	called   int
	mu       sync.Mutex
}

func (r *stubReplayRule) ID() string { return r.id }

func (r *stubReplayRule) Run(_ context.Context, _ *sql.DB, _ supplydeferral.ReplayInput) ([]supplydeferral.ReplayFinding, error) {
	r.mu.Lock()
	r.called++
	r.mu.Unlock()
	out := make([]supplydeferral.ReplayFinding, len(r.findings))
	for i, f := range r.findings {
		if f.RuleID == "" {
			f.RuleID = r.id
		}
		out[i] = f
	}
	return out, nil
}

// notifyRecorder captures every notify-after invocation. Tests swap
// the package-level notifyAfterFn for r.fn.
type notifyRecorder struct {
	mu     sync.Mutex
	labels []string
}

func (r *notifyRecorder) fn(_ context.Context, label string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.labels = append(r.labels, label)
	return nil
}

func (r *notifyRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.labels))
	copy(out, r.labels)
	return out
}

// withNotifyStub replaces the package-level notifyAfterFn for the
// duration of a test, restoring the original on cleanup.
func withNotifyStub(t *testing.T, fn func(context.Context, string) error) {
	t.Helper()
	orig := notifyAfterFn
	notifyAfterFn = fn
	t.Cleanup(func() { notifyAfterFn = orig })
}

// withDeps registers SupplyRecheckDeps for the duration of a test.
// Tests that run runSupplyTokenRecheck directly (not via dispatch)
// don't need this; tests exercising RunDogByName / dispatch do.
func withDeps(t *testing.T, deps *SupplyRecheckDeps) {
	t.Helper()
	RegisterSupplyRecheckDeps(deps)
	t.Cleanup(func() { RegisterSupplyRecheckDeps(nil) })
}

// initRepoWithGemfile spawns a tempdir git repo with a Gemfile committed
// on `feature/x`. Skips the test when git is missing.
func initRepoWithGemfile(t *testing.T, gemfileContent string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	gitRun := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	gitRun("init", "-b", "main")
	gitRun("config", "user.email", "t@t")
	gitRun("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "initial")
	gitRun("checkout", "-b", "feature/x")
	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte(gemfileContent), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "add Gemfile")
	return dir
}

// ── Tests ────────────────────────────────────────────────────────────────

// TestSupplyTokenRecheck_HealthOK_ReplaysDeferrals — health probe
// succeeds; the replay walks pending deferrals and processes them.
func TestSupplyTokenRecheck_HealthOK_ReplaysDeferrals(t *testing.T) {
	repo := initRepoWithGemfile(t, "gem 'redis', '5.0.0'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed one deferral.
	id, err := supplydeferral.RecordDeferral(db, 100, supplydeferral.DeferralPayload{
		RuleKey: "SUPPLY-001", ManifestPath: "Gemfile", Branch: "feature/x", CommitSHA: "abc",
	})
	if err != nil {
		t.Fatalf("RecordDeferral: %v", err)
	}

	rule := &stubReplayRule{id: "SUPPLY-001"} // no findings → resolved_late
	ca := &stubCA{healthErr: nil}             // healthy

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	deps := &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{"SUPPLY-001": rule},
		RepoResolver: func(int) (string, error) { return repo, nil },
	}

	if err := runSupplyTokenRecheck(context.Background(), db, deps, testLogger{}); err != nil {
		t.Fatalf("runSupplyTokenRecheck: %v", err)
	}

	if ca.calls() != 1 {
		t.Errorf("Health calls: want 1, got %d", ca.calls())
	}
	if rule.called != 1 {
		t.Errorf("rule.called: want 1, got %d", rule.called)
	}

	// Original deferral should be resolved_late.
	var disp string
	if err := db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, id).Scan(&disp); err != nil {
		t.Fatal(err)
	}
	if disp != supplydeferral.DispositionResolvedLate {
		t.Errorf("disposition: want %s, got %q", supplydeferral.DispositionResolvedLate, disp)
	}

	// One per-branch ping should have been emitted.
	labels := rec.snapshot()
	if len(labels) != 1 {
		t.Fatalf("expected 1 notify-after invocation, got %d: %v", len(labels), labels)
	}
	if !strings.Contains(labels[0], "feature/x") || !strings.Contains(labels[0], "resolved_late") {
		t.Errorf("ping label missing branch/outcome: %q", labels[0])
	}
}

// TestSupplyTokenRecheck_TokenExpired_NotifiesOnce — first call fires
// the operator ping; subsequent calls during the same outage are silent.
func TestSupplyTokenRecheck_TokenExpired_NotifiesOnce(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ca := &stubCA{healthErr: codeartifact.ErrTokenExpired}

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	deps := &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{},
		RepoResolver: func(int) (string, error) { return "", nil },
	}

	// First tick → notify.
	if err := runSupplyTokenRecheck(context.Background(), db, deps, testLogger{}); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("first tick notify count: want 1, got %d", got)
	}

	// Second tick during same outage → debounced.
	if err := runSupplyTokenRecheck(context.Background(), db, deps, testLogger{}); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("second tick notify count: want 1 (debounced), got %d", got)
	}

	// Flag should be set.
	if !supplyTokenAlreadyNotified(db) {
		t.Errorf("notified flag should be set after first tick")
	}

	// Third tick still debounced.
	if err := runSupplyTokenRecheck(context.Background(), db, deps, testLogger{}); err != nil {
		t.Fatal(err)
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("third tick notify count: want 1, got %d", got)
	}
}

// TestSupplyTokenRecheck_HealthRecovered_ClearsNotified — after the
// notified flag is set, a successful Health probe clears it so the
// next expiry re-pings.
func TestSupplyTokenRecheck_HealthRecovered_ClearsNotified(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ca := &stubCA{healthErr: codeartifact.ErrTokenExpired}

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	deps := &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{},
		RepoResolver: func(int) (string, error) { return "", nil },
	}

	// 1. Token expires → ping + flag set.
	_ = runSupplyTokenRecheck(context.Background(), db, deps, testLogger{})
	if !supplyTokenAlreadyNotified(db) {
		t.Fatal("expected notified flag set after first expiry")
	}

	// 2. Token recovers → flag cleared.
	ca.setErr(nil)
	if err := runSupplyTokenRecheck(context.Background(), db, deps, testLogger{}); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if supplyTokenAlreadyNotified(db) {
		t.Errorf("notified flag should be cleared after recovery")
	}

	// 3. Token expires AGAIN → fresh ping (count = 2 now).
	ca.setErr(codeartifact.ErrTokenExpired)
	_ = runSupplyTokenRecheck(context.Background(), db, deps, testLogger{})
	labels := rec.snapshot()
	if len(labels) != 2 {
		t.Errorf("expected 2 pings (initial + re-expiry), got %d: %v", len(labels), labels)
	}
}

// TestSupplyTokenRecheck_RegisteredAtCorrectCadence — the dog is in
// dogOrder + dogCooldowns at 30 min, and TestListDogs sees it.
func TestSupplyTokenRecheck_RegisteredAtCorrectCadence(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dogs := ListDogs(db)
	var found *DogStatus
	for i := range dogs {
		if dogs[i].Name == "supply-token-recheck" {
			found = &dogs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("supply-token-recheck not in ListDogs output")
	}
	if found.Cooldown.Minutes() != 30 {
		t.Errorf("cooldown: want 30m, got %v", found.Cooldown)
	}

	// Belt-and-braces: verify it's in dogOrder so RunDogs picks it up.
	hit := false
	for _, name := range dogOrder {
		if name == "supply-token-recheck" {
			hit = true
			break
		}
	}
	if !hit {
		t.Errorf("supply-token-recheck missing from dogOrder")
	}
}

// TestSupplyTokenRecheck_OtherHealthError_PropagatesAsDogFailure —
// errors that aren't ErrTokenExpired surface as the dog's exit error.
func TestSupplyTokenRecheck_OtherHealthError_PropagatesAsDogFailure(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ca := &stubCA{healthErr: errors.New("network unreachable")}
	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	deps := &SupplyRecheckDeps{CA: ca, Rules: map[string]supplydeferral.ReplayableRule{}, RepoResolver: func(int) (string, error) { return "", nil }}
	err := runSupplyTokenRecheck(context.Background(), db, deps, testLogger{})
	if err == nil {
		t.Fatalf("expected non-ErrTokenExpired health error to surface")
	}
	if !strings.Contains(err.Error(), "health probe failed") {
		t.Errorf("error should mention 'health probe failed', got %v", err)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("non-token errors should not trigger notify-after, got %d pings", got)
	}
}

// TestSupplyTokenRecheck_NoDeps_LogsAndReturnsNil — when the daemon
// hasn't called RegisterSupplyRecheckDeps the dog logs and exits 0
// (no operator mail).
func TestSupplyTokenRecheck_NoDeps_LogsAndReturnsNil(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Ensure no deps registered.
	RegisterSupplyRecheckDeps(nil)
	t.Cleanup(func() { RegisterSupplyRecheckDeps(nil) })

	if err := dogSupplyTokenRecheck(context.Background(), db, testLogger{}); err != nil {
		t.Errorf("expected nil error when deps unregistered, got %v", err)
	}
}

// TestSupplyTokenRecheck_DispatchViaRunDogByName — RunDogByName
// reaches the dog through the standard dispatcher.
func TestSupplyTokenRecheck_DispatchViaRunDogByName(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ca := &stubCA{healthErr: nil}
	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	withDeps(t, &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{},
		RepoResolver: func(int) (string, error) { return "", nil },
	})

	// Use a mock librarian (the dog itself doesn't use it, but
	// RunDogByName threads the parameter through). nil ca because the
	// supply-token-recheck dog reads its codeartifact.Client from
	// SupplyRecheckDeps (registered via RegisterSupplyRecheckDeps), not
	// the RunDogByName ca arg.
	if err := RunDogByName(context.Background(), db, "supply-token-recheck", librarian.NewMock(), nil, testLogger{}); err != nil {
		t.Errorf("RunDogByName: %v", err)
	}
	if ca.calls() != 1 {
		t.Errorf("dispatch should reach the dog; Health calls=%d, want 1", ca.calls())
	}
}

// TestSupplyTokenRecheck_StillFlagged_PingEmbedsRule — when replay
// still flags a finding, the per-branch ping carries the rule + manifest
// in the label so the operator can act on it.
func TestSupplyTokenRecheck_StillFlagged_PingEmbedsRule(t *testing.T) {
	repo := initRepoWithGemfile(t, "gem 'evilpkg', '0.0.1'\n")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := supplydeferral.RecordDeferral(db, 200, supplydeferral.DeferralPayload{
		RuleKey: "SUPPLY-001", ManifestPath: "Gemfile", Branch: "feature/x", CommitSHA: "abc",
	}); err != nil {
		t.Fatalf("RecordDeferral: %v", err)
	}

	rule := &stubReplayRule{
		id: "SUPPLY-001",
		findings: []supplydeferral.ReplayFinding{
			{RuleID: "SUPPLY-001", Severity: "advise", Path: "Gemfile", Message: "evilpkg@0.0.1 hallucinated"},
		},
	}

	ca := &stubCA{healthErr: nil}
	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	deps := &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{"SUPPLY-001": rule},
		RepoResolver: func(int) (string, error) { return repo, nil },
	}

	if err := runSupplyTokenRecheck(context.Background(), db, deps, testLogger{}); err != nil {
		t.Fatal(err)
	}

	labels := rec.snapshot()
	if len(labels) != 1 {
		t.Fatalf("expected 1 ping, got %d", len(labels))
	}
	if !strings.Contains(labels[0], "still_flagged") || !strings.Contains(labels[0], "SUPPLY-001") || !strings.Contains(labels[0], "evilpkg") {
		t.Errorf("ping should embed outcome+rule+reason; got %q", labels[0])
	}
}

// TestDefaultRepoResolver_LooksUpRepoPath — production resolver maps
// task_id → BountyBoard.target_repo → Repositories.local_path.
func TestDefaultRepoResolver_LooksUpRepoPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a repo + a task pointing at it.
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")
	res, err := db.Exec(`INSERT INTO BountyBoard (target_repo, type, status) VALUES (?, 'X', 'Pending')`, "myrepo")
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := res.LastInsertId()

	resolver := DefaultRepoResolver(db)
	got, err := resolver(int(taskID))
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if got != "/tmp/myrepo" {
		t.Errorf("repo path: want /tmp/myrepo, got %q", got)
	}

	// Unknown task → ("", nil).
	got2, err := resolver(99999)
	if err != nil {
		t.Errorf("unknown task should not error, got %v", err)
	}
	if got2 != "" {
		t.Errorf("unknown task should return empty, got %q", got2)
	}
}

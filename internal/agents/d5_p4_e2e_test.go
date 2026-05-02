package agents

// D5 Phase 4 Slice δ — end-to-end fixture sweep for the SUPPLY-* deferral
// recovery loop.
//
// The α + β + γ slices each pin their own boundary in isolation; this
// sweep is the integration that walks them in sequence as one operator
// would experience the recovery flow:
//
//   1. Astromech-style: a real git commit on a real ask-branch lands a
//      manifest change (a new dep).
//   2. ISBReview-style: SUPPLY-001 fires against the ManifestGatedInput
//      built from that diff. CodeArtifact returns ErrTokenExpired →
//      supplydeferral.RecordDeferral writes a SecurityFindings row with
//      disposition='token_expired'. The astromech is NOT blocked.
//   3. Convoy reaches DraftPROpen. ConvoyReview runs → the γ gate fires
//      because the convoy's ask-branch carries a token_expired SUPPLY-*
//      finding. Convoy → AwaitingSupplyRecheck. The LLM pass is
//      short-circuited; no Claude call is spent.
//   4. Operator runs `umt artifacts`; the AWS token recovers (the test
//      flips the stub from ErrTokenExpired to nil).
//   5. supply-token-recheck dog (β) runs. Health probe passes.
//      ReplayPendingDeferrals walks the token_expired rows, replays the
//      registered SUPPLY-001 ReplayableRule against the branch tip's
//      manifest, and either flips the original row to resolved_late
//      (clean replay) or superseded + a new disposition='block' row
//      (still-flagged replay).
//   6. Convoy operator flips the convoy back to DraftPROpen. ConvoyReview
//      runs again → the gate sees zero remaining token_expired rows, so
//      it passes; the LLM pass actually runs this time.
//
// The six tests in this file each pin a distinct end-to-end shape:
//
//   - TestD5_E2E_RecoveryHappyPath: replay clears the deferral cleanly.
//   - TestD5_E2E_RecoveryWithStillFlagged: replay leaves a block row.
//   - TestD5_E2E_TokenNeverRecovers: debounce holds; convoy stays
//     gated; only one operator ping.
//   - TestD5_E2E_BranchDeleted_BeforeReplay: branch_gone path flips the
//     deferral; gate stops blocking.
//   - TestD5_E2E_AllowlistRefreshFeedsTyposquat: α + SUPPLY-002 chain.
//   - TestD5_E2E_DogsRegisteredCorrectly: both supply dogs in dogOrder
//     at the right cadences.
//
// Discipline:
//   - No t.Parallel — manifests.Default + the package-level deps
//     registry + the notifyAfterFn var are global singletons.
//   - Real SQLite via store.InitHolocronDSN(":memory:").
//   - Real git via tempdir + os/exec.
//   - All stubs cleaned up via t.Cleanup so a panic in one test doesn't
//     leak state into the next.
//   - No mocks of internal packages; only the codeartifact.Client
//     interface boundary + supplydeferral.ReplayableRule + the
//     notifyAfterFn package var.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb"
	"force-orchestrator/internal/isb/rules"
	"force-orchestrator/internal/isb/scanners/manifests"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"

	// Force-register every parser the e2e fixtures touch. Slice γ's tests
	// load gemfile only; the typosquat scenario also exercises npm.
	_ "force-orchestrator/internal/isb/scanners/manifests/gemfile"
	_ "force-orchestrator/internal/isb/scanners/manifests/npm"
)

// ── Local fixtures (e2e-scoped) ─────────────────────────────────────────────

// e2eRepoOnBranch initialises a tempdir git repo, commits a baseline
// README on `main`, then checks out `branch`. Returns the absolute path
// to the repo root. The branch starts at parity with main; callers
// commit the manifest change(s) afterwards via e2eCommitFile.
func e2eRepoOnBranch(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	gitRun := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	gitRun("init", "-b", "main")
	gitRun("config", "user.email", "t@t")
	gitRun("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "initial")
	gitRun("checkout", "-b", branch)
	return dir
}

// e2eCommitFile writes content into repoDir/path and commits with msg.
// Mirrors astromech's "Bash + git commit" tail in its post-step flow.
func e2eCommitFile(t *testing.T, repoDir, path, content, msg string) {
	t.Helper()
	full := filepath.Join(repoDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repoDir, "add", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s", out)
	}
	cmd = exec.Command("git", "-C", repoDir, "commit", "-m", msg)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s", out)
	}
}

// e2eDeleteBranch deletes a branch from repoDir's index. Used by the
// "branch deleted before replay" test to simulate a convoy's ask-branch
// being garbage-collected mid-flight.
func e2eDeleteBranch(t *testing.T, repoDir, branch string) {
	t.Helper()
	// Switch off the branch first (we can't delete a branch we're on).
	cmd := exec.Command("git", "-C", repoDir, "checkout", "main")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout main: %s", out)
	}
	cmd = exec.Command("git", "-C", repoDir, "branch", "-D", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch -D %s: %s", branch, out)
	}
}

// e2eToggleStub is the test seam for "AWS recovers mid-flight". It
// embeds the same per-call counters as stubCA but exposes a triple of
// errors (DescribePackageVersion, ListPackages, Health) the test can
// flip atomically. The underlying stubCA shape is reused for the Health
// probe (the dog reads .calls() to assert probe count); for the
// describe path we need a richer per-(name,version) handler so the test
// can program "redis@5.0.0 → 200" but "ghostpkg@0.0.0 → ErrPackageNotFound".
type e2eStubCA struct {
	// Health-probe error. ErrTokenExpired during the outage window;
	// nil after recovery.
	healthErr error
	healthN   int

	// Per-key handler for DescribePackageVersion. Falls back to
	// describeDefault when the (name, version) tuple is absent.
	describeHandlers map[string]func() (codeartifact.PackageVersionInfo, error)
	describeDefault  func() (codeartifact.PackageVersionInfo, error)
	describeN        int

	// ListPackages output (per-ecosystem) for the typosquat e2e scenario.
	listResults map[codeartifact.Ecosystem][]codeartifact.Package
	listErrs    map[codeartifact.Ecosystem]error
}

func newE2EStubCA() *e2eStubCA {
	return &e2eStubCA{
		describeHandlers: map[string]func() (codeartifact.PackageVersionInfo, error){},
		listResults:      map[codeartifact.Ecosystem][]codeartifact.Package{},
		listErrs:         map[codeartifact.Ecosystem]error{},
	}
}

func (s *e2eStubCA) DescribePackageVersion(_ context.Context, eco codeartifact.Ecosystem, name, version string) (codeartifact.PackageVersionInfo, error) {
	s.describeN++
	if s.healthErr != nil && errors.Is(s.healthErr, codeartifact.ErrTokenExpired) {
		return codeartifact.PackageVersionInfo{}, codeartifact.ErrTokenExpired
	}
	key := fmt.Sprintf("%s/%s@%s", eco, name, version)
	if h, ok := s.describeHandlers[key]; ok {
		return h()
	}
	if s.describeDefault != nil {
		return s.describeDefault()
	}
	return codeartifact.PackageVersionInfo{Ecosystem: eco, Name: name, Version: version, Status: "Published"}, nil
}

func (s *e2eStubCA) ListPackages(_ context.Context, eco codeartifact.Ecosystem) ([]codeartifact.Package, error) {
	if err, ok := s.listErrs[eco]; ok {
		return nil, err
	}
	return s.listResults[eco], nil
}

func (s *e2eStubCA) Health(_ context.Context) error {
	s.healthN++
	return s.healthErr
}

// onDescribe registers a (eco, name, version) handler. Returns the
// stub for chaining.
func (s *e2eStubCA) onDescribe(eco codeartifact.Ecosystem, name, version string, fn func() (codeartifact.PackageVersionInfo, error)) {
	s.describeHandlers[fmt.Sprintf("%s/%s@%s", eco, name, version)] = fn
}

var _ codeartifact.Client = (*e2eStubCA)(nil)

// replayAdapter wraps an isb.ManifestGatedRule (e.g. the *supply001
// returned by rules.NewSUPPLY001) into the supplydeferral.ReplayableRule
// shape. The dog/gate consume the latter; this is the wiring the
// daemon owns at startup. Keeping it test-local in the e2e file
// matches the "wire test-locally" directive — slice δ deliberately
// scopes daemon-side adapter construction out (see roadmap.md
// §"daemon-side wiring (where the codeartifact.Client is constructed
// and injected) lands later").
type replayAdapter struct {
	mgrule isb.ManifestGatedRule
}

func newReplayAdapter(r isb.ManifestGatedRule) *replayAdapter {
	return &replayAdapter{mgrule: r}
}

func (a *replayAdapter) ID() string { return a.mgrule.ID() }

// Run translates ReplayInput → isb.ManifestGatedInput and the rule's
// Findings → ReplayFindings. This is the production daemon's job; we
// inline it here for the e2e because slice δ deliberately scopes
// daemon-side adapter wiring out (see roadmap.md §"daemon-side wiring
// (where the codeartifact.Client is constructed and injected) lands
// later").
func (a *replayAdapter) Run(ctx context.Context, db *sql.DB, in supplydeferral.ReplayInput) ([]supplydeferral.ReplayFinding, error) {
	mgIn := isb.ManifestGatedInput{
		SourceTaskID: in.SourceTaskID,
		TargetRepo:   in.TargetRepo,
		Branch:       in.Branch,
		CommitSHA:    in.CommitSHA,
	}
	for _, cm := range in.ChangedManifests {
		mgIn.ChangedManifests = append(mgIn.ChangedManifests, isb.ChangedManifest{
			Path:       cm.Path,
			Ecosystem:  cm.Ecosystem,
			DepsAdded:  cm.DepsAdded,
			AfterBytes: cm.After,
		})
	}
	findings, err := a.mgrule.Run(ctx, db, mgIn)
	if err != nil {
		return nil, err
	}
	out := make([]supplydeferral.ReplayFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, supplydeferral.ReplayFinding{
			RuleID:   f.RuleID,
			Severity: string(f.Severity),
			Path:     f.Path,
			Line:     f.Line,
			Message:  f.Message,
		})
	}
	return out, nil
}

var _ supplydeferral.ReplayableRule = (*replayAdapter)(nil)

// e2eBuildManifestGatedInput constructs the ManifestGatedInput that
// ISBReview would synthesise for a freshly-committed Gemfile change on
// branch. The fixture commits a manifest with a single dep and we
// reuse the parser's output as DepsAdded (the parser is already loaded
// via the gemfile init() blank import).
func e2eBuildManifestGatedInput(t *testing.T, repoDir, branch, manifestPath string) isb.ManifestGatedInput {
	t.Helper()
	parser, ok := manifests.Default().Detect(manifestPath)
	if !ok {
		t.Fatalf("no parser registered for %s", manifestPath)
	}
	content, err := os.ReadFile(filepath.Join(repoDir, manifestPath))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	deps, err := parser.Parse(manifestPath, content)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	eco, _ := manifests.Default().EcosystemFor(manifestPath)
	// Branch tip SHA — best-effort; the deferral payload accepts "" so
	// failure is non-fatal here.
	cmd := exec.Command("git", "-C", repoDir, "rev-parse", branch)
	out, _ := cmd.Output()
	sha := strings.TrimSpace(string(out))
	return isb.ManifestGatedInput{
		SourceTaskID: 1,
		TargetRepo:   "api",
		Branch:       branch,
		CommitSHA:    sha,
		ChangedManifests: []isb.ChangedManifest{
			{
				Path:       manifestPath,
				Ecosystem:  eco,
				DepsAdded:  deps,
				AfterBytes: content,
			},
		},
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────

// TestD5_E2E_RecoveryHappyPath walks the canonical recovery flow:
// commit lands → SUPPLY-001 defers (token expired) → gate blocks convoy
// → token recovers → dog replays cleanly → original deferral flips to
// resolved_late → second ConvoyReview pass with status restored sees
// no remaining deferrals → gate passes → LLM runs.
func TestD5_E2E_RecoveryHappyPath(t *testing.T) {
	const branch = "force/ask-1-test"

	// 1. Real git repo + ask-branch + manifest commit.
	repoDir := e2eRepoOnBranch(t, branch)
	gemfile := "gem 'redis', '5.0.0'\n"
	e2eCommitFile(t, repoDir, "Gemfile", gemfile, "add redis dep")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// 2. Wire the convoy + ask-branch row → repo path.
	convoyID := seedDraftPROpenConvoy(t, db)
	// Override the seed's /tmp/api path with our real tempdir.
	store.AddRepo(db, "api", repoDir, "")

	// 3. Stub CodeArtifact: token expired → describe + health both
	// return ErrTokenExpired.
	stub := newE2EStubCA()
	stub.healthErr = codeartifact.ErrTokenExpired

	// 4. SUPPLY-001 rule bound to the stub. When token recovers we'll
	// reuse the SAME rule instance (its 24h positive-cache survives the
	// recovery; intentional — production has the same property).
	rule := rules.NewSUPPLY001(stub)

	// 5. Initial scan: SUPPLY-001 fires against the freshly-committed
	// manifest. Token expired → deferral row written, no findings.
	mgIn := e2eBuildManifestGatedInput(t, repoDir, branch, "Gemfile")
	findings, err := rule.Run(context.Background(), db, mgIn)
	if err != nil {
		t.Fatalf("SUPPLY-001 Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("SUPPLY-001 should defer (no findings); got %d: %+v", len(findings), findings)
	}
	// Exactly one disposition='token_expired' SecurityFindings row.
	pending, err := supplydeferral.ListPendingDeferrals(db, branch)
	if err != nil {
		t.Fatalf("ListPendingDeferrals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 deferred finding, got %d", len(pending))
	}
	if pending[0].Payload.RuleKey != "SUPPLY-001" {
		t.Errorf("deferral rule_key: want SUPPLY-001, got %q", pending[0].Payload.RuleKey)
	}
	deferralID := pending[0].FindingID

	// 6. Wire RegisterSupplyRecheckDeps with the same stub + adapter.
	withDeps(t, &SupplyRecheckDeps{
		CA: stub,
		Rules: map[string]supplydeferral.ReplayableRule{
			"SUPPLY-001": newReplayAdapter(rule),
		},
		RepoResolver: func(int) (string, error) { return repoDir, nil },
	})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	// 7. ConvoyReview pass #1: token still expired → gate blocks.
	llmStub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	bounty1 := seedConvoyReviewBounty(t, db, 9001, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty1,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	if got := llmStub.CallCount(); got != 0 {
		t.Errorf("pass #1 LLM CallCount: want 0 (gate blocked), got %d", got)
	}
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != ConvoyStatusAwaitingSupplyRecheck {
		t.Fatalf("pass #1 convoy status: want AwaitingSupplyRecheck, got %s", convoy.Status)
	}

	// 8. Token recovers — operator ran umt artifacts.
	stub.healthErr = nil
	// Configure the stub so the dep lookup succeeds when the dog
	// replays SUPPLY-001 against the branch tip.
	stub.onDescribe(codeartifact.EcosystemRubyGems, "redis", "5.0.0",
		func() (codeartifact.PackageVersionInfo, error) {
			return codeartifact.PackageVersionInfo{Name: "redis", Version: "5.0.0", Status: "Published"}, nil
		})

	// 9. supply-token-recheck dog runs. Health probe passes →
	// ReplayPendingDeferrals walks the row, replays SUPPLY-001 (which
	// now sees a clean dep), flips the row to resolved_late.
	if err := dogSupplyTokenRecheck(context.Background(), db, testLogger{}); err != nil {
		t.Fatalf("dogSupplyTokenRecheck: %v", err)
	}
	var disp string
	if err := db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, deferralID).Scan(&disp); err != nil {
		t.Fatalf("read deferral disposition: %v", err)
	}
	if disp != supplydeferral.DispositionResolvedLate {
		t.Errorf("post-replay deferral disposition: want %s, got %q",
			supplydeferral.DispositionResolvedLate, disp)
	}
	// No new SecurityFindings rows beyond the original token_expired (now resolved_late).
	var totalRows int
	db.QueryRow(`SELECT COUNT(*) FROM SecurityFindings WHERE rule_id = 'SUPPLY-001'`).Scan(&totalRows)
	if totalRows != 1 {
		t.Errorf("expected 1 SUPPLY-001 row total post-replay, got %d", totalRows)
	}

	// 10. Operator restores convoy status to DraftPROpen (the standard
	// ship-it-handoff transition once the gate clears).
	if err := store.SetConvoyStatus(db, convoyID, "DraftPROpen"); err != nil {
		t.Fatalf("SetConvoyStatus DraftPROpen: %v", err)
	}

	// 11. ConvoyReview pass #2: zero remaining token_expired rows →
	// gate passes → LLM runs.
	bounty2 := seedConvoyReviewBounty(t, db, 9002, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty2,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	if got := llmStub.CallCount(); got != 1 {
		t.Errorf("pass #2 LLM CallCount: want 1 (gate passes), got %d", got)
	}
	convoy = store.GetConvoy(db, convoyID)
	if convoy.Status == ConvoyStatusAwaitingSupplyRecheck {
		t.Errorf("pass #2 convoy status: still AwaitingSupplyRecheck after recovery")
	}
}

// TestD5_E2E_RecoveryWithStillFlagged walks the still_flagged variant:
// the replay against the branch tip discovers a hallucinated package
// (CodeArtifact returns 404 for the dep). The deferral row flips to
// 'superseded'; a fresh disposition='' (open) block row is inserted.
// The gate's recount finds zero remaining token_expired rows, so the
// gate PASSES — the new block row is the standard ISB block-eval path's
// concern, not the gate's.
func TestD5_E2E_RecoveryWithStillFlagged(t *testing.T) {
	const branch = "force/ask-1-test"

	repoDir := e2eRepoOnBranch(t, branch)
	gemfile := "gem 'evilpkg', '0.0.1'\n"
	e2eCommitFile(t, repoDir, "Gemfile", gemfile, "add evilpkg")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	store.AddRepo(db, "api", repoDir, "")

	stub := newE2EStubCA()
	stub.healthErr = codeartifact.ErrTokenExpired

	rule := rules.NewSUPPLY001(stub)

	// Initial deferral.
	mgIn := e2eBuildManifestGatedInput(t, repoDir, branch, "Gemfile")
	if _, err := rule.Run(context.Background(), db, mgIn); err != nil {
		t.Fatalf("initial SUPPLY-001 Run: %v", err)
	}
	pending, _ := supplydeferral.ListPendingDeferrals(db, branch)
	if len(pending) != 1 {
		t.Fatalf("expected 1 deferral, got %d", len(pending))
	}
	deferralID := pending[0].FindingID

	withDeps(t, &SupplyRecheckDeps{
		CA: stub,
		Rules: map[string]supplydeferral.ReplayableRule{
			"SUPPLY-001": newReplayAdapter(rule),
		},
		RepoResolver: func(int) (string, error) { return repoDir, nil },
	})
	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	llmStub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	// Pass #1 — gate blocks on token expired.
	bounty1 := seedConvoyReviewBounty(t, db, 9101, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty1,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})
	if got := llmStub.CallCount(); got != 0 {
		t.Errorf("pass #1 LLM count: want 0, got %d", got)
	}
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != ConvoyStatusAwaitingSupplyRecheck {
		t.Fatalf("pass #1 status: want AwaitingSupplyRecheck, got %s", convoy.Status)
	}

	// Token recovers, but this time the dep is genuinely hallucinated —
	// CodeArtifact 404s on it. NOTE: the SUPPLY-001 rule caches positive
	// hits but never caches 404s (anti-cheat), so flipping describe
	// behavior here is honoured.
	stub.healthErr = nil
	stub.onDescribe(codeartifact.EcosystemRubyGems, "evilpkg", "0.0.1",
		func() (codeartifact.PackageVersionInfo, error) {
			return codeartifact.PackageVersionInfo{}, codeartifact.ErrPackageNotFound
		})

	// Dog runs → replay sees the rule fire → original row → superseded;
	// new disposition='' block row inserted.
	if err := dogSupplyTokenRecheck(context.Background(), db, testLogger{}); err != nil {
		t.Fatalf("dogSupplyTokenRecheck: %v", err)
	}
	var disp string
	db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, deferralID).Scan(&disp)
	if disp != supplydeferral.DispositionSuperseded {
		t.Errorf("original deferral disposition: want %s, got %q",
			supplydeferral.DispositionSuperseded, disp)
	}
	// Fresh open block row exists.
	var blockCount int
	db.QueryRow(`SELECT COUNT(*) FROM SecurityFindings
		WHERE rule_id = 'SUPPLY-001' AND severity = 'block'
		  AND IFNULL(disposition, '') = ''`).Scan(&blockCount)
	if blockCount != 1 {
		t.Errorf("expected 1 fresh open block row, got %d", blockCount)
	}

	// Operator restores DraftPROpen.
	if err := store.SetConvoyStatus(db, convoyID, "DraftPROpen"); err != nil {
		t.Fatalf("SetConvoyStatus: %v", err)
	}

	// Pass #2 — gate passes (no remaining token_expired rows). The new
	// block row is for the standard ISB block-eval path; the gate
	// neither reads it nor blocks on it.
	bounty2 := seedConvoyReviewBounty(t, db, 9102, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty2,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})
	if got := llmStub.CallCount(); got != 1 {
		t.Errorf("pass #2 LLM count: want 1 (gate passes; ISB block flows separately), got %d", got)
	}
	convoy = store.GetConvoy(db, convoyID)
	if convoy.Status == ConvoyStatusAwaitingSupplyRecheck {
		t.Errorf("pass #2 should NOT be AwaitingSupplyRecheck; got %s", convoy.Status)
	}
}

// TestD5_E2E_TokenNeverRecovers — the operator never runs umt
// artifacts. Multiple dog ticks fire under the same outage; the
// debounced notify-after path emits exactly one ping. The convoy stays
// gated. Deferrals stay token_expired.
func TestD5_E2E_TokenNeverRecovers(t *testing.T) {
	const branch = "force/ask-1-test"

	repoDir := e2eRepoOnBranch(t, branch)
	e2eCommitFile(t, repoDir, "Gemfile", "gem 'redis', '5.0.0'\n", "add redis")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	store.AddRepo(db, "api", repoDir, "")

	stub := newE2EStubCA()
	stub.healthErr = codeartifact.ErrTokenExpired
	rule := rules.NewSUPPLY001(stub)

	mgIn := e2eBuildManifestGatedInput(t, repoDir, branch, "Gemfile")
	if _, err := rule.Run(context.Background(), db, mgIn); err != nil {
		t.Fatalf("SUPPLY-001 Run: %v", err)
	}
	pending, _ := supplydeferral.ListPendingDeferrals(db, branch)
	if len(pending) != 1 {
		t.Fatalf("expected 1 deferral, got %d", len(pending))
	}
	deferralID := pending[0].FindingID

	withDeps(t, &SupplyRecheckDeps{
		CA: stub,
		Rules: map[string]supplydeferral.ReplayableRule{
			"SUPPLY-001": newReplayAdapter(rule),
		},
		RepoResolver: func(int) (string, error) { return repoDir, nil },
	})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	// Three dog ticks under the same outage. First fires the operator
	// ping; second + third are debounced.
	for i := 0; i < 3; i++ {
		if err := dogSupplyTokenRecheck(context.Background(), db, testLogger{}); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	pings := rec.snapshot()
	if len(pings) != 1 {
		t.Errorf("expected 1 debounced ping over 3 token-expired ticks, got %d: %v", len(pings), pings)
	}
	// Deferral still token_expired.
	var disp string
	db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, deferralID).Scan(&disp)
	if disp != supplydeferral.DeferralDisposition {
		t.Errorf("deferral disposition: want %s (still pending), got %q",
			supplydeferral.DeferralDisposition, disp)
	}

	// ConvoyReview still gates on the unresolved deferral.
	llmStub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})
	bounty := seedConvoyReviewBounty(t, db, 9201, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})
	if got := llmStub.CallCount(); got != 0 {
		t.Errorf("LLM count: want 0 (still gated), got %d", got)
	}
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != ConvoyStatusAwaitingSupplyRecheck {
		t.Errorf("convoy status: want AwaitingSupplyRecheck, got %s", convoy.Status)
	}
}

// TestD5_E2E_BranchDeleted_BeforeReplay — token recovers, but the
// convoy's ask-branch was garbage-collected (rebased away) before the
// dog could run. ReplayPendingDeferrals detects the missing branch
// (rev-parse fails) and flips the original row to 'branch_gone'. The
// convoy is no longer blocked because the deferral is no longer
// token_expired.
func TestD5_E2E_BranchDeleted_BeforeReplay(t *testing.T) {
	const branch = "force/ask-1-test"

	repoDir := e2eRepoOnBranch(t, branch)
	e2eCommitFile(t, repoDir, "Gemfile", "gem 'redis', '5.0.0'\n", "add redis")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	store.AddRepo(db, "api", repoDir, "")

	stub := newE2EStubCA()
	stub.healthErr = codeartifact.ErrTokenExpired
	rule := rules.NewSUPPLY001(stub)

	mgIn := e2eBuildManifestGatedInput(t, repoDir, branch, "Gemfile")
	if _, err := rule.Run(context.Background(), db, mgIn); err != nil {
		t.Fatalf("SUPPLY-001 Run: %v", err)
	}
	pending, _ := supplydeferral.ListPendingDeferrals(db, branch)
	if len(pending) != 1 {
		t.Fatalf("expected 1 deferral, got %d", len(pending))
	}
	deferralID := pending[0].FindingID

	// Branch gets deleted (e.g. convoy abandoned, ask-branch GC'd).
	e2eDeleteBranch(t, repoDir, branch)

	withDeps(t, &SupplyRecheckDeps{
		CA: stub,
		Rules: map[string]supplydeferral.ReplayableRule{
			"SUPPLY-001": newReplayAdapter(rule),
		},
		RepoResolver: func(int) (string, error) { return repoDir, nil },
	})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	// Token recovers; dog runs.
	stub.healthErr = nil
	if err := dogSupplyTokenRecheck(context.Background(), db, testLogger{}); err != nil {
		t.Fatalf("dogSupplyTokenRecheck: %v", err)
	}
	var disp string
	db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, deferralID).Scan(&disp)
	if disp != supplydeferral.DispositionBranchGone {
		t.Errorf("disposition: want %s, got %q", supplydeferral.DispositionBranchGone, disp)
	}

	// Convoy now passes the gate — the deferral is no longer in
	// token_expired state. (The convoy itself is in DraftPROpen still
	// because nobody ever flipped it to AwaitingSupplyRecheck — the
	// gate only ran once recovery occurred + branch was already gone.)
	llmStub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})
	bounty := seedConvoyReviewBounty(t, db, 9301, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})
	if got := llmStub.CallCount(); got != 1 {
		t.Errorf("LLM count: want 1 (no remaining deferrals → gate passes), got %d", got)
	}
}

// TestD5_E2E_AllowlistRefreshFeedsTyposquat — verifies α + SUPPLY-002
// chain end-to-end: the supply-allowlist-refresh dog (α) populates
// SystemConfig.supply_allowlist_npm; SUPPLY-002 then uses that allowlist
// to detect a typo of "express" ("expres", distance 1).
func TestD5_E2E_AllowlistRefreshFeedsTyposquat(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Initial state: empty allowlist (no SystemConfig row).
	if got := store.GetConfig(db, "supply_allowlist_npm", ""); got != "" {
		t.Fatalf("precondition: npm allowlist should be empty, got %q", got)
	}

	// Stub CA with package list for npm + benign empties for others.
	stub := newE2EStubCA()
	stub.listResults[codeartifact.EcosystemNPM] = []codeartifact.Package{
		{Ecosystem: codeartifact.EcosystemNPM, Name: "express"},
		{Ecosystem: codeartifact.EcosystemNPM, Name: "lodash"},
	}
	// Other ecosystems: provide some content so the dog can complete
	// without errors. Empty slices are fine — the dog still writes the
	// (empty) allowlist row + last-refresh stamp.
	stub.listResults[codeartifact.EcosystemPyPI] = nil
	stub.listResults[codeartifact.EcosystemRubyGems] = nil
	stub.listResults[codeartifact.EcosystemMaven] = nil

	if err := dogSupplyAllowlistRefresh(context.Background(), db, stub, testLogger{}); err != nil {
		t.Fatalf("dogSupplyAllowlistRefresh: %v", err)
	}

	allow := store.GetConfig(db, "supply_allowlist_npm", "")
	if !strings.Contains(allow, "express") || !strings.Contains(allow, "lodash") {
		t.Errorf("npm allowlist after refresh: want express+lodash, got %q", allow)
	}

	// Now run SUPPLY-002 against a manifest introducing "expres" (typo).
	rule002 := rules.NewSUPPLY002()
	in := isb.ManifestGatedInput{
		SourceTaskID: 1,
		TargetRepo:   "api",
		Branch:       "force/ask-typo",
		CommitSHA:    "deadbeef",
		ChangedManifests: []isb.ChangedManifest{
			{
				Path:      "package.json",
				Ecosystem: manifests.EcosystemNPM,
				DepsAdded: []manifests.Dependency{
					{Ecosystem: manifests.EcosystemNPM, Name: "expres", Version: "4.0.0", Source: manifests.SourceDirect},
				},
			},
		},
	}
	findings, err := rule002.Run(context.Background(), db, in)
	if err != nil {
		t.Fatalf("SUPPLY-002 Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 typosquat finding, got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "express") {
		t.Errorf("finding should mention closest=express; got %q", findings[0].Message)
	}
	if findings[0].RuleID != "SUPPLY-002" {
		t.Errorf("rule id: want SUPPLY-002, got %s", findings[0].RuleID)
	}

	// Belt-and-braces: the last_refresh stamp parses cleanly.
	last := store.GetConfig(db, "supply_allowlist_npm_last_refresh", "")
	if last == "" {
		t.Errorf("supply_allowlist_npm_last_refresh not stamped")
	} else if parsed, perr := store.ParseSQLiteTime(last); perr != nil {
		t.Errorf("last_refresh parse: %v", perr)
	} else if time.Since(parsed) > time.Minute {
		t.Errorf("last_refresh too far in the past: %s", last)
	}
}

// TestD5_E2E_DogsRegisteredCorrectly — the daemon's RunDogs (via
// ListDogs) sees both supply dogs at the right cadences. This is the
// integration test for the registry wiring: a future PR that adds a
// new supply dog must update both dogOrder and dogCooldowns.
func TestD5_E2E_DogsRegisteredCorrectly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dogs := ListDogs(db)
	cadence := map[string]time.Duration{}
	for _, d := range dogs {
		cadence[d.Name] = d.Cooldown
	}

	if got, ok := cadence["supply-allowlist-refresh"]; !ok {
		t.Errorf("supply-allowlist-refresh missing from ListDogs")
	} else if got != 24*time.Hour {
		t.Errorf("supply-allowlist-refresh cadence: want 24h, got %v", got)
	}

	if got, ok := cadence["supply-token-recheck"]; !ok {
		t.Errorf("supply-token-recheck missing from ListDogs")
	} else if got != 30*time.Minute {
		t.Errorf("supply-token-recheck cadence: want 30m, got %v", got)
	}

	// Belt-and-braces: dogOrder also carries them. ListDogs is built
	// from dogOrder so this is mostly a structural assertion.
	want := map[string]bool{"supply-allowlist-refresh": true, "supply-token-recheck": true}
	for _, name := range dogOrder {
		delete(want, name)
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for k := range want {
			missing = append(missing, k)
		}
		t.Errorf("dogOrder missing entries: %v", missing)
	}
}


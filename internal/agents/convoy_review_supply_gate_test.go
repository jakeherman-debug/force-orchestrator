package agents

// D5 P4 γ — ConvoyReview AwaitingSupplyRecheck gate tests.
//
// These tests pin the gate's six invariants (see roadmap.md §
// "Layer 2 — ConvoyReview gate"):
//
//   1. No deferrals → gate passes; ConvoyReview proceeds normally.
//   2. Deferrals exist + CA token still expired → gate blocks; convoy
//      stamped AwaitingSupplyRecheck; reason includes the count.
//   3. Deferrals exist + CA recovered + replay resolves all → gate
//      passes; ConvoyReview proceeds (and any new block findings are
//      handled by the standard ISB path).
//   4. Deferrals exist + CA recovered + replay leaves still_flagged
//      blocks → gate passes (the new block rows aren't deferrals; they
//      flow through ConvoyReview's regular finding stream).
//   5. Replay deps unwired (deps == nil) → gate falls back to read-only
//      block; convoy stamped AwaitingSupplyRecheck without a CA call.
//   6. Status persistence — once stamped AwaitingSupplyRecheck, the
//      ship-it surface refuses to advance the convoy.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/isb/supplydeferral"
	"force-orchestrator/internal/store"

	// Force-register the gemfile parser so the inline replay path can
	// read manifests at the branch tip.
	_ "force-orchestrator/internal/isb/scanners/manifests/gemfile"
)

// ── Test fixtures (gate-specific) ─────────────────────────────────────────

// initRepoWithGemfileForGate spawns a tempdir git repo with a Gemfile
// committed on `branch`. Mirrors the supply-token-recheck dog test
// fixture, but parameterised on branch name so the gate tests can use
// the seedDraftPROpenConvoy default ("force/ask-1-test").
func initRepoWithGemfileForGate(t *testing.T, branch, gemfileContent string) string {
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
	gitRun("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte(gemfileContent), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "add Gemfile")
	return dir
}

// seedSupplyDeferral inserts one disposition='token_expired' SUPPLY-*
// finding scoped to (branch, manifestPath). Returns the new finding id.
func seedSupplyDeferral(t *testing.T, db *sql.DB, taskID int, branch, manifestPath string) int {
	t.Helper()
	id, err := supplydeferral.RecordDeferral(db, taskID, supplydeferral.DeferralPayload{
		RuleKey:      "SUPPLY-001",
		ManifestPath: manifestPath,
		Branch:       branch,
		CommitSHA:    "abc",
	})
	if err != nil {
		t.Fatalf("RecordDeferral: %v", err)
	}
	if id == 0 {
		t.Fatalf("RecordDeferral returned 0 — possible dedup hit")
	}
	return id
}

// seedConvoyReviewBounty mirrors the inline boilerplate the existing
// convoy-review tests use: insert a Locked ConvoyReview row with the
// payload pointing at convoyID. Returns the bounty struct ready for
// runConvoyReview.
func seedConvoyReviewBounty(t *testing.T, db *sql.DB, bountyID, convoyID int) *store.Bounty {
	t.Helper()
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: bountyID, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (?, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`,
		bountyID, string(payload), convoyID)
	return bounty
}

// ── Tests ─────────────────────────────────────────────────────────────────

// TestConvoyReview_NoDeferrals_Proceeds — no token_expired rows on any
// ask-branch in the convoy. Gate passes; ConvoyReview runs the LLM
// path and completes normally.
func TestConvoyReview_NoDeferrals_Proceeds(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// Seed a CodeEdit so summarizeConvoyTasks has something.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Completed', 'add rate limit patterns', ?, 5, datetime('now'))`, convoyID)

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	// Deps registered with healthy CA — exercises the no-deferrals
	// fast path: countConvoyDeferrals returns 0, gate skips the Health
	// probe, ConvoyReview proceeds.
	ca := &stubCA{healthErr: nil}
	withDeps(t, &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{},
		RepoResolver: func(int) (string, error) { return "", nil },
	})

	bounty := seedConvoyReviewBounty(t, db, 7001, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// LLM was called — gate let the pass through.
	if got := stub.CallCount(); got != 1 {
		t.Errorf("LLM CallCount: want 1, got %d (gate may have wrongly blocked)", got)
	}
	// Health probe was NOT called: countConvoyDeferrals returned 0
	// before the gate reached the CA short-circuit.
	if ca.calls() != 0 {
		t.Errorf("CA Health calls: want 0 (no deferrals → no probe), got %d", ca.calls())
	}
	// Convoy still DraftPROpen (no gate stamp).
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != "DraftPROpen" {
		t.Errorf("convoy status: want DraftPROpen, got %s", convoy.Status)
	}
	// No gate ping.
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("notify-after pings: want 0, got %d (%v)", got, rec.snapshot())
	}
}

// TestConvoyReview_DeferralsExist_TokenStillExpired_BlocksWithReason —
// deferrals on an ask-branch, CA Health returns ErrTokenExpired. Gate
// blocks: convoy → AwaitingSupplyRecheck, ConvoyReview returns early
// (no LLM call), notify-after fired with the count + branch.
func TestConvoyReview_DeferralsExist_TokenStillExpired_BlocksWithReason(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// seedDraftPROpenConvoy uses ask_branch="force/ask-1-test".
	seedSupplyDeferral(t, db, 1, "force/ask-1-test", "Gemfile")

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	ca := &stubCA{healthErr: codeartifact.ErrTokenExpired}
	withDeps(t, &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{},
		RepoResolver: func(int) (string, error) { return "", nil },
	})

	bounty := seedConvoyReviewBounty(t, db, 7002, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// Convoy stamped AwaitingSupplyRecheck.
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != ConvoyStatusAwaitingSupplyRecheck {
		t.Errorf("convoy status: want AwaitingSupplyRecheck, got %s", convoy.Status)
	}
	// LLM was NOT called — gate short-circuited.
	if got := stub.CallCount(); got != 0 {
		t.Errorf("LLM CallCount: want 0 (gate blocked before LLM), got %d", got)
	}
	// notify-after fired exactly once with the reason.
	labels := rec.snapshot()
	if len(labels) != 1 {
		t.Fatalf("expected 1 notify-after invocation, got %d: %v", len(labels), labels)
	}
	if !strings.Contains(labels[0], fmt.Sprintf("Convoy #%d", convoyID)) {
		t.Errorf("ping label missing convoy id: %q", labels[0])
	}
	if !strings.Contains(labels[0], "1 SUPPLY-* check deferred") {
		t.Errorf("ping label missing count phrasing: %q", labels[0])
	}
	if !strings.Contains(labels[0], "umt artifacts") {
		t.Errorf("ping label missing remediation hint: %q", labels[0])
	}
	// Bounty Completed (deferred — not Failed).
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 7002`).Scan(&status)
	if status != "Completed" {
		t.Errorf("bounty status: want Completed (deferred), got %s", status)
	}
}

// TestConvoyReview_DeferralsExist_TokenRecovered_ReplayResolvesAll_Proceeds —
// deferrals on ask-branch, CA Health OK, the registered ReplayableRule
// returns no findings → all rows flip to resolved_late. Gate then sees
// zero remaining deferrals and passes; ConvoyReview proceeds normally.
func TestConvoyReview_DeferralsExist_TokenRecovered_ReplayResolvesAll_Proceeds(t *testing.T) {
	repo := initRepoWithGemfileForGate(t, "force/ask-1-test", "gem 'redis', '5.0.0'\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	deferralID := seedSupplyDeferral(t, db, 1, "force/ask-1-test", "Gemfile")

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	rule := &stubReplayRule{id: "SUPPLY-001"} // no findings → resolved_late
	ca := &stubCA{healthErr: nil}
	withDeps(t, &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{"SUPPLY-001": rule},
		RepoResolver: func(int) (string, error) { return repo, nil },
	})

	bounty := seedConvoyReviewBounty(t, db, 7003, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// Original deferral flipped to resolved_late.
	var disp string
	db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, deferralID).Scan(&disp)
	if disp != supplydeferral.DispositionResolvedLate {
		t.Errorf("deferral disposition: want %s, got %q", supplydeferral.DispositionResolvedLate, disp)
	}
	// Gate passed → LLM ran.
	if got := stub.CallCount(); got != 1 {
		t.Errorf("LLM CallCount: want 1 (replay cleared deferrals → gate passes), got %d", got)
	}
	// Convoy stayed DraftPROpen — no gate stamp.
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != "DraftPROpen" {
		t.Errorf("convoy status: want DraftPROpen, got %s", convoy.Status)
	}
	// No gate ping (no block).
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("notify-after pings: want 0 (gate passed), got %d (%v)", got, rec.snapshot())
	}
}

// TestConvoyReview_DeferralsExist_TokenRecovered_ReplayFlagsSome_Proceeds —
// replay produces still_flagged: original row flips to superseded, a
// fresh disposition='block' row is inserted. The gate's recount finds
// zero remaining token_expired rows, so the gate PASSES. The new block
// row is the standard ISB block-eval path's job, not the gate's.
func TestConvoyReview_DeferralsExist_TokenRecovered_ReplayFlagsSome_Proceeds(t *testing.T) {
	repo := initRepoWithGemfileForGate(t, "force/ask-1-test", "gem 'evilpkg', '0.0.1'\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	deferralID := seedSupplyDeferral(t, db, 1, "force/ask-1-test", "Gemfile")

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	rule := &stubReplayRule{
		id: "SUPPLY-001",
		findings: []supplydeferral.ReplayFinding{
			{RuleID: "SUPPLY-001", Severity: "block", Path: "Gemfile", Message: "evilpkg@0.0.1 hallucinated"},
		},
	}
	ca := &stubCA{healthErr: nil}
	withDeps(t, &SupplyRecheckDeps{
		CA:           ca,
		Rules:        map[string]supplydeferral.ReplayableRule{"SUPPLY-001": rule},
		RepoResolver: func(int) (string, error) { return repo, nil },
	})

	bounty := seedConvoyReviewBounty(t, db, 7004, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// Original deferral was superseded.
	var disp string
	db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, deferralID).Scan(&disp)
	if disp != supplydeferral.DispositionSuperseded {
		t.Errorf("deferral disposition: want %s, got %q", supplydeferral.DispositionSuperseded, disp)
	}
	// A fresh block-severity row exists (the still_flagged outcome).
	// Per supplydeferral/replay.go, the new row carries severity='block'
	// and disposition='' (no terminal flip — the row enters the open
	// bucket so ConvoyReview's normal block-eval picks it up).
	var blockCount int
	db.QueryRow(`SELECT COUNT(*) FROM SecurityFindings
		WHERE rule_id = 'SUPPLY-001' AND severity = 'block'
		  AND IFNULL(disposition, '') = ''`).Scan(&blockCount)
	if blockCount != 1 {
		t.Errorf("expected 1 fresh open block row from still_flagged replay, got %d", blockCount)
	}
	// Gate PASSED — no remaining token_expired rows. LLM ran.
	if got := stub.CallCount(); got != 1 {
		t.Errorf("LLM CallCount: want 1 (gate passes; new block flows via ISB), got %d", got)
	}
	// Convoy stayed DraftPROpen.
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != "DraftPROpen" {
		t.Errorf("convoy status: want DraftPROpen, got %s", convoy.Status)
	}
	// No gate ping (gate passed).
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("notify-after pings: want 0 (gate passed), got %d (%v)", got, rec.snapshot())
	}
}

// TestConvoyReview_NoDepsRegistered_FallsBackToReadOnly — when the
// daemon hasn't called RegisterSupplyRecheckDeps, the gate still
// honours an existing token_expired row and blocks read-only (no CA
// call possible).
func TestConvoyReview_NoDepsRegistered_FallsBackToReadOnly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	seedSupplyDeferral(t, db, 1, "force/ask-1-test", "Gemfile")

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	rec := &notifyRecorder{}
	withNotifyStub(t, rec.fn)

	// Explicitly clear deps for this test.
	RegisterSupplyRecheckDeps(nil)
	t.Cleanup(func() { RegisterSupplyRecheckDeps(nil) })

	bounty := seedConvoyReviewBounty(t, db, 7005, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// Convoy stamped AwaitingSupplyRecheck (read-only block).
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != ConvoyStatusAwaitingSupplyRecheck {
		t.Errorf("convoy status: want AwaitingSupplyRecheck, got %s", convoy.Status)
	}
	// LLM not called.
	if got := stub.CallCount(); got != 0 {
		t.Errorf("LLM CallCount: want 0 (gate blocked), got %d", got)
	}
	// notify-after fired.
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("notify-after pings: want 1, got %d", got)
	}
}

// TestConvoyReview_AwaitingSupplyRecheck_StatusPersisted — once the
// gate stamps the convoy, the status survives across reads AND the
// dashboard "Ship It" surface refuses to advance the convoy. This
// pins the operator-facing UX invariant: the gate is a hard block.
func TestConvoyReview_AwaitingSupplyRecheck_StatusPersisted(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	seedSupplyDeferral(t, db, 1, "force/ask-1-test", "Gemfile")

	// Stub Claude (won't be called, but the harness loads the profile).
	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	withNotifyStub(t, (&notifyRecorder{}).fn)

	// Deps with token-expired CA → deterministic gate block.
	withDeps(t, &SupplyRecheckDeps{
		CA:           &stubCA{healthErr: codeartifact.ErrTokenExpired},
		Rules:        map[string]supplydeferral.ReplayableRule{},
		RepoResolver: func(int) (string, error) { return "", nil },
	})

	bounty := seedConvoyReviewBounty(t, db, 7006, convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty,
		mustLoadCapProfile(t, "convoy-review"), testLogger{})

	// Re-read → status survives.
	convoy := store.GetConvoy(db, convoyID)
	if convoy.Status != ConvoyStatusAwaitingSupplyRecheck {
		t.Fatalf("convoy status not persisted: want AwaitingSupplyRecheck, got %s", convoy.Status)
	}

	// Ship-It surface honours the gate.
	//
	// Both the dashboard handler (internal/dashboard/ship.go:23) and
	// the CLI ship handler (cmd/force/convoy_pr.go:62) gate strictly
	// on `convoy.Status == "DraftPROpen"`; any other value (including
	// AwaitingSupplyRecheck) makes both surfaces refuse to advance.
	// Asserting on the status directly is the structural pin: if the
	// gate ever stamps a status the ship surfaces happen to allow,
	// this assertion fails before downstream behaviour drifts.
	if convoy.Status == "DraftPROpen" {
		t.Errorf("AwaitingSupplyRecheck must NOT equal DraftPROpen — ship-it would advance a blocked convoy")
	}

	// Once the gate clears, the operator can flip the status back to
	// DraftPROpen via the standard PR-state path. After that, ship-it
	// would proceed (modulo CodeArtifact-stubbed gh ops, which are out
	// of this test's scope — we just want to prove the flag is the
	// blocker).
	if err := store.SetConvoyStatus(db, convoyID, "DraftPROpen"); err != nil {
		t.Fatalf("SetConvoyStatus(DraftPROpen): %v", err)
	}
	convoy = store.GetConvoy(db, convoyID)
	if convoy.Status != "DraftPROpen" {
		t.Fatalf("expected status flip back to DraftPROpen, got %s", convoy.Status)
	}
}

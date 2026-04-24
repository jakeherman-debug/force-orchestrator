package agents

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// Fix #8 Phase B — error-propagation coverage for the pilot-package handlers.
//
// Each of the five pilot files (pilot.go, pilot_askbranch.go, pilot_rebase.go,
// pilot_rebase_agent.go, pilot_repo_config.go) migrated its
// `// TODO(Fix #8b): propagate error` markers to explicit `if err := ...;
// err != nil { logger.Printf("...: %v — <recovery hint>", ...) }` guards.
// These tests induce a deterministic DB fault (DROP TABLE BountyBoard) so every
// post-LLM `store.FailBounty` / `store.UpdateBountyStatus` call fails, and
// assert the recovery hint appears in the logger output.
//
// Pattern mirrors the Phase A integration test in
// fix8a_error_propagation_test.go: build a real SQLite via :memory:, drop the
// table, capture the log through `log.New(buf, "", 0)`, assert the hot-path
// recovery-hint vocabulary.

// loggerBuf is a *log.Logger wrapper implementing the agents logger interface
// (Printf-only). Used across the per-file subtests below.
type loggerBuf struct{ *log.Logger }

func newLoggerBuf() (*bytes.Buffer, loggerBuf) {
	var buf bytes.Buffer
	return &buf, loggerBuf{log.New(&buf, "", 0)}
}

// ── pilot.go — runFindPRTemplate ────────────────────────────────────────────

// TestFix8B_RunFindPRTemplate_FailBountyErrorSurfaces exercises the error-
// handling path when the payload is invalid AND the DB has been nuked so
// FailBounty itself fails. The handler must log the fallback with the
// "stale-lock detector will recover" recovery hint.
func TestFix8B_RunFindPRTemplate_FailBountyErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a bounty with a deliberately malformed payload so FailBounty fires.
	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'FindPRTemplate', 'Pending', 'not json', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	// Drop BountyBoard so every downstream FailBounty/UpdateBountyStatus fails.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop BountyBoard: %v", err)
	}

	buf, lg := newLoggerBuf()
	runFindPRTemplate(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty after invalid payload failed") {
		t.Errorf("expected FailBounty recovery-hint log from runFindPRTemplate, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected stale-lock recovery hint, got:\n%s", logs)
	}
}

// ── pilot_askbranch.go — runCreateAskBranch ────────────────────────────────

// TestFix8B_RunCreateAskBranch_FailBountyErrorSurfaces induces the invalid-
// payload branch and then kills BountyBoard so FailBounty errors.
func TestFix8B_RunCreateAskBranch_FailBountyErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'CreateAskBranch', 'Pending', 'not json', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	buf, lg := newLoggerBuf()
	runCreateAskBranch(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty after invalid payload failed") {
		t.Errorf("expected FailBounty recovery-hint log from runCreateAskBranch, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected stale-lock recovery hint, got:\n%s", logs)
	}
}

// TestFix8B_RunCleanupAskBranch_FailBountyErrorSurfaces covers the cleanup
// handler's invalid-payload branch.
func TestFix8B_RunCleanupAskBranch_FailBountyErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'CleanupAskBranch', 'Pending', 'not json', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	buf, lg := newLoggerBuf()
	runCleanupAskBranch(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty after invalid payload failed") {
		t.Errorf("expected FailBounty recovery-hint log from runCleanupAskBranch, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected stale-lock recovery hint, got:\n%s", logs)
	}
}

// ── pilot_rebase.go — runRebaseAskBranch ───────────────────────────────────

// TestFix8B_RunRebaseAskBranch_FailBountyErrorSurfaces exercises the invalid-
// payload path. The handler must surface the FailBounty error with the
// stale-lock recovery hint (the rebase flow is ultimately recovered by the
// main-drift-watch dog for the successful cases, but for a malformed payload
// there is no repo/convoy to re-trigger against — stale-lock is the correct
// recovery vocabulary).
func TestFix8B_RunRebaseAskBranch_FailBountyErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'RebaseAskBranch', 'Pending', 'not json', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	buf, lg := newLoggerBuf()
	runRebaseAskBranch(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty after invalid payload failed") {
		t.Errorf("expected FailBounty recovery-hint log from runRebaseAskBranch, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected stale-lock recovery hint, got:\n%s", logs)
	}
}

// TestFix8B_RunRebaseAskBranch_NoAskBranchPath_MarkCompletedErrorSurfaces
// exercises the "no ask-branch" no-op branch with a DB fault on
// UpdateBountyStatus — verifies the main-drift-watch recovery hint fires.
func TestFix8B_RunRebaseAskBranch_NoAskBranchPath_MarkCompletedErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Valid payload that will reach the "no ask-branch → complete" path.
	payload, _ := json.Marshal(rebasePayload{ConvoyID: 99, Repo: "noop"})
	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, 'noop', 'RebaseAskBranch', 'Pending', ?, datetime('now'))`,
		string(payload))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	// No ConvoyAskBranch row exists → the handler enters the no-op path and
	// tries to UpdateBountyStatus(Completed). Dropping BountyBoard forces that
	// UPDATE to fail so the recovery-hint log fires.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	buf, lg := newLoggerBuf()
	runRebaseAskBranch(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "failed to mark Completed after no-op") {
		t.Errorf("expected no-op Completed recovery-hint log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "main-drift-watch will retry") {
		t.Errorf("expected main-drift-watch recovery hint, got:\n%s", logs)
	}
}

// ── pilot_rebase_agent.go — runRebaseAgentBranch ───────────────────────────

// TestFix8B_RunRebaseAgentBranch_FailBountyErrorSurfaces exercises the
// invalid-payload path.
func TestFix8B_RunRebaseAgentBranch_FailBountyErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'RebaseAgentBranch', 'Pending', 'not json', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	buf, lg := newLoggerBuf()
	runRebaseAgentBranch(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty after invalid payload failed") {
		t.Errorf("expected FailBounty recovery-hint log from runRebaseAgentBranch, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected stale-lock recovery hint, got:\n%s", logs)
	}
}

// ── pilot_repo_config.go — runRevalidateRepoConfig ─────────────────────────

// TestFix8B_RunRevalidateRepoConfig_FailBountyErrorSurfaces exercises the
// invalid-payload path of the repo-config revalidator.
func TestFix8B_RunRevalidateRepoConfig_FailBountyErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'RevalidateRepoConfig', 'Pending', 'not json', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	buf, lg := newLoggerBuf()
	runRevalidateRepoConfig(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty after invalid payload failed") {
		t.Errorf("expected FailBounty recovery-hint log from runRevalidateRepoConfig, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected stale-lock recovery hint, got:\n%s", logs)
	}
}

// TestFix8B_RunRevalidateRepoConfig_RepoRemovedPath_MarkCompletedErrorSurfaces
// exercises the "repo removed → complete" branch with UpdateBountyStatus
// failing; the handler must log the stale-lock recovery hint.
func TestFix8B_RunRevalidateRepoConfig_RepoRemovedPath_MarkCompletedErrorSurfaces(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	payload, _ := json.Marshal(revalidatePayload{Repo: "ghost"})
	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, 'ghost', 'RevalidateRepoConfig', 'Pending', ?, datetime('now'))`,
		string(payload))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	// Repo 'ghost' is not registered → handler enters "repo was removed" branch.
	// Drop BountyBoard so the subsequent UpdateBountyStatus fails.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	buf, lg := newLoggerBuf()
	runRevalidateRepoConfig(db, bounty, lg)

	logs := buf.String()
	if !strings.Contains(logs, "failed to mark Completed after repo") {
		t.Errorf("expected repo-removed Completed recovery-hint log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected stale-lock recovery hint, got:\n%s", logs)
	}
}

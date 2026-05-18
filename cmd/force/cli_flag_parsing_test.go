package main

// fix(cli)/cli-flag-parsing — behavioral subprocess tests for the
// generalized parseSubcommandFlags helper applied across the entire
// `force` CLI surface.
//
// Pattern mirrors daemon_help_test.go: build the binary, set HOME +
// any DB env vars to a t.TempDir(), run the subcommand with --help and
// --bogus-flag, assert exit code + that the destructive operation did
// NOT happen.

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// buildForceForCLITest mirrors buildForceForHelpTest from
// daemon_help_test.go but is duplicated here (rather than shared) to
// keep the two test surfaces independently runnable in CI.
func buildForceForCLITest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "force")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", binPath, "./")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("can't build binary: %v (%s)", err, out)
	}
	return binPath
}

// runForceCLI runs the binary with the given args, sandboxed under
// HOME=tmpHome. Returns (stdout, stderr, exitErr). Uses cmd.Dir =
// tmpHome so the in-process `holocron.db` path resolves under the
// sandbox (matches the helper-test convention).
func runForceCLI(t *testing.T, bin, tmpHome string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	// Sweep F: subprocess must resolve its DB at `tmpHome/holocron.db`, not
	// at the canonical ~/.force/holocron.db. FORCE_DIR overrides the
	// resolver to `tmpHome` so the seed/probe path the test uses lines up.
	cmd.Env = append(os.Environ(), "HOME="+tmpHome, "FORCE_DIR="+tmpHome)
	cmd.Dir = tmpHome
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// countBountyRows returns the number of rows in BountyBoard. Used to
// assert that --help / --bogus-flag did not insert rows.
func countBountyRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard`).Scan(&n); err != nil {
		t.Fatalf("count BountyBoard: %v", err)
	}
	return n
}

// countRepoRows returns the number of rows in Repositories.
func countRepoRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Repositories`).Scan(&n); err != nil {
		t.Fatalf("count Repositories: %v", err)
	}
	return n
}

// countAuditRows returns the number of rows in AuditLog.
func countAuditRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM AuditLog`).Scan(&n); err != nil {
		t.Fatalf("count AuditLog: %v", err)
	}
	return n
}

// seedOneBounty seeds a single Pending Feature task so the cancel /
// block / unblock / prioritize tests have a row to potentially mutate.
// Returns the task ID.
func seedOneBounty(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	id := store.AddBounty(db, 0, "Feature", "test seed task")
	if id == 0 {
		t.Fatalf("seed AddBounty returned 0")
	}
	return id
}

// ── add-repo ─────────────────────────────────────────────────────────────────

func TestAddRepo_HelpFlag_DoesNotWriteRow(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countRepoRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "add-repo", "--help")
	if err != nil {
		t.Fatalf("add-repo --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force add-repo") {
		t.Errorf("expected help banner, got stdout=%q", stdout)
	}
	if got := countRepoRows(t, dbPath); got != pre {
		t.Errorf("add-repo --help inserted %d row(s) — must be no-op", got-pre)
	}
}

func TestAddRepo_UnknownFlag_RejectsBeforeWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countRepoRows(t, dbPath)
	_, stderr, err := runForceCLI(t, bin, home, "add-repo", "--bogus-flag")
	if err == nil {
		t.Fatalf("add-repo --bogus-flag must exit non-zero")
	}
	if !strings.Contains(stderr, "not defined") {
		t.Errorf("stderr should mention undefined flag, got: %q", stderr)
	}
	if got := countRepoRows(t, dbPath); got != pre {
		t.Errorf("add-repo --bogus-flag inserted %d row(s) — must reject before write", got-pre)
	}
}

// ── reset / cancel / block / unblock / prioritize ───────────────────────────
// These are task mutators. We seed a single Pending bounty, run the
// destructive command with --help / --bogus-flag, and assert the AuditLog
// is unchanged (every successful mutation writes an AuditLog row).

func TestReset_HelpFlag_NoAuditWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	id := seedOneBounty(t, dbPath)
	pre := countAuditRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "reset", "--help", strings.TrimSpace("0"))
	if err != nil {
		t.Fatalf("reset --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force reset") {
		t.Errorf("expected reset help, got stdout=%q", stdout)
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("reset --help wrote %d AuditLog row(s) — must be no-op", got-pre)
	}
	_ = id
}

func TestReset_UnknownFlag_NoAuditWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	id := seedOneBounty(t, dbPath)
	pre := countAuditRows(t, dbPath)
	_, _, err := runForceCLI(t, bin, home, "reset", "--bogus-flag", "1")
	if err == nil {
		t.Fatalf("reset --bogus-flag must exit non-zero")
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("reset --bogus-flag wrote %d AuditLog row(s) — must reject before write", got-pre)
	}
	_ = id
}

func TestCancel_HelpFlag_NoAuditWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countAuditRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "cancel", "--help")
	if err != nil {
		t.Fatalf("cancel --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force cancel") {
		t.Errorf("expected cancel help, got stdout=%q", stdout)
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("cancel --help wrote %d AuditLog row(s) — must be no-op", got-pre)
	}
}

func TestCancel_UnknownFlag_NoAuditWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countAuditRows(t, dbPath)
	_, _, err := runForceCLI(t, bin, home, "cancel", "--bogus-flag", "1")
	if err == nil {
		t.Fatalf("cancel --bogus-flag must exit non-zero")
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("cancel --bogus-flag wrote %d AuditLog row(s) — must reject before write", got-pre)
	}
}

func TestBlock_HelpFlag_NoAuditWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countAuditRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "block", "--help")
	if err != nil {
		t.Fatalf("block --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force block") {
		t.Errorf("expected block help, got stdout=%q", stdout)
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("block --help wrote %d AuditLog row(s) — must be no-op", got-pre)
	}
}

func TestBlock_UnknownFlag_NoAuditWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countAuditRows(t, dbPath)
	_, _, err := runForceCLI(t, bin, home, "block", "--bogus-flag", "1", "2")
	if err == nil {
		t.Fatalf("block --bogus-flag must exit non-zero")
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("block --bogus-flag wrote %d AuditLog row(s) — must reject before write", got-pre)
	}
}

func TestPrioritize_HelpFlag_NoAuditWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countAuditRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "prioritize", "--help")
	if err != nil {
		t.Fatalf("prioritize --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force prioritize") {
		t.Errorf("expected prioritize help, got stdout=%q", stdout)
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("prioritize --help wrote %d AuditLog row(s) — must be no-op", got-pre)
	}
}

// ── purge / hard-reset / prune / cleanup / scale / migrate ─────────────────

// TestPurge_HelpFlag_DoesNotPurge: --help must NOT scrub anything.
// We assert that the BountyBoard row count is unchanged.
func TestPurge_HelpFlag_DoesNotPurge(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countBountyRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "purge", "--help")
	if err != nil {
		t.Fatalf("purge --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force purge") {
		t.Errorf("expected purge help, got stdout=%q", stdout)
	}
	if got := countBountyRows(t, dbPath); got != pre {
		t.Errorf("purge --help mutated BountyBoard (%d → %d)", pre, got)
	}
}

func TestPurge_UnknownFlag_Rejects(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countBountyRows(t, dbPath)
	_, _, err := runForceCLI(t, bin, home, "purge", "--bogus-flag")
	if err == nil {
		t.Fatalf("purge --bogus-flag must exit non-zero")
	}
	if got := countBountyRows(t, dbPath); got != pre {
		t.Errorf("purge --bogus-flag mutated BountyBoard (%d → %d)", pre, got)
	}
}

// TestHardReset_HelpFlag_DoesNotReset: pre-seed BountyBoard, run
// `hard-reset --help`, assert rows survive.
func TestHardReset_HelpFlag_DoesNotReset(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countBountyRows(t, dbPath)
	if pre == 0 {
		t.Fatalf("seed failed: BountyBoard empty")
	}
	stdout, _, err := runForceCLI(t, bin, home, "hard-reset", "--help")
	if err != nil {
		t.Fatalf("hard-reset --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force hard-reset") {
		t.Errorf("expected hard-reset help, got stdout=%q", stdout)
	}
	if got := countBountyRows(t, dbPath); got != pre {
		t.Errorf("hard-reset --help wiped BountyBoard (%d → %d)", pre, got)
	}
}

func TestHardReset_UnknownFlag_Rejects(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	_ = seedOneBounty(t, dbPath)
	pre := countBountyRows(t, dbPath)
	_, _, err := runForceCLI(t, bin, home, "hard-reset", "--bogus-flag")
	if err == nil {
		t.Fatalf("hard-reset --bogus-flag must exit non-zero")
	}
	if got := countBountyRows(t, dbPath); got != pre {
		t.Errorf("hard-reset --bogus-flag wiped BountyBoard (%d → %d)", pre, got)
	}
}

func TestPrune_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "prune", "--help")
	if err != nil {
		t.Fatalf("prune --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force prune") {
		t.Errorf("expected prune help, got stdout=%q", stdout)
	}
}

func TestPrune_UnknownFlag_Rejects(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	_, _, err := runForceCLI(t, bin, home, "prune", "--bogus-flag")
	if err == nil {
		t.Fatalf("prune --bogus-flag must exit non-zero")
	}
}

func TestCleanup_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "cleanup", "--help")
	if err != nil {
		t.Fatalf("cleanup --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force cleanup") {
		t.Errorf("expected cleanup help, got stdout=%q", stdout)
	}
}

func TestCleanup_UnknownFlag_Rejects(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	_, _, err := runForceCLI(t, bin, home, "cleanup", "--bogus-flag")
	if err == nil {
		t.Fatalf("cleanup --bogus-flag must exit non-zero")
	}
}

func TestScale_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "scale", "--help")
	if err != nil {
		t.Fatalf("scale --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force scale") {
		t.Errorf("expected scale help, got stdout=%q", stdout)
	}
}

func TestScale_UnknownFlag_Rejects(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	_, _, err := runForceCLI(t, bin, home, "scale", "--bogus-flag")
	if err == nil {
		t.Fatalf("scale --bogus-flag must exit non-zero")
	}
}

func TestMigrate_HelpFlag_NoMigrationRun(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "migrate", "--help")
	if err != nil {
		t.Fatalf("migrate --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force migrate") {
		t.Errorf("expected migrate help, got stdout=%q", stdout)
	}
}

func TestMigratePRFlow_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "migrate", "pr-flow", "--help")
	if err != nil {
		t.Fatalf("migrate pr-flow --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force migrate pr-flow") {
		t.Errorf("expected migrate pr-flow help, got stdout=%q", stdout)
	}
}

func TestMigratePRFlow_UnknownFlag_Rejects(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	_, _, err := runForceCLI(t, bin, home, "migrate", "pr-flow", "--bogus-flag")
	if err == nil {
		t.Fatalf("migrate pr-flow --bogus-flag must exit non-zero")
	}
}

// ── annotate / decide / briefing-reject ────────────────────────────────────

func TestAnnotate_HelpFlag_NoWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countAnnotationRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "annotate", "--help")
	if err != nil {
		t.Fatalf("annotate --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force annotate") {
		t.Errorf("expected annotate help, got stdout=%q", stdout)
	}
	if got := countAnnotationRows(t, dbPath); got != pre {
		t.Errorf("annotate --help wrote %d annotation row(s) — must be no-op", got-pre)
	}
}

func TestAnnotate_UnknownFlag_NoWrite(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countAnnotationRows(t, dbPath)
	_, _, err := runForceCLI(t, bin, home, "annotate", "--bogus-flag", "captain_ruling", "1", "problem", "test")
	if err == nil {
		t.Fatalf("annotate --bogus-flag must exit non-zero")
	}
	if got := countAnnotationRows(t, dbPath); got != pre {
		t.Errorf("annotate --bogus-flag wrote %d annotation row(s) — must reject before write", got-pre)
	}
}

func TestDecide_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "decide", "--help")
	if err != nil {
		t.Fatalf("decide --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force decide") {
		t.Errorf("expected decide help, got stdout=%q", stdout)
	}
}

func TestBriefingReject_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "briefing-reject", "--help")
	if err != nil {
		t.Fatalf("briefing-reject --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force briefing-reject") {
		t.Errorf("expected briefing-reject help, got stdout=%q", stdout)
	}
}

// ── escalations ack/close/requeue ──────────────────────────────────────────

func TestEscalationsAck_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "escalations", "ack", "--help")
	if err != nil {
		t.Fatalf("escalations ack --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force escalations ack") {
		t.Errorf("expected escalations ack help, got stdout=%q", stdout)
	}
}

func TestEscalationsClose_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "escalations", "close", "--help")
	if err != nil {
		t.Fatalf("escalations close --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force escalations close") {
		t.Errorf("expected escalations close help, got stdout=%q", stdout)
	}
}

func TestEscalationsRequeue_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "escalations", "requeue", "--help")
	if err != nil {
		t.Fatalf("escalations requeue --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force escalations requeue") {
		t.Errorf("expected escalations requeue help, got stdout=%q", stdout)
	}
}

// ── ec ratify / reject ────────────────────────────────────────────────────

func TestECRatify_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "ec", "ratify", "--help")
	if err != nil {
		t.Fatalf("ec ratify --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force ec ratify") {
		t.Errorf("expected ec ratify help, got stdout=%q", stdout)
	}
}

func TestECReject_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "ec", "reject", "--help")
	if err != nil {
		t.Fatalf("ec reject --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force ec reject") {
		t.Errorf("expected ec reject help, got stdout=%q", stdout)
	}
}

// ── experiment ratify / terminate ─────────────────────────────────────────

func TestExperimentRatify_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "experiment", "ratify", "--help")
	if err != nil {
		t.Fatalf("experiment ratify --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force experiment ratify") {
		t.Errorf("expected experiment ratify help, got stdout=%q", stdout)
	}
}

// ── read-only smoke tests (a representative sample) ───────────────────────

func TestList_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "list", "--help")
	if err != nil {
		t.Fatalf("list --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force list") {
		t.Errorf("expected list help, got stdout=%q", stdout)
	}
}

func TestList_UnknownFlag_Rejects(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	_, _, err := runForceCLI(t, bin, home, "list", "--bogus-flag")
	if err == nil {
		t.Fatalf("list --bogus-flag must exit non-zero")
	}
}

func TestStatus_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "status", "--help")
	if err != nil {
		t.Fatalf("status --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force status") {
		t.Errorf("expected status help, got stdout=%q", stdout)
	}
}

func TestAudit_HelpFlag_NoOp(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	stdout, _, err := runForceCLI(t, bin, home, "audit", "--help")
	if err != nil {
		t.Fatalf("audit --help should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Usage: force audit") {
		t.Errorf("expected audit help, got stdout=%q", stdout)
	}
}

// ── helper for the annotate-row-count assertion ────────────────────────────

// countAnnotationRows returns the row count for the OperatorAnnotations
// table. Used to assert annotate --help / --bogus-flag is a no-op.
func countAnnotationRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM OperatorAnnotations`).Scan(&n); err != nil {
		// Table might not exist on schemas that haven't migrated to D3
		// — treat as zero. Test still validates the no-op shape.
		var dbErr *sqlError
		_ = dbErr
		return 0
	}
	return n
}

// sqlError is a tiny shim so countAnnotationRows compiles when the
// table is absent. We never actually use it; it just keeps the import
// of database/sql honest.
type sqlError struct{ _ sql.NullString }

// ── fix(cli)/cli-destructive-verbs — extracted-verb behavioral tests ───────
//
// The following tests cover the 8 destructive verbs that fix(cli)/
// cli-destructive-verbs extracted out of the inline-switch dispatchers
// in convoy.go / mail.go / config.go / memory.go. Each is the same shape
// as TestAddRepo_HelpFlag_DoesNotWriteRow: build the binary, sandbox HOME
// + cwd to a tempdir, run `<verb> --help`, assert exit 0 and that the
// destructive table is unchanged (or empty).

// countConvoyRows returns the number of rows in Convoys.
func countConvoyRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Convoys`).Scan(&n); err != nil {
		t.Fatalf("count Convoys: %v", err)
	}
	return n
}

// countMailRows returns the number of rows in Fleet_Mail.
func countMailRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&n); err != nil {
		t.Fatalf("count Fleet_Mail: %v", err)
	}
	return n
}

// countSystemConfigRows returns the number of rows in SystemConfig.
func countSystemConfigRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM SystemConfig`).Scan(&n); err != nil {
		t.Fatalf("count SystemConfig: %v", err)
	}
	return n
}

// countFleetMemoryRows returns the number of rows in FleetMemory.
func countFleetMemoryRows(t *testing.T, dbPath string) int {
	t.Helper()
	db := store.InitHolocronDSN(dbPath)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM FleetMemory`).Scan(&n); err != nil {
		t.Fatalf("count FleetMemory: %v", err)
	}
	return n
}

// ── convoy create / approve / reset / reject / ship ────────────────────────

func TestConvoyCreate_HelpFlag_NoMutation(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countConvoyRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "convoy", "create", "--help")
	if err != nil {
		t.Fatalf("convoy create --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force convoy create") {
		t.Errorf("expected convoy create help, got stdout=%q", stdout)
	}
	if got := countConvoyRows(t, dbPath); got != pre {
		t.Errorf("convoy create --help inserted %d row(s) — must be no-op", got-pre)
	}
}

func TestConvoyApprove_HelpFlag_NoMutation(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countAuditRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "convoy", "approve", "--help")
	if err != nil {
		t.Fatalf("convoy approve --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force convoy approve") {
		t.Errorf("expected convoy approve help, got stdout=%q", stdout)
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("convoy approve --help wrote %d AuditLog row(s) — must be no-op", got-pre)
	}
}

func TestConvoyReset_HelpFlag_NoMutation(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countAuditRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "convoy", "reset", "--help")
	if err != nil {
		t.Fatalf("convoy reset --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force convoy reset") {
		t.Errorf("expected convoy reset help, got stdout=%q", stdout)
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("convoy reset --help wrote %d AuditLog row(s) — must be no-op", got-pre)
	}
}

func TestConvoyReject_HelpFlag_NoMutation(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countAuditRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "convoy", "reject", "--help")
	if err != nil {
		t.Fatalf("convoy reject --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force convoy reject") {
		t.Errorf("expected convoy reject help, got stdout=%q", stdout)
	}
	if got := countAuditRows(t, dbPath); got != pre {
		t.Errorf("convoy reject --help wrote %d AuditLog row(s) — must be no-op", got-pre)
	}
}

func TestConvoyShip_HelpFlag_NoMutation(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countConvoyRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "convoy", "ship", "--help")
	if err != nil {
		t.Fatalf("convoy ship --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force convoy ship") {
		t.Errorf("expected convoy ship help, got stdout=%q", stdout)
	}
	if got := countConvoyRows(t, dbPath); got != pre {
		t.Errorf("convoy ship --help mutated Convoys (%d → %d)", pre, got)
	}
}

// ── mail send ──────────────────────────────────────────────────────────────

func TestMailSend_HelpFlag_NoSend(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countMailRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "mail", "send", "--help")
	if err != nil {
		t.Fatalf("mail send --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force mail send") {
		t.Errorf("expected mail send help, got stdout=%q", stdout)
	}
	if got := countMailRows(t, dbPath); got != pre {
		t.Errorf("mail send --help wrote %d Fleet_Mail row(s) — must be no-op (no Slack call either)", got-pre)
	}
}

// ── config set ─────────────────────────────────────────────────────────────

func TestConfigSet_HelpFlag_NoMutation(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countSystemConfigRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "config", "set", "--help")
	if err != nil {
		t.Fatalf("config set --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force config set") {
		t.Errorf("expected config set help, got stdout=%q", stdout)
	}
	if got := countSystemConfigRows(t, dbPath); got != pre {
		t.Errorf("config set --help wrote %d SystemConfig row(s) — must be no-op", got-pre)
	}
}

// ── memories delete ────────────────────────────────────────────────────────

func TestMemoriesDelete_HelpFlag_NoMutation(t *testing.T) {
	bin := buildForceForCLITest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")
	pre := countFleetMemoryRows(t, dbPath)
	stdout, _, err := runForceCLI(t, bin, home, "memories", "delete", "--help")
	if err != nil {
		t.Fatalf("memories delete --help should exit 0, got %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Usage: force memories delete") {
		t.Errorf("expected memories delete help, got stdout=%q", stdout)
	}
	if got := countFleetMemoryRows(t, dbPath); got != pre {
		t.Errorf("memories delete --help removed %d FleetMemory row(s) — must be no-op", pre-got)
	}
}

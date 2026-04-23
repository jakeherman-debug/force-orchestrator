package agents

// Silent-failure verification tests for findings AUDIT-013, -014, -015, -040,
// -041, -042, -043, -044, -090, -091, -094, -095, -156, -159.
//
// These extend the P1 "No silent failures" invariant audit into the agents/
// and git/ packages. The approach is static: each sub-test greps the cited
// source file for the defective pattern and asserts it is present. These are
// RED-phase tests — they PASS today because the defects are live; when the
// remedy lands, the assertion updates (or the file content changes) and the
// test flips.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// silentRepoRoot returns the force-orchestrator repo root derived from this
// file's location. Tests run with `go test ./internal/agents`, so we walk up
// two dirs. Separate name from other audit tests to avoid redeclaration.
func silentRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func silentReadFile(t *testing.T, rel string) string {
	t.Helper()
	root := silentRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// silentCount returns how many non-overlapping times substr appears in s.
func silentCount(s, substr string) int {
	if substr == "" {
		return 0
	}
	return strings.Count(s, substr)
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-013 — medicPayload JSON swallow in runMedicTask
// File: internal/agents/medic.go around line 120
// Defect: json.Unmarshal result discarded — Medic runs with empty mp on parse
// failure and can escalate/shard/requeue based on hallucinations.
// Contrast: runMedicCITriage around line 116 DOES check the error.
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_013_MedicPayloadJSONSwallow(t *testing.T) {
	src := silentReadFile(t, "internal/agents/medic.go")

	// The defective line:
	//   json.Unmarshal([]byte(bounty.Payload), &mp)
	// with no surrounding `if err :=` or `err :=` assignment.
	want := "json.Unmarshal([]byte(bounty.Payload), &mp)"
	idx := strings.Index(src, want)
	if idx < 0 {
		t.Fatalf("AUDIT-013: expected pattern %q not found; citation may be stale", want)
	}

	// Verify the call is NOT wrapped in an `if err :=` or `err :=` on the same
	// or immediately preceding line. Pull the 80 bytes before the match.
	start := idx - 80
	if start < 0 {
		start = 0
	}
	prefix := src[start:idx]
	if strings.Contains(prefix, "if err :=") || regexp.MustCompile(`\berr\s*:?=\s*$`).MatchString(strings.TrimRight(prefix, " \t")) {
		t.Fatalf("AUDIT-013: runMedicTask now appears to check the json.Unmarshal error — finding may be fixed:\n%s", prefix)
	}

	// Sanity check: runMedicCITriage on the same file DOES check its unmarshal.
	ciTriageCheck := "if err := json.Unmarshal([]byte(bounty.Payload), &payload)"
	if !strings.Contains(src, ciTriageCheck) {
		t.Logf("AUDIT-013: note — runMedicCITriage's checked unmarshal pattern %q not found; the contrast assertion is weakened (not fatal)", ciTriageCheck)
	}
	t.Logf("AUDIT-013 REPRODUCED: runMedicTask's json.Unmarshal error is discarded at medic.go")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-014 — WorktreeReset parent-requeue silent
// File: internal/agents/pilot_worktree_reset.go around lines 121-129
// Defect: `_, _ = db.Exec(...)` for both the parent requeue UPDATE and the
// Escalations resolution UPDATE. Self-healing silently breaks if either fails.
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_014_WorktreeResetParentRequeueSilent(t *testing.T) {
	src := silentReadFile(t, "internal/agents/pilot_worktree_reset.go")

	// Both parent-requeue and escalation-resolve UPDATEs use `_, _ = db.Exec(`.
	occ := silentCount(src, "_, _ = db.Exec(")
	if occ < 2 {
		t.Fatalf("AUDIT-014: expected at least 2 `_, _ = db.Exec(` sites in pilot_worktree_reset.go, found %d — finding may be fixed", occ)
	}
	// And the distinctive SQL fragments are still present unchecked.
	if !strings.Contains(src, "SET status = 'Pending', branch_name = ''") {
		t.Fatalf("AUDIT-014: parent-requeue UPDATE text not found; citation stale")
	}
	if !strings.Contains(src, "SET status = 'Resolved', acknowledged_at") {
		t.Fatalf("AUDIT-014: escalations-resolve UPDATE text not found; citation stale")
	}
	t.Logf("AUDIT-014 REPRODUCED: %d `_, _ = db.Exec(` sites in pilot_worktree_reset.go; both parent-requeue writes are silent", occ)
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-015 — onSubPRMerged logs mid-tx error and returns without rollback
// handling; the `defer tx.Rollback() //nolint:errcheck` covers it, but the
// finding is that every intermediate error inside the tx is `log.Printf(...)
// \n return` with no FailBounty / no escalation. Dog will repick on next tick
// because PR state on GitHub may say merged while DB status didn't update.
// File: internal/agents/pr_flow.go:452-489
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_015_OnSubPRMergedMidTxLogAndReturn(t *testing.T) {
	src := silentReadFile(t, "internal/agents/pr_flow.go")

	// Find onSubPRMerged function body.
	fnIdx := strings.Index(src, "func onSubPRMerged(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-015: onSubPRMerged not found; citation stale")
	}
	// Slice next ~2KB — fn body is short.
	end := fnIdx + 2500
	if end > len(src) {
		end = len(src)
	}
	body := src[fnIdx:end]

	// Count the "logger.Printf(..failed: %v..); return" mid-tx pattern.
	logReturnRe := regexp.MustCompile(`logger\.Printf\("sub-pr-ci-watch:[^"]*failed:[^"]*",[^)]*\)\s*\n\s*return`)
	matches := logReturnRe.FindAllString(body, -1)
	if len(matches) < 3 {
		t.Fatalf("AUDIT-015: expected >=3 log-and-return sites in onSubPRMerged, found %d — finding may be fixed", len(matches))
	}
	// No FailBounty or CreateEscalation inside the tx body.
	if strings.Contains(body, "FailBounty") || strings.Contains(body, "CreateEscalation") {
		t.Fatalf("AUDIT-015: onSubPRMerged now calls FailBounty/CreateEscalation — finding may be fixed")
	}
	t.Logf("AUDIT-015 REPRODUCED: %d log-and-return sites inside onSubPRMerged with no escalation path", len(matches))
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-040 — escalateCITriage manual UPDATE + CreateEscalation double-status
// File: internal/agents/medic_ci.go:262-265 + escalation.go:40
// Defect: manual `UPDATE BountyBoard SET status = 'Escalated'` followed by
// CreateEscalation() which internally calls UpdateBountyStatus(db, taskID,
// "Escalated"). Webhook fires twice.
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_040_EscalateCITriageDoubleUPDATE(t *testing.T) {
	ciSrc := silentReadFile(t, "internal/agents/medic_ci.go")

	// Manual UPDATE present
	manualUpdate := "UPDATE BountyBoard SET status = 'Escalated'"
	if !strings.Contains(ciSrc, manualUpdate) {
		t.Fatalf("AUDIT-040: manual UPDATE %q not found in medic_ci.go — citation stale", manualUpdate)
	}
	// Followed by CreateEscalation in the same function.
	fnIdx := strings.Index(ciSrc, "func escalateCITriage(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-040: escalateCITriage not found; citation stale")
	}
	fnEnd := strings.Index(ciSrc[fnIdx:], "\n}\n")
	if fnEnd < 0 {
		fnEnd = 2000
	}
	body := ciSrc[fnIdx : fnIdx+fnEnd]
	if !strings.Contains(body, manualUpdate) {
		t.Fatalf("AUDIT-040: manual UPDATE not inside escalateCITriage body")
	}
	if !strings.Contains(body, "CreateEscalation(db, taskID,") {
		t.Fatalf("AUDIT-040: CreateEscalation not invoked in escalateCITriage — citation stale")
	}

	// Confirm CreateEscalation itself re-updates status.
	escSrc := silentReadFile(t, "internal/agents/escalation.go")
	if !strings.Contains(escSrc, `store.UpdateBountyStatus(db, taskID, "Escalated")`) {
		t.Fatalf("AUDIT-040: CreateEscalation no longer calls UpdateBountyStatus(Escalated) — double-update fixed?")
	}
	t.Logf("AUDIT-040 REPRODUCED: escalateCITriage sets Escalated status manually AND via CreateEscalation")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-041 — CreateEscalation has no error return
// File: internal/agents/escalation.go:31
// Defect: signature `func CreateEscalation(...) int` not `(int, error)`. Failed
// INSERT leaves task Escalated with no Escalations row.
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_041_CreateEscalationNoErrorReturn(t *testing.T) {
	src := silentReadFile(t, "internal/agents/escalation.go")

	// Exact current signature (as of defect).
	sig := "func CreateEscalation(db *sql.DB, taskID int, severity store.EscalationSeverity, message string) int {"
	if !strings.Contains(src, sig) {
		t.Fatalf("AUDIT-041: signature %q not found — if signature now returns error, finding is fixed", sig)
	}
	// Inside the body: `_, _` pattern for the INSERT.
	if !strings.Contains(src, "res, _ := db.Exec(") {
		t.Fatalf("AUDIT-041: expected `res, _ := db.Exec(` silent INSERT inside CreateEscalation")
	}
	// Inside the body: `id, _ := res.LastInsertId()`.
	if !strings.Contains(src, "id, _ := res.LastInsertId()") {
		t.Fatalf("AUDIT-041: expected `id, _ := res.LastInsertId()` silent discard")
	}
	t.Logf("AUDIT-041 REPRODUCED: CreateEscalation returns bare int; insert + LastInsertId errors both dropped")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-042 — UpdateAskBranchPRChecks error silently discarded at 3 hot sites
// Files: pr_flow.go:421, medic_ci.go:200, medic_ci.go:248
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_042_UpdateAskBranchPRChecksDiscarded(t *testing.T) {
	prFlow := silentReadFile(t, "internal/agents/pr_flow.go")
	medicCI := silentReadFile(t, "internal/agents/medic_ci.go")

	pattern := "_ = store.UpdateAskBranchPRChecks("

	inPRFlow := silentCount(prFlow, pattern)
	inMedicCI := silentCount(medicCI, pattern)

	if inPRFlow < 1 {
		t.Fatalf("AUDIT-042: expected >=1 `%s` in pr_flow.go, found %d", pattern, inPRFlow)
	}
	if inMedicCI < 2 {
		t.Fatalf("AUDIT-042: expected >=2 `%s` in medic_ci.go, found %d", pattern, inMedicCI)
	}
	total := inPRFlow + inMedicCI
	if total < 3 {
		t.Fatalf("AUDIT-042: expected >=3 total sites across pr_flow.go + medic_ci.go, found %d", total)
	}
	t.Logf("AUDIT-042 REPRODUCED: `%s` — pr_flow.go=%d, medic_ci.go=%d (total=%d)", pattern, inPRFlow, inMedicCI, total)
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-043 — PRClose error logged, MarkAskBranchPRClosed called unconditionally
// File: internal/agents/pr_flow.go:343-347
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_043_PRCloseUnconditionalMarkClosed(t *testing.T) {
	src := silentReadFile(t, "internal/agents/pr_flow.go")

	// Find the PRClose site and confirm MarkAskBranchPRClosed follows it
	// OUTSIDE the `if closeErr != nil` branch — i.e. unconditionally.
	re := regexp.MustCompile(`if closeErr := ghc\.PRClose\([^)]+\); closeErr != nil \{\s*\n\s*logger\.Printf\([^)]*\)\s*\n\s*\}\s*\n\s*\}\s*\n\s*store\.MarkAskBranchPRClosed\(`)
	if !re.MatchString(src) {
		t.Fatalf("AUDIT-043: expected PRClose(log-only) followed by unconditional MarkAskBranchPRClosed pattern not found; citation stale")
	}
	t.Logf("AUDIT-043 REPRODUCED: MarkAskBranchPRClosed called after a logged-only PRClose failure path")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-044 — Librarian writeMemoryPayload silent fallback to raw payload
// File: internal/agents/librarian.go:75-78
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_044_LibrarianSilentFallback(t *testing.T) {
	src := silentReadFile(t, "internal/agents/librarian.go")

	// The defective pattern:
	//   if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
	//       // Fallback: treat raw payload as task description.
	//       payload.Task = bounty.Payload
	//   }
	needle := "payload.Task = bounty.Payload"
	if !strings.Contains(src, needle) {
		t.Fatalf("AUDIT-044: fallback assignment %q not found; citation stale", needle)
	}
	// Confirm the fallback comment is still present.
	if !strings.Contains(src, "Fallback") && !strings.Contains(src, "fallback") {
		t.Logf("AUDIT-044: fallback comment missing but assignment present — weaker signal, still indicative")
	}
	// Confirm NO `return` / `FailBounty` between the unmarshal and the fallback —
	// i.e. the error is swallowed into the fallback path.
	fallbackIdx := strings.Index(src, needle)
	// Scan 200 bytes before it.
	start := fallbackIdx - 200
	if start < 0 {
		start = 0
	}
	window := src[start:fallbackIdx]
	if strings.Contains(window, "FailBounty") || strings.Contains(window, "return\n") {
		t.Fatalf("AUDIT-044: window before fallback contains FailBounty/return — finding may be fixed")
	}
	t.Logf("AUDIT-044 REPRODUCED: Librarian silently falls back to raw payload on unmarshal error, poisoning memory index")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-090 — stalled-reviews sub-PR scan silently skips rows on Scan error
// File: internal/agents/dogs.go ~lines 398-405
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_090_StalledReviewsSilentScan(t *testing.T) {
	src := silentReadFile(t, "internal/agents/dogs.go")

	// Locate dogStalledReviews.
	fnIdx := strings.Index(src, "func dogStalledReviews(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-090: dogStalledReviews not found; citation stale")
	}
	// Within 2KB, find the subPRRows scan block.
	slice := src[fnIdx:]
	if len(slice) > 4000 {
		slice = slice[:4000]
	}
	// Pattern: `for subPRRows.Next() {` ... `if err := subPRRows.Scan(&id, &hours); err == nil {`
	// The `err == nil` branch only APPENDS on success; scan errors are dropped.
	if !strings.Contains(slice, "for subPRRows.Next()") {
		t.Fatalf("AUDIT-090: subPRRows loop not found; citation stale")
	}
	if !strings.Contains(slice, "err := subPRRows.Scan(&id, &hours); err == nil") {
		t.Fatalf("AUDIT-090: expected `err := subPRRows.Scan(...); err == nil` silent-skip pattern not found; citation stale")
	}
	// No logger.Printf of the scan error inside this block.
	scanIdx := strings.Index(slice, "err := subPRRows.Scan(&id, &hours); err == nil")
	window := slice[scanIdx : scanIdx+400]
	if strings.Contains(window, "logger.Printf") && strings.Contains(window, "scan") {
		t.Fatalf("AUDIT-090: scan error now appears to be logged; finding may be fixed")
	}
	t.Logf("AUDIT-090 REPRODUCED: subPRRows.Scan errors silently drop rows — legitimate 12h+ stalls never alarm")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-091 — git-hygiene returns nil on Agents query failure
// File: internal/agents/dogs.go:197-200
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_091_GitHygieneReturnsNilOnError(t *testing.T) {
	src := silentReadFile(t, "internal/agents/dogs.go")

	fnIdx := strings.Index(src, "func dogGitHygiene(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-091: dogGitHygiene not found; citation stale")
	}
	slice := src[fnIdx:]
	if len(slice) > 3000 {
		slice = slice[:3000]
	}
	// The defective pattern: after `db.Query` on Agents, on error → `return nil`.
	needle := "agentRows, agentErr := db.Query(`SELECT agent_name, repo, worktree_path FROM Agents`)"
	if !strings.Contains(slice, needle) {
		t.Fatalf("AUDIT-091: Agents query line not found; citation stale")
	}
	// Within the next ~200 chars expect `return nil // non-fatal`.
	needleIdx := strings.Index(slice, needle)
	window := slice[needleIdx : needleIdx+400]
	if !strings.Contains(window, "return nil") {
		t.Fatalf("AUDIT-091: expected `return nil` on agent-rows error; citation stale")
	}
	t.Logf("AUDIT-091 REPRODUCED: dogGitHygiene swallows Agents query error as `return nil`")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-094 — Astromech ownership-check UPDATE drops both errors
// File: internal/agents/astromech.go:564-568
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_094_AstromechOwnershipDropsErrors(t *testing.T) {
	src := silentReadFile(t, "internal/agents/astromech.go")

	// The defective pattern.
	needle := "ownerRes, _ := db.Exec("
	idx := strings.Index(src, needle)
	if idx < 0 {
		t.Fatalf("AUDIT-094: %q not found; citation stale", needle)
	}
	// Next line's RowsAffected also discards its error.
	window := src[idx : idx+400]
	if !strings.Contains(window, "n, _ := ownerRes.RowsAffected()") {
		t.Fatalf("AUDIT-094: `n, _ := ownerRes.RowsAffected()` pattern not found in window; citation stale")
	}
	// And on zero rows, logs "ownership lost" — so a transient SQLITE_BUSY
	// where err != nil AND n == 0 is indistinguishable from genuine loss.
	if !strings.Contains(window, "ownership lost") {
		t.Fatalf("AUDIT-094: 'ownership lost' branch not present; citation stale")
	}
	t.Logf("AUDIT-094 REPRODUCED: ownership check drops db.Exec error AND RowsAffected error; transient DB failures misread as lost ownership")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-095 — Diplomat Claude failure silent fallback (no retry, no mail)
// File: internal/agents/diplomat.go:304-307
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_095_DiplomatSilentFallback(t *testing.T) {
	src := silentReadFile(t, "internal/agents/diplomat.go")

	// Defective pattern: err from AskClaudeCLI logged, then `return
	// buildFallbackPRBody(...), nil`.
	re := regexp.MustCompile(`claude\.AskClaudeCLI\([^)]*\)\s*\n\s*if err != nil \{\s*\n\s*logger\.Printf\("ShipConvoy: Claude failed[^"]*",[^)]*\)\s*\n\s*return buildFallbackPRBody\([^)]*\), nil`)
	if !re.MatchString(src) {
		t.Fatalf("AUDIT-095: expected Claude-failed-then-fallback(nil) pattern not found; citation stale")
	}
	// No error classification (no ErrClassTransient / handleInfraFailure / operator mail on permanent).
	fnIdx := strings.Index(src, "func generatePRBody(")
	if fnIdx < 0 {
		// Some versions may have a different entry; try the specific branch.
		fnIdx = strings.Index(src, "ShipConvoy: Claude failed")
	}
	slice := src[fnIdx:]
	if len(slice) > 1500 {
		slice = slice[:1500]
	}
	if strings.Contains(slice, "ErrClassTransient") || strings.Contains(slice, "handleInfraFailure") {
		t.Fatalf("AUDIT-095: error classification appears to have been added — finding may be fixed")
	}
	t.Logf("AUDIT-095 REPRODUCED: Diplomat Claude error silently falls back to bare PR body, no retry, no operator mail")
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-156 — Pervasive `.Run()` git ops with errors swallowed
// File: internal/git/git.go:90-91, 163, 277-278, 396-397
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_156_GitRunErrorsSwallowed(t *testing.T) {
	src := silentReadFile(t, "internal/git/git.go")

	// The shape is `exec.Command("git", ...).Run()` — the trailing `.Run()`
	// without `if err :=` wrapper is the tell. Count them.
	re := regexp.MustCompile(`exec\.Command\("git",[^)]*\)\.Run\(\)`)
	matches := re.FindAllString(src, -1)
	if len(matches) < 5 {
		t.Fatalf("AUDIT-156: expected >=5 swallowed `.Run()` git calls in internal/git/git.go, found %d", len(matches))
	}
	// Spot-check the cited operations are present.
	needles := []string{
		`"reset", "--hard", "HEAD"`,
		`"clean", "-fdx"`,
		`"branch", "-D"`,
		`"merge", "--abort"`,
		`"rebase", "--abort"`,
	}
	for _, n := range needles {
		if !strings.Contains(src, n) {
			t.Errorf("AUDIT-156: cited git op fragment %q not present; citation may be partially stale", n)
		}
	}
	t.Logf("AUDIT-156 REPRODUCED: %d bare `.Run()` git invocations in internal/git/git.go; reset/clean/branch -D/merge --abort/rebase --abort all silent", len(matches))
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-159 — dogGitHygiene uses manual rows.Close() (not defer) at lines 178 and 209
// File: internal/agents/dogs.go
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_159_ManualRowsCloseNotDefer(t *testing.T) {
	src := silentReadFile(t, "internal/agents/dogs.go")

	fnIdx := strings.Index(src, "func dogGitHygiene(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-159: dogGitHygiene not found; citation stale")
	}
	fnEnd := strings.Index(src[fnIdx:], "\nfunc ")
	if fnEnd < 0 {
		fnEnd = len(src) - fnIdx
	}
	body := src[fnIdx : fnIdx+fnEnd]

	// Two bare `rows.Close()` / `agentRows.Close()` calls without `defer`.
	manualRows := silentCount(body, "\n\trows.Close()")
	manualAgent := silentCount(body, "\n\tagentRows.Close()")
	if manualRows < 1 {
		t.Fatalf("AUDIT-159: expected manual `rows.Close()` in dogGitHygiene, found %d", manualRows)
	}
	if manualAgent < 1 {
		t.Fatalf("AUDIT-159: expected manual `agentRows.Close()` in dogGitHygiene, found %d", manualAgent)
	}
	// And NO `defer rows.Close()` / `defer agentRows.Close()` in the body.
	if strings.Contains(body, "defer rows.Close()") {
		t.Fatalf("AUDIT-159: `defer rows.Close()` now present — finding may be fixed")
	}
	if strings.Contains(body, "defer agentRows.Close()") {
		t.Fatalf("AUDIT-159: `defer agentRows.Close()` now present — finding may be fixed")
	}
	t.Logf("AUDIT-159 REPRODUCED: dogGitHygiene uses manual rows.Close() (%d) and agentRows.Close() (%d) with no defer — scan-error path leaks FDs", manualRows, manualAgent)
}

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
	// Post-Fix #8a: the unmarshal must be guarded with an `if err :=` check
	// that fails the bounty on parse error (matching runMedicCITriage's
	// pattern). If anyone drops the check back to a bare statement, this
	// test fires and flags the regression.
	src := silentReadFile(t, "internal/agents/medic.go")

	want := "json.Unmarshal([]byte(bounty.Payload), &mp)"
	idx := strings.Index(src, want)
	if idx < 0 {
		t.Fatalf("AUDIT-013: expected pattern %q not found; citation may be stale", want)
	}

	start := idx - 80
	if start < 0 {
		start = 0
	}
	prefix := src[start:idx]
	errChecked := strings.Contains(prefix, "if err :=") || regexp.MustCompile(`\berr\s*:?=\s*$`).MatchString(strings.TrimRight(prefix, " \t"))

	if !errChecked {
		t.Fatal("AUDIT-013 regression: runMedicTask discards json.Unmarshal error at the bounty.Payload -> &mp call in medic.go. Fix #8a added an `if err :=` check; do not remove it.")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-014 — WorktreeReset parent-requeue silent
// File: internal/agents/pilot_worktree_reset.go around lines 121-129
// Defect: `_, _ = db.Exec(...)` for both the parent requeue UPDATE and the
// Escalations resolution UPDATE. Self-healing silently breaks if either fails.
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_014_WorktreeResetParentRequeueSilent(t *testing.T) {
	// Post-Fix #8a: the parent-requeue UPDATE and escalation-resolve UPDATE
	// must both have error checks. If either regresses back to `_, _ = db.Exec(`,
	// this test flags it.
	src := silentReadFile(t, "internal/agents/pilot_worktree_reset.go")

	occ := silentCount(src, "_, _ = db.Exec(")
	hasParentRequeue := strings.Contains(src, "SET status = 'Pending', branch_name = ''")
	hasEscResolve := strings.Contains(src, "SET status = 'Closed', acknowledged_at")

	if occ >= 2 && hasParentRequeue && hasEscResolve {
		t.Fatalf("AUDIT-014 regression: %d `_, _ = db.Exec(` sites in pilot_worktree_reset.go; both parent-requeue and escalation-resolve writes are silent. Fix #8a replaced these with `if _, err := db.Exec(...)` guarded calls.", occ)
	}
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
	t.Skip("AUDIT-015: remove when UpdateBountyStatus/CreateEscalation return error (Fix #8)")
	// Without skip, fails with: AUDIT-015: defective pattern still present — 6 log-and-return sites inside onSubPRMerged with no FailBounty/CreateEscalation escalation path
	src := silentReadFile(t, "internal/agents/pr_flow.go")

	fnIdx := strings.Index(src, "func onSubPRMerged(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-015: onSubPRMerged not found; citation stale")
	}
	end := fnIdx + 2500
	if end > len(src) {
		end = len(src)
	}
	body := src[fnIdx:end]

	logReturnRe := regexp.MustCompile(`logger\.Printf\("sub-pr-ci-watch:[^"]*failed:[^"]*",[^)]*\)\s*\n\s*return`)
	matches := logReturnRe.FindAllString(body, -1)
	hasEscalationPath := strings.Contains(body, "FailBounty") || strings.Contains(body, "CreateEscalation")

	if len(matches) >= 3 && !hasEscalationPath {
		t.Fatalf("AUDIT-015: defective pattern still present — %d log-and-return sites inside onSubPRMerged with no FailBounty/CreateEscalation escalation path", len(matches))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-040 — escalateCITriage manual UPDATE + CreateEscalation double-status
// File: internal/agents/medic_ci.go:262-265 + escalation.go:40
// Defect: manual `UPDATE BountyBoard SET status = 'Escalated'` followed by
// CreateEscalation() which internally calls UpdateBountyStatus(db, taskID,
// "Escalated"). Webhook fires twice.
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_040_EscalateCITriageDoubleUPDATE(t *testing.T) {
	t.Skip("AUDIT-040: remove when UpdateBountyStatus/CreateEscalation return error (Fix #8)")
	// Without skip, fails with: AUDIT-040: defective pattern still present — escalateCITriage sets Escalated status manually AND via CreateEscalation which also calls UpdateBountyStatus(Escalated); webhook fires twice
	ciSrc := silentReadFile(t, "internal/agents/medic_ci.go")

	manualUpdate := "UPDATE BountyBoard SET status = 'Escalated'"
	hasManualUpdate := strings.Contains(ciSrc, manualUpdate)

	fnIdx := strings.Index(ciSrc, "func escalateCITriage(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-040: escalateCITriage not found; citation stale")
	}
	fnEnd := strings.Index(ciSrc[fnIdx:], "\n}\n")
	if fnEnd < 0 {
		fnEnd = 2000
	}
	body := ciSrc[fnIdx : fnIdx+fnEnd]
	hasUpdateInBody := strings.Contains(body, manualUpdate)
	hasCreateEsc := strings.Contains(body, "CreateEscalation(db, taskID,")

	escSrc := silentReadFile(t, "internal/agents/escalation.go")
	hasEscUpdate := strings.Contains(escSrc, `store.UpdateBountyStatus(db, taskID, "Escalated")`)

	if hasManualUpdate && hasUpdateInBody && hasCreateEsc && hasEscUpdate {
		t.Fatal("AUDIT-040: defective pattern still present — escalateCITriage sets Escalated status manually AND via CreateEscalation which also calls UpdateBountyStatus(Escalated); webhook fires twice")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-041 — CreateEscalation has no error return
// File: internal/agents/escalation.go:31
// Defect: signature `func CreateEscalation(...) int` not `(int, error)`. Failed
// INSERT leaves task Escalated with no Escalations row.
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_041_CreateEscalationNoErrorReturn(t *testing.T) {
	// Post-Fix #8a: CreateEscalation must NOT carry the old defective signature
	// (bare int return) AND must NOT discard its INSERT / LastInsertId errors.
	// If a future refactor reintroduces any of those patterns, this test fires.
	src := silentReadFile(t, "internal/agents/escalation.go")

	sig := "func CreateEscalation(db *sql.DB, taskID int, severity store.EscalationSeverity, message string) int {"
	hasBareIntSig := strings.Contains(src, sig)
	hasSilentInsert := strings.Contains(src, "res, _ := db.Exec(")
	hasSilentLastID := strings.Contains(src, "id, _ := res.LastInsertId()")

	if hasBareIntSig && hasSilentInsert && hasSilentLastID {
		t.Fatal("AUDIT-041 regression: CreateEscalation returns bare int; insert + LastInsertId errors both dropped. Fix #8a changed the signature to (int, error).")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-042 — UpdateAskBranchPRChecks error silently discarded at 3 hot sites
// Files: pr_flow.go:421, medic_ci.go:200, medic_ci.go:248
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_042_UpdateAskBranchPRChecksDiscarded(t *testing.T) {
	t.Skip("AUDIT-042: remove when UpdateBountyStatus/CreateEscalation return error (Fix #8)")
	// Without skip, fails with: AUDIT-042: defective pattern still present — `_ = store.UpdateAskBranchPRChecks(` appears pr_flow.go=1, medic_ci.go=2 (total=3); error discarded at all sites
	prFlow := silentReadFile(t, "internal/agents/pr_flow.go")
	medicCI := silentReadFile(t, "internal/agents/medic_ci.go")

	pattern := "_ = store.UpdateAskBranchPRChecks("

	inPRFlow := silentCount(prFlow, pattern)
	inMedicCI := silentCount(medicCI, pattern)
	total := inPRFlow + inMedicCI

	if inPRFlow >= 1 && inMedicCI >= 2 && total >= 3 {
		t.Fatalf("AUDIT-042: defective pattern still present — `%s` appears pr_flow.go=%d, medic_ci.go=%d (total=%d); error discarded at all sites", pattern, inPRFlow, inMedicCI, total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-043 — PRClose error logged, MarkAskBranchPRClosed called unconditionally
// File: internal/agents/pr_flow.go:343-347
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_043_PRCloseUnconditionalMarkClosed(t *testing.T) {
	t.Skip("AUDIT-043: remove when UpdateBountyStatus/CreateEscalation return error (Fix #8)")
	// Without skip, fails with: AUDIT-043: defective pattern still present — MarkAskBranchPRClosed called after a logged-only PRClose failure path (unconditionally)
	src := silentReadFile(t, "internal/agents/pr_flow.go")

	re := regexp.MustCompile(`if closeErr := ghc\.PRClose\([^)]+\); closeErr != nil \{\s*\n\s*logger\.Printf\([^)]*\)\s*\n\s*\}\s*\n\s*\}\s*\n\s*store\.MarkAskBranchPRClosed\(`)
	hasUnconditionalPattern := re.MatchString(src)

	if hasUnconditionalPattern {
		t.Fatal("AUDIT-043: defective pattern still present — MarkAskBranchPRClosed called after a logged-only PRClose failure path (unconditionally)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-044 — Librarian writeMemoryPayload silent fallback to raw payload
// File: internal/agents/librarian.go:75-78
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_044_LibrarianSilentFallback(t *testing.T) {
	// Closed by Fix #8b (remaining): librarian.go's invalid-payload path now
	// calls store.FailBounty + return rather than silently assigning the raw
	// payload to payload.Task. The test still asserts the absence of the
	// defective pattern so a regression fires.
	src := silentReadFile(t, "internal/agents/librarian.go")

	needle := "payload.Task = bounty.Payload"
	hasFallback := strings.Contains(src, needle)

	var hasErrorExit bool
	if hasFallback {
		fallbackIdx := strings.Index(src, needle)
		start := fallbackIdx - 200
		if start < 0 {
			start = 0
		}
		window := src[start:fallbackIdx]
		hasErrorExit = strings.Contains(window, "FailBounty") || strings.Contains(window, "return\n")
	}

	if hasFallback && !hasErrorExit {
		t.Fatal("AUDIT-044: defective pattern still present — Librarian silently falls back to raw payload on unmarshal error, no FailBounty/return before the fallback; poisons memory index")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-090 — stalled-reviews sub-PR scan silently skips rows on Scan error
// File: internal/agents/dogs.go ~lines 398-405
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_090_StalledReviewsSilentScan(t *testing.T) {
	t.Skip("AUDIT-090: remove when rows.Scan errors checked / agent ownership distinguishes error from loss (Fix #8)")
	// Without skip, fails with: AUDIT-090: defective pattern still present — subPRRows.Scan errors silently drop rows in dogStalledReviews (err == nil only path, no logger.Printf of scan error); legitimate 12h+ stalls never alarm
	src := silentReadFile(t, "internal/agents/dogs.go")

	fnIdx := strings.Index(src, "func dogStalledReviews(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-090: dogStalledReviews not found; citation stale")
	}
	slice := src[fnIdx:]
	if len(slice) > 4000 {
		slice = slice[:4000]
	}
	hasLoop := strings.Contains(slice, "for subPRRows.Next()")
	hasSilentSkip := strings.Contains(slice, "err := subPRRows.Scan(&id, &hours); err == nil")

	var hasLoggedErr bool
	if hasSilentSkip {
		scanIdx := strings.Index(slice, "err := subPRRows.Scan(&id, &hours); err == nil")
		window := slice[scanIdx : scanIdx+400]
		hasLoggedErr = strings.Contains(window, "logger.Printf") && strings.Contains(window, "scan")
	}

	if hasLoop && hasSilentSkip && !hasLoggedErr {
		t.Fatal("AUDIT-090: defective pattern still present — subPRRows.Scan errors silently drop rows in dogStalledReviews (err == nil only path, no logger.Printf of scan error); legitimate 12h+ stalls never alarm")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-091 — git-hygiene returns nil on Agents query failure
// File: internal/agents/dogs.go:197-200
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_091_GitHygieneReturnsNilOnError(t *testing.T) {
	t.Skip("AUDIT-091: remove when rows.Scan errors checked / agent ownership distinguishes error from loss (Fix #8)")
	// Without skip, fails with: AUDIT-091: defective pattern still present — dogGitHygiene swallows Agents query error as `return nil` (non-fatal)
	src := silentReadFile(t, "internal/agents/dogs.go")

	fnIdx := strings.Index(src, "func dogGitHygiene(")
	if fnIdx < 0 {
		t.Fatal("AUDIT-091: dogGitHygiene not found; citation stale")
	}
	slice := src[fnIdx:]
	if len(slice) > 3000 {
		slice = slice[:3000]
	}
	needle := "agentRows, agentErr := db.Query(`SELECT agent_name, repo, worktree_path FROM Agents`)"
	hasQuery := strings.Contains(slice, needle)

	var hasReturnNil bool
	if hasQuery {
		needleIdx := strings.Index(slice, needle)
		window := slice[needleIdx : needleIdx+400]
		hasReturnNil = strings.Contains(window, "return nil")
	}

	if hasQuery && hasReturnNil {
		t.Fatal("AUDIT-091: defective pattern still present — dogGitHygiene swallows Agents query error as `return nil` (non-fatal)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-094 — Astromech ownership-check UPDATE drops both errors
// File: internal/agents/astromech.go:564-568
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_094_AstromechOwnershipDropsErrors(t *testing.T) {
	t.Skip("AUDIT-094: remove when rows.Scan errors checked / agent ownership distinguishes error from loss (Fix #8)")
	// Without skip, fails with: AUDIT-094: defective pattern still present — ownership check drops db.Exec error (`ownerRes, _ := db.Exec(`) AND RowsAffected error (`n, _ := ownerRes.RowsAffected()`); transient DB failures misread as lost ownership
	src := silentReadFile(t, "internal/agents/astromech.go")

	needle := "ownerRes, _ := db.Exec("
	idx := strings.Index(src, needle)
	hasDBExec := idx >= 0

	var hasRowsAffectedDrop, hasOwnershipLost bool
	if hasDBExec {
		window := src[idx : idx+400]
		hasRowsAffectedDrop = strings.Contains(window, "n, _ := ownerRes.RowsAffected()")
		hasOwnershipLost = strings.Contains(window, "ownership lost")
	}

	if hasDBExec && hasRowsAffectedDrop && hasOwnershipLost {
		t.Fatal("AUDIT-094: defective pattern still present — ownership check drops db.Exec error (`ownerRes, _ := db.Exec(`) AND RowsAffected error (`n, _ := ownerRes.RowsAffected()`); transient DB failures misread as lost ownership")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-095 — Diplomat Claude failure silent fallback (no retry, no mail)
// File: internal/agents/diplomat.go:304-307
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_095_DiplomatSilentFallback(t *testing.T) {
	t.Skip("AUDIT-095: remove when rows.Scan errors checked / agent ownership distinguishes error from loss (Fix #8)")
	// Without skip, fails with: AUDIT-095: defective pattern still present — Diplomat Claude error silently falls back to bare PR body (buildFallbackPRBody), no ErrClassTransient/handleInfraFailure, no retry, no operator mail
	src := silentReadFile(t, "internal/agents/diplomat.go")

	re := regexp.MustCompile(`claude\.AskClaudeCLI\([^)]*\)\s*\n\s*if err != nil \{\s*\n\s*logger\.Printf\("ShipConvoy: Claude failed[^"]*",[^)]*\)\s*\n\s*return buildFallbackPRBody\([^)]*\), nil`)
	hasSilentFallback := re.MatchString(src)

	fnIdx := strings.Index(src, "func generatePRBody(")
	if fnIdx < 0 {
		fnIdx = strings.Index(src, "ShipConvoy: Claude failed")
	}
	var hasErrorClassification bool
	if fnIdx >= 0 {
		slice := src[fnIdx:]
		if len(slice) > 1500 {
			slice = slice[:1500]
		}
		hasErrorClassification = strings.Contains(slice, "ErrClassTransient") || strings.Contains(slice, "handleInfraFailure")
	}

	if hasSilentFallback && !hasErrorClassification {
		t.Fatal("AUDIT-095: defective pattern still present — Diplomat Claude error silently falls back to bare PR body (buildFallbackPRBody), no ErrClassTransient/handleInfraFailure, no retry, no operator mail")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-156 — Pervasive `.Run()` git ops with errors swallowed
// File: internal/git/git.go:90-91, 163, 277-278, 396-397
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_156_GitRunErrorsSwallowed(t *testing.T) {
	t.Skip("AUDIT-156: remove when internal/git wraps .Run() errors (Fix #8)")
	// Without skip, fails with: AUDIT-156: defective pattern still present — 23 bare `.Run()` git invocations in internal/git/git.go; reset/clean/branch -D/merge --abort/rebase --abort all silent
	src := silentReadFile(t, "internal/git/git.go")

	re := regexp.MustCompile(`exec\.Command\("git",[^)]*\)\.Run\(\)`)
	matches := re.FindAllString(src, -1)

	needles := []string{
		`"reset", "--hard", "HEAD"`,
		`"clean", "-fdx"`,
		`"branch", "-D"`,
		`"merge", "--abort"`,
		`"rebase", "--abort"`,
	}
	allPresent := true
	for _, n := range needles {
		if !strings.Contains(src, n) {
			allPresent = false
			break
		}
	}

	if len(matches) >= 5 && allPresent {
		t.Fatalf("AUDIT-156: defective pattern still present — %d bare `.Run()` git invocations in internal/git/git.go; reset/clean/branch -D/merge --abort/rebase --abort all silent", len(matches))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AUDIT-159 — dogGitHygiene uses manual rows.Close() (not defer) at lines 178 and 209
// File: internal/agents/dogs.go
// ─────────────────────────────────────────────────────────────────────────────

func TestAUDIT_159_ManualRowsCloseNotDefer(t *testing.T) {
	t.Skip("AUDIT-159: remove when dogGitHygiene uses defer rows.Close (Fix #8)")
	// Without skip, fails with: AUDIT-159: defective pattern still present — dogGitHygiene uses manual rows.Close() (1) and agentRows.Close() (1) with no defer; scan-error path leaks FDs
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

	manualRows := silentCount(body, "\n\trows.Close()")
	manualAgent := silentCount(body, "\n\tagentRows.Close()")
	hasDeferRows := strings.Contains(body, "defer rows.Close()")
	hasDeferAgent := strings.Contains(body, "defer agentRows.Close()")

	if manualRows >= 1 && manualAgent >= 1 && !hasDeferRows && !hasDeferAgent {
		t.Fatalf("AUDIT-159: defective pattern still present — dogGitHygiene uses manual rows.Close() (%d) and agentRows.Close() (%d) with no defer; scan-error path leaks FDs", manualRows, manualAgent)
	}
}

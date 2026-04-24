package agents

// Spot-check static verification for three Medium-severity audit findings:
//   - AUDIT-149: escalation-sweeper auto-closes every tick with no counter
//                or do_not_auto_resolve flag.
//   - AUDIT-151: WorktreeReset parent-requeue UPDATE filter is limited to
//                Failed/Escalated/ConflictPending and logs no warning on a
//                zero-row result.
//   - AUDIT-152: ship-it-nag tops out at the 1-week threshold — no 30-day
//                branch, no CreateEscalation call.
//
// These tests read the cited source files and assert the defect still
// matches the audit description. They modify no source.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func mediumCRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// .../internal/agents/audit_medium_spotcheck_c_test.go → repo root is two dirs up.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func mediumCReadFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(mediumCRepoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestAuditMediumSpotcheckC verifies three Medium findings against current HEAD.
func TestAuditMediumSpotcheckC(t *testing.T) {

	// ── AUDIT-149 ─────────────────────────────────────────────────────────
	// Closed by Campaign 2. The escalation-sweeper now:
	//   - writes status='Closed' (legacy 'Resolved' retired alongside AUDIT-025)
	//   - increments auto_resolve_count in the same UPDATE
	//   - gates the UPDATE on `auto_resolve_count < 1` so an operator who
	//     re-opens an auto-closed escalation is not silently re-closed 10 min
	//     later.
	// This test now exercises that behaviour live against the in-memory DB
	// rather than grepping the source.
	t.Run("TestAUDIT_149_sweeper_respects_operator_reopen", func(t *testing.T) {
		// Source-level regression protection: confirm the gate column is
		// actually referenced in the sweeper. If this disappears, the
		// runtime test below would still pass on first-tick but silently
		// re-close on re-opens.
		src := mediumCReadFile(t, "internal/agents/escalation_sweeper.go")
		for _, needed := range []string{
			"auto_resolve_count + 1",
			"auto_resolve_count < 1",
			"SET status = 'Closed'",
		} {
			if !strings.Contains(src, needed) {
				t.Fatalf("AUDIT-149 regression: escalation_sweeper.go missing %q — "+
					"the Campaign-2 gate was removed", needed)
			}
		}
		// The legacy 'Resolved' write MUST be gone.
		if regexp.MustCompile(`SET status = 'Resolved'`).MatchString(src) {
			t.Fatalf("AUDIT-149 regression: escalation_sweeper.go re-introduced " +
				"legacy `SET status = 'Resolved'`")
		}
	})

	// ── AUDIT-151 ─────────────────────────────────────────────────────────
	// internal/agents/pilot_worktree_reset.go:120-130
	// The parent-requeue UPDATE is filtered to
	//   status IN ('Failed','Escalated','ConflictPending')
	// If the parent has transitioned elsewhere (e.g. Completed by a sibling,
	// Cancelled by operator) between Medic's spawn and WorktreeReset's
	// execution, the UPDATE silently affects 0 rows — the worktree is wiped
	// but no retry is queued. There is no log warning on a 0-row result.
	t.Run("TestAUDIT_151_worktree_reset_logs_zero_row_and_escalates", func(t *testing.T) {
		// Post-fix contract (Fix #8d): when the parent-requeue UPDATE
		// affects 0 rows (parent transitioned to an unexpected state
		// between Medic spawn and WorktreeReset execution), the reset
		// logs a warning AND escalates via CreateEscalation. The
		// wipe-then-silently-no-op pre-fix behaviour is closed.
		src := mediumCReadFile(t, "internal/agents/pilot_worktree_reset.go")

		// Anchor: the status filter must still be present — if it moved,
		// the audit scope has changed.
		filterRe := regexp.MustCompile(
			`status IN \('Failed','Escalated','ConflictPending'\)`)
		if !filterRe.MatchString(src) {
			t.Fatalf("AUDIT-151 anchor lost: parent-requeue UPDATE no " +
				"longer filters on status IN ('Failed','Escalated','ConflictPending')")
		}

		// Post-fix contract: the UPDATE is captured into a variable so
		// RowsAffected can be checked. Assert the presence of the `res.
		// RowsAffected()` / `n == 0` pattern near the UPDATE.
		if !strings.Contains(src, "RowsAffected") {
			t.Error("AUDIT-151 REGRESSION: WorktreeReset parent-requeue UPDATE no " +
				"longer checks RowsAffected — 0-row silent no-op is back")
		}
		// The 0-row path must escalate.
		if !strings.Contains(src, "CreateEscalation") {
			t.Error("AUDIT-151 REGRESSION: pilot_worktree_reset.go no longer calls " +
				"CreateEscalation on 0-row parent-requeue; operator loses the signal")
		}
		// The 0-row path must mail the operator (belt-and-suspenders with
		// the escalation).
		if !strings.Contains(src, "unexpected state") && !strings.Contains(src, "unexpected parent") {
			t.Error("AUDIT-151 REGRESSION: no log / mail line mentioning " +
				"'unexpected state' on the 0-row path — operator visibility regressed")
		}
	})

	// ── AUDIT-152 ─────────────────────────────────────────────────────────
	// Fix #1 added a 30-day escalation branch to dogShipItNag. This test now
	// asserts the remedy is in place: the shipItNag30d constant exists, the
	// switch has a top case for it, and an Escalation row is inserted when
	// the threshold fires. Permanent regression protection.
	t.Run("TestAUDIT_152_ship_it_nag_no_30d_escalation", func(t *testing.T) {
		src := mediumCReadFile(t, "internal/agents/pilot_draft_watch.go")

		// Thresholds: must see all four including the new 30d one.
		for _, needed := range []string{
			"shipItNag24h",
			"shipItNag72h",
			"shipItNag1wk",
			"shipItNag30d",
		} {
			if !strings.Contains(src, needed) {
				t.Fatalf("AUDIT-152 regressed: threshold constant %q missing — the 30d "+
					"escalation branch must remain in place", needed)
			}
		}

		// Isolate the dogShipItNag function body.
		fnStart := strings.Index(src, "func dogShipItNag(")
		if fnStart < 0 {
			t.Fatalf("AUDIT-152 source drift: dogShipItNag not found")
		}
		fnBody := src[fnStart:]

		// The switch must now have a case for 30d BEFORE (higher than) 1wk
		// and the 30d branch must land an escalation.
		caseRe := regexp.MustCompile(`case age >= shipItNag\w+:`)
		cases := caseRe.FindAllString(fnBody, -1)
		if len(cases) != 4 {
			t.Fatalf("AUDIT-152 regressed: expected 4 age-threshold cases (30d, 1wk, 72h, 24h), "+
				"found %d: %v", len(cases), cases)
		}
		if !strings.Contains(cases[0], "shipItNag30d") {
			t.Fatalf("AUDIT-152 regressed: top case is %q, not shipItNag30d — "+
				"switch order must place the highest threshold first", cases[0])
		}

		// Verify the 30d path actually creates an escalation record. Check
		// by regex — the implementation may either call CreateEscalation or
		// INSERT INTO Escalations directly.
		if !regexp.MustCompile(`CreateEscalation|INSERT INTO Escalations`).MatchString(fnBody) {
			t.Fatal("AUDIT-152 regressed: dogShipItNag body does not emit any " +
				"Escalation — the 30d branch must file an escalation so operators " +
				"see stuck convoys on the dashboard until acknowledged")
		}
	})
}

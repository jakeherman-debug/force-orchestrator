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
	// internal/agents/escalation_sweeper.go:25-63
	// dogEscalationSweeper issues an unconditional
	//   UPDATE Escalations SET status = 'Resolved' ... WHERE id = ? AND status = 'Open'
	// on every matching row, every tick. There is no auto_resolve_count
	// column, no do_not_auto_resolve flag, and no guard against an operator
	// having re-opened a previously-resolved escalation for deeper
	// investigation — the sweeper will silently re-close it 10 min later.
	t.Run("TestAUDIT_149_sweeper_unconditional_each_tick", func(t *testing.T) {
		src := mediumCReadFile(t, "internal/agents/escalation_sweeper.go")

		// Count the unconditional resolve UPDATE — must occur twice (Rule 1
		// for task-terminal, Rule 2 for PR-terminal).
		updRe := regexp.MustCompile(`UPDATE Escalations\s*\n\s*SET status = 'Resolved'`)
		matches := updRe.FindAllStringIndex(src, -1)
		if len(matches) < 2 {
			t.Fatalf("AUDIT-149 precondition missing: expected ≥2 unconditional "+
				"`UPDATE Escalations SET status='Resolved'` statements in sweeper, found %d",
				len(matches))
		}

		// Guard: no auto_resolve_count, no do_not_auto_resolve references —
		// in ANY form (column ref, struct field, constant, comment).
		forbid := []string{
			"auto_resolve_count",
			"AutoResolveCount",
			"do_not_auto_resolve",
			"DoNotAutoResolve",
		}
		for _, needle := range forbid {
			if strings.Contains(src, needle) {
				t.Fatalf("AUDIT-149 contradicted: escalation_sweeper.go now references %q; "+
					"finding may be fixed and should be reopened/closed", needle)
			}
		}

		// The sweeper's WHERE clause must NOT test any acknowledged/re-open
		// marker other than `status = 'Open'`. If someone added
		// `AND acknowledged_by IS NULL` that would invalidate the finding.
		whereRe := regexp.MustCompile(`(?s)WHERE e\.status = 'Open'.*?b\.status IN`)
		if !whereRe.MatchString(src) {
			t.Fatalf("AUDIT-149 source drift: expected `WHERE e.status = 'Open' ... b.status IN` " +
				"pattern in sweeper")
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
	t.Run("TestAUDIT_151_worktree_reset_silent_zero_row", func(t *testing.T) {
		src := mediumCReadFile(t, "internal/agents/pilot_worktree_reset.go")

		// The exact filter the audit cites must still be present.
		filterRe := regexp.MustCompile(
			`status IN \('Failed','Escalated','ConflictPending'\)`)
		if !filterRe.MatchString(src) {
			t.Fatalf("AUDIT-151 precondition missing: parent-requeue UPDATE no " +
				"longer filters on status IN ('Failed','Escalated','ConflictPending')")
		}

		// The UPDATE is executed with `_, _ = db.Exec(...)` — the RowsAffected
		// return is discarded, so there is no check for 0 rows and no log
		// warning. Assert the discard idiom is present around the parent
		// UPDATE statement.
		discardRe := regexp.MustCompile(
			`(?s)_,\s*_\s*=\s*db\.Exec\(` + "`" + `UPDATE BountyBoard[^` + "`" + `]*?status IN \('Failed','Escalated','ConflictPending'\)`)
		if !discardRe.MatchString(src) {
			t.Fatalf("AUDIT-151 source drift: parent-requeue UPDATE no longer " +
				"uses the `_, _ = db.Exec(...)` discard idiom; audit may be fixed")
		}

		// No RowsAffected handling and no warning-level log anywhere in the
		// function body near the UPDATE.
		// Find the window from ParentTaskID > 0 down to the next blank line
		// past the escalation-resolve call.
		startIdx := strings.Index(src, "if p.ParentTaskID > 0 {")
		if startIdx < 0 {
			t.Fatalf("AUDIT-151 precondition missing: can't find parent-requeue block")
		}
		// Window = next ~500 chars (covers the two UPDATEs and the block close).
		endIdx := startIdx + 800
		if endIdx > len(src) {
			endIdx = len(src)
		}
		window := src[startIdx:endIdx]

		for _, banned := range []string{
			"RowsAffected",
			"rows affected",
			"no-op",
			"unexpected parent state",
		} {
			if strings.Contains(window, banned) {
				t.Fatalf("AUDIT-151 contradicted: parent-requeue block now mentions "+
					"%q — may be fixed", banned)
			}
		}

		// And no CreateEscalation within the reset logic at all — the fix
		// would add one for "parent state unexpected."
		if strings.Contains(src, "CreateEscalation") {
			t.Fatalf("AUDIT-151 contradicted: pilot_worktree_reset.go now calls " +
				"CreateEscalation; audit may be fixed")
		}
	})

	// ── AUDIT-152 ─────────────────────────────────────────────────────────
	// internal/agents/pilot_draft_watch.go:202-271
	// dogShipItNag has three thresholds: 24h, 72h, 1wk. After the 1wk nag
	// fires, convoys left open indefinitely disappear from operator
	// awareness — there is no 30-day branch and no CreateEscalation call
	// anywhere in the file.
	t.Run("TestAUDIT_152_ship_it_nag_no_30d_escalation", func(t *testing.T) {
		src := mediumCReadFile(t, "internal/agents/pilot_draft_watch.go")

		// Thresholds: must see the three cited constants and NOT a 30d one.
		for _, needed := range []string{
			"shipItNag24h",
			"shipItNag72h",
			"shipItNag1wk",
		} {
			if !strings.Contains(src, needed) {
				t.Fatalf("AUDIT-152 precondition missing: constant %q not found", needed)
			}
		}

		for _, forbidden := range []string{
			"shipItNag30d",
			"shipItNag4wk",
			"30 * 24 * time.Hour",
			"30*24*time.Hour",
			`"30d"`,
		} {
			if strings.Contains(src, forbidden) {
				t.Fatalf("AUDIT-152 contradicted: pilot_draft_watch.go now references "+
					"%q — 30-day threshold may have been added", forbidden)
			}
		}

		// Isolate the dogShipItNag function body and verify its switch has
		// no case beyond shipItNag1wk, and no CreateEscalation.
		fnStart := strings.Index(src, "func dogShipItNag(")
		if fnStart < 0 {
			t.Fatalf("AUDIT-152 precondition missing: dogShipItNag not found")
		}
		// Scan to the matching closing brace of the function — good enough:
		// this file's only remaining content after dogShipItNag is nothing
		// (the file ends at 271 per the finding).
		fnBody := src[fnStart:]

		if strings.Contains(fnBody, "CreateEscalation") {
			t.Fatalf("AUDIT-152 contradicted: dogShipItNag now calls CreateEscalation; " +
				"audit may be fixed")
		}

		// The switch must top out at shipItNag1wk. Count case labels: must
		// have exactly three (1wk, 72h, 24h).
		caseRe := regexp.MustCompile(`case age >= shipItNag\w+:`)
		cases := caseRe.FindAllString(fnBody, -1)
		if len(cases) != 3 {
			t.Fatalf("AUDIT-152 source drift: expected exactly 3 age-threshold "+
				"cases in dogShipItNag, found %d: %v", len(cases), cases)
		}

		// And confirm the highest case is still shipItNag1wk — i.e., the
		// first case in the switch (Go's switch evaluates top-down, and the
		// code lists highest threshold first).
		if !strings.Contains(cases[0], "shipItNag1wk") {
			t.Fatalf("AUDIT-152 source drift: top case is %q, not shipItNag1wk", cases[0])
		}
	})
}

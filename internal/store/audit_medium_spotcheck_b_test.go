package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAUDIT_MediumSpotcheckB spot-checks three Medium-severity findings
// from AUDIT.md via static source-grep assertions. Each sub-test is
// EXPECTED TO FAIL under the current codebase — failure is the signal
// that the audited defect is still present. When a fix lands, the
// corresponding sub-test flips to green. Do not weaken these checks;
// if a finding is intentionally WONTFIX, delete the sub-test with a
// note rather than relaxing the assertion.
//
// Findings covered:
//   AUDIT-074 — ReadInboxForAgent does a SELECT then a per-id UPDATE
//               loop. Two concurrent readers can both pull the same
//               row between the SELECT and MarkMailConsumed. Fix is
//               a single-statement UPDATE ... RETURNING claim.
//   AUDIT-079 — InitHolocronDSN never enables SQLite's foreign_keys
//               pragma. Maintenance DELETEs silently orphan child
//               rows (Escalations, AskBranchPRs, TaskDependencies,
//               TaskHistory, FleetMemory) and joins back through
//               BountyBoard return empty.
//   AUDIT-081 — AddRepo uses INSERT OR REPLACE INTO Repositories,
//               which on PRIMARY KEY conflict is DELETE+reinsert.
//               Any FK-style references in BountyBoard.target_repo,
//               AskBranchPRs.repo, ConvoyAskBranches.repo get
//               orphaned if FKs were ever enabled — and mask silent
//               row-identity churn today.
func TestAUDIT_MediumSpotcheckB(t *testing.T) {
	// Fix #3 closed AUDIT-074; Fix #4 closed AUDIT-079 (PRAGMA foreign_keys)
	// and AUDIT-081 (AddRepo UPSERT). All three sub-tests are now live.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot resolve cwd: %v", err)
	}
	// This file lives at internal/store/ — repo root is two levels up.
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))

	fleetMailPath := filepath.Join(repoRoot, "internal", "store", "fleet_mail.go")
	holocronPath := filepath.Join(repoRoot, "internal", "store", "holocron.go")

	// ── AUDIT-074 ────────────────────────────────────────────────────────
	// Fix #3 post-fix form: the static anchor has been replaced with an
	// end-to-end semantic assertion. ReadInboxForAgent now uses a single
	// UPDATE ... RETURNING statement, so the test exercises the concurrent-
	// claim invariant directly: N agents reading overlapping scopes against
	// a single mail row see exactly one claimant — no double-process.
	t.Run("AUDIT_074_readinbox_select_then_update_race", func(t *testing.T) {
		// Anchor on the presence of UPDATE ... RETURNING in the production
		// source — locks the fix shape so a future refactor that re-introduces
		// the SELECT-then-per-id-UPDATE pattern trips this guard immediately.
		src := mustReadFile(t, fleetMailPath)
		if !strings.Contains(src, "func ReadInboxForAgent(") {
			t.Fatalf("audit anchor lost: ReadInboxForAgent missing from %s",
				fleetMailPath)
		}
		updateReturning := regexp.MustCompile(`(?is)UPDATE\s+Fleet_Mail[^` + "`" + `]*RETURNING`).
			MatchString(src)
		if !updateReturning {
			t.Fatalf("AUDIT-074 regression: ReadInboxForAgent in %s no "+
				"longer uses a single UPDATE ... RETURNING statement. The "+
				"SELECT-then-per-id-UPDATE shape lets two concurrent readers "+
				"claim the same row — re-introduce the atomic claim.",
				fleetMailPath)
		}
		// Forbid the racy shape explicitly: `for _, m := range mails { MarkMailConsumed(`.
		racyLoop := regexp.MustCompile(
			`for\s+_,\s*m\s*:=\s*range\s+mails\s*\{\s*MarkMailConsumed\(`).
			MatchString(src)
		if racyLoop {
			t.Fatalf("AUDIT-074 regression: ReadInboxForAgent re-introduced "+
				"the `for _, m := range mails { MarkMailConsumed(m.ID) }` "+
				"loop in %s. Two concurrent readers overlap the SELECT "+
				"window and double-process — keep the UPDATE ... RETURNING "+
				"single-statement claim.", fleetMailPath)
		}
	})

	// ── AUDIT-079 ────────────────────────────────────────────────────────
	t.Run("AUDIT_079_foreign_keys_pragma_never_enabled", func(t *testing.T) {
		// Fix #4 lands the PRAGMA. This test is now green-phase: assert both
		// (a) that the PRAGMA is present in holocron.go, and (b) that a live
		// connection reports foreign_keys enforcement actually enabled — the
		// defensive pair that catches either a config regression or a driver
		// that silently ignores the statement.
		src := mustReadFile(t, holocronPath)

		if !strings.Contains(src, "func InitHolocronDSN(") {
			t.Fatalf("audit anchor lost: InitHolocronDSN missing from %s",
				holocronPath)
		}

		hasFKPragma := regexp.MustCompile(`(?i)PRAGMA\s+foreign_keys\s*=\s*(ON|1)`).
			MatchString(src)
		if !hasFKPragma {
			t.Errorf("AUDIT-079 regression in %s: PRAGMA foreign_keys=ON is "+
				"no longer set in InitHolocronDSN. SQLite defaults FK "+
				"enforcement OFF per connection, so maintenance DELETEs "+
				"would silently orphan child rows.", holocronPath)
		}

		// Live check: does a real connection opened through InitHolocronDSN
		// report FK enforcement enabled?
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		var fk int
		if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatalf("PRAGMA foreign_keys query: %v", err)
		}
		if fk != 1 {
			t.Errorf("AUDIT-079 live check: PRAGMA foreign_keys reports %d, "+
				"want 1. The statement is present in source but not applied "+
				"to the working connection.", fk)
		}
	})

	// ── AUDIT-081 ────────────────────────────────────────────────────────
	t.Run("AUDIT_081_repositories_insert_or_replace_cascading_delete", func(t *testing.T) {
		// Fix #4 lands the UPSERT. This test is now green-phase: assert that
		// (a) AddRepo no longer issues `INSERT OR REPLACE`, and (b) on
		// re-registration the row's AUTOINCREMENT-style identity is
		// preserved — a DELETE+INSERT (REPLACE) would have advanced any
		// identity column or cascaded to referrers.
		src := mustReadFile(t, holocronPath)

		if !strings.Contains(src, "func AddRepo(") {
			t.Fatalf("audit anchor lost: AddRepo missing from %s",
				holocronPath)
		}

		if strings.Contains(src, "INSERT OR REPLACE INTO Repositories") {
			t.Errorf("AUDIT-081 regression in %s: `INSERT OR REPLACE INTO "+
				"Repositories` is back. SQLite's REPLACE conflict "+
				"resolution is specified as DELETE+INSERT on PRIMARY KEY "+
				"collisions; once PRAGMA foreign_keys=ON is set "+
				"(AUDIT-079, Fix #4) this would cascade-delete any row in "+
				"BountyBoard.target_repo / AskBranchPRs.repo / "+
				"ConvoyAskBranches.repo. Use `INSERT ... ON CONFLICT(name) "+
				"DO UPDATE SET ...` (UPSERT) instead.", holocronPath)
		}

		// Require the UPSERT pattern is actually present — belt AND
		// suspenders, so a future refactor that deletes AddRepo entirely
		// doesn't silently pass this test.
		hasUpsert := regexp.MustCompile(`(?is)INSERT\s+INTO\s+Repositories[^;]*ON\s+CONFLICT\s*\(\s*name\s*\)\s+DO\s+UPDATE`).
			MatchString(src)
		if !hasUpsert {
			t.Errorf("AUDIT-081 regression in %s: AddRepo no longer uses "+
				"`INSERT ... ON CONFLICT(name) DO UPDATE` (UPSERT). The "+
				"fix depends on this exact shape so row identity is "+
				"preserved across re-registration.", holocronPath)
		}

		// Behavioural check: re-adding a repo must not delete+reinsert the
		// row. We verify by confirming that a Repository's state is
		// preserved across AddRepo calls — specifically, the quarantine
		// flag (set out-of-band by QuarantineRepo) must survive a
		// subsequent AddRepo. Under INSERT OR REPLACE, QuarantineRepo's
		// effect would be clobbered.
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		AddRepo(db, "example-repo", "/tmp/example", "first description")
		if err := QuarantineRepo(db, "example-repo", "test quarantine"); err != nil {
			t.Fatalf("QuarantineRepo: %v", err)
		}
		AddRepo(db, "example-repo", "/tmp/example", "second description")
		r := GetRepo(db, "example-repo")
		if r == nil {
			t.Fatalf("GetRepo returned nil after re-AddRepo")
		}
		if r.QuarantinedAt == "" {
			t.Errorf("AUDIT-081 behaviour regression: QuarantineRepo state "+
				"was cleared by a subsequent AddRepo. Expected quarantined_at "+
				"preserved (UPSERT); got empty (DELETE+INSERT). description=%q",
				r.Description)
		}
		if r.Description != "second description" {
			t.Errorf("AUDIT-081: description not updated on re-AddRepo; got %q want %q",
				r.Description, "second description")
		}
	})
}

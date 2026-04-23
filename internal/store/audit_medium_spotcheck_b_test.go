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
	// Umbrella test — each sub-test keeps its own skip until the matching
	// fix lands. Fix #4 removed the skips on AUDIT_079 (PRAGMA foreign_keys)
	// and AUDIT_081 (AddRepo UPSERT); AUDIT_074 stays skipped until Fix #3.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot resolve cwd: %v", err)
	}
	// This file lives at internal/store/ — repo root is two levels up.
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))

	fleetMailPath := filepath.Join(repoRoot, "internal", "store", "fleet_mail.go")
	holocronPath := filepath.Join(repoRoot, "internal", "store", "holocron.go")

	// ── AUDIT-074 ────────────────────────────────────────────────────────
	t.Run("AUDIT_074_readinbox_select_then_update_race", func(t *testing.T) {
		t.Skip("AUDIT-074: remove when ReadInboxForAgent uses UPDATE ... RETURNING (Fix #3)")
		// Without skip, fails with: AUDIT-074: fleet_mail.go still uses SELECT-then-per-id-UPDATE in ReadInboxForAgent — two concurrent agents with overlapping to_agent/role scope can both claim the same unconsumed mail row.
		src := mustReadFile(t, fleetMailPath)

		// Anchor: the function must still exist at this location.
		if !strings.Contains(src, "func ReadInboxForAgent(") {
			t.Fatalf("audit anchor lost: ReadInboxForAgent missing from %s",
				fleetMailPath)
		}

		// Narrow to the function body (from the signature to the next
		// top-level "\nfunc " or EOF), then assert shape on the slice.
		sigIdx := strings.Index(src, "func ReadInboxForAgent(")
		if sigIdx < 0 {
			t.Fatalf("audit anchor lost: ReadInboxForAgent signature not "+
				"findable via string search in %s", fleetMailPath)
		}
		rest := src[sigIdx:]
		// Next top-level function starts at "\nfunc " — cut there to bound
		// the body region we assert against.
		end := strings.Index(rest[1:], "\nfunc ")
		var body string
		if end < 0 {
			body = rest
		} else {
			body = rest[:end+1]
		}

		// The body must still do the SELECT then per-id MarkMailConsumed
		// loop — that is the racy shape we're calling out.
		hasSelect := strings.Contains(body, "db.Query(")
		hasPerIDMark := regexp.MustCompile(
			`for\s+_,\s*m\s*:=\s*range\s+mails\s*\{\s*MarkMailConsumed\(`).
			MatchString(body)
		if !hasSelect {
			t.Fatalf("ReadInboxForAgent no longer contains db.Query( — "+
				"audit anchor moved; update this test. Path: %s",
				fleetMailPath)
		}
		if !hasPerIDMark {
			t.Fatalf("ReadInboxForAgent no longer ends with a per-id "+
				"MarkMailConsumed loop — audit anchor moved; update "+
				"this test. Path: %s", fleetMailPath)
		}

		// The fix would introduce `UPDATE ... RETURNING` (single-statement
		// claim) inside the ReadInboxForAgent body. If that pattern is
		// present in the body region, the fix has landed.
		updateReturning := regexp.MustCompile(`(?is)UPDATE\s+Fleet_Mail[^` + "`" + `]*RETURNING`).
			MatchString(body)
		if updateReturning {
			t.Errorf("AUDIT-074 appears fixed in %s (UPDATE ... RETURNING "+
				"present). Update this test to assert the single-statement "+
				"claim's semantics directly.", fleetMailPath)
		} else {
			t.Errorf("AUDIT-074: %s still uses SELECT-then-per-id-UPDATE "+
				"in ReadInboxForAgent — two concurrent agents with "+
				"overlapping to_agent/role scope can both claim the "+
				"same unconsumed mail row and double-process its payload.",
				fleetMailPath)
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

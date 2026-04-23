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
	t.Skip("AUDIT-074/079/081: remove when sub-test fixes land (Fix #3 / Fix #4 companion)")
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
		t.Skip("AUDIT-079: remove when PRAGMA foreign_keys=ON set + cascade audited (Fix #4 companion)")
		// Without skip, fails with: AUDIT-079: holocron.go never executes `PRAGMA foreign_keys=ON`. SQLite defaults FK enforcement OFF per connection, so maintenance DELETEs create orphan rows.
		src := mustReadFile(t, holocronPath)

		// Anchor: the DSN initialiser must still exist.
		if !strings.Contains(src, "func InitHolocronDSN(") {
			t.Fatalf("audit anchor lost: InitHolocronDSN missing from %s",
				holocronPath)
		}

		// journal_mode=WAL is the existing PRAGMA; its absence means the
		// init shape changed and the test target needs revisiting.
		if !strings.Contains(src, "PRAGMA journal_mode=WAL") {
			t.Fatalf("audit anchor lost: PRAGMA journal_mode=WAL no longer "+
				"in %s — InitHolocronDSN shape changed; re-verify the "+
				"foreign_keys check.", holocronPath)
		}

		// The fix would add a PRAGMA foreign_keys=ON statement somewhere
		// in holocron.go (or in an imported helper, but the canonical
		// location is beside the journal_mode line). A broad grep across
		// the file suffices for the static check.
		hasFKPragma := regexp.MustCompile(`(?i)PRAGMA\s+foreign_keys\s*=\s*(ON|1)`).
			MatchString(src)
		if hasFKPragma {
			t.Errorf("AUDIT-079 appears fixed in %s (PRAGMA foreign_keys "+
				"present). Update this test to assert cascade/restrict "+
				"semantics directly.", holocronPath)
		} else {
			t.Errorf("AUDIT-079: %s never executes `PRAGMA foreign_keys=ON`. "+
				"SQLite defaults FK enforcement OFF per connection, so "+
				"maintenance DELETEs create orphan Escalations / "+
				"AskBranchPRs / TaskDependencies / TaskHistory / "+
				"FleetMemory rows; escalation-sweeper's JOIN to "+
				"BountyBoard returns empty for those orphans and the "+
				"self-healing sweep silently misses them.", holocronPath)
		}
	})

	// ── AUDIT-081 ────────────────────────────────────────────────────────
	t.Run("AUDIT_081_repositories_insert_or_replace_cascading_delete", func(t *testing.T) {
		t.Skip("AUDIT-081: remove when AddRepo uses ON CONFLICT DO UPDATE (Fix #4 companion)")
		// Without skip, fails with: AUDIT-081: holocron.go still uses `INSERT OR REPLACE INTO Repositories` in AddRepo. SQLite's REPLACE conflict resolution is DELETE+INSERT on PRIMARY KEY collisions.
		src := mustReadFile(t, holocronPath)

		// Anchor: AddRepo must still exist — it's the named culprit.
		if !strings.Contains(src, "func AddRepo(") {
			t.Fatalf("audit anchor lost: AddRepo missing from %s",
				holocronPath)
		}

		// The exact cited statement — its absence means the shape moved
		// and this test needs re-aiming.
		if !strings.Contains(src, "INSERT OR REPLACE INTO Repositories") {
			t.Errorf("AUDIT-081 appears fixed in %s: `INSERT OR REPLACE "+
				"INTO Repositories` no longer present — AddRepo likely "+
				"switched to `INSERT ... ON CONFLICT DO UPDATE` (UPSERT) "+
				"or a guarded UPDATE. Update this test to assert the "+
				"non-destructive write semantics directly.", holocronPath)
			return
		}

		// Static-only claim: SQLite's `INSERT OR REPLACE` on a PRIMARY
		// KEY conflict is specified as DELETE-then-INSERT (see
		// https://sqlite.org/lang_conflict.html — REPLACE algorithm).
		// That means any row in BountyBoard.target_repo,
		// AskBranchPRs.repo, or ConvoyAskBranches.repo that logically
		// references Repositories(name) is (a) silently detached from
		// its row identity on every re-register, and (b) would cascade
		// to NULL / be refused if FKs were ever turned on (see AUDIT-079).
		//
		// The fix the audit suggests is documenting immutability and
		// having RemoveRepo refuse when active referrers exist. That
		// would appear as a refusal path in RemoveRepo referencing the
		// child tables by name.
		removeRepoRefusalHints := []string{
			"BountyBoard", "AskBranchPRs", "ConvoyAskBranches",
		}
		// Rough check: does RemoveRepo (if present) even mention any
		// of the referring tables? If yes the fix may be in-flight.
		if strings.Contains(src, "func RemoveRepo(") {
			mentions := 0
			for _, tok := range removeRepoRefusalHints {
				if strings.Contains(src, tok) {
					mentions++
				}
			}
			if mentions > 0 {
				t.Logf("AUDIT-081 fix-in-progress hint: RemoveRepo exists "+
					"and holocron.go references %d of the three child "+
					"tables (%v). Verify RemoveRepo actually refuses on "+
					"active referrers rather than just naming them.",
					mentions, removeRepoRefusalHints)
			}
		}

		t.Errorf("AUDIT-081: %s still uses `INSERT OR REPLACE INTO "+
			"Repositories` in AddRepo. SQLite's REPLACE conflict "+
			"resolution is specified as DELETE+INSERT on PRIMARY KEY "+
			"(name) collisions — every re-registration of a repo "+
			"silently churns row identity out from under "+
			"BountyBoard.target_repo / AskBranchPRs.repo / "+
			"ConvoyAskBranches.repo references, and would cascade to "+
			"orphan those rows the moment PRAGMA foreign_keys=ON "+
			"lands (see AUDIT-079). Replace with `INSERT ... ON "+
			"CONFLICT(name) DO UPDATE SET ...` (UPSERT, row-identity "+
			"preserving) and document name immutability.", holocronPath)
	})
}

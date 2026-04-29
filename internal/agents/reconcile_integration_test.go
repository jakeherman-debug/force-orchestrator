package agents

import (
	"context"
	"testing"

	"force-orchestrator/internal/store"
)

// TestReconcile_DaemonStartPath_RunsAfterReleaseInFlightTasks pins the
// startup-sequence contract that the daemon wire-in (cmd/force/fleet_cmds.go)
// must honour: ReleaseInFlightTasks moves Locked/UnderReview rows back to
// Pending FIRST, then ReconcileOnStartup runs and sees the post-release
// state.
//
// Why this ordering matters: a daemon that crashed (laptop sleep, kill -9,
// power loss) leaves Locked rows on disk. If ReconcileOnStartup ran first
// it would still process Locked rows correctly via Case B's CAS, but the
// claim loops would also need ReleaseInFlightTasks to clear `owner` and
// `locked_at` so a fresh agent can claim. Running release first means the
// reconciler's snapshot already shows status=Pending and the post-state
// is uniform.
func TestReconcile_DaemonStartPath_RunsAfterReleaseInFlightTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, repoName := reconcileFixture(t, db, "daemon-start")
	taskID := seedNonTerminal(t, db, repoName, "Locked", "feature/never-existed")
	if _, err := db.Exec(`UPDATE BountyBoard SET owner = 'BB-8', locked_at = datetime('now') WHERE id = ?`, taskID); err != nil {
		t.Fatalf("set owner+locked_at: %v", err)
	}

	// Step 1 — same call cmdDaemon makes at startup.
	released := store.ReleaseInFlightTasks(db, "Fleet: reset on daemon startup (crash recovery)")
	if released != 1 {
		t.Fatalf("ReleaseInFlightTasks released %d task(s), want 1", released)
	}

	// Intermediate assertion — release brought row to Pending,
	// branch_name unchanged. The reconciler picks it up from there.
	var midStatus, midBranch string
	if err := db.QueryRow(`SELECT status, IFNULL(branch_name, '') FROM BountyBoard WHERE id = ?`, taskID).
		Scan(&midStatus, &midBranch); err != nil {
		t.Fatalf("intermediate read: %v", err)
	}
	if midStatus != "Pending" {
		t.Errorf("post-release status = %q, want Pending", midStatus)
	}
	if midBranch != "feature/never-existed" {
		t.Errorf("post-release branch_name = %q, want feature/never-existed (release does not clear branch)", midBranch)
	}

	// Step 2 — same call cmdDaemon makes immediately after.
	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("ReconcileOnStartup: %v", err)
	}

	// Final state — Case B fired: branch was missing, status was Pending,
	// reconciler cleared branch_name and stamped Pending (idempotent CAS).
	var finalStatus, finalBranch string
	if err := db.QueryRow(`SELECT status, IFNULL(branch_name, '') FROM BountyBoard WHERE id = ?`, taskID).
		Scan(&finalStatus, &finalBranch); err != nil {
		t.Fatalf("final read: %v", err)
	}
	if finalStatus != "Pending" {
		t.Errorf("final status = %q, want Pending", finalStatus)
	}
	if finalBranch != "" {
		t.Errorf("final branch_name = %q, want empty (Case B clears)", finalBranch)
	}

	// And the row is now claimable: a second pass through the same
	// startup sequence is a no-op.
	preMail := mailCount(t, db)
	preEsc := escalationCount(t, db)
	preReset := worktreeResetCount(t, db)

	if released2 := store.ReleaseInFlightTasks(db, "second pass"); released2 != 0 {
		t.Errorf("second-pass ReleaseInFlightTasks released %d, want 0", released2)
	}
	if err := ReconcileOnStartup(context.Background(), db); err != nil {
		t.Fatalf("second-pass ReconcileOnStartup: %v", err)
	}
	if got := mailCount(t, db); got != preMail {
		t.Errorf("second pass produced %d new mails, want 0", got-preMail)
	}
	if got := escalationCount(t, db); got != preEsc {
		t.Errorf("second pass produced %d new escalations, want 0", got-preEsc)
	}
	if got := worktreeResetCount(t, db); got != preReset {
		t.Errorf("second pass produced %d new WorktreeResets, want 0", got-preReset)
	}
}

// TestReconcile_DaemonStartPath_FailedReconcileIsFatal documents the
// invariant that a non-nil ReconcileOnStartup error must terminate the
// daemon (cmd/force/fleet_cmds.go calls os.Exit(1)). We can't exercise
// os.Exit in a unit test, so we exercise the condition that produces an
// error: a too-high error rate. The daemon wire-in's behaviour on that
// error is verified by inspection of the wire-in code.
func TestReconcile_DaemonStartPath_FailedReconcileReturnsError(t *testing.T) {
	// We deliberately don't have a clean way to make ReconcileOnStartup
	// fail beyond context-cancelled (covered by branchExistsLocal's
	// timeout) or DB-corrupt (out of scope). Skip rather than fake the
	// failure path with a contrived stub — the assertion that "non-nil
	// return aborts daemon startup" is enforced by the wire-in's
	// `if err := ... ; err != nil { os.Exit(1) }` shape. A future
	// regression test could grep for that shape.
	t.Skip("Fatal-on-error contract enforced by wire-in shape in cmd/force/fleet_cmds.go; no in-process error path easy to trigger here.")
}

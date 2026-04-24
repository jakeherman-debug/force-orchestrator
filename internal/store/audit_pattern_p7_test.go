package store

import (
	"sync"
	"testing"
)

// TestPattern_P7_ConcurrentCancelVsApproveRace verifies the post-Fix-#8d
// invariant (closes AUDIT-027 / AUDIT-072): when an operator CancelTask
// races a Jedi Council approve on the same task, the approve path MUST go
// through UpdateBountyStatusFrom(id, "UnderReview", "Completed") so that a
// Cancel that landed first makes the approve a silent no-op
// (rowsAffected=0), preserving the operator's deliberate action.
//
// The pre-fix red-phase reproduced deterministically: 20/20 trials saw
// Completed clobber Cancelled because UpdateBountyStatus is a blind write
// with no AND status=? clause. The post-fix contract is STRONGER than "no
// clobber" — the approve caller observes rowsAffected=0 explicitly, so it
// can skip downstream side effects (webhook, audit log, WriteMemory spawn)
// that would lie about work that actually got cancelled.
//
// Assertion strengthening vs the red-phase test: this test now asserts
//   (a) cancelWon == true for every trial (Cancel always runs first and lands)
//   (b) approveRowsAffected == 0 for every trial (the guard caught it)
//   (c) finalStatus == "Cancelled" for every trial (the guard didn't need to rely on commit order)
func TestPattern_P7_ConcurrentCancelVsApproveRace(t *testing.T) {
	const trials = 20

	type result struct {
		cancelWon            bool  // CancelTask returned true (1 row updated)
		approveRowsAffected  int64 // post-fix UpdateBountyStatusFrom return
		finalStatus          string
		clobbered            bool // cancelWon && finalStatus != "Cancelled"
		approveEscapedGuard  bool // cancelWon && approveRowsAffected != 0
	}

	results := make([]result, 0, trials)

	for i := 0; i < trials; i++ {
		db := InitHolocronDSN(":memory:")

		// Seed a task in AwaitingCouncilReview — the Council has not yet
		// claimed it. The real race moves through UnderReview (ClaimForReview),
		// but we test the guard at its strongest: the approve path reads
		// from UnderReview and writes to Completed. Here we simulate that
		// contract by claiming the review seat for the test goroutine.
		id := AddBounty(db, 0, "CodeEdit", "p7-race-test")
		if err := UpdateBountyStatus(db, id, "AwaitingCouncilReview"); err != nil {
			t.Fatalf("seed UpdateBountyStatus: %v", err)
		}
		claimed, ok := ClaimForReview(db, "council-0")
		if !ok || claimed.ID != id {
			t.Fatalf("ClaimForReview seed failed: ok=%v claimed=%+v want id=%d", ok, claimed, id)
		}
		// After ClaimForReview the task is in UnderReview. This is the
		// state the post-fix Council approve writes from.

		start := make(chan struct{})
		cancelDone := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)

		var cancelWon bool
		var approveRows int64
		var approveErr error
		go func() {
			defer wg.Done()
			<-start
			cancelWon = CancelTask(db, id, "operator cancelled")
			close(cancelDone)
		}()

		go func() {
			defer wg.Done()
			<-start
			<-cancelDone
			// Post-fix Council approval: source-status guard via
			// UpdateBountyStatusFrom. Expected: approveRows == 0 because
			// CancelTask already transitioned the task out of UnderReview.
			approveRows, approveErr = UpdateBountyStatusFrom(db, id, "UnderReview", "Completed")
		}()

		close(start)
		wg.Wait()

		if approveErr != nil {
			t.Fatalf("trial %d: UpdateBountyStatusFrom error: %v", i, approveErr)
		}

		var finalStatus string
		if err := db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&finalStatus); err != nil {
			t.Fatalf("trial %d: read final status: %v", i, err)
		}
		db.Close()

		r := result{
			cancelWon:           cancelWon,
			approveRowsAffected: approveRows,
			finalStatus:         finalStatus,
		}
		if cancelWon && finalStatus != "Cancelled" {
			r.clobbered = true
		}
		if cancelWon && approveRows != 0 {
			r.approveEscapedGuard = true
		}
		results = append(results, r)
	}

	var cancelWins, clobbers, guardEscapes, cancelledFinal, completedFinal int
	for _, r := range results {
		if r.cancelWon {
			cancelWins++
		}
		if r.clobbered {
			clobbers++
		}
		if r.approveEscapedGuard {
			guardEscapes++
		}
		switch r.finalStatus {
		case "Cancelled":
			cancelledFinal++
		case "Completed":
			completedFinal++
		}
	}

	// Post-fix contract: CancelTask ran first every trial (we sequenced it);
	// the approve MUST have been refused with rowsAffected=0; final state
	// MUST be Cancelled for every trial.
	if cancelWins != trials {
		t.Errorf("AUDIT-P7 (post-fix): expected CancelTask to win every trial (sequenced); got cancelWins=%d/%d", cancelWins, trials)
	}
	if clobbers > 0 {
		t.Errorf("AUDIT-P7 (AUDIT-027, AUDIT-072): %d/%d trials clobbered Cancelled with Completed; "+
			"UpdateBountyStatusFrom(id, \"UnderReview\", \"Completed\") should have returned 0 rows. "+
			"cancelWins=%d, finalCancelled=%d, finalCompleted=%d",
			clobbers, trials, cancelWins, cancelledFinal, completedFinal)
	}
	if guardEscapes > 0 {
		t.Errorf("AUDIT-P7: UpdateBountyStatusFrom returned rowsAffected > 0 in %d/%d trials despite "+
			"CancelTask having already transitioned the task. The AND status=? guard is not active.",
			guardEscapes, trials)
	}
	if cancelledFinal != trials {
		t.Errorf("AUDIT-P7: expected final status Cancelled every trial; got %d Cancelled / %d Completed",
			cancelledFinal, completedFinal)
	}
}

// TestPattern_P7_ResetTaskResurrectsCompleted verifies AUDIT-026: `ResetTask`
// has no source-status guard, so a `Completed` task can be resurrected to
// `Pending` (or `AwaitingCouncilReview` if a branch_name is set). The retry
// endpoints, `CloseEscalation(requeue=true)`, and the Jedi Council
// ancestor-walk all call it unconditionally — which means a race between
// "task just finished" and "operator clicks retry on a stale dashboard view"
// can un-complete finished work and re-run it, duplicating commits / PRs /
// mail.
func TestPattern_P7_ResetTaskResurrectsCompleted(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a task and transition it cleanly to Completed.
	id := AddBounty(db, 0, "CodeEdit", "p7-reset-resurrect-test")
	UpdateBountyStatus(db, id, "Completed")

	// Sanity: status is actually Completed before the call.
	var before string
	if err := db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&before); err != nil {
		t.Fatalf("read before failed: %v", err)
	}
	if before != "Completed" {
		t.Fatalf("seed precondition failed: expected Completed, got %q", before)
	}

	// Now call ResetTask. In a correctly guarded implementation this would
	// be a no-op (Completed is terminal; reset should only act on
	// Failed/Escalated/stuck states). Today the function has no source
	// guard and will clobber the status.
	ResetTask(db, id)

	var after string
	if err := db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&after); err != nil {
		t.Fatalf("read after failed: %v", err)
	}

	// Invariant: Completed is terminal. ResetTask must not resurrect it.
	if after != "Completed" {
		t.Errorf("AUDIT-P7 (AUDIT-026): ResetTask resurrected a Completed task to %q. "+
			"ResetTask (tasks.go:297-315) has no source-status guard and unconditionally "+
			"rewrites Completed tasks. Fix: `AND status NOT IN ('Completed','Cancelled')` "+
			"on both UPDATE branches (the branch_name='' path and the branch_name!='' path).",
			after)
	}
}

package store

import (
	"sync"
	"testing"
)

// TestPattern_P7_ConcurrentCancelVsApproveRace verifies the P7 audit pattern:
// state transitions are unguarded. `UpdateBountyStatus` and `CancelTask` both
// blind-UPDATE without `AND status=<expected_from>`, so an operator cancel
// and an agent approve racing on the same task resolve nondeterministically —
// whichever UPDATE commits last wins, regardless of whether the prior state
// still makes the transition legal.
//
// Expected (correct) behavior: if `CancelTask` wins the race and lands first,
// the subsequent `UpdateBountyStatus(..., "Completed")` must be a no-op
// (0 rows affected because status no longer equals the expected 'from').
// Today, `UpdateBountyStatus` has no `from` guard, so the later writer always
// clobbers the earlier one. Over 20 iterations we should see a consistent
// outcome where "Cancelled wins" stays Cancelled — today we see "Completed"
// clobbering even when CancelTask logically ran first and the task is in the
// middle of being cancelled.
//
// The test asserts: across 20 racing trials, whenever the operator's
// CancelTask returned rowsAffected=true (meaning it succeeded at some point
// during the race), the final status MUST be 'Cancelled'. Today, because
// UpdateBountyStatus unconditionally overwrites, the final status frequently
// ends up 'Completed' despite the cancel having "succeeded" — proving
// AUDIT-027 / AUDIT-072.
func TestPattern_P7_ConcurrentCancelVsApproveRace(t *testing.T) {
	t.Skip("AUDIT-027/AUDIT-072: remove when UpdateBountyStatusFrom(id, from, to) guards state transitions (Fix #8/#5)")
	// Without skip, fails with:
	//   audit_pattern_p7_test.go:135: AUDIT-P7 (AUDIT-027, AUDIT-072): detected 20/20 clobbers
	//   where CancelTask succeeded but a later unguarded UpdateBountyStatus("Completed")
	//   overwrote 'Cancelled'. Stats: cancelWins=20, finalCancelled=0, finalCompleted=20.
	//   UpdateBountyStatus has no source-status guard so it blind-writes regardless of whether
	//   the prior state still permits the transition.
	// Fail rate under `-race -count=5`: 5/5 runs fail, 20/20 clobbers per run (deterministic).
	const trials = 20

	type result struct {
		cancelWon     bool   // CancelTask returned true (1 row updated)
		finalStatus   string // status after both goroutines returned
		clobbered     bool   // cancelWon==true but finalStatus != Cancelled
	}

	results := make([]result, 0, trials)

	// To deterministically expose the unconditional clobber, we sequence
	// the two writers: Cancel fires FIRST and succeeds, THEN the approve
	// fires. A correct, guarded implementation would make the second
	// UPDATE a no-op (rows-affected == 0) because the task is no longer
	// in an approvable state. Today's implementation blind-writes and
	// clobbers 'Cancelled' with 'Completed' every time. The start-gate
	// pattern is still used so the structure mirrors the real race; the
	// sequencing inside each trial simply pins which writer wins for
	// assertion purposes.
	for i := 0; i < trials; i++ {
		db := InitHolocronDSN(":memory:")

		// Seed a task in a state where both transitions are plausible mid-life
		// (e.g. AwaitingCouncilReview — the task is about to be approved by
		// the Jedi Council, and concurrently the operator hits Cancel on the
		// dashboard).
		id := AddBounty(db, 0, "CodeEdit", "p7-race-test")
		UpdateBountyStatus(db, id, "AwaitingCouncilReview")

		// Start gate + a sequencing channel so we pin Cancel-first,
		// Approve-second. (The true concurrency-mode race also exhibits
		// the bug, but its outcome depends on SQLite write serialization;
		// a deterministic sequencing makes the assertion crisp.)
		start := make(chan struct{})
		cancelDone := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)

		var cancelWon bool
		go func() {
			defer wg.Done()
			<-start
			// Operator clicks Cancel from the dashboard.
			cancelWon = CancelTask(db, id, "operator cancelled")
			close(cancelDone)
		}()

		go func() {
			defer wg.Done()
			<-start
			<-cancelDone
			// Jedi Council approval path (inflight on a stale snapshot):
			// UpdateBountyStatus(..., "Completed"). (Jedi approves by
			// flipping the task to Completed in
			// internal/agents/jedi_council.go:363.) A guarded
			// implementation would see status!='AwaitingCouncilReview'
			// anymore and no-op; today it unconditionally writes.
			UpdateBountyStatus(db, id, "Completed")
		}()

		close(start)
		wg.Wait()

		var finalStatus string
		db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&finalStatus)
		db.Close()

		r := result{
			cancelWon:   cancelWon,
			finalStatus: finalStatus,
		}
		// Invariant: Cancel fired and succeeded first. The task MUST
		// remain Cancelled. A later unconditional UpdateBountyStatus
		// writing "Completed" over "Cancelled" is the P7 violation.
		if cancelWon && finalStatus != "Cancelled" {
			r.clobbered = true
		}
		results = append(results, r)
	}

	// Collect stats for a meaningful failure message.
	var cancelWins, clobbers, cancelledFinal, completedFinal int
	for _, r := range results {
		if r.cancelWon {
			cancelWins++
		}
		if r.clobbered {
			clobbers++
		}
		switch r.finalStatus {
		case "Cancelled":
			cancelledFinal++
		case "Completed":
			completedFinal++
		}
	}

	// Deterministic outcome expectation: either the approve UPDATE landed
	// first and CancelTask's `status != 'Completed'` guard blocked it
	// (cancelWon=false, finalStatus=Completed), OR CancelTask landed first
	// and approve must be a no-op (cancelWon=true, finalStatus=Cancelled).
	// Any trial where cancelWon=true AND finalStatus!="Cancelled" is a
	// definitive clobber and proves the audit.
	if clobbers > 0 {
		t.Errorf("AUDIT-P7 (AUDIT-027, AUDIT-072): detected %d/%d clobbers where CancelTask "+
			"succeeded but a later unguarded UpdateBountyStatus(\"Completed\") overwrote "+
			"'Cancelled'. Stats: cancelWins=%d, finalCancelled=%d, finalCompleted=%d. "+
			"UpdateBountyStatus has no source-status guard (tasks.go:184-189) so it "+
			"blind-writes regardless of whether the prior state still permits the "+
			"transition. Fix: UpdateBountyStatusFrom(db, id, from, to) returning "+
			"rowsAffected so callers can detect lost races.",
			clobbers, trials, cancelWins, cancelledFinal, completedFinal)
	}

	// Even in the absence of clobbers on a given run, the mixed-outcome
	// distribution itself is diagnostic: a guarded implementation would
	// produce a single dominant outcome family per trial (cancel-then-no-op
	// approve OR approve-then-no-op cancel). Today, the outcome is purely
	// determined by commit order.
	if cancelledFinal > 0 && completedFinal > 0 {
		t.Logf("AUDIT-P7 diagnostic: 20 trials yielded mixed finals — %d Cancelled, %d Completed. "+
			"Both transitions are unguarded; outcome depends solely on which UPDATE the "+
			"SQLite writer serializes last.", cancelledFinal, completedFinal)
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
	t.Skip("AUDIT-026: remove when UpdateBountyStatusFrom(id, from, to) guards state transitions (Fix #8/#5)")
	// Without skip, fails with:
	//   audit_pattern_p7_test.go:195: AUDIT-P7 (AUDIT-026): ResetTask resurrected a Completed
	//   task to "Pending". ResetTask (tasks.go:297-315) has no source-status guard and
	//   unconditionally rewrites Completed tasks. Fix: `AND status NOT IN ('Completed','Cancelled')`
	//   on both UPDATE branches (the branch_name='' path and the branch_name!='' path).
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

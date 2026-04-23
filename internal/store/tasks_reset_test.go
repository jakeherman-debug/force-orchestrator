package store

// Fix #6 — ResetTaskFull counter-preservation tests.
//
// AUDIT-005 / AUDIT-133: the prior ResetTaskFull zeroed retry_count and
// infra_failures on every Medic requeue, which eliminated all downstream
// bounds on the Astromech→Council→Medic→Astromech loop. The new contract
// preserves those counters so the auto-shard-on-zero-commits gate and the
// MaxInfraFailures gate keep accumulating across Medic cycles.

import (
	"database/sql"
	"testing"
)

// seedCompleteBounty inserts a BountyBoard row with retry_count=r,
// infra_failures=i, medic_requeue_count=m, and returns its id. Used by the
// ResetTaskFull tests below.
func seedCompleteBounty(t *testing.T, db *sql.DB, r, i, m int) int {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload,
			owner, retry_count, infra_failures, medic_requeue_count,
			convoy_id, checkpoint, branch_name, priority)
		VALUES (0, 'test', 'CodeEdit', 'Failed', 'p',
			'astro-1', ?, ?, ?,
			0, 'cp', 'br', 0)`, r, i, m)
	if err != nil {
		t.Fatalf("insert bounty: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last id: %v", err)
	}
	return int(id)
}

func readCounters(t *testing.T, db *sql.DB, id int) (retry, infra, medic int, branchName, status string) {
	t.Helper()
	err := db.QueryRow(`SELECT retry_count, infra_failures, IFNULL(medic_requeue_count,0),
		IFNULL(branch_name,''), status FROM BountyBoard WHERE id=?`, id).
		Scan(&retry, &infra, &medic, &branchName, &status)
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	return
}

// TestResetTaskFull_PreservesRetryCount is the canonical unit test cited by
// AUDIT-133 — it asserts the contract change from Fix #6: ResetTaskFull
// preserves retry_count and infra_failures so downstream auto-shard gates
// keep accumulating across Medic cycles.
func TestResetTaskFull_PreservesRetryCount(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := seedCompleteBounty(t, db, 4, 3, 1)

	ResetTaskFull(db, id)

	retry, infra, medic, branch, status := readCounters(t, db, id)
	if retry != 4 {
		t.Fatalf("retry_count: want preserved=4, got %d", retry)
	}
	if infra != 3 {
		t.Fatalf("infra_failures: want preserved=3, got %d", infra)
	}
	// medic_requeue_count is not the subject of ResetTaskFull — it is
	// incremented separately by applyMedicRequeue and must not be cleared.
	if medic != 1 {
		t.Fatalf("medic_requeue_count: want preserved=1, got %d", medic)
	}
	// Status must be Pending with branch_name cleared (the rest of the reset
	// contract is unchanged — the fresh-attempt semantic is preserved).
	if status != "Pending" {
		t.Fatalf("status: want Pending, got %q", status)
	}
	if branch != "" {
		t.Fatalf("branch_name: want cleared, got %q", branch)
	}
}

// TestResetTaskFull_Idempotent ensures running ResetTaskFull twice produces
// the same observable state. Counters do not grow, status stays Pending.
// Catches a regression where someone might accidentally INCREMENT a counter
// on reset.
func TestResetTaskFull_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := seedCompleteBounty(t, db, 2, 2, 0)
	ResetTaskFull(db, id)
	r1, i1, _, _, s1 := readCounters(t, db, id)

	ResetTaskFull(db, id)
	r2, i2, _, _, s2 := readCounters(t, db, id)

	if r1 != r2 || i1 != i2 || s1 != s2 {
		t.Fatalf("non-idempotent: run1=(%d,%d,%s) run2=(%d,%d,%s)", r1, i1, s1, r2, i2, s2)
	}
	if r1 != 2 || i1 != 2 {
		t.Fatalf("counters changed by reset: want (2,2), got (%d,%d)", r1, i1)
	}
}

// TestIncrementMedicRequeue_AccumulatesAcrossResets verifies the Fix #6
// invariant: calling ResetTaskFull between Medic requeues does NOT clear
// the requeue counter, so the cap at maxMedicRequeues remains effective
// across multiple loop iterations.
func TestIncrementMedicRequeue_AccumulatesAcrossResets(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := seedCompleteBounty(t, db, 0, 0, 0)

	for expected := 1; expected <= 3; expected++ {
		ResetTaskFull(db, id)
		got := IncrementMedicRequeue(db, id)
		if got != expected {
			t.Fatalf("iteration %d: IncrementMedicRequeue returned %d, want %d", expected, got, expected)
		}
		if cur := GetMedicRequeueCount(db, id); cur != expected {
			t.Fatalf("iteration %d: GetMedicRequeueCount returned %d, want %d", expected, cur, expected)
		}
	}
}

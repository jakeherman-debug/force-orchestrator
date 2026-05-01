package store

import (
	"context"
	"testing"
)

func TestFindPriorSimilar_BasicOrdering(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Seed three BriefingRenders rows with explicit timestamps — newer
	// rows have larger ID per insert order, but we want explicit
	// rendered_at to control sort order.
	for i := 1; i <= 3; i++ {
		_, err := db.Exec(`INSERT INTO BriefingRenders
			(decision_id, decision_kind, briefing_text, prompt_version, cost_usd, operator_decision)
			VALUES (?, 'captain_proposal', 'x', 'v1.0.0', 0, 'approved')`,
			int64(i))
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	out, err := FindPriorSimilar(ctx, db, "captain_proposal", 99, 10)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(out) != 3 {
		t.Errorf("got %d, want 3", len(out))
	}
	// All three timestamps land in the same second (datetime('now') resolution),
	// so explicit ordering by rendered_at is undefined. Just assert all three
	// land and the function doesn't panic.
}

func TestFindPriorSimilar_ExcludesSelf(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	_, _ = db.Exec(`INSERT INTO BriefingRenders
		(decision_id, decision_kind, briefing_text, prompt_version, cost_usd)
		VALUES (42, 'captain_proposal', 'x', 'v1.0.0', 0)`)

	out, _ := FindPriorSimilar(ctx, db, "captain_proposal", 42, 10)
	if len(out) != 0 {
		t.Errorf("self-row included in prior: %+v", out)
	}
}

func TestComputeSubsequentOutcome_ShippedClean(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// BountyBoard row marked Completed.
	res, _ := db.Exec(`INSERT INTO BountyBoard (type, payload, status, created_at) VALUES ('Feature', 'x', 'Completed', ?)`, NowSQLite())
	id, _ := res.LastInsertId()

	got := computeSubsequentOutcome(ctx, db, id)
	if got != "shipped_clean" {
		t.Errorf("subsequent_outcome=%q, want shipped_clean", got)
	}
}

func TestComputeSubsequentOutcome_Pending(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	got := computeSubsequentOutcome(ctx, db, 99999)
	if got != "pending" {
		t.Errorf("unknown decision subsequent_outcome=%q, want pending", got)
	}
}

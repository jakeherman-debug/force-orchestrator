package store

import (
	"context"
	"testing"
)

func TestTrustDials_BootstrapAndDefault(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Default before bootstrap = 70 (the brief).
	got, err := GetCurrentTrustDial(ctx, db, "op@example.com", "captain")
	if err != nil {
		t.Fatalf("get pre-bootstrap: %v", err)
	}
	if got != 70 {
		t.Errorf("pre-bootstrap dial=%d, want 70", got)
	}

	// Bootstrap writes one row per agent.
	if err := BootstrapTrustDials(ctx, db, "op@example.com"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	dials, err := ListCurrentTrustDials(ctx, db, "op@example.com")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(dials) != len(FleetAgentRoster) {
		t.Errorf("got %d dials, want %d (one per agent)", len(dials), len(FleetAgentRoster))
	}
	for _, d := range dials {
		if d.DialValue != 70 {
			t.Errorf("agent=%s dial=%d, want 70", d.Agent, d.DialValue)
		}
		if d.SetBy != string(TrustDialSystemDefault) {
			t.Errorf("agent=%s set_by=%q, want system_default", d.Agent, d.SetBy)
		}
	}

	// Bootstrap is idempotent.
	if err := BootstrapTrustDials(ctx, db, "op@example.com"); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	dials2, _ := ListCurrentTrustDials(ctx, db, "op@example.com")
	if len(dials2) != len(FleetAgentRoster) {
		t.Errorf("after second bootstrap got %d dials, want %d", len(dials2), len(FleetAgentRoster))
	}
}

func TestTrustDials_OperatorWriteAndHistory(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	if err := BootstrapTrustDials(ctx, db, "op@example.com"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Operator writes a new value.
	if err := SetTrustDial(ctx, db, TrustDial{
		OperatorEmail: "op@example.com",
		Agent:         "captain",
		DialValue:     30,
		SetBy:         string(TrustDialOperator),
		Rationale:     "captain has been overaggressive",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// SetTrustDial retries with sub-second suffixes on UNIQUE collisions
	// so we don't need to sleep between writes here.
	if err := SetTrustDial(ctx, db, TrustDial{
		OperatorEmail: "op@example.com",
		Agent:         "captain",
		DialValue:     85,
		SetBy:         string(TrustDialOperator),
		Rationale:     "regained trust after 4 ships clean",
	}); err != nil {
		t.Fatalf("second set: %v", err)
	}

	// Current = latest by set_at = 85.
	cur, _ := GetCurrentTrustDial(ctx, db, "op@example.com", "captain")
	if cur != 85 {
		t.Errorf("current dial=%d, want 85", cur)
	}

	// History contains all three rows (bootstrap + 30 + 85), newest first.
	hist, err := ListTrustDialHistory(ctx, db, "op@example.com", "captain")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len=%d, want 3", len(hist))
	}
	if hist[0].DialValue != 85 || hist[1].DialValue != 30 || hist[2].DialValue != 70 {
		t.Errorf("history order wrong: %v", []int{hist[0].DialValue, hist[1].DialValue, hist[2].DialValue})
	}
}

func TestTrustDials_RangeRejection(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	for _, bad := range []int{-1, 101, 200} {
		err := SetTrustDial(ctx, db, TrustDial{
			OperatorEmail: "op@example.com",
			Agent:         "captain",
			DialValue:     bad,
			SetBy:         string(TrustDialOperator),
		})
		if err == nil {
			t.Errorf("dial_value=%d accepted; should reject [0,100]", bad)
		}
	}
}

func TestFrictionTierFor(t *testing.T) {
	cases := []struct {
		dial int
		base string
		want string
	}{
		{30, "medium", "high"},   // low trust shifts up
		{50, "medium", "medium"}, // mid stays
		{90, "medium", "low"},    // high trust shifts down
		{30, "high", "high"},     // high never shifts down
		{90, "high", "high"},     // even at max trust
		{30, "low", "medium"},    // low trust shifts low up too
		{90, "low", "low"},
	}
	for _, c := range cases {
		got := FrictionTierFor(c.dial, c.base)
		if got != c.want {
			t.Errorf("FrictionTierFor(%d, %q) = %q, want %q", c.dial, c.base, got, c.want)
		}
	}
}

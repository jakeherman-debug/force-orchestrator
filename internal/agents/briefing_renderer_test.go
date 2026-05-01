package agents

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/agents/briefing_prompts"
	"force-orchestrator/internal/store"
)

func TestBriefingRenderer_RoundTrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	br, err := RenderBriefing(ctx, db, "captain_proposal", 42, 70)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if br.PromptVersion != briefing_prompts.PromptVersion {
		t.Errorf("prompt_version=%q, want %q", br.PromptVersion, briefing_prompts.PromptVersion)
	}
	if !strings.Contains(br.BriefingText, "captain_proposal/42") {
		t.Errorf("briefing_text should mention decision; got %q", br.BriefingText)
	}

	// Re-render returns existing row, doesn't double-insert.
	br2, err := RenderBriefing(ctx, db, "captain_proposal", 42, 70)
	if err != nil {
		t.Fatalf("re-render: %v", err)
	}
	if br2.ID != br.ID {
		t.Errorf("re-render created new row; ID=%d vs %d", br2.ID, br.ID)
	}

	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM BriefingRenders WHERE decision_id = 42 AND decision_kind = 'captain_proposal'`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 BriefingRenders row, got %d", n)
	}
}

func TestBriefingRenderer_FallbackOnCostCap(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Set cap very low.
	if _, err := db.Exec(`INSERT INTO SystemConfig (key, value) VALUES ('briefing_render_daily_cap_usd', '0.0001') ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("seed cap: %v", err)
	}

	// First render is always above cap (since cap is 0.0001 and a single
	// render costs 0.005). Wait, the function checks before adding.
	// First call: sum=0, cap=0.0001 → not over. Inserts row at cost_usd=0.005.
	br1, _ := RenderBriefing(ctx, db, "captain_proposal", 1, 70)
	if br1.BriefingText == briefing_prompts.FallbackBriefing {
		t.Errorf("first render hit fallback; should not")
	}
	// Second call: sum=0.005 > 0.0001 → fallback.
	br2, _ := RenderBriefing(ctx, db, "captain_proposal", 2, 70)
	if br2.BriefingText != briefing_prompts.FallbackBriefing {
		t.Errorf("second render did not hit fallback; got %q", br2.BriefingText)
	}
}

func TestRecordBriefingDecision(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	br, _ := RenderBriefing(ctx, db, "captain_proposal", 42, 70)
	if err := RecordBriefingDecision(ctx, db, br.ID, "approved", 45, "", "", 0); err != nil {
		t.Fatalf("record: %v", err)
	}
	br2, _ := RenderBriefing(ctx, db, "captain_proposal", 42, 70)
	if br2.OperatorDecision != "approved" {
		t.Errorf("operator_decision=%q, want approved", br2.OperatorDecision)
	}
	if br2.DecisionTimeSeconds != 45 {
		t.Errorf("decision_time_seconds=%d, want 45", br2.DecisionTimeSeconds)
	}
}

func TestListBriefingQueue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Seed two awaiting + one escalated.
	_, err := db.Exec(`INSERT INTO BountyBoard (type, payload, status, created_at) VALUES
		('Feature', 'A', 'AwaitingCaptainReview', ?),
		('CodeEdit', 'B', 'AwaitingCouncilReview', ?),
		('Investigate', 'C', 'Escalated', ?)`,
		store.NowSQLite(), store.NowSQLite(), store.NowSQLite())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	q, err := ListBriefingQueue(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(q) != 3 {
		t.Fatalf("queue len=%d, want 3", len(q))
	}
	// Escalated is high-stakes; should sort first by status DESC.
	if q[0].StakesTier != "high" {
		t.Errorf("first row stakes=%q, want high", q[0].StakesTier)
	}
}

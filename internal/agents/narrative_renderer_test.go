package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/agents/narrative_prompts"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// TestNarrativeCostEstimate_ScalesWithEvents is the contract test for
// the post-placeholder estimator: more events → higher cost (because
// the per-event input-token term grows). Both estimates must be
// strictly positive — the unknown-model floor protects against a 0
// return that would short-circuit the daily-cap accumulator.
func TestNarrativeCostEstimate_ScalesWithEvents(t *testing.T) {
	lo := narrativeCostEstimate(1)
	hi := narrativeCostEstimate(50)
	if lo <= 0 {
		t.Errorf("estimate(1) = %v, want > 0", lo)
	}
	if hi <= 0 {
		t.Errorf("estimate(50) = %v, want > 0", hi)
	}
	if !(hi > lo) {
		t.Errorf("estimate(50)=%v should exceed estimate(1)=%v — token-scaling broken", hi, lo)
	}
}

// TestNarrativeCostEstimate_UsesPriceTable confirms the estimate
// matches what claude.CostUSD would return for the haiku-4-5 model
// directly. Operator price-table edits should flow through to the
// renderer's cap.
func TestNarrativeCostEstimate_UsesPriceTable(t *testing.T) {
	// Empty-event path: 600 input tokens + 200 output tokens.
	want := claude.CostUSD("claude-haiku-4-5", 600, 200)
	if want <= 0 {
		t.Fatalf("claude.CostUSD baseline returned 0 — price table missing claude-haiku-4-5")
	}
	got := narrativeCostEstimate(0)
	if got != want {
		t.Errorf("narrativeCostEstimate(0) = %v, want %v (must mirror claude.CostUSD)", got, want)
	}
}

// TestNarrativeCostEstimate_NegativeEventCount asserts the defensive
// clamp: a negative count is treated as zero, never as a price-table
// underflow.
func TestNarrativeCostEstimate_NegativeEventCount(t *testing.T) {
	got := narrativeCostEstimate(-3)
	want := narrativeCostEstimate(0)
	if got != want {
		t.Errorf("estimate(-3) = %v, want %v (negative clamps to 0)", got, want)
	}
}

func TestNarrativeRenderer_Tick(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := renderOneNarrative(context.Background(), db, time.Now()); err != nil {
		t.Fatalf("first render: %v", err)
	}

	rows, err := ListLatestNarrativeRenders(context.Background(), db, 5)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].PromptVersion != narrative_prompts.PromptVersion {
		t.Errorf("prompt_version=%q, want %q", rows[0].PromptVersion, narrative_prompts.PromptVersion)
	}
}

func TestNarrativeRenderer_EstopFallback(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Activate e-stop.
	SetEstop(db, true)

	if err := renderOneNarrative(context.Background(), db, time.Now()); err != nil {
		t.Fatalf("e-stop render: %v", err)
	}
	rows, _ := ListLatestNarrativeRenders(context.Background(), db, 5)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !strings.HasPrefix(rows[0].Prose, "🛑 E-STOP active") {
		t.Errorf("e-stop prose=%q, want EstopFallbackProse prefix", rows[0].Prose)
	}
}

func TestNarrativeRenderer_DailyCostCap(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Set the cap very low so the first row triggers the cap on the next render.
	if _, err := db.Exec(`INSERT INTO SystemConfig (key, value) VALUES ('narrative_render_daily_cap_usd', '0.0001') ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("seed cap: %v", err)
	}

	// First render (under cap).
	_ = renderOneNarrative(ctx, db, time.Now())
	// Second render: cost from first puts us over.
	if err := renderOneNarrative(ctx, db, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("second render: %v", err)
	}

	rows, _ := ListLatestNarrativeRenders(ctx, db, 5)
	// At least one of the rows should be the FallbackProse.
	found := false
	for _, r := range rows {
		if r.Prose == narrative_prompts.FallbackProse {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("fallback prose never written; rows=%v", rows)
	}
}

func TestNarrativeRenderer_GoroutineHonoursCancel(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())

	spawnNarrativeRendererForTest(ctx, db, 25*time.Millisecond, time.Now)
	time.Sleep(120 * time.Millisecond)

	var before int
	_ = db.QueryRow(`SELECT COUNT(*) FROM NarrativeRenders`).Scan(&before)
	cancel()
	time.Sleep(150 * time.Millisecond)
	var after int
	_ = db.QueryRow(`SELECT COUNT(*) FROM NarrativeRenders`).Scan(&after)
	if after-before > 1 {
		t.Errorf("renderer continued ticking after cancel: rows added = %d", after-before)
	}
}

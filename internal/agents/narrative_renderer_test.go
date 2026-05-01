package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/agents/narrative_prompts"
	"force-orchestrator/internal/store"
)

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

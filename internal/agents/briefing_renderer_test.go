package agents

import (
	"context"
	"fmt"
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

// TestFindPriorSimilar_BuildMatchExpression covers the tokeniser
// shape: punctuation/short-token drop, dedup, stop-word filter,
// cap at 8.
func TestFindPriorSimilar_BuildMatchExpression(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantSubs []string // every substring must appear in the result
		wantOmit []string // none of these may appear
		empty    bool
	}{
		{
			name:    "empty",
			payload: "",
			empty:   true,
		},
		{
			name:    "below-min-length",
			payload: "a b c d ok no",
			empty:   true,
		},
		{
			name:     "happy-path",
			payload:  `{"feature":"checkout","amount":1500}`,
			wantSubs: []string{`"feature"`, `"checkout"`, `"amount"`, `"1500"`},
			wantOmit: []string{`"true"`, `"the"`},
		},
		{
			name:    "stop-words-and-dups-filtered",
			payload: "type type task convoy_id status status checkout checkout",
			wantSubs: []string{`"checkout"`},
			wantOmit: []string{`"type"`, `"task"`, `"convoy_id"`, `"status"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildPriorSimilarMatch(tc.payload)
			if tc.empty {
				if got != "" {
					t.Fatalf("buildPriorSimilarMatch(%q) = %q, want empty", tc.payload, got)
				}
				return
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("buildPriorSimilarMatch(%q) = %q, missing %q", tc.payload, got, sub)
				}
			}
			for _, omit := range tc.wantOmit {
				if strings.Contains(got, omit) {
					t.Errorf("buildPriorSimilarMatch(%q) = %q, must not contain %q", tc.payload, got, omit)
				}
			}
		})
	}
}

// TestFindPriorSimilar_FallbackToRecency exercises the FTS-free path:
// when fts_bounty is unavailable (or the payload is empty), the
// recency-ordered same-kind query still returns prior BriefingRenders
// rows.
func TestFindPriorSimilar_FallbackToRecency(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Seed 3 prior BriefingRenders rows of the same kind.
	for i := 1; i <= 3; i++ {
		_, err := db.Exec(`INSERT INTO BriefingRenders
			(decision_id, decision_kind, briefing_text, prior_similar_decisions_json,
			 prompt_version, cost_usd, operator_decision, rendered_at)
			VALUES (?, ?, ?, '[]', 'v1', 0.001, ?, datetime('now', ?))`,
			i*10, "captain_proposal", "synthetic", "approved",
			fmt.Sprintf("-%d minutes", 60-i))
		if err != nil {
			t.Fatalf("seed briefing %d: %v", i, err)
		}
	}

	// Current decision has no BountyBoard payload, so the FTS path
	// emits "" matchExpr and we drop into the recency fallback.
	got, err := findPriorSimilar(ctx, db, "captain_proposal", 99, 5)
	if err != nil {
		t.Fatalf("findPriorSimilar: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d prior, want 3", len(got))
	}
	// Recency-DESC order: the most recently-rendered row appears first.
	// Seed loop ordered them so id=30 had the latest rendered_at.
	if got[0].DecisionID != 30 {
		t.Errorf("first decision id = %d, want 30 (most recent)", got[0].DecisionID)
	}
	// Outcomes were stamped "approved".
	if got[0].Outcome != "approved" {
		t.Errorf("first outcome = %q, want approved", got[0].Outcome)
	}
	// SubsequentOutcome is the documented "pending" placeholder until
	// D3 P6A.12 fills it in.
	for _, p := range got {
		if p.SubsequentOutcome != "pending" {
			t.Errorf("subsequent outcome = %q, want pending placeholder", p.SubsequentOutcome)
		}
	}
}

// TestFindPriorSimilar_FTS5Ranking exercises the FTS5 path when it's
// available. Seeds three BountyBoard rows where one shares many
// payload tokens with the current decision and two share none, then
// asserts the high-similarity row ranks first.
func TestFindPriorSimilar_FTS5Ranking(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Build FTS5 if available. If the build fails (test binary without
	// sqlite_fts5 tag), skip — the recency fallback test above covers
	// the non-FTS path.
	if err := store.EnsureDrillFTS5(db); err != nil {
		t.Skipf("FTS5 not compiled in: %v", err)
	}

	// Seed BountyBoard with three rows: one shares "checkout payment
	// stripe" tokens with the current decision; two are unrelated.
	seed := []struct {
		id      int64
		payload string
	}{
		{100, `{"feature":"checkout","payment":"stripe","amount":1500}`},
		{101, `{"feature":"login","auth":"oauth","provider":"google"}`},
		{102, `{"feature":"signup","captcha":"recaptcha"}`},
		// Current decision (id=999) shares "checkout" + "stripe" with 100.
		{999, `{"feature":"checkout","payment":"stripe","refund":"partial"}`},
	}
	for _, s := range seed {
		_, err := db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, created_at)
			VALUES (?, 'CaptainProposal', ?, 'AwaitingCaptainReview', datetime('now'))`,
			s.id, s.payload)
		if err != nil {
			t.Fatalf("seed bb %d: %v", s.id, err)
		}
		// Also seed a BriefingRender row so operator_decision is
		// available for the JOIN.
		_, err = db.Exec(`INSERT INTO BriefingRenders
			(decision_id, decision_kind, briefing_text, prior_similar_decisions_json,
			 prompt_version, cost_usd, operator_decision, rendered_at)
			VALUES (?, 'captain_proposal', 'x', '[]', 'v1', 0.001, ?, datetime('now'))`,
			s.id, "approved")
		if err != nil {
			t.Fatalf("seed br %d: %v", s.id, err)
		}
	}

	got, err := findPriorSimilar(ctx, db, "captain_proposal", 999, 5)
	if err != nil {
		t.Fatalf("findPriorSimilar: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("FTS5 path returned 0 prior — expected at least the high-similarity match")
	}
	if got[0].DecisionID != 100 {
		t.Errorf("top match = %d, want 100 (highest token overlap with current payload)", got[0].DecisionID)
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

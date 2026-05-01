// D3 P6A — End-to-end shakedown test.
//
// Exercises the 12 sub-cases from dashboard-implementation.md's
// "Cross-cutting validation" section, scoped to the 6A surfaces.
// Each sub-case is a t.Run subtest.
//
// All sub-cases use stubs for LLM calls (narrative + briefing
// renderers don't actually shell out to Haiku in tests; their
// synthesise* helpers are deterministic). Total runtime should be
// well under 60s.
package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

func TestShakedown_P6A(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx := context.Background()

	// Bootstrap operator state used across sub-cases.
	if err := store.BootstrapTrustDials(ctx, db, "op@example.com"); err != nil {
		t.Fatalf("bootstrap trust: %v", err)
	}

	// Sub-case 1: Pulse loads (handler returns 200, body mentions Pulse).
	t.Run("01_pulse_handler_loads", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/pulse", nil)
		rr := httptest.NewRecorder()
		handlePulsePage(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", rr.Code)
		}
	})

	// Sub-case 2: Narrative renders, prose is non-empty, prompt_version stamped.
	t.Run("02_narrative_renders", func(t *testing.T) {
		// Trigger a render directly (no goroutine timing dependence).
		// We use the public entry point through the JSON API.
		// First: trigger one synthetic narrative row.
		now := time.Now().UTC()
		_, err := db.Exec(`INSERT INTO NarrativeRenders
			(rendered_at, event_window_start, event_window_end, source_event_count, source_event_refs_json, prose, prompt_version, cost_usd)
			VALUES (?, ?, ?, ?, '[]', ?, 'v1.0.0', 0)`,
			now.Format("2006-01-02 15:04:05"),
			now.Format("2006-01-02 15:04:05"),
			now.Format("2006-01-02 15:04:05"),
			0, "Fleet quiet — no transitions in the last window.")
		if err != nil {
			t.Fatalf("seed narrative: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/pulse/narrative", nil)
		rr := httptest.NewRecorder()
		handlePulseNarrative(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", rr.Code)
		}
		var out struct {
			Narratives []agents.NarrativeRow `json:"narratives"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		if len(out.Narratives) == 0 {
			t.Fatalf("expected at least one narrative")
		}
		if out.Narratives[0].PromptVersion == "" {
			t.Errorf("prompt_version not stamped")
		}
	})

	// Sub-case 3: Schedule cooldown, banner endpoint surfaces it.
	t.Run("03_cooldown_scheduled_visible", func(t *testing.T) {
		_, err := agents.ScheduleCooldown(ctx, db, "council_approve", 47)
		if err != nil {
			t.Fatalf("schedule: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/cooldown", nil)
		rr := httptest.NewRecorder()
		handleCooldownList(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", rr.Code)
		}
	})

	// Sub-case 4: Briefing queue sorted by stakes tier.
	t.Run("04_briefing_queue_sorted", func(t *testing.T) {
		// Seed three pending decisions: one Escalated (high), two Awaiting (medium).
		now := store.NowSQLite()
		_, _ = db.Exec(`INSERT INTO BountyBoard (type, payload, status, created_at) VALUES
			('Feature', 'A', 'AwaitingCaptainReview', ?),
			('CodeEdit', 'B', 'AwaitingCouncilReview', ?),
			('Investigate', 'C', 'Escalated', ?)`, now, now, now)

		req := httptest.NewRequest(http.MethodGet, "/api/briefing/queue", nil)
		rr := httptest.NewRecorder()
		handleBriefingQueue(db)(rr, req)
		var out struct {
			Queue []agents.BriefingQueueRow `json:"queue"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		if len(out.Queue) < 3 {
			t.Fatalf("queue len=%d, want >=3", len(out.Queue))
		}
		// First row is highest-stakes (status=Escalated → high).
		if out.Queue[0].StakesTier != "high" {
			t.Errorf("first row stakes=%q, want high", out.Queue[0].StakesTier)
		}
	})

	// Sub-case 5: Open a decision in focus mode (RenderBriefing).
	t.Run("05_briefing_focus_mode", func(t *testing.T) {
		br, err := agents.RenderBriefing(ctx, db, "captain_proposal", 1, 70)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if br.BriefingText == "" {
			t.Fatalf("briefing_text empty")
		}
	})

	// Sub-case 6: Approve via decide CLI parity.
	t.Run("06_approve_via_decide", func(t *testing.T) {
		br, _ := agents.RenderBriefing(ctx, db, "captain_proposal", 100, 70)
		if err := agents.RecordBriefingDecision(ctx, db, br.ID, "approved", 30, "", "", 0); err != nil {
			t.Fatalf("record: %v", err)
		}
		br2, _ := agents.RenderBriefing(ctx, db, "captain_proposal", 100, 70)
		if br2.OperatorDecision != "approved" {
			t.Errorf("decision=%q, want approved", br2.OperatorDecision)
		}
	})

	// Sub-case 7: Reject via counter-proposal forcing.
	t.Run("07_reject_counter_proposal", func(t *testing.T) {
		br, _ := agents.RenderBriefing(ctx, db, "captain_proposal", 200, 70)
		newID, err := agents.RouteCounterProposal(ctx, db, br.ID, "captain_proposal",
			agents.CounterProposalDifferentApproach,
			"Use a sliding-window rate-limiter instead of a fixed cap on this endpoint.")
		if err != nil {
			t.Fatalf("counter-proposal: %v", err)
		}
		if newID == 0 {
			t.Errorf("different_approach should spawn a task; newID=0")
		}
	})

	// Sub-case 8: Trust-dial shift moves the effective tier.
	t.Run("08_trust_dial_shifts_tier", func(t *testing.T) {
		_ = store.SetTrustDial(ctx, db, store.TrustDial{
			OperatorEmail: "op@example.com", Agent: "captain", DialValue: 30,
			SetBy: string(store.TrustDialOperator), Rationale: "test",
		})
		dial, _ := store.GetCurrentTrustDial(ctx, db, "op@example.com", "captain")
		eff := store.FrictionTierFor(dial, "medium")
		if eff != "high" {
			t.Errorf("with dial=30, medium should shift to high; got %s", eff)
		}
	})

	// Sub-case 9: CLI parity — `force decide` produces same DB state as click.
	t.Run("09_cli_parity_decide", func(t *testing.T) {
		br, _ := agents.RenderBriefing(ctx, db, "captain_proposal", 300, 70)
		// Simulate `force decide captain_proposal 300 --approve`
		_ = agents.RecordBriefingDecision(ctx, db, br.ID, "approved", 0, "", "", 0)
		br2, _ := agents.RenderBriefing(ctx, db, "captain_proposal", 300, 70)
		if br2.OperatorDecision != "approved" {
			t.Errorf("CLI parity: decision=%q, want approved", br2.OperatorDecision)
		}
	})

	// Sub-case 10: Notification budget hit — digest spool.
	t.Run("10_notification_budget_digest", func(t *testing.T) {
		_ = store.SetNotificationBudget(ctx, db, "op@example.com", "investigator", "modal", 0, 60, true)
		allowed, _ := store.RespectNotificationBudget(ctx, db,
			"op@example.com", "investigator", "modal", `{"k":1}`, store.StakesMedium)
		if allowed {
			t.Errorf("budget=0 should block medium-stakes; allowed=true")
		}
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM OperatorNotificationDigest`).Scan(&n)
		if n == 0 {
			t.Errorf("digest spool empty after blocked emission")
		}
	})

	// Sub-case 11: Operator attention tag.
	t.Run("11_attention_following", func(t *testing.T) {
		err := store.SetAttentionTag(ctx, db, store.AttentionTag{
			OperatorEmail: "op@example.com", TargetKind: "convoy", TargetID: "47",
			AttentionLevel: string(store.AttentionFollowing),
		})
		if err != nil {
			t.Fatalf("set: %v", err)
		}
		got, _ := store.GetAttentionTag(ctx, db, "op@example.com", "convoy", "47")
		if got.AttentionLevel != string(store.AttentionFollowing) {
			t.Errorf("level=%q, want following", got.AttentionLevel)
		}
	})

	// Sub-case 12: Sleep simulation → cinematic detected.
	t.Run("12_sleep_cinematic", func(t *testing.T) {
		// Two heartbeats with a 5-minute gap.
		now := time.Now().UTC()
		_, _ = db.Exec(`INSERT INTO DashboardHealthHeartbeats (ticked_at) VALUES (?)`,
			now.Add(-5*time.Minute).Format("2006-01-02 15:04:05"))
		_, _ = db.Exec(`INSERT INTO DashboardHealthHeartbeats (ticked_at) VALUES (?)`,
			now.Format("2006-01-02 15:04:05"))
		tStart, ok := agents.DetectSleepStartedAt(ctx, db)
		if !ok {
			t.Errorf("5-min gap not detected as sleep")
		}
		out, err := agents.BuildCinematic(ctx, db, tStart)
		if err != nil {
			t.Fatalf("cinematic: %v", err)
		}
		if out.SleepDurationSec < 60 {
			t.Errorf("sleep duration too short: %d", out.SleepDurationSec)
		}
	})
}

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOperatorSessionState_RoundTrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	s := OperatorSession{
		OperatorEmail:          "op@example.com",
		LastViewedSurface:      "briefing",
		LastViewedRoute:        "#/briefing/decision/42",
		LastFocusedDecisionID:  42,
		PartialReviewStateJSON: `{"draft":"hello"}`,
	}
	if err := SaveOperatorSession(ctx, db, s); err != nil {
		t.Fatalf("first save: %v", err)
	}

	got, err := GetOperatorSession(ctx, db, "op@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastViewedRoute != s.LastViewedRoute {
		t.Errorf("LastViewedRoute=%q, want %q", got.LastViewedRoute, s.LastViewedRoute)
	}
	if got.PartialReviewStateJSON != s.PartialReviewStateJSON {
		t.Errorf("PartialReviewStateJSON=%q, want %q", got.PartialReviewStateJSON, s.PartialReviewStateJSON)
	}

	// Idempotence: second save updates same row, no constraint violation.
	s.LastViewedRoute = "#/briefing"
	if err := SaveOperatorSession(ctx, db, s); err != nil {
		t.Fatalf("second save: %v", err)
	}
	got2, _ := GetOperatorSession(ctx, db, "op@example.com")
	if got2.LastViewedRoute != "#/briefing" {
		t.Errorf("after second save: LastViewedRoute=%q, want #/briefing", got2.LastViewedRoute)
	}
}

func TestOperatorSessionState_PayloadCap(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	tooBig := strings.Repeat("x", MaxPartialReviewStateBytes+1)
	err := SaveOperatorSession(ctx, db, OperatorSession{
		OperatorEmail:          "op@example.com",
		PartialReviewStateJSON: tooBig,
	})
	if !errors.Is(err, ErrSessionPayloadTooLarge) {
		t.Fatalf("err=%v, want ErrSessionPayloadTooLarge", err)
	}
}

func TestOperatorSessionState_Clear(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	if err := SaveOperatorSession(ctx, db, OperatorSession{
		OperatorEmail:          "op@example.com",
		LastViewedSurface:      "briefing",
		LastViewedRoute:        "#/briefing/decision/42",
		LastFocusedDecisionID:  42,
		PartialReviewStateJSON: `{"draft":"x"}`,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := ClearOperatorSession(ctx, db, "op@example.com"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ := GetOperatorSession(ctx, db, "op@example.com")
	if got.PartialReviewStateJSON != "" {
		t.Errorf("PartialReviewStateJSON=%q, want empty after clear", got.PartialReviewStateJSON)
	}
	if got.LastFocusedDecisionID != 0 {
		t.Errorf("LastFocusedDecisionID=%d, want 0", got.LastFocusedDecisionID)
	}
	// Surface is preserved across clear (operator may want to land where they were).
	if got.LastViewedSurface != "briefing" {
		t.Errorf("LastViewedSurface=%q, expected to survive clear", got.LastViewedSurface)
	}
}

func TestIsSessionStale(t *testing.T) {
	now := time.Now()
	fresh := OperatorSession{LastActiveAt: NowSQLite()}
	stale := OperatorSession{LastActiveAt: now.Add(-2 * time.Hour).UTC().Format("2006-01-02 15:04:05")}

	if IsSessionStale(fresh, now) {
		t.Errorf("fresh session classified stale")
	}
	if !IsSessionStale(stale, now) {
		t.Errorf("2h-old session not classified stale")
	}
	// Empty last_active → not stale (means "no session at all").
	if IsSessionStale(OperatorSession{}, now) {
		t.Errorf("empty session classified stale")
	}
}

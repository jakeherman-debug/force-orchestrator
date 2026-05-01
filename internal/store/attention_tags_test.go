package store

import (
	"context"
	"strings"
	"testing"
)

func TestAttentionTags_RoundTrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Default for unset is "normal".
	got, err := GetAttentionTag(ctx, db, "op@example.com", "convoy", "47")
	if err != nil {
		t.Fatalf("get unset: %v", err)
	}
	if got.AttentionLevel != string(AttentionNormal) {
		t.Errorf("unset default=%q, want normal", got.AttentionLevel)
	}

	// Set following.
	if err := SetAttentionTag(ctx, db, AttentionTag{
		OperatorEmail:  "op@example.com",
		TargetKind:     "convoy",
		TargetID:       "47",
		AttentionLevel: string(AttentionFollowing),
	}); err != nil {
		t.Fatalf("set following: %v", err)
	}
	got, _ = GetAttentionTag(ctx, db, "op@example.com", "convoy", "47")
	if got.AttentionLevel != string(AttentionFollowing) {
		t.Errorf("set following: level=%q, want following", got.AttentionLevel)
	}

	// Upsert: change to muted requires rationale.
	if err := SetAttentionTag(ctx, db, AttentionTag{
		OperatorEmail:  "op@example.com",
		TargetKind:     "convoy",
		TargetID:       "47",
		AttentionLevel: string(AttentionMuted),
	}); err == nil {
		t.Errorf("muted without rationale was accepted")
	}
	if err := SetAttentionTag(ctx, db, AttentionTag{
		OperatorEmail:  "op@example.com",
		TargetKind:     "convoy",
		TargetID:       "47",
		AttentionLevel: string(AttentionMuted),
		Rationale:      "noisy convoy, mid-investigation",
	}); err != nil {
		t.Fatalf("muted with rationale: %v", err)
	}
	got, _ = GetAttentionTag(ctx, db, "op@example.com", "convoy", "47")
	if got.AttentionLevel != string(AttentionMuted) {
		t.Errorf("upsert to muted: level=%q, want muted", got.AttentionLevel)
	}
	if got.Rationale == "" {
		t.Errorf("rationale lost on upsert")
	}
}

func TestAttentionTags_InvalidLevelRejected(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	err := SetAttentionTag(ctx, db, AttentionTag{
		OperatorEmail:  "op@example.com",
		TargetKind:     "convoy",
		TargetID:       "47",
		AttentionLevel: "screaming",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("invalid level was accepted; got %v", err)
	}
}

func TestAttentionTags_List(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	for _, t2 := range []struct {
		kind, id, level string
	}{
		{"convoy", "47", string(AttentionFollowing)},
		{"convoy", "48", string(AttentionFollowing)},
		{"agent", "investigator", string(AttentionMuted)},
	} {
		_ = SetAttentionTag(ctx, db, AttentionTag{
			OperatorEmail:  "op@example.com",
			TargetKind:     t2.kind,
			TargetID:       t2.id,
			AttentionLevel: t2.level,
			Rationale:      "test",
		})
	}
	tags, err := ListAttentionTags(ctx, db, "op@example.com")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tags) != 3 {
		t.Errorf("list len=%d, want 3", len(tags))
	}
}

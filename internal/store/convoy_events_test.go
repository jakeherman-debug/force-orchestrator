package store

import (
	"testing"
)

func TestAppendConvoyEvent_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, err := CreateConvoy(db, "[1] timeline")
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendConvoyEvent(db, int64(cid), "status_change", "pending", "active", "kickoff"); err != nil {
		t.Fatalf("AppendConvoyEvent: %v", err)
	}

	events, err := ListConvoyEvents(db, int64(cid))
	if err != nil {
		t.Fatalf("ListConvoyEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	got := events[0]
	if got.ConvoyID != int64(cid) || got.EventType != "status_change" ||
		got.OldValue != "pending" || got.NewValue != "active" || got.Detail != "kickoff" {
		t.Errorf("row fields wrong: %+v", got)
	}
	if got.ID == 0 {
		t.Errorf("expected non-zero id, got %d", got.ID)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("expected non-zero CreatedAt")
	}
}

func TestAppendConvoyEvent_EmptyOptionalFields(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] nulls")
	if err := AppendConvoyEvent(db, int64(cid), "shipped", "", "", ""); err != nil {
		t.Fatal(err)
	}
	events, err := ListConvoyEvents(db, int64(cid))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].OldValue != "" || events[0].NewValue != "" || events[0].Detail != "" {
		t.Errorf("expected empty strings for nullable columns, got %+v", events[0])
	}
}

func TestAppendConvoyEvent_Validates(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] validate")
	if err := AppendConvoyEvent(db, 0, "status_change", "", "", ""); err == nil {
		t.Errorf("expected error for convoyID <= 0")
	}
	if err := AppendConvoyEvent(db, int64(cid), "", "", "", ""); err == nil {
		t.Errorf("expected error for empty eventType")
	}
}

func TestListConvoyEvents_OrderedAscAndScopedByConvoy(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cidA, _ := CreateConvoy(db, "[1] A")
	cidB, _ := CreateConvoy(db, "[2] B")

	// Insert three events on A, one on B. Ordering should be insertion order since
	// the id tiebreaker kicks in when created_at timestamps collide at 1s resolution.
	steps := []struct {
		cid      int
		eventType string
		newValue string
	}{
		{cidA, "ask_branch_created", "force/ask-1"},
		{cidA, "draft_pr_opened", "https://example/pr/1"},
		{cidB, "status_change", "active"},
		{cidA, "shipped", ""},
	}
	for _, s := range steps {
		if err := AppendConvoyEvent(db, int64(s.cid), s.eventType, "", s.newValue, ""); err != nil {
			t.Fatal(err)
		}
	}

	aEvents, err := ListConvoyEvents(db, int64(cidA))
	if err != nil {
		t.Fatal(err)
	}
	if len(aEvents) != 3 {
		t.Fatalf("expected 3 events for convoy A, got %d", len(aEvents))
	}
	want := []string{"ask_branch_created", "draft_pr_opened", "shipped"}
	for i, e := range aEvents {
		if e.EventType != want[i] {
			t.Errorf("A[%d] = %q, want %q", i, e.EventType, want[i])
		}
	}

	bEvents, _ := ListConvoyEvents(db, int64(cidB))
	if len(bEvents) != 1 || bEvents[0].EventType != "status_change" {
		t.Errorf("convoy B events wrong: %+v", bEvents)
	}
}

func TestListConvoyEvents_EmptyWhenNoRows(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] empty")
	events, err := ListConvoyEvents(db, int64(cid))
	if err != nil {
		t.Fatalf("ListConvoyEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

// Idempotence-ish: AppendConvoyEvent is an append operation, so "running twice"
// means two rows — each call is its own distinct event. This test pins that
// contract so nobody sneaks in a dedupe that would silently drop real events.
func TestAppendConvoyEvent_AppendTwiceProducesTwoRows(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] dup")
	if err := AppendConvoyEvent(db, int64(cid), "status_change", "a", "b", ""); err != nil {
		t.Fatal(err)
	}
	if err := AppendConvoyEvent(db, int64(cid), "status_change", "a", "b", ""); err != nil {
		t.Fatal(err)
	}
	events, _ := ListConvoyEvents(db, int64(cid))
	if len(events) != 2 {
		t.Fatalf("expected 2 events after two appends, got %d", len(events))
	}
	if events[0].ID == events[1].ID {
		t.Errorf("expected distinct IDs, got %d and %d", events[0].ID, events[1].ID)
	}
}

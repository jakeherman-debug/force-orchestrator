package store

import "testing"

func TestRecordAndListConvoyEvents_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := CreateConvoy(db, "test-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	if err := RecordConvoyEvent(db, convoyID, "task_completed", "task 42 done"); err != nil {
		t.Fatalf("RecordConvoyEvent: %v", err)
	}
	if err := RecordConvoyEvent(db, convoyID, "convoy_completed", ""); err != nil {
		t.Fatalf("RecordConvoyEvent: %v", err)
	}

	events, err := ListConvoyEvents(db, convoyID)
	if err != nil {
		t.Fatalf("ListConvoyEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EventType != "task_completed" {
		t.Errorf("expected first event_type 'task_completed', got %q", events[0].EventType)
	}
	if events[0].Detail != "task 42 done" {
		t.Errorf("expected detail 'task 42 done', got %q", events[0].Detail)
	}
	if events[0].ConvoyID != convoyID {
		t.Errorf("expected convoy_id %d, got %d", convoyID, events[0].ConvoyID)
	}
	if events[1].EventType != "convoy_completed" {
		t.Errorf("expected second event_type 'convoy_completed', got %q", events[1].EventType)
	}
	if events[1].Detail != "" {
		t.Errorf("expected empty detail for nil-stored event, got %q", events[1].Detail)
	}
}

func TestListConvoyEvents_EmptyForUnknownConvoy(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	events, err := ListConvoyEvents(db, 9999)
	if err != nil {
		t.Fatalf("ListConvoyEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown convoy_id, got %d", len(events))
	}
}

func TestRecordConvoyEvent_DoubleInsert(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := CreateConvoy(db, "double-insert-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	if err := RecordConvoyEvent(db, convoyID, "started", "first call"); err != nil {
		t.Fatalf("first RecordConvoyEvent: %v", err)
	}
	if err := RecordConvoyEvent(db, convoyID, "started", "second call"); err != nil {
		t.Fatalf("second RecordConvoyEvent: %v", err)
	}

	events, err := ListConvoyEvents(db, convoyID)
	if err != nil {
		t.Fatalf("ListConvoyEvents: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 rows after double insert, got %d", len(events))
	}
}

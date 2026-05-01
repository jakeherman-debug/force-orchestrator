package store

import (
	"context"
	"testing"
)

// TestAnnotations covers 6B.8 invariants:
//   - Insert: required fields validated, flag enum enforced.
//   - Update: cross-operator edits rejected.
//   - Delete: cross-operator deletes rejected.
//   - List by event: returns only annotations on that (kind, ref).
//   - List by flag: returns only that flag, ordered DESC.
func TestAnnotations(t *testing.T) {
	t.Run("insert_validates_required_fields", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		// Missing operator_email
		_, err := InsertAnnotation(context.Background(), db, Annotation{
			EventKind: "llm_call", EventRef: "1", NoteText: "n", Flag: "",
		})
		if err == nil {
			t.Error("expected error on missing operator_email")
		}
		// Missing event_kind
		_, err = InsertAnnotation(context.Background(), db, Annotation{
			OperatorEmail: "op", EventRef: "1", NoteText: "n",
		})
		if err == nil {
			t.Error("expected error on missing event_kind")
		}
		// Missing note text
		_, err = InsertAnnotation(context.Background(), db, Annotation{
			OperatorEmail: "op", EventKind: "k", EventRef: "1",
		})
		if err == nil {
			t.Error("expected error on missing note_text")
		}
		// Invalid flag
		_, err = InsertAnnotation(context.Background(), db, Annotation{
			OperatorEmail: "op", EventKind: "k", EventRef: "1", NoteText: "n", Flag: "bogus",
		})
		if err == nil {
			t.Error("expected error on invalid flag")
		}
	})

	t.Run("happy_path_round_trip", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		id, err := InsertAnnotation(context.Background(), db, Annotation{
			OperatorEmail: "op", EventKind: "llm_call", EventRef: "42",
			NoteText: "this prompt is missing context", Flag: "problem",
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if id == 0 {
			t.Fatal("expected non-zero id")
		}

		// List by event
		rows, _ := ListAnnotationsForEvent(context.Background(), db, "llm_call", "42")
		if len(rows) != 1 || rows[0].Flag != "problem" {
			t.Errorf("list-by-event: %+v", rows)
		}

		// List by flag
		flagged, _ := ListAnnotationsByFlag(context.Background(), db, "problem", 10)
		if len(flagged) != 1 {
			t.Errorf("list-by-flag: %+v", flagged)
		}

		// Update
		err = UpdateAnnotation(context.Background(), db, id, "op", "now follow up", "follow_up")
		if err != nil {
			t.Errorf("update: %v", err)
		}
		rows, _ = ListAnnotationsForEvent(context.Background(), db, "llm_call", "42")
		if rows[0].NoteText != "now follow up" || rows[0].Flag != "follow_up" {
			t.Errorf("post-update: %+v", rows[0])
		}

		// Cross-operator update rejected
		err = UpdateAnnotation(context.Background(), db, id, "OTHER", "evil", "")
		if err == nil {
			t.Error("expected cross-operator update rejection")
		}

		// Cross-operator delete rejected
		err = DeleteAnnotation(context.Background(), db, id, "OTHER")
		if err == nil {
			t.Error("expected cross-operator delete rejection")
		}

		// Same-operator delete works
		if err := DeleteAnnotation(context.Background(), db, id, "op"); err != nil {
			t.Errorf("delete: %v", err)
		}
		rows, _ = ListAnnotationsForEvent(context.Background(), db, "llm_call", "42")
		if len(rows) != 0 {
			t.Errorf("expected empty after delete: %+v", rows)
		}
	})

	t.Run("idempotence", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		// Two inserts on same event create two rows (operator may
		// add multiple notes on one event).
		for i := 0; i < 2; i++ {
			_, err := InsertAnnotation(context.Background(), db, Annotation{
				OperatorEmail: "op", EventKind: "k", EventRef: "1", NoteText: "n", Flag: "",
			})
			if err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
		rows, _ := ListAnnotationsForEvent(context.Background(), db, "k", "1")
		if len(rows) != 2 {
			t.Errorf("expected 2 rows, got %d", len(rows))
		}
	})
}

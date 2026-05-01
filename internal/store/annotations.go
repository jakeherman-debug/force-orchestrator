// Package store — D3 P6B.8 OperatorEventAnnotations CRUD.
//
// Annotations are operator notes attached to a drill event (LLM call,
// task transition, git op, narrative, cycle, ruling, etc.). They
// carry a `flag` field — 'problem' / 'interesting' / 'follow_up' /
// '' — that drives Reflection's "events you flagged" panel and feeds
// Investigator's pattern detection (read-only signal).
//
// CRUD shape:
//   - InsertAnnotation: operator-only writes. The agent / system code
//     path never inserts here; Pattern P-AnnotationOperatorOnly walks
//     production code and rejects any non-operator INSERT.
//   - UpdateAnnotation: same operator-only constraint.
//   - DeleteAnnotation: operator-only.
//   - ListAnnotationsForEvent: read for the drill event card hover.
//   - ListAnnotationsByFlag: read for the Reflection panel.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// Annotation is the row shape surfaced to handlers + the dashboard.
type Annotation struct {
	ID            int64  `json:"id"`
	OperatorEmail string `json:"operator_email"`
	EventKind     string `json:"event_kind"`
	EventRef      string `json:"event_ref"`
	NoteText      string `json:"note_text"`
	Flag          string `json:"flag"`
	NotedAt       string `json:"noted_at"`
}

// validAnnotationFlags is the closed enum the SPA radio buttons emit.
// '' (empty) means "note without a flag." Other values are rejected.
var validAnnotationFlags = map[string]bool{
	"":           true,
	"problem":    true,
	"interesting": true,
	"follow_up":  true,
}

// InsertAnnotation writes one row. operator_email is required so the
// row cannot be confused with an agent-written note. Flag must be
// in the closed enum.
//
// Operator-only: Pattern P-AnnotationOperatorOnly walks production
// code and rejects calls to InsertAnnotation from non-handler paths.
func InsertAnnotation(ctx context.Context, db *sql.DB, a Annotation) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("InsertAnnotation: nil db")
	}
	if a.OperatorEmail == "" {
		return 0, fmt.Errorf("InsertAnnotation: operator_email required")
	}
	if a.EventKind == "" || a.EventRef == "" {
		return 0, fmt.Errorf("InsertAnnotation: event_kind + event_ref required")
	}
	if a.NoteText == "" {
		return 0, fmt.Errorf("InsertAnnotation: note_text required")
	}
	if !validAnnotationFlags[a.Flag] {
		return 0, fmt.Errorf("InsertAnnotation: invalid flag %q", a.Flag)
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO OperatorEventAnnotations
		   (operator_email, event_kind, event_ref, note_text, flag, noted_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		a.OperatorEmail, a.EventKind, a.EventRef, a.NoteText, a.Flag, NowSQLite(),
	)
	if err != nil {
		return 0, fmt.Errorf("InsertAnnotation: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UpdateAnnotation modifies an existing operator note. Caller's
// operator_email must match the existing row's (no cross-operator
// edits). Returns sql.ErrNoRows if the (id, operator) doesn't match.
func UpdateAnnotation(ctx context.Context, db *sql.DB, id int64, operatorEmail, noteText, flag string) error {
	if db == nil {
		return fmt.Errorf("UpdateAnnotation: nil db")
	}
	if !validAnnotationFlags[flag] {
		return fmt.Errorf("UpdateAnnotation: invalid flag %q", flag)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE OperatorEventAnnotations
		    SET note_text = ?, flag = ?
		  WHERE id = ? AND operator_email = ?`,
		noteText, flag, id, operatorEmail,
	)
	if err != nil {
		return fmt.Errorf("UpdateAnnotation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteAnnotation removes one operator note. Same operator-match
// gate as UpdateAnnotation.
func DeleteAnnotation(ctx context.Context, db *sql.DB, id int64, operatorEmail string) error {
	if db == nil {
		return fmt.Errorf("DeleteAnnotation: nil db")
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM OperatorEventAnnotations WHERE id = ? AND operator_email = ?`,
		id, operatorEmail,
	)
	if err != nil {
		return fmt.Errorf("DeleteAnnotation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListAnnotationsForEvent returns all annotations for one (kind, ref)
// pair. Drill renders these as small icons next to the event card;
// hover reveals the note text.
func ListAnnotationsForEvent(ctx context.Context, db *sql.DB, eventKind, eventRef string) ([]Annotation, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, operator_email, event_kind, event_ref, note_text, IFNULL(flag,''), noted_at
		   FROM OperatorEventAnnotations
		  WHERE event_kind = ? AND event_ref = ?
		  ORDER BY id ASC`,
		eventKind, eventRef,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Annotation
	for rows.Next() {
		var a Annotation
		if scanErr := rows.Scan(&a.ID, &a.OperatorEmail, &a.EventKind, &a.EventRef, &a.NoteText, &a.Flag, &a.NotedAt); scanErr != nil {
			log.Printf("annotations.go:ListAnnotationsForEvent: scan: %v", scanErr)
			continue
		}
		out = append(out, a)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("annotations.go:ListAnnotationsForEvent: rows iter: %v", rErr)
	}
	return out, nil
}

// ListAnnotationsByFlag returns annotations matching a flag, ordered
// by noted_at DESC. Used by the Reflection "events you flagged this
// month" panel.
func ListAnnotationsByFlag(ctx context.Context, db *sql.DB, flag string, limit int) ([]Annotation, error) {
	if !validAnnotationFlags[flag] {
		return nil, fmt.Errorf("ListAnnotationsByFlag: invalid flag %q", flag)
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, operator_email, event_kind, event_ref, note_text, IFNULL(flag,''), noted_at
		   FROM OperatorEventAnnotations
		  WHERE flag = ?
		  ORDER BY noted_at DESC
		  LIMIT ?`,
		flag, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Annotation
	for rows.Next() {
		var a Annotation
		if scanErr := rows.Scan(&a.ID, &a.OperatorEmail, &a.EventKind, &a.EventRef, &a.NoteText, &a.Flag, &a.NotedAt); scanErr != nil {
			log.Printf("annotations.go:ListAnnotationsByFlag: scan: %v", scanErr)
			continue
		}
		out = append(out, a)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("annotations.go:ListAnnotationsByFlag: rows iter: %v", rErr)
	}
	return out, nil
}

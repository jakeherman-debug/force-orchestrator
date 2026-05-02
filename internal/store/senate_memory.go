// Package store: SenateMemory — append-only memory store the Senator
// reads in its prompt context. D4 Phase 3.
//
// Schema lives in schema.go (createSchema + runMigrations) and
// schema/schema.sql. Every mutator returns error per CLAUDE.md
// § "No silent failures".
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SenateMemoryEntry is the in-memory shape of one SenateMemory row.
type SenateMemoryEntry struct {
	ID              int
	Senator         string
	Topic           string
	Summary         string
	Source          string // 'rejection' | 'commit' | 'escalation' | 'manual' | 'bootstrap'
	Weight          float64
	RetrievalCount  int
	LastConsultedAt string
	CreatedAt       string
}

// InsertSenateMemory appends one memory row. Returns the new id.
// Senator + Summary are required; defaults are applied for the rest.
func InsertSenateMemory(db *sql.DB, e SenateMemoryEntry) (int, error) {
	if e.Senator == "" {
		return 0, errors.New("InsertSenateMemory: Senator required")
	}
	if e.Summary == "" {
		return 0, errors.New("InsertSenateMemory: Summary required")
	}
	if e.Source == "" {
		e.Source = "manual"
	}
	if e.Weight == 0 {
		e.Weight = 1.0
	}
	res, err := db.Exec(`
		INSERT INTO SenateMemory (senator, topic, summary, source, weight)
		VALUES (?, ?, ?, ?, ?)`,
		e.Senator, e.Topic, e.Summary, e.Source, e.Weight)
	if err != nil {
		return 0, fmt.Errorf("InsertSenateMemory(%s): %w", e.Senator, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("InsertSenateMemory(%s): LastInsertId: %w", e.Senator, err)
	}
	return int(id), nil
}

// ListSenateMemory returns the top-K memories for a Senator, ordered by
// weight DESC then created_at DESC. K=0 defaults to 50 (Senate prompt
// budget). Caller is responsible for further filtering / formatting.
func ListSenateMemory(db *sql.DB, senator string, k int) ([]SenateMemoryEntry, error) {
	if senator == "" {
		return nil, errors.New("ListSenateMemory: senator required")
	}
	if k <= 0 {
		k = 50
	}
	rows, err := db.Query(`
		SELECT id, senator, IFNULL(topic,''), summary, IFNULL(source,'manual'),
		       IFNULL(weight, 1.0), IFNULL(retrieval_count, 0),
		       IFNULL(last_consulted_at,''), IFNULL(created_at,'')
		  FROM SenateMemory
		 WHERE senator = ?
		 ORDER BY weight DESC, created_at DESC, id DESC
		 LIMIT ?`, senator, k)
	if err != nil {
		return nil, fmt.Errorf("ListSenateMemory(%s): %w", senator, err)
	}
	defer rows.Close()
	var out []SenateMemoryEntry
	for rows.Next() {
		var e SenateMemoryEntry
		if scanErr := rows.Scan(&e.ID, &e.Senator, &e.Topic, &e.Summary, &e.Source,
			&e.Weight, &e.RetrievalCount, &e.LastConsultedAt, &e.CreatedAt); scanErr != nil {
			return nil, fmt.Errorf("ListSenateMemory(%s): scan: %w", senator, scanErr)
		}
		out = append(out, e)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListSenateMemory(%s): rows.Err: %w", senator, rErr)
	}
	return out, nil
}

// MarkSenateMemoryConsulted increments retrieval_count and stamps
// last_consulted_at. Called when a memory shows up in a Senator's
// review-time prompt context. Idempotent w.r.t. concurrent retrieval.
func MarkSenateMemoryConsulted(db *sql.DB, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		_, err := db.Exec(`
			UPDATE SenateMemory
			   SET retrieval_count   = IFNULL(retrieval_count, 0) + 1,
			       last_consulted_at = datetime('now')
			 WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("MarkSenateMemoryConsulted(id=%d): %w", id, err)
		}
	}
	return nil
}

// CountSenateMemory returns the count of memory rows for a Senator.
// Used by the senate-refresh dog to decide whether a digest write is a
// "first batch" or a follow-on, and by tests for parity assertions.
func CountSenateMemory(db *sql.DB, senator string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM SenateMemory WHERE senator = ?`, senator).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountSenateMemory(%s): %w", senator, err)
	}
	return n, nil
}

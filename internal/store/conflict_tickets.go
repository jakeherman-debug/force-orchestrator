// Package store — D4 Phase 0 — ConflictTickets CRUD + DetectConflicts.
//
// DetectConflicts walks FleetMemory pairs and emits ConflictTickets for
// pairs that look contradictory. The check uses a deterministic
// contradiction-pattern detector (negation antonyms in summaries
// describing the same files) with a budget for an LLM judge layer
// when LIVE_HAIKU_DISABLED is unset (the LLM call site is fronted by
// the Librarian Client; this store helper only runs the deterministic
// pre-screen).
//
// Operator surface. Tickets land with status='open'; the dashboard
// endpoint /api/conflicts/tickets lists them. ResolveConflictTicket
// is called by the operator-action endpoint to flip status='resolved'
// + record a resolution_note.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ContradictionMarkers is the deterministic pre-screen vocabulary used
// by DetectConflicts. Two memories whose summaries touch the same
// files but differ on one of these markers are flagged as candidates
// for the LLM judge layer (or surface as ticket reason='regex' when
// LIVE_HAIKU_DISABLED is set).
//
// The list is intentionally short — the goal is high precision on
// the deterministic path so the operator's queue is not flooded with
// false positives. Hand-curated against the shakedown fixtures.
var ContradictionMarkers = []struct {
	a string
	b string
}{
	{"never", "always"},
	{"always", "never"},
	{"required", "forbidden"},
	{"forbidden", "required"},
	{"must not", "must"},
	{"do not", "must"},
	{"avoid", "prefer"},
	{"prefer", "avoid"},
	{"deprecated", "use"},
}

// DetectConflicts scans FleetMemory rows for contradictory pairs and
// inserts ConflictTickets rows for newly-detected pairs. Returns the
// count of new tickets inserted. Idempotence: a pair (A,B) that
// already has an open ticket is not re-inserted.
//
// Algorithm (deterministic-only path):
//  1. Walk all (A,B) FleetMemory pairs where A.id < B.id and A.repo
//     == B.repo. (Cross-repo "contradictions" don't make sense — a
//     rule about authn in repo X cannot contradict an authn rule in
//     repo Y; they're scoped differently.)
//  2. For each pair, compute the marker score: number of (mark_a,
//     mark_b) tuples where summary_A contains mark_a AND summary_B
//     contains mark_b (or vice versa).
//  3. If marker score > 0 AND files-changed sets overlap (Jaccard >
//     0.5), insert a ConflictTicket. Files overlap is the false-
//     positive guard: two memories disagreeing about whether to
//     "always" do X are only contradictory if they're talking about
//     the same X.
//
// LLM-judge layer is intentionally NOT routed through here — it lives
// at the Librarian Client surface (see internal/clients/librarian/).
// This function is the deterministic pre-screen that the dog calls
// directly so the unit-test path doesn't depend on Claude.
func DetectConflicts(ctx context.Context, db *sql.DB) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, repo, IFNULL(summary, ''), IFNULL(files_changed, '')
		  FROM FleetMemory
		 WHERE IFNULL(canonical_id, 0) = 0
		 ORDER BY repo, id`)
	if err != nil {
		return 0, fmt.Errorf("DetectConflicts: query: %w", err)
	}
	type rowSnap struct {
		id      int
		repo    string
		summary string
		files   string
	}
	var all []rowSnap
	for rows.Next() {
		var r rowSnap
		if err := rows.Scan(&r.id, &r.repo, &r.summary, &r.files); err != nil {
			rows.Close()
			return 0, fmt.Errorf("DetectConflicts: scan: %w", err)
		}
		all = append(all, r)
	}
	if rerr := rows.Err(); rerr != nil {
		rows.Close()
		return 0, fmt.Errorf("DetectConflicts: rows iter: %w", rerr)
	}
	rows.Close()

	// Group by repo to bound the O(n^2) walk.
	byRepo := map[string][]rowSnap{}
	for _, r := range all {
		byRepo[r.repo] = append(byRepo[r.repo], r)
	}

	inserted := 0
	for _, bucket := range byRepo {
		for i := 0; i < len(bucket); i++ {
			for j := i + 1; j < len(bucket); j++ {
				a, b := bucket[i], bucket[j]
				if !markerContradiction(a.summary, b.summary) {
					continue
				}
				if filesJaccard(a.files, b.files) < 0.5 {
					continue
				}
				ok, err := insertConflictTicketIfNew(ctx, db, a.id, b.id, "regex")
				if err != nil {
					return inserted, err
				}
				if ok {
					inserted++
				}
			}
		}
	}
	return inserted, nil
}

// markerContradiction returns true if any (a, b) marker pair appears
// with a in summary_X and b in summary_Y (or vice versa).
func markerContradiction(summaryA, summaryB string) bool {
	la := strings.ToLower(summaryA)
	lb := strings.ToLower(summaryB)
	for _, m := range ContradictionMarkers {
		if strings.Contains(la, m.a) && strings.Contains(lb, m.b) {
			return true
		}
		if strings.Contains(lb, m.a) && strings.Contains(la, m.b) {
			return true
		}
	}
	return false
}

// filesJaccard returns Jaccard similarity over comma-split file path
// sets. 0 if either side is empty.
func filesJaccard(a, b string) float64 {
	setA := splitFiles(a)
	setB := splitFiles(b)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	intersect := 0
	for k := range setA {
		if _, ok := setB[k]; ok {
			intersect++
		}
	}
	union := len(setA) + len(setB) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

func splitFiles(csv string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, tok := range strings.Split(csv, ",") {
		t := strings.TrimSpace(tok)
		if t == "" {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

// insertConflictTicketIfNew inserts a ticket only if no row already
// exists for the pair (a_id, b_id). Returns (true, nil) on insert,
// (false, nil) if the pair already had a ticket. Pair ordering is
// canonicalised so (A,B) and (B,A) collapse to the same row.
func insertConflictTicketIfNew(ctx context.Context, db *sql.DB, idA, idB int, reason string) (bool, error) {
	if idA > idB {
		idA, idB = idB, idA
	}
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ConflictTickets
		  WHERE memory_a_id = ? AND memory_b_id = ?`,
		idA, idB).Scan(&exists); err != nil {
		return false, fmt.Errorf("insertConflictTicketIfNew: count: %w", err)
	}
	if exists > 0 {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ConflictTickets (memory_a_id, memory_b_id, reason, status)
		VALUES (?, ?, ?, 'open')`, idA, idB, reason); err != nil {
		return false, fmt.Errorf("insertConflictTicketIfNew: insert: %w", err)
	}
	return true, nil
}

// ListOpenConflictTickets returns every ticket whose status='open',
// newest-first, capped at limit.
func ListOpenConflictTickets(ctx context.Context, db *sql.DB, limit int) ([]ConflictTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, memory_a_id, memory_b_id, IFNULL(reason, ''), IFNULL(status, ''),
		       IFNULL(created_at, ''), IFNULL(resolved_at, ''), IFNULL(resolution_note, '')
		  FROM ConflictTickets
		 WHERE status = 'open'
		 ORDER BY id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ListOpenConflictTickets: query: %w", err)
	}
	defer rows.Close()
	var out []ConflictTicket
	for rows.Next() {
		var t ConflictTicket
		if err := rows.Scan(&t.ID, &t.MemoryAID, &t.MemoryBID, &t.Reason, &t.Status,
			&t.CreatedAt, &t.ResolvedAt, &t.ResolutionNote); err != nil {
			return nil, fmt.Errorf("ListOpenConflictTickets: scan: %w", err)
		}
		out = append(out, t)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("ListOpenConflictTickets: rows iter: %w", rerr)
	}
	return out, nil
}

// ResolveConflictTicket flips status='resolved' + records the
// resolution note + stamps resolved_at. Returns an error if the
// ticket id doesn't exist OR if the ticket is already resolved (so
// double-resolution from a duplicate operator click is surfaced
// rather than silently swallowed).
func ResolveConflictTicket(ctx context.Context, db *sql.DB, ticketID int, note string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE ConflictTickets
		   SET status = 'resolved',
		       resolved_at = datetime('now'),
		       resolution_note = ?
		 WHERE id = ?
		   AND status = 'open'`, note, ticketID)
	if err != nil {
		return fmt.Errorf("ResolveConflictTicket: update: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ResolveConflictTicket: rows-affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("ResolveConflictTicket: ticket %d not found OR already resolved", ticketID)
	}
	return nil
}

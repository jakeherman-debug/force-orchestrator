// Package store — prompt_byte_attribution.go (D2 T1-2).
//
// PromptByteAttribution captures the source-tag breakdown of every LLM
// prompt assembly. One row per (call, source_tag) so the dashboard /
// operator mail can show "captain's last call was 60% file_read, 25%
// claude_md, 10% task_payload" without re-parsing the prompt.
//
// The application layer (internal/agents/llm_boundary.go) constrains
// source_tag to a fixed enum; the schema keeps it TEXT for forward
// compat. This file is the persistence boundary only — callers MUST go
// through RecordSourceTags so the validation lives in one place.

package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SourceContribution is one slice of an assembled LLM prompt, tagged
// with its provenance and byte size. RecordSourceTags writes a row per
// contribution.
type SourceContribution struct {
	SourceTag string // one of llm_boundary.go's SourceTag* constants
	Bytes     int    // len(content) of this slice in bytes (UTF-8)
}

// PromptByteAttribution is a single recorded contribution row, returned
// by ListPromptByteAttributionsForTask and dashboard aggregation
// queries.
type PromptByteAttribution struct {
	ID            int
	TaskID        int
	AgentName     string
	CallTimestamp string
	SourceTag     string
	Bytes         int
}

// RecordSourceTags writes one PromptByteAttribution row per non-zero
// contribution. callTS is the wall-clock timestamp the LLM call started
// (use store.NowSQLite() at the call site so all rows for one prompt
// share an exact-string key); zero-byte contributions are dropped (no
// signal). agentName is required (we cannot meaningfully record an
// attribution without it). taskID == 0 is permitted for context-less
// calls (boot, classifier).
//
// Returns an error when the INSERT fails OR when the input is malformed
// (empty agent_name, all-empty source_tag entries). Per the new-mutator
// policy: this helper returns error and the caller MUST check it.
func RecordSourceTags(db *sql.DB, taskID int, agentName, callTS string, sources []SourceContribution) error {
	if db == nil {
		return fmt.Errorf("RecordSourceTags: nil db")
	}
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("RecordSourceTags: agent_name is required")
	}
	if callTS == "" {
		callTS = NowSQLite()
	}
	// Filter: drop zero-byte contributions. This is intentional — a
	// prompt with no content from a particular source has no
	// observable signal.
	filtered := sources[:0:0]
	for _, c := range sources {
		if c.Bytes <= 0 {
			continue
		}
		if strings.TrimSpace(c.SourceTag) == "" {
			return fmt.Errorf("RecordSourceTags: empty source_tag in contribution (bytes=%d)", c.Bytes)
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return nil // nothing to record
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("RecordSourceTags: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(
		`INSERT INTO PromptByteAttribution
		   (task_id, agent_name, call_timestamp, source_tag, bytes)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("RecordSourceTags: prepare: %w", err)
	}
	defer stmt.Close() //nolint:errcheck

	for _, c := range filtered {
		if _, err := stmt.Exec(taskID, agentName, callTS, c.SourceTag, c.Bytes); err != nil {
			return fmt.Errorf("RecordSourceTags: insert source=%s bytes=%d: %w", c.SourceTag, c.Bytes, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("RecordSourceTags: commit: %w", err)
	}
	return nil
}

// ListPromptByteAttributionsForTask returns every contribution row
// recorded for a given task, ordered (call_timestamp ASC, id ASC).
// Returns (nil, nil) for an empty result.
func ListPromptByteAttributionsForTask(db *sql.DB, taskID int) ([]PromptByteAttribution, error) {
	rows, err := db.Query(
		`SELECT id, task_id, agent_name, call_timestamp, source_tag, bytes
		   FROM PromptByteAttribution
		  WHERE task_id = ?
		  ORDER BY call_timestamp ASC, id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("ListPromptByteAttributionsForTask: query: %w", err)
	}
	defer rows.Close()
	var out []PromptByteAttribution
	for rows.Next() {
		var r PromptByteAttribution
		if err := rows.Scan(&r.ID, &r.TaskID, &r.AgentName, &r.CallTimestamp, &r.SourceTag, &r.Bytes); err != nil {
			return nil, fmt.Errorf("ListPromptByteAttributionsForTask: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListPromptByteAttributionsForTask: rows iter: %w", err)
	}
	return out, nil
}

// AgentSourceBreakdown is the aggregated per-source byte total for a
// rolling window query (PromptByteAttributionByAgent). Sources are
// returned in descending bytes order so the caller can render a top-N
// list without re-sorting.
type AgentSourceBreakdown struct {
	AgentName string
	Calls     int                   // distinct call_timestamp count for this agent in window
	TotalBytes int64                // SUM(bytes) across every source for this agent
	BySource  map[string]int64      // source_tag → bytes summed in window
	Ordered   []AgentSourceTagTotal // BySource as a sorted slice (desc by bytes)
}

// AgentSourceTagTotal is one entry in AgentSourceBreakdown.Ordered.
type AgentSourceTagTotal struct {
	SourceTag string
	Bytes     int64
}

// PromptByteAttributionByAgent returns a per-agent breakdown over a
// rolling window. since is the lower bound (`call_timestamp >= since`);
// pass an empty string for "all time". The dashboard uses a 7-day
// window; tests typically pass NowSQLite()-1h. Empty result is
// returned as ([]AgentSourceBreakdown{}, nil).
func PromptByteAttributionByAgent(db *sql.DB, since string) ([]AgentSourceBreakdown, error) {
	q := `SELECT agent_name, source_tag, SUM(bytes) AS total_bytes,
	             COUNT(DISTINCT call_timestamp) AS call_count
	        FROM PromptByteAttribution`
	args := []any{}
	if since != "" {
		q += ` WHERE call_timestamp >= ?`
		args = append(args, since)
	}
	q += ` GROUP BY agent_name, source_tag ORDER BY agent_name ASC, total_bytes DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("PromptByteAttributionByAgent: query: %w", err)
	}
	defer rows.Close()

	// Group rows in-memory by agent. A single SQL query without window
	// functions can't emit "calls per agent" cleanly without a self-join,
	// so we re-derive Calls by reading the COUNT(DISTINCT) emitted on
	// every row (which is per agent_name + source_tag — wrong) and then
	// querying a second pass for the per-agent distinct call count.
	byAgent := map[string]*AgentSourceBreakdown{}
	for rows.Next() {
		var agent, tag string
		var total int64
		var perTagCalls int
		if err := rows.Scan(&agent, &tag, &total, &perTagCalls); err != nil {
			return nil, fmt.Errorf("PromptByteAttributionByAgent: scan: %w", err)
		}
		bd, ok := byAgent[agent]
		if !ok {
			bd = &AgentSourceBreakdown{
				AgentName: agent,
				BySource:  map[string]int64{},
			}
			byAgent[agent] = bd
		}
		bd.BySource[tag] += total
		bd.TotalBytes += total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PromptByteAttributionByAgent: rows iter: %w", err)
	}

	// Second pass: per-agent distinct call_timestamp count.
	q2 := `SELECT agent_name, COUNT(DISTINCT call_timestamp) FROM PromptByteAttribution`
	args2 := []any{}
	if since != "" {
		q2 += ` WHERE call_timestamp >= ?`
		args2 = append(args2, since)
	}
	q2 += ` GROUP BY agent_name`
	rows2, err := db.Query(q2, args2...)
	if err != nil {
		return nil, fmt.Errorf("PromptByteAttributionByAgent: calls query: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var agent string
		var n int
		if err := rows2.Scan(&agent, &n); err != nil {
			return nil, fmt.Errorf("PromptByteAttributionByAgent: calls scan: %w", err)
		}
		if bd, ok := byAgent[agent]; ok {
			bd.Calls = n
		}
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("PromptByteAttributionByAgent: calls rows iter: %w", err)
	}

	// Materialise Ordered slices and emit a stable agent order.
	out := make([]AgentSourceBreakdown, 0, len(byAgent))
	for _, bd := range byAgent {
		bd.Ordered = make([]AgentSourceTagTotal, 0, len(bd.BySource))
		for tag, b := range bd.BySource {
			bd.Ordered = append(bd.Ordered, AgentSourceTagTotal{SourceTag: tag, Bytes: b})
		}
		sort.Slice(bd.Ordered, func(i, j int) bool {
			if bd.Ordered[i].Bytes != bd.Ordered[j].Bytes {
				return bd.Ordered[i].Bytes > bd.Ordered[j].Bytes
			}
			return bd.Ordered[i].SourceTag < bd.Ordered[j].SourceTag
		})
		out = append(out, *bd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentName < out[j].AgentName })
	return out, nil
}

// PromptByteAttributionWindowSince returns a SQLite-shaped timestamp
// for `now - duration`. Provided for callers (dashboard, tests) that
// want a 7-day-rolling window without doing the time math themselves.
func PromptByteAttributionWindowSince(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format("2006-01-02 15:04:05")
}

// Package store — D3 P6B.3 / 6B.4 / 6B.5 Drill query layer.
//
// The Drill diagnostic surface needs a unified, time-ordered event
// stream pulled from many source tables: TaskHistory (status
// transitions), LLMCallTranscripts (every Claude call), GitOperationLog
// (every git/gh op), ConvoyReviewCycles, Escalations, BriefingRenders
// (operator decisions), AskBranchPRs (sub-PR state), and
// OperatorEventAnnotations (Drill-only operator notes).
//
// The query is a UNION ALL across a small per-source SELECT, each
// projecting the same column shape: (timestamp, kind, ref_id, actor,
// summary, raw_json). Pagination is via LIMIT N OFFSET; the convoy
// scope is enforced by the convoy_id filter on each branch.
//
// Performance — the brief targets < 500 ms for a 1000-event convoy.
// Each branch hits an index on (convoy_id, timestamp) or equivalent
// (idx_llmct_task, idx_gol_convoy, idx_crc_convoy, idx_bounty_convoy_status,
// idx_oea_event). The UNION is executed by SQLite as a sort-merge over
// the per-branch results; with 200-event pagination the typical
// response size is well under 200 KB.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// DrillEvent is one row in the unified event stream. Kind is the
// per-source label (e.g. "task_transition", "llm_call", "git_op",
// "cycle", "escalation", "decision", "annotation"). RefID is the
// per-source primary key so the SPA can navigate to the event drill
// view (#/drill/event/<kind>/<id>).
type DrillEvent struct {
	Timestamp string `json:"timestamp"`
	Kind      string `json:"kind"`
	RefID     int64  `json:"ref_id"`
	Actor     string `json:"actor"`
	Summary   string `json:"summary"`
	TaskID    int64  `json:"task_id,omitempty"`
}

// LoadConvoyDrillEvents returns the unified event stream for a convoy
// in chronological order. limit caps the response; offset enables
// pagination. The returned slice is ordered ASC by timestamp so the
// SPA can render top-to-bottom.
func LoadConvoyDrillEvents(ctx context.Context, db *sql.DB, convoyID int, limit, offset int) ([]DrillEvent, error) {
	if convoyID <= 0 {
		return nil, fmt.Errorf("LoadConvoyDrillEvents: convoy_id must be positive, got %d", convoyID)
	}
	if limit <= 0 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	// Per-branch projections share the column shape. The IFNULL wrap
	// on each text column keeps NULL columns (from older rows written
	// pre-migration) rendering as "" rather than panicking the Scan.
	q := `
SELECT * FROM (
	SELECT created_at AS ts, 'task_transition' AS kind, id AS ref_id,
	       agent AS actor, IFNULL(outcome, '') || ' on task ' || task_id AS summary,
	       task_id AS task_id
	  FROM TaskHistory
	 WHERE task_id IN (SELECT id FROM BountyBoard WHERE convoy_id = ?)

	UNION ALL

	SELECT call_started_at AS ts, 'llm_call' AS kind, id AS ref_id,
	       agent AS actor,
	       agent || ' · ' || IFNULL(prompt_version, '(unversioned)') AS summary,
	       task_id AS task_id
	  FROM LLMCallTranscripts
	 WHERE task_id IN (SELECT id FROM BountyBoard WHERE convoy_id = ?)

	UNION ALL

	SELECT started_at AS ts, 'git_op' AS kind, id AS ref_id,
	       'git' AS actor,
	       operation || IFNULL(' ('||branch||')', '') AS summary,
	       task_id AS task_id
	  FROM GitOperationLog
	 WHERE convoy_id = ?

	UNION ALL

	SELECT cycle_started_at AS ts, 'cycle' AS kind, id AS ref_id,
	       'convoy-review' AS actor,
	       'cycle ' || cycle_number AS summary,
	       0 AS task_id
	  FROM ConvoyReviewCycles
	 WHERE convoy_id = ?

	UNION ALL

	SELECT noted_at AS ts, 'annotation' AS kind, id AS ref_id,
	       operator_email AS actor,
	       IFNULL(flag,'note') || ': ' || substr(note_text, 1, 80) AS summary,
	       0 AS task_id
	  FROM OperatorEventAnnotations
	 WHERE event_kind != ''
	   AND event_ref IN (
	      SELECT CAST(id AS TEXT) FROM BountyBoard WHERE convoy_id = ?
	   )
) AS events
ORDER BY ts ASC
LIMIT ? OFFSET ?;`

	rows, err := db.QueryContext(ctx, q, convoyID, convoyID, convoyID, convoyID, convoyID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("LoadConvoyDrillEvents: %w", err)
	}
	defer rows.Close()
	var out []DrillEvent
	for rows.Next() {
		var e DrillEvent
		if scanErr := rows.Scan(&e.Timestamp, &e.Kind, &e.RefID, &e.Actor, &e.Summary, &e.TaskID); scanErr != nil {
			log.Printf("drill_queries.go:LoadConvoyDrillEvents: scan error: %v", scanErr)
			continue
		}
		out = append(out, e)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("LoadConvoyDrillEvents: rows iter: %w", rErr)
	}
	return out, nil
}

// LoadTaskDrillEvents returns the unified event stream for a single
// task. Same shape as LoadConvoyDrillEvents but scoped by task_id.
func LoadTaskDrillEvents(ctx context.Context, db *sql.DB, taskID int, limit, offset int) ([]DrillEvent, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("LoadTaskDrillEvents: task_id must be positive, got %d", taskID)
	}
	if limit <= 0 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	q := `
SELECT * FROM (
	SELECT created_at AS ts, 'task_transition' AS kind, id AS ref_id,
	       agent AS actor, IFNULL(outcome,'') AS summary, task_id AS task_id
	  FROM TaskHistory
	 WHERE task_id = ?

	UNION ALL

	SELECT call_started_at AS ts, 'llm_call' AS kind, id AS ref_id,
	       agent AS actor,
	       agent || ' · ' || IFNULL(prompt_version, '(unversioned)') AS summary,
	       task_id AS task_id
	  FROM LLMCallTranscripts
	 WHERE task_id = ?

	UNION ALL

	SELECT started_at AS ts, 'git_op' AS kind, id AS ref_id,
	       'git' AS actor,
	       operation || IFNULL(' ('||branch||')', '') AS summary,
	       task_id AS task_id
	  FROM GitOperationLog
	 WHERE task_id = ?
) AS events
ORDER BY ts ASC
LIMIT ? OFFSET ?;`

	rows, err := db.QueryContext(ctx, q, taskID, taskID, taskID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("LoadTaskDrillEvents: %w", err)
	}
	defer rows.Close()
	var out []DrillEvent
	for rows.Next() {
		var e DrillEvent
		if scanErr := rows.Scan(&e.Timestamp, &e.Kind, &e.RefID, &e.Actor, &e.Summary, &e.TaskID); scanErr != nil {
			log.Printf("drill_queries.go:LoadTaskDrillEvents: scan error: %v", scanErr)
			continue
		}
		out = append(out, e)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("LoadTaskDrillEvents: rows iter: %w", rErr)
	}
	return out, nil
}

// ConvoyDrillSpend is the per-(task,agent) cost rollup for a convoy.
type ConvoyDrillSpend struct {
	TaskID  int64   `json:"task_id"`
	Agent   string  `json:"agent"`
	CostUSD float64 `json:"cost_usd"`
	Calls   int     `json:"calls"`
	TokIn   int     `json:"tokens_in"`
	TokOut  int     `json:"tokens_out"`
}

// LoadConvoyDrillSpend computes a per-(task,agent) cost rollup for a
// convoy by joining LLMCallTranscripts to BountyBoard via task_id.
// Used by the right-hand spend inspector in the convoy drill view.
func LoadConvoyDrillSpend(ctx context.Context, db *sql.DB, convoyID int) ([]ConvoyDrillSpend, error) {
	if convoyID <= 0 {
		return nil, fmt.Errorf("LoadConvoyDrillSpend: convoy_id must be positive")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT t.task_id, t.agent, SUM(t.cost_usd), COUNT(*),
		        SUM(t.input_tokens), SUM(t.output_tokens)
		   FROM LLMCallTranscripts t
		   JOIN BountyBoard b ON b.id = t.task_id
		  WHERE b.convoy_id = ?
		  GROUP BY t.task_id, t.agent
		  ORDER BY SUM(t.cost_usd) DESC`,
		convoyID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConvoyDrillSpend
	for rows.Next() {
		var r ConvoyDrillSpend
		if scanErr := rows.Scan(&r.TaskID, &r.Agent, &r.CostUSD, &r.Calls, &r.TokIn, &r.TokOut); scanErr != nil {
			log.Printf("drill_queries.go:LoadConvoyDrillSpend: scan error: %v", scanErr)
			continue
		}
		out = append(out, r)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("LoadConvoyDrillSpend: rows iter: %w", rErr)
	}
	return out, nil
}

// LoadEventDetails returns the full body for a single drill event.
// The kind selects which source table to query; the SPA's per-kind
// renderer handles the JSON shape.
//
// Anti-cheat: kind is matched against a closed allowlist so a crafted
// URL can't pivot to a non-drill table.
func LoadEventDetails(ctx context.Context, db *sql.DB, kind string, refID int64) (map[string]any, error) {
	out := map[string]any{"kind": kind, "ref_id": refID}
	switch strings.ToLower(kind) {
	case "llm_call":
		var (
			agent, pv, started, completed, sysP, usrP, resp, toolJ, archived string
			taskID                                                            int64
			cost                                                              float64
			tokIn, tokOut, cacheR, cacheC                                     int
		)
		err := db.QueryRowContext(ctx,
			`SELECT task_id, agent, prompt_version, call_started_at, call_completed_at,
			        system_prompt, user_prompt, response_text, tool_calls_json, archived_at,
			        cost_usd, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
			   FROM LLMCallTranscripts WHERE id = ?`, refID,
		).Scan(&taskID, &agent, &pv, &started, &completed, &sysP, &usrP, &resp, &toolJ, &archived,
			&cost, &tokIn, &tokOut, &cacheR, &cacheC)
		if err != nil {
			return nil, err
		}
		out["task_id"] = taskID
		out["agent"] = agent
		out["prompt_version"] = pv
		out["call_started_at"] = started
		out["call_completed_at"] = completed
		out["system_prompt"] = sysP
		out["user_prompt"] = usrP
		out["response_text"] = resp
		out["tool_calls_json"] = toolJ
		out["archived_at"] = archived
		out["cost_usd"] = cost
		out["input_tokens"] = tokIn
		out["output_tokens"] = tokOut
		out["cache_read_tokens"] = cacheR
		out["cache_creation_tokens"] = cacheC
	case "git_op":
		var (
			operation, repo, args, started, stdoutEx, stderrEx, branch, beforeSHA, afterSHA string
			taskID, convoyID, durMs, exitCode                                                int64
		)
		err := db.QueryRowContext(ctx,
			`SELECT operation, repo, args_json, started_at,
			        stdout_excerpt, stderr_excerpt, branch, before_sha, after_sha,
			        task_id, convoy_id, duration_ms, exit_code
			   FROM GitOperationLog WHERE id = ?`, refID,
		).Scan(&operation, &repo, &args, &started, &stdoutEx, &stderrEx, &branch, &beforeSHA, &afterSHA,
			&taskID, &convoyID, &durMs, &exitCode)
		if err != nil {
			return nil, err
		}
		out["operation"] = operation
		out["repo"] = repo
		out["args_json"] = args
		out["started_at"] = started
		out["stdout_excerpt"] = stdoutEx
		out["stderr_excerpt"] = stderrEx
		out["branch"] = branch
		out["before_sha"] = beforeSHA
		out["after_sha"] = afterSHA
		out["task_id"] = taskID
		out["convoy_id"] = convoyID
		out["duration_ms"] = durMs
		out["exit_code"] = exitCode
	case "task_transition":
		var (
			taskID, attempt                int64
			agent, sessID, claudeOut, outcome, promptVer, createdAt, memIDs string
			tokIn, tokOut                  int
			cost                           float64
		)
		err := db.QueryRowContext(ctx,
			`SELECT task_id, attempt, agent, session_id, claude_output, outcome,
			        tokens_in, tokens_out, cost_usd_estimate, IFNULL(memory_ids,''),
			        IFNULL(prompt_version,''), IFNULL(created_at,'')
			   FROM TaskHistory WHERE id = ?`, refID,
		).Scan(&taskID, &attempt, &agent, &sessID, &claudeOut, &outcome,
			&tokIn, &tokOut, &cost, &memIDs, &promptVer, &createdAt)
		if err != nil {
			return nil, err
		}
		out["task_id"] = taskID
		out["attempt"] = attempt
		out["agent"] = agent
		out["session_id"] = sessID
		out["claude_output"] = claudeOut
		out["outcome"] = outcome
		out["tokens_in"] = tokIn
		out["tokens_out"] = tokOut
		out["cost_usd_estimate"] = cost
		out["memory_ids"] = memIDs
		out["prompt_version"] = promptVer
		out["created_at"] = createdAt
	case "cycle":
		var (
			convoyID, cycleNum int64
			specVer, started, completed, outcomes, fixSpawned, amendments string
		)
		err := db.QueryRowContext(ctx,
			`SELECT convoy_id, cycle_number, spec_version_at_start,
			        cycle_started_at, IFNULL(cycle_completed_at,''),
			        IFNULL(outcomes_json,'{}'), IFNULL(fix_tasks_spawned_json,'[]'),
			        IFNULL(amendments_proposed_json,'[]')
			   FROM ConvoyReviewCycles WHERE id = ?`, refID,
		).Scan(&convoyID, &cycleNum, &specVer, &started, &completed,
			&outcomes, &fixSpawned, &amendments)
		if err != nil {
			return nil, err
		}
		out["convoy_id"] = convoyID
		out["cycle_number"] = cycleNum
		out["spec_version_at_start"] = specVer
		out["cycle_started_at"] = started
		out["cycle_completed_at"] = completed
		out["outcomes_json"] = outcomes
		out["fix_tasks_spawned_json"] = fixSpawned
		out["amendments_proposed_json"] = amendments
	case "annotation":
		var (
			operator, ek, er, note, flag, noted string
		)
		err := db.QueryRowContext(ctx,
			`SELECT operator_email, event_kind, event_ref, note_text, IFNULL(flag,''), noted_at
			   FROM OperatorEventAnnotations WHERE id = ?`, refID,
		).Scan(&operator, &ek, &er, &note, &flag, &noted)
		if err != nil {
			return nil, err
		}
		out["operator_email"] = operator
		out["event_kind"] = ek
		out["event_ref"] = er
		out["note_text"] = note
		out["flag"] = flag
		out["noted_at"] = noted
	default:
		return nil, fmt.Errorf("unknown drill event kind: %q", kind)
	}
	return out, nil
}

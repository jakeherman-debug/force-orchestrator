// Package store — D3 P6B.6 Drill free-text search via sqlite_fts5.
//
// Builds external-content fts5 virtual tables shadowing:
//   - LLMCallTranscripts.{system_prompt, user_prompt, response_text}
//   - BountyBoard.payload
//   - GitOperationLog.{stdout_excerpt, stderr_excerpt}
//   - ConvoyReviewCycles.{outcomes_json, amendments_proposed_json}
//   - BriefingRenders.briefing_text
//   - OperatorEventAnnotations.note_text
//
// Each fts5 table uses content='<source-table>', content_rowid='id'
// (external-content mode) so we don't double-store the bodies. AFTER
// INSERT / UPDATE / DELETE triggers keep the index in sync.
//
// The search endpoint returns a unified result list with snippet +
// score so the SPA can render highlighted hits across all sources.
//
// The build runs once per holocron init (idempotent: CREATE VIRTUAL
// TABLE IF NOT EXISTS, CREATE TRIGGER IF NOT EXISTS); existing
// content is back-populated on first build via INSERT INTO ftsX
// SELECT.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// DrillSearchResult is one row in the unified search response.
type DrillSearchResult struct {
	Kind    string  `json:"kind"`     // 'llm_call' | 'task' | 'git_op' | 'cycle' | 'briefing' | 'annotation'
	RefID   int64   `json:"ref_id"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

// EnsureDrillFTS5 creates the fts5 virtual tables + sync triggers if
// they don't already exist. Safe to call repeatedly — every CREATE
// is `IF NOT EXISTS`. Returns an error so the caller can surface
// build failures (e.g. a SQLite without sqlite_fts5 compiled in).
func EnsureDrillFTS5(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("EnsureDrillFTS5: nil db")
	}

	stmts := []string{
		// --- LLMCallTranscripts ---
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_llmct USING fts5(
		    system_prompt, user_prompt, response_text,
		    content='LLMCallTranscripts', content_rowid='id'
		);`,
		// Back-populate (NOOP after first run; the OR REPLACE shape
		// would be wrong for fts5 external-content; INSERT OR IGNORE
		// keyed on rowid keeps it idempotent.).
		`INSERT INTO fts_llmct(rowid, system_prompt, user_prompt, response_text)
		    SELECT id, system_prompt, user_prompt, response_text FROM LLMCallTranscripts
		     WHERE id NOT IN (SELECT rowid FROM fts_llmct);`,
		`CREATE TRIGGER IF NOT EXISTS llmct_ai AFTER INSERT ON LLMCallTranscripts BEGIN
		   INSERT INTO fts_llmct(rowid, system_prompt, user_prompt, response_text)
		      VALUES (new.id, new.system_prompt, new.user_prompt, new.response_text);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS llmct_au AFTER UPDATE ON LLMCallTranscripts BEGIN
		   INSERT INTO fts_llmct(fts_llmct, rowid, system_prompt, user_prompt, response_text)
		      VALUES('delete', old.id, old.system_prompt, old.user_prompt, old.response_text);
		   INSERT INTO fts_llmct(rowid, system_prompt, user_prompt, response_text)
		      VALUES (new.id, new.system_prompt, new.user_prompt, new.response_text);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS llmct_ad AFTER DELETE ON LLMCallTranscripts BEGIN
		   INSERT INTO fts_llmct(fts_llmct, rowid, system_prompt, user_prompt, response_text)
		      VALUES('delete', old.id, old.system_prompt, old.user_prompt, old.response_text);
		END;`,

		// --- BountyBoard.payload ---
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_bounty USING fts5(
		    payload, content='BountyBoard', content_rowid='id'
		);`,
		`INSERT INTO fts_bounty(rowid, payload)
		    SELECT id, IFNULL(payload, '') FROM BountyBoard
		     WHERE id NOT IN (SELECT rowid FROM fts_bounty);`,
		`CREATE TRIGGER IF NOT EXISTS bounty_ai AFTER INSERT ON BountyBoard BEGIN
		   INSERT INTO fts_bounty(rowid, payload) VALUES (new.id, IFNULL(new.payload,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS bounty_au AFTER UPDATE ON BountyBoard BEGIN
		   INSERT INTO fts_bounty(fts_bounty, rowid, payload) VALUES('delete', old.id, IFNULL(old.payload,''));
		   INSERT INTO fts_bounty(rowid, payload) VALUES (new.id, IFNULL(new.payload,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS bounty_ad AFTER DELETE ON BountyBoard BEGIN
		   INSERT INTO fts_bounty(fts_bounty, rowid, payload) VALUES('delete', old.id, IFNULL(old.payload,''));
		END;`,

		// --- GitOperationLog ---
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_gitop USING fts5(
		    stdout_excerpt, stderr_excerpt,
		    content='GitOperationLog', content_rowid='id'
		);`,
		`INSERT INTO fts_gitop(rowid, stdout_excerpt, stderr_excerpt)
		    SELECT id, IFNULL(stdout_excerpt,''), IFNULL(stderr_excerpt,'') FROM GitOperationLog
		     WHERE id NOT IN (SELECT rowid FROM fts_gitop);`,
		`CREATE TRIGGER IF NOT EXISTS gitop_ai AFTER INSERT ON GitOperationLog BEGIN
		   INSERT INTO fts_gitop(rowid, stdout_excerpt, stderr_excerpt)
		      VALUES (new.id, IFNULL(new.stdout_excerpt,''), IFNULL(new.stderr_excerpt,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS gitop_au AFTER UPDATE ON GitOperationLog BEGIN
		   INSERT INTO fts_gitop(fts_gitop, rowid, stdout_excerpt, stderr_excerpt)
		      VALUES('delete', old.id, IFNULL(old.stdout_excerpt,''), IFNULL(old.stderr_excerpt,''));
		   INSERT INTO fts_gitop(rowid, stdout_excerpt, stderr_excerpt)
		      VALUES (new.id, IFNULL(new.stdout_excerpt,''), IFNULL(new.stderr_excerpt,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS gitop_ad AFTER DELETE ON GitOperationLog BEGIN
		   INSERT INTO fts_gitop(fts_gitop, rowid, stdout_excerpt, stderr_excerpt)
		      VALUES('delete', old.id, IFNULL(old.stdout_excerpt,''), IFNULL(old.stderr_excerpt,''));
		END;`,

		// --- ConvoyReviewCycles ---
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_cycle USING fts5(
		    outcomes_json, amendments_proposed_json,
		    content='ConvoyReviewCycles', content_rowid='id'
		);`,
		`INSERT INTO fts_cycle(rowid, outcomes_json, amendments_proposed_json)
		    SELECT id, IFNULL(outcomes_json,''), IFNULL(amendments_proposed_json,'') FROM ConvoyReviewCycles
		     WHERE id NOT IN (SELECT rowid FROM fts_cycle);`,
		`CREATE TRIGGER IF NOT EXISTS cycle_ai AFTER INSERT ON ConvoyReviewCycles BEGIN
		   INSERT INTO fts_cycle(rowid, outcomes_json, amendments_proposed_json)
		      VALUES (new.id, IFNULL(new.outcomes_json,''), IFNULL(new.amendments_proposed_json,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS cycle_au AFTER UPDATE ON ConvoyReviewCycles BEGIN
		   INSERT INTO fts_cycle(fts_cycle, rowid, outcomes_json, amendments_proposed_json)
		      VALUES('delete', old.id, IFNULL(old.outcomes_json,''), IFNULL(old.amendments_proposed_json,''));
		   INSERT INTO fts_cycle(rowid, outcomes_json, amendments_proposed_json)
		      VALUES (new.id, IFNULL(new.outcomes_json,''), IFNULL(new.amendments_proposed_json,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS cycle_ad AFTER DELETE ON ConvoyReviewCycles BEGIN
		   INSERT INTO fts_cycle(fts_cycle, rowid, outcomes_json, amendments_proposed_json)
		      VALUES('delete', old.id, IFNULL(old.outcomes_json,''), IFNULL(old.amendments_proposed_json,''));
		END;`,

		// --- BriefingRenders ---
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_briefing USING fts5(
		    briefing_text, content='BriefingRenders', content_rowid='id'
		);`,
		`INSERT INTO fts_briefing(rowid, briefing_text)
		    SELECT id, IFNULL(briefing_text,'') FROM BriefingRenders
		     WHERE id NOT IN (SELECT rowid FROM fts_briefing);`,
		`CREATE TRIGGER IF NOT EXISTS briefing_ai AFTER INSERT ON BriefingRenders BEGIN
		   INSERT INTO fts_briefing(rowid, briefing_text) VALUES (new.id, IFNULL(new.briefing_text,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS briefing_au AFTER UPDATE ON BriefingRenders BEGIN
		   INSERT INTO fts_briefing(fts_briefing, rowid, briefing_text) VALUES('delete', old.id, IFNULL(old.briefing_text,''));
		   INSERT INTO fts_briefing(rowid, briefing_text) VALUES (new.id, IFNULL(new.briefing_text,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS briefing_ad AFTER DELETE ON BriefingRenders BEGIN
		   INSERT INTO fts_briefing(fts_briefing, rowid, briefing_text) VALUES('delete', old.id, IFNULL(old.briefing_text,''));
		END;`,

		// --- OperatorEventAnnotations ---
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_annotation USING fts5(
		    note_text, content='OperatorEventAnnotations', content_rowid='id'
		);`,
		`INSERT INTO fts_annotation(rowid, note_text)
		    SELECT id, IFNULL(note_text,'') FROM OperatorEventAnnotations
		     WHERE id NOT IN (SELECT rowid FROM fts_annotation);`,
		`CREATE TRIGGER IF NOT EXISTS oea_ai AFTER INSERT ON OperatorEventAnnotations BEGIN
		   INSERT INTO fts_annotation(rowid, note_text) VALUES (new.id, IFNULL(new.note_text,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS oea_au AFTER UPDATE ON OperatorEventAnnotations BEGIN
		   INSERT INTO fts_annotation(fts_annotation, rowid, note_text) VALUES('delete', old.id, IFNULL(old.note_text,''));
		   INSERT INTO fts_annotation(rowid, note_text) VALUES (new.id, IFNULL(new.note_text,''));
		END;`,
		`CREATE TRIGGER IF NOT EXISTS oea_ad AFTER DELETE ON OperatorEventAnnotations BEGIN
		   INSERT INTO fts_annotation(fts_annotation, rowid, note_text) VALUES('delete', old.id, IFNULL(old.note_text,''));
		END;`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("EnsureDrillFTS5: %s: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// SearchDrill runs `q` against all fts5 tables and returns merged
// results. scope is "global" or "convoy" (with convoyID). limit caps
// total rows; the per-source budget is roughly limit/6.
func SearchDrill(ctx context.Context, db *sql.DB, q string, scope string, convoyID int, limit int) ([]DrillSearchResult, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	perSource := limit/6 + 1

	// fts5 query with snippet() function. snippet args:
	//   table, column-index, prefix, suffix, ellipsis, max-tokens.
	// The negative bm25 score makes higher relevance = larger value
	// when sorted DESC.
	type src struct {
		table   string
		colExpr string
		kind    string
		colIdx  int
	}
	sources := []src{
		{"fts_llmct", "user_prompt", "llm_call", 1},
		{"fts_bounty", "payload", "task", 0},
		{"fts_gitop", "stdout_excerpt", "git_op", 0},
		{"fts_cycle", "outcomes_json", "cycle", 0},
		{"fts_briefing", "briefing_text", "briefing", 0},
		{"fts_annotation", "note_text", "annotation", 0},
	}

	var results []DrillSearchResult
	for _, s := range sources {
		// Build per-source query. Convoy scope folds in via a JOIN
		// for fts_llmct / fts_bounty / fts_gitop; the rest is global
		// even when scope is "convoy" because they're not directly
		// convoy-scoped at the row level.
		var query string
		var args []any
		switch {
		case scope == "convoy" && convoyID > 0 && s.kind == "llm_call":
			query = fmt.Sprintf(
				`SELECT f.rowid, snippet(%s, %d, '<<','>>','…', 16), bm25(%s)
				   FROM %s f
				   JOIN LLMCallTranscripts t ON t.id = f.rowid
				   JOIN BountyBoard b ON b.id = t.task_id
				  WHERE %s MATCH ?
				    AND b.convoy_id = ?
				  ORDER BY bm25(%s) LIMIT ?`,
				s.table, s.colIdx, s.table, s.table, s.table, s.table)
			args = []any{q, convoyID, perSource}
		case scope == "convoy" && convoyID > 0 && s.kind == "task":
			query = fmt.Sprintf(
				`SELECT f.rowid, snippet(%s, %d, '<<','>>','…', 16), bm25(%s)
				   FROM %s f
				   JOIN BountyBoard b ON b.id = f.rowid
				  WHERE %s MATCH ?
				    AND b.convoy_id = ?
				  ORDER BY bm25(%s) LIMIT ?`,
				s.table, s.colIdx, s.table, s.table, s.table, s.table)
			args = []any{q, convoyID, perSource}
		case scope == "convoy" && convoyID > 0 && s.kind == "git_op":
			query = fmt.Sprintf(
				`SELECT f.rowid, snippet(%s, %d, '<<','>>','…', 16), bm25(%s)
				   FROM %s f
				   JOIN GitOperationLog g ON g.id = f.rowid
				  WHERE %s MATCH ?
				    AND g.convoy_id = ?
				  ORDER BY bm25(%s) LIMIT ?`,
				s.table, s.colIdx, s.table, s.table, s.table, s.table)
			args = []any{q, convoyID, perSource}
		default:
			query = fmt.Sprintf(
				`SELECT rowid, snippet(%s, %d, '<<','>>','…', 16), bm25(%s)
				   FROM %s
				  WHERE %s MATCH ?
				  ORDER BY bm25(%s) LIMIT ?`,
				s.table, s.colIdx, s.table, s.table, s.table, s.table)
			args = []any{q, perSource}
		}

		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			// fts5 syntax errors on user input shouldn't error the
			// whole search — just log + skip this source.
			log.Printf("drill_search.go:SearchDrill: %s skipped: %v", s.table, err)
			continue
		}
		for rows.Next() {
			var r DrillSearchResult
			r.Kind = s.kind
			if scanErr := rows.Scan(&r.RefID, &r.Snippet, &r.Score); scanErr != nil {
				continue
			}
			results = append(results, r)
		}
		if rErr := rows.Err(); rErr != nil {
			log.Printf("drill_search.go:SearchDrill: %s rows iter error: %v", s.table, rErr)
		}
		rows.Close()
	}

	// Sort by score (lower bm25 = better; we surface as raw score).
	// Cap at limit.
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

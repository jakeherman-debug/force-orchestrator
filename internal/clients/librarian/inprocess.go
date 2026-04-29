package librarian

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"force-orchestrator/internal/store"
)

const defaultScopeLimit = 100

// inProcessClient is the in-process Client backing. It enqueues
// WriteMemory bounties via store.AddBounty(Tx) for the Librarian Spawn
// loop in internal/agents/librarian.go to consume, and reads directly
// from the FleetMemory table for the Get* / Update / Remove methods.
//
// This is the unexported concrete type; callers obtain it through
// NewInProcess. Pattern P16 forbids agent code from referencing this
// struct directly.
type inProcessClient struct {
	db *sql.DB
}

// NewInProcess returns a Client backed by the supplied holocron.db
// handle. The returned Client is safe for concurrent use — every method
// scopes its DB work to a single statement or one open transaction.
func NewInProcess(db *sql.DB) Client {
	return &inProcessClient{db: db}
}

// writeMemoryPayload mirrors the JSON shape that the Librarian Spawn
// loop expects in WriteMemory bounty payloads. The struct intentionally
// duplicates the one in internal/agents/librarian.go so the client
// package does not import the agents package; both definitions must
// stay in lockstep (Memory data type → bounty payload). If the on-wire
// shape changes, update both ends in the same commit.
type writeMemoryPayload struct {
	Task     string `json:"task"`
	Files    string `json:"files"`
	Feedback string `json:"feedback"`
	Diff     string `json:"diff"`
	Repo     string `json:"repo"`
}

func encodeWritePayload(m Memory) (string, error) {
	body, err := json.Marshal(writeMemoryPayload{
		Task:     m.Task,
		Files:    m.Files,
		Feedback: m.Feedback,
		Diff:     m.Diff,
		Repo:     m.Repo,
	})
	if err != nil {
		return "", fmt.Errorf("librarian: marshal WriteMemory payload: %w", err)
	}
	return string(body), nil
}

// WriteMemory enqueues a WriteMemory bounty using a fresh single-shot
// insert. The returned int is the BountyBoard.id of the queued task.
func (c *inProcessClient) WriteMemory(ctx context.Context, m Memory) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	payload, err := encodeWritePayload(m)
	if err != nil {
		return 0, err
	}
	id := store.AddBounty(c.db, m.ParentTaskID, "WriteMemory", payload)
	return id, nil
}

// WriteMemoryTx enqueues a WriteMemory bounty inside the caller's open
// transaction. The caller is responsible for tx.Commit / tx.Rollback;
// this method only issues the INSERT.
func (c *inProcessClient) WriteMemoryTx(ctx context.Context, tx *sql.Tx, m Memory) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if tx == nil {
		return 0, fmt.Errorf("librarian: WriteMemoryTx requires a non-nil *sql.Tx")
	}
	payload, err := encodeWritePayload(m)
	if err != nil {
		return 0, err
	}
	id, err := store.AddBountyTx(tx, m.ParentTaskID, "WriteMemory", payload)
	if err != nil {
		return 0, fmt.Errorf("librarian: enqueue WriteMemory: %w", err)
	}
	return id, nil
}

// GetMemoriesForTask returns FleetMemory rows for the given parent task.
// Empty result is returned as (nil, nil); a query error is returned as
// (nil, err).
func (c *inProcessClient) GetMemoriesForTask(ctx context.Context, taskID int) ([]Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, repo, task_id, IFNULL(outcome,''), IFNULL(summary,''),
		       IFNULL(files_changed,''), IFNULL(topic_tags,''),
		       IFNULL(created_at,'')
		  FROM FleetMemory
		 WHERE task_id = ?
		 ORDER BY created_at DESC, id DESC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("librarian: GetMemoriesForTask query: %w", err)
	}
	defer rows.Close()
	out, scanErr := scanMemoryRows(rows)
	if scanErr != nil {
		return nil, scanErr
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("librarian: GetMemoriesForTask rows iter: %w", rerr)
	}
	return out, nil
}

// GetMemoriesByScope returns FleetMemory rows matching the supplied
// scope. Returns ErrEmptyScope if neither Repo nor SinceCreatedAt is
// set; ErrInvalidLimit if Limit is negative.
func (c *inProcessClient) GetMemoriesByScope(ctx context.Context, s Scope) ([]Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Repo == "" && s.SinceCreatedAt == "" {
		return nil, ErrEmptyScope
	}
	if s.Limit < 0 {
		return nil, ErrInvalidLimit
	}
	limit := s.Limit
	if limit == 0 {
		limit = defaultScopeLimit
	}

	q := `SELECT id, repo, task_id, IFNULL(outcome,''), IFNULL(summary,''),
	             IFNULL(files_changed,''), IFNULL(topic_tags,''),
	             IFNULL(created_at,'')
	        FROM FleetMemory WHERE 1=1`
	var args []any
	if s.Repo != "" {
		q += ` AND repo = ?`
		args = append(args, s.Repo)
	}
	if s.SinceCreatedAt != "" {
		q += ` AND created_at >= ?`
		args = append(args, s.SinceCreatedAt)
	}
	if s.Outcome != "" {
		q += ` AND outcome = ?`
		args = append(args, s.Outcome)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("librarian: GetMemoriesByScope query: %w", err)
	}
	defer rows.Close()
	out, scanErr := scanMemoryRows(rows)
	if scanErr != nil {
		return nil, scanErr
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("librarian: GetMemoriesByScope rows iter: %w", rerr)
	}
	return out, nil
}

// UpdateMemory rewrites summary / files_changed / topic_tags on an
// existing row. Empty-string fields are skipped (a single space " " is
// treated as "clear this field" so callers have an explicit way to
// blank a value). The FTS row is rewritten in the same transaction.
func (c *inProcessClient) UpdateMemory(ctx context.Context, memoryID int, u MemoryUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.Summary == "" && u.FilesChanged == "" && u.TopicTags == "" {
		return nil // nothing to do
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("librarian: UpdateMemory begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Read current values so we can rewrite the FTS row with the merged shape.
	var summary, files, tags string
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT IFNULL(summary,''), IFNULL(files_changed,''), IFNULL(topic_tags,'')
		   FROM FleetMemory WHERE id = ?`, memoryID).
		Scan(&summary, &files, &tags); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("librarian: UpdateMemory read: %w", scanErr)
	}

	if u.Summary != "" {
		summary = normalizeUpdateField(u.Summary)
	}
	if u.FilesChanged != "" {
		files = normalizeUpdateField(u.FilesChanged)
	}
	if u.TopicTags != "" {
		tags = normalizeUpdateField(u.TopicTags)
	}

	if _, execErr := tx.ExecContext(ctx,
		`UPDATE FleetMemory
		    SET summary = ?, files_changed = ?, topic_tags = ?
		  WHERE id = ?`,
		summary, files, tags, memoryID); execErr != nil {
		return fmt.Errorf("librarian: UpdateMemory update: %w", execErr)
	}

	// FTS row carries rowid = FleetMemory.id; rewrite by delete+insert
	// rather than UPDATE so we don't need an FTS-specific clause.
	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM FleetMemory_fts WHERE rowid = ?`, memoryID); execErr != nil {
		return fmt.Errorf("librarian: UpdateMemory fts delete: %w", execErr)
	}
	if _, execErr := tx.ExecContext(ctx,
		`INSERT INTO FleetMemory_fts(rowid, summary, files_changed, topic_tags)
		 VALUES (?, ?, ?, ?)`,
		memoryID, summary, files, tags); execErr != nil {
		return fmt.Errorf("librarian: UpdateMemory fts insert: %w", execErr)
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("librarian: UpdateMemory commit: %w", commitErr)
	}
	return nil
}

// RemoveMemory deletes a memory row by ID via store.DeleteFleetMemory.
// Returns ErrNotFound if no row matched.
func (c *inProcessClient) RemoveMemory(ctx context.Context, memoryID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !store.DeleteFleetMemory(c.db, memoryID) {
		return ErrNotFound
	}
	return nil
}

// summarizeContextOverflowSystemPrompt instructs Claude to compress
// the provided prompt down to a target byte budget. The instructions
// are deliberately strict — we want a tight compression, not a
// general "shorten this" rewrite, because the output goes back into a
// downstream agent's prompt. Tone, schema, and JSON-block boundaries
// must be preserved.
const summarizeContextOverflowSystemPrompt = `You are the Fleet Librarian's context-overflow summarizer. You receive an LLM prompt that exceeds an agent's byte budget. Your job is to produce a SHORTER version of the SAME prompt that:

1. Preserves every section header (lines starting with #, ##, etc.)
2. Preserves every JSON schema instruction verbatim
3. Preserves every XML sentinel tag (<user_content>, </user_content>) intact and in place
4. Preserves every fleet-rule clause exactly
5. Compresses long file_read / diff blocks by:
   - Keeping the first 200 lines and last 100 lines verbatim
   - Replacing the middle with "[... N lines elided by context-overflow summarizer ...]"
6. Keeps the total byte length AT OR BELOW the target budget supplied in the user prompt

Respond with ONLY the shortened prompt. No preamble. No explanation. No markdown fence around it. The output replaces the input verbatim in the downstream LLM call, so anything you add (commentary, headers like "Here is the summary:") will derail the next agent.

If you cannot reduce below the target budget while preserving the rules above, return the prompt as-is — the caller routes that case through handleInfraFailure rather than silently truncating.`

// SummarizeForContextOverflow (D2 T1-2) condenses an over-cap LLM
// prompt via a single-turn Claude call. Implementation note: we do
// NOT use a Haiku-tier model here yet — the claude CLI doesn't expose
// per-call model selection through our wrapper. When the wrapper
// gains a model arg, switch this call to it; for now the default
// model handles the summarization, which is still cheaper than
// returning ErrContextOverflow and letting the parent task burn a
// retry on a context-overflow infra failure.
//
// targetBytes is forwarded to the prompt so Claude knows the budget.
// The returned string MAY exceed targetBytes — the caller checks the
// length and routes to ErrContextOverflow on failure. We don't
// silently truncate; that would slice a JSON block or XML tag and
// produce malformed output for the downstream agent.
func (c *inProcessClient) SummarizeForContextOverflow(ctx context.Context, prompt string, targetBytes int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if targetBytes <= 0 {
		return "", fmt.Errorf("librarian: SummarizeForContextOverflow: targetBytes must be positive, got %d", targetBytes)
	}
	// The summarize itself is reachable from a callsite that holds an
	// agent attribution context; stamp our own attribution so the
	// recursive ingress check in claude.go records "librarian" as the
	// caller and counts these bytes against the librarian agent's cap
	// (which is large by default — we don't want a feedback loop
	// where the summarizer overflows AGAIN).
	subCtx := contextOverflowCallContext(ctx, prompt)
	userPrompt := fmt.Sprintf("TARGET BUDGET: %d bytes (UTF-8). Below is the prompt to compress.\n\n%s",
		targetBytes, prompt)

	out, err := callClaudeForSummarize(subCtx, summarizeContextOverflowSystemPrompt, userPrompt)
	if err != nil {
		return "", fmt.Errorf("librarian: SummarizeForContextOverflow Claude call: %w", err)
	}
	// Strip any trailing claude_usage annotation that the runner
	// may append; we want only the summary text.
	out = stripUsageAnnotation(out)
	return strings.TrimSpace(out), nil
}

// stripUsageAnnotation removes the trailing
// "[claude_usage: X input Y output]" line injected by the CLI runner
// from a result string. Same logic as claude.ExtractJSON's leading
// strip, exposed here so we don't drag a JSON-fence parse along.
func stripUsageAnnotation(s string) string {
	idx := strings.LastIndex(s, "\n[claude_usage:")
	if idx < 0 {
		return s
	}
	return s[:idx]
}

// normalizeUpdateField maps the " " sentinel back to "" so callers can
// explicitly clear a field via UpdateMemory. Any other input passes
// through unchanged.
func normalizeUpdateField(v string) string {
	if v == " " {
		return ""
	}
	return v
}

// scanMemoryRows is the shared row-scanner for the read methods. The
// caller is responsible only for rows.Close(); rows.Err() is checked
// here (Pattern P1.1) so the helper is a single inspection unit.
func scanMemoryRows(rows *sql.Rows) ([]Memory, error) {
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(
			&m.ID, &m.Repo, &m.ParentTaskID, &m.Outcome, &m.Summary,
			&m.Files, &m.TopicTags, &m.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("librarian: scan FleetMemory row: %w", err)
		}
		out = append(out, m)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("librarian: scan FleetMemory rows iter: %w", rerr)
	}
	return out, nil
}

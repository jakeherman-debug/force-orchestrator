package librarian

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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

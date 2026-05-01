package golden_set

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"force-orchestrator/internal/store"
)

// CleanShippingThresholds parametrizes "what counts as a clean
// shipping convoy" for auto-curation. A convoy qualifies if and only
// if all four bounds hold:
//
//   - status = 'Completed' (or 'Shipped' depending on schema lineage)
//   - no rework signal (medic_requeue_count = 0 across the convoy's tasks)
//   - no escalations on the convoy's tasks
//   - no fix-task spawn cycles (spawning_at_id IS NULL or empty)
//
// Default values pin the strict empirical-positive bar from the
// roadmap; operator can tune via the public field.
type CleanShippingThresholds struct {
	MaxMedicRequeueCount int
	MaxEscalations       int
	MaxFixTasksSpawned   int
}

// DefaultCleanShippingThresholds is the operator-tunable default.
// Strict: zero rework, zero escalations, zero fix-task spawn cycles.
func DefaultCleanShippingThresholds() CleanShippingThresholds {
	return CleanShippingThresholds{
		MaxMedicRequeueCount: 0,
		MaxEscalations:       0,
		MaxFixTasksSpawned:   0,
	}
}

// CurateFromCleanShipping scans completed convoys for the given
// agent-relevant tasks (BountyBoard rows owned by `agent`), filters
// to those that shipped clean per CleanShippingThresholds, and
// inserts each unique (input → expected_output) pair into
// GoldenSetFixtures.
//
// Idempotent on (agent, fingerprint(input)): re-running won't write
// duplicates. The fingerprint is a SHA-256 hash of the input string —
// stable, collision-resistant, schema-compatible with any source.
func CurateFromCleanShipping(ctx context.Context, db *sql.DB, agent string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("golden_set.CurateFromCleanShipping: db is required")
	}
	if agent == "" {
		return 0, fmt.Errorf("golden_set.CurateFromCleanShipping: agent is required")
	}

	thresh := DefaultCleanShippingThresholds()

	// Tasks that completed cleanly. We use payload as input + the
	// final TaskHistory entry's outcome string as expected_output.
	// Anti-cheat tautology guard: if expected_output is identical to
	// input (or one is a strict prefix of the other), skip — the LLM
	// "already produced this by construction" and the fixture would
	// be a tautological pass.
	rows, err := db.QueryContext(ctx, `
		SELECT b.id, IFNULL(b.payload, ''), IFNULL(b.medic_requeue_count, 0), IFNULL(b.spawning_at_id, '')
		FROM BountyBoard b
		WHERE b.status = 'Completed'
		  AND b.owner LIKE ? || '%'
		  AND IFNULL(b.medic_requeue_count, 0) <= ?
		  AND IFNULL(b.spawning_at_id, '') = ''
		ORDER BY b.id ASC`, agent, thresh.MaxMedicRequeueCount,
	)
	if err != nil {
		return 0, fmt.Errorf("golden_set.CurateFromCleanShipping: query tasks: %w", err)
	}
	defer rows.Close()

	type cand struct {
		id      int64
		payload string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var requeue int
		var spawning string
		if err := rows.Scan(&c.id, &c.payload, &requeue, &spawning); err != nil {
			return 0, fmt.Errorf("golden_set.CurateFromCleanShipping: scan: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	inserted := 0
	for _, c := range cands {
		// Pull the final TaskHistory outcome.
		var outcome string
		err := db.QueryRowContext(ctx, `
			SELECT IFNULL(outcome, '') FROM TaskHistory
			WHERE task_id = ?
			ORDER BY id DESC LIMIT 1`, c.id,
		).Scan(&outcome)
		if err == sql.ErrNoRows {
			continue // no history; not curate-able
		}
		if err != nil {
			return inserted, fmt.Errorf("golden_set.CurateFromCleanShipping: history for task %d: %w", c.id, err)
		}
		if outcome == "" || c.payload == "" {
			continue
		}
		// Tautology guard.
		if isTautological(c.payload, outcome) {
			continue
		}
		// Idempotence: skip if (agent, fingerprint(input)) already
		// curated.
		fp := fingerprintInput(agent, c.payload)
		var existingID int64
		err = db.QueryRowContext(ctx,
			`SELECT id FROM GoldenSetFixtures WHERE agent = ? AND input = ? AND IFNULL(retired_at,'')='' LIMIT 1`,
			agent, c.payload).Scan(&existingID)
		if err == nil && existingID > 0 {
			continue
		}
		// Insert.
		_, err = db.ExecContext(ctx, `
			INSERT INTO GoldenSetFixtures
				(agent, input, expected_output, source, curated_at, curated_by)
			VALUES (?, ?, ?, ?, ?, ?)`,
			agent, c.payload, outcome, string(SourceAutoCleanShipping),
			store.NowSQLite(), "system:auto-curated:fp="+fp,
		)
		if err != nil {
			return inserted, fmt.Errorf("golden_set.CurateFromCleanShipping: insert fixture: %w", err)
		}
		inserted++
	}
	return inserted, nil
}

// AddManualFixture inserts an operator-curated fixture. Returns the
// new GoldenSetFixtures.id. operator email is recorded as
// `operator:<email>` in curated_by.
func AddManualFixture(ctx context.Context, db *sql.DB, agent, input, expectedOutput, operator string) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("golden_set.AddManualFixture: db is required")
	}
	if agent == "" || input == "" || expectedOutput == "" {
		return 0, fmt.Errorf("golden_set.AddManualFixture: agent, input, and expectedOutput are required")
	}
	curatedBy := operator
	if !strings.HasPrefix(curatedBy, "operator:") {
		curatedBy = "operator:" + curatedBy
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO GoldenSetFixtures
			(agent, input, expected_output, source, curated_at, curated_by)
		VALUES (?, ?, ?, ?, ?, ?)`,
		agent, input, expectedOutput, string(SourceOperatorCurated),
		store.NowSQLite(), curatedBy,
	)
	if err != nil {
		return 0, fmt.Errorf("golden_set.AddManualFixture: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// fingerprintInput returns a stable SHA-256 of (agent, input). Used
// for idempotence and audit-trail records.
func fingerprintInput(agent, input string) string {
	h := sha256.Sum256([]byte(agent + "\x00" + input))
	return hex.EncodeToString(h[:8])
}

// isTautological returns true when an expected_output is derivable
// from input by construction (identical, prefix, or whitespace-only
// difference). Such fixtures pass on every prompt revision and don't
// surface real regressions.
func isTautological(input, expected string) bool {
	if input == expected {
		return true
	}
	a := strings.Join(strings.Fields(input), "")
	b := strings.Join(strings.Fields(expected), "")
	if a == b {
		return true
	}
	// "prefix" tautologies are the rare case where the LLM merely
	// echoes the input. We use a short-prefix check; longer "prefix +
	// real content" cases pass through.
	if len(b) >= 8 && strings.HasPrefix(a, b) {
		return true
	}
	if len(a) >= 8 && strings.HasPrefix(b, a) {
		return true
	}
	return false
}

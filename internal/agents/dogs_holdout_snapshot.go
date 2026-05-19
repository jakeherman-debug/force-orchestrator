// dogs_holdout_snapshot.go — D17 Phase 1B — daily holdout-snapshot dog.
//
// dogHoldoutSnapshot computes a deterministic SHA-256 fleet-state hash from:
//   1. Active agent counts by type (owner prefix from Locked BountyBoard rows)
//   2. BountyBoard task distribution by status
//   3. Distinct model identifiers from TreatmentSpecs
//
// The hash captures a stable summary of the fleet's configuration at the time
// of the snapshot. Identical fleet states produce identical hashes (determinism
// guaranteed by sorting all inputs before hashing). The hash is written into
// GlobalHoldouts.fleet_state_hash for every active (non-retired) row where
// the column is currently empty — updating only empty rows prevents clobbering
// an operator-set value while still backfilling rows created before this dog
// shipped. On a re-run, rows already holding a hash are updated unconditionally
// so the hash stays current (same-state → same-hash idempotence holds).
package agents

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"force-orchestrator/internal/store"
)

// dogHoldoutSnapshot is the dog body for "holdout-snapshot". It is called by
// runDog via the switch dispatch in dogs.go on every qualifying inquisitor tick
// (24h cooldown in dogCooldowns).
func dogHoldoutSnapshot(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	_ = ctx // no remote I/O; ctx is threaded for future expansion

	hash, err := computeFleetStateHash(db)
	if err != nil {
		return fmt.Errorf("holdout-snapshot: compute fleet state hash: %w", err)
	}

	// Load all active (non-retired) GlobalHoldouts rows.
	rows, err := db.Query(`SELECT id FROM GlobalHoldouts WHERE IFNULL(retired_at, '') = ''`)
	if err != nil {
		return fmt.Errorf("holdout-snapshot: list active holdouts: %w", err)
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if sErr := rows.Scan(&id); sErr != nil {
			return fmt.Errorf("holdout-snapshot: scan holdout id: %w", sErr)
		}
		ids = append(ids, id)
	}
	if rErr := rows.Err(); rErr != nil {
		return fmt.Errorf("holdout-snapshot: iterate holdouts: %w", rErr)
	}

	if len(ids) == 0 {
		logger.Printf("Dog holdout-snapshot: no active GlobalHoldouts rows — nothing to update")
		return nil
	}

	updated := 0
	for _, id := range ids {
		if uErr := store.UpdateHoldoutFleetStateHash(db, id, hash); uErr != nil {
			return fmt.Errorf("holdout-snapshot: update holdout %d: %w", id, uErr)
		}
		updated++
	}
	logger.Printf("Dog holdout-snapshot: wrote fleet_state_hash=%s to %d holdout(s)", hash[:12]+"…", updated)
	return nil
}

// fleetStateEntry is a single line of the fleet state contributed to the hash.
// All entries are sorted before hashing so insertion order doesn't matter.
type fleetStateEntry struct {
	key   string
	value string
}

// computeFleetStateHash queries three fleet-state dimensions from the DB and
// returns a hex-encoded SHA-256 of the sorted, canonical string representation.
//
// Dimensions:
//  1. task_status:<status>=<count> — BountyBoard row counts per status.
//  2. agent_type:<owner_prefix>=<count> — Locked rows per owner prefix
//     (prefix is the first segment before '/' or ':' to group by agent kind).
//  3. model_tier:<model_identifier> — every distinct model identifier in
//     TreatmentSpecs (presence, not count — the set of deployed models is
//     the fleet configuration signal).
//
// Any query error is returned and the caller surfaces it via the dog-mail path.
func computeFleetStateHash(db *sql.DB) (string, error) {
	var entries []fleetStateEntry

	// Dimension 1 — task distribution by status.
	statusRows, err := db.Query(`
		SELECT IFNULL(status, ''), COUNT(*)
		FROM BountyBoard
		GROUP BY status
		ORDER BY status ASC
	`)
	if err != nil {
		return "", fmt.Errorf("query task status distribution: %w", err)
	}
	defer statusRows.Close()
	for statusRows.Next() {
		var status string
		var count int
		if sErr := statusRows.Scan(&status, &count); sErr != nil {
			return "", fmt.Errorf("scan task status row: %w", sErr)
		}
		entries = append(entries, fleetStateEntry{
			key:   "task_status:" + status,
			value: fmt.Sprintf("%d", count),
		})
	}
	if rErr := statusRows.Err(); rErr != nil {
		return "", fmt.Errorf("iterate task status rows: %w", rErr)
	}

	// Dimension 2 — active agent counts by type (owner prefix from Locked rows).
	// We group by the first path segment of the owner string to cluster by agent
	// kind (e.g. "astromech", "captain", "council") without depending on the
	// exact owner format.
	agentRows, err := db.Query(`
		SELECT IFNULL(owner, ''), COUNT(*)
		FROM BountyBoard
		WHERE status = 'Locked'
		GROUP BY owner
		ORDER BY owner ASC
	`)
	if err != nil {
		return "", fmt.Errorf("query active agents: %w", err)
	}
	defer agentRows.Close()
	// Aggregate per prefix to produce a stable type-level count.
	prefixCounts := map[string]int{}
	for agentRows.Next() {
		var owner string
		var count int
		if sErr := agentRows.Scan(&owner, &count); sErr != nil {
			return "", fmt.Errorf("scan agent row: %w", sErr)
		}
		prefix := agentPrefix(owner)
		prefixCounts[prefix] += count
	}
	if rErr := agentRows.Err(); rErr != nil {
		return "", fmt.Errorf("iterate agent rows: %w", rErr)
	}
	// Collect sorted prefix entries.
	prefixes := make([]string, 0, len(prefixCounts))
	for p := range prefixCounts {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		entries = append(entries, fleetStateEntry{
			key:   "agent_type:" + p,
			value: fmt.Sprintf("%d", prefixCounts[p]),
		})
	}

	// Dimension 3 — model tiers: distinct model_identifier values in TreatmentSpecs.
	modelRows, err := db.Query(`
		SELECT DISTINCT IFNULL(model_identifier, '')
		FROM TreatmentSpecs
		WHERE model_identifier != ''
		ORDER BY model_identifier ASC
	`)
	if err != nil {
		return "", fmt.Errorf("query model tiers: %w", err)
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var model string
		if sErr := modelRows.Scan(&model); sErr != nil {
			return "", fmt.Errorf("scan model row: %w", sErr)
		}
		entries = append(entries, fleetStateEntry{
			key:   "model_tier:" + model,
			value: "present",
		})
	}
	if rErr := modelRows.Err(); rErr != nil {
		return "", fmt.Errorf("iterate model rows: %w", rErr)
	}

	// Sort all entries by key for determinism (the status/prefix queries are
	// already sorted individually, but mixing the three dimensions requires a
	// global sort across all keys).
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].key != entries[j].key {
			return entries[i].key < entries[j].key
		}
		return entries[i].value < entries[j].value
	})

	// Serialize to a canonical string and hash it.
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "%s=%s\n", e.key, e.value)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return fmt.Sprintf("%x", sum), nil
}

// agentPrefix extracts the first segment of an owner string for grouping by
// agent kind. The owner format varies ("astromech-1", "captain:repo",
// "council/task-42", etc.); we split on the first non-alphanumeric character.
// An empty or unrecognizable owner becomes "unknown".
func agentPrefix(owner string) string {
	if owner == "" {
		return "unknown"
	}
	for i, r := range owner {
		if i > 0 && (r == '-' || r == ':' || r == '/' || r == '_') {
			return owner[:i]
		}
	}
	return owner
}

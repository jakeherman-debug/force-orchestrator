package store

// D3 fix-loop-1 / γ3 — Spec deprecation flow (concern #9, exit criterion 14d).
//
// Operators (only — never agents) deprecate ATs or exit-criteria entries via
// the dashboard. Deprecation moves the entry from `verification_spec_json.ats[]`
// (or `.exit_criteria[]`) into `verification_spec_json.deprecated[]` with:
//
//   - removed_at (UTC SQLite timestamp)
//   - removed_by_email (must be supplied by the operator endpoint)
//   - rationale (≥ 20 chars, non-blank)
//   - removal_kind ('mistake' | 'superseded' | 'satisfied' | 'out_of_scope')
//   - optional superseded_by ({kind, ref})
//
// The hard-delete path is forbidden — historical cycle outcomes still
// reference the AT, so we move the row, never drop it.
//
// Pattern test wiring (slice α — P21 AT-removal-is-operator-only):
//   - This writer takes a removedByEmail argument and refuses an empty
//     value. Agent paths cannot synthesise a sensible email; operator
//     paths supply it from the request body. The unit test for this
//     file plants a "removed_by_email = ''" call and asserts the writer
//     refuses it.
//   - Pattern P21's AST-level walk over LLM proposal schemas is owned by
//     slice α; this writer is the runtime safety net.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const minDeprecationRationaleLen = 20

// allowedRemovalKinds — keep in sync with roadmap §1188.
var allowedRemovalKinds = map[string]bool{
	"mistake":      true,
	"superseded":   true,
	"satisfied":    true,
	"out_of_scope": true,
}

// SpecItemKind is whether the deprecated item was an AT or an EC. Stored on
// the deprecated entry so the operator UI can render distinct chips.
type SpecItemKind string

const (
	SpecItemAT SpecItemKind = "at"
	SpecItemEC SpecItemKind = "ec"
)

// DeprecateSpecItemArgs is the payload for DeprecateSpecItem. Wrapping the
// arguments in a struct lets the dashboard handler pass exactly what came
// from the request body without positional drift.
type DeprecateSpecItemArgs struct {
	ConvoyID        int
	ItemID          string       // e.g. "AT-3" or "EC-1"
	ItemKind        SpecItemKind // "at" | "ec"
	Rationale       string
	RemovalKind     string
	RemovedByEmail  string
	SupersededByRef string // optional; empty means none
	SupersededByKnd string // "at" | "fleet_rule"; optional
}

// DeprecateSpecItem moves the named AT or EC from active to deprecated[]
// inside Convoys.verification_spec_json. Idempotent on repeat calls (an
// already-deprecated item returns nil without re-stamping the timestamp).
//
// Validation:
//   - ItemID, ItemKind, RemovedByEmail, RemovalKind required
//   - Rationale ≥ 20 chars after TrimSpace
//   - RemovalKind in the controlled enum
//   - The convoy must exist
//   - The item must currently be in the active list (or already in
//     deprecated[]). Deprecating a non-existent item returns an error so
//     the operator UI can show "this AT is unknown."
//
// Concurrency: the read-modify-write is wrapped in a transaction so
// two concurrent operators can't half-deprecate the same spec.
func DeprecateSpecItem(db *sql.DB, args DeprecateSpecItemArgs) error {
	if args.ConvoyID <= 0 {
		return fmt.Errorf("DeprecateSpecItem: ConvoyID must be positive (got %d)", args.ConvoyID)
	}
	if strings.TrimSpace(args.ItemID) == "" {
		return fmt.Errorf("DeprecateSpecItem: ItemID required")
	}
	if args.ItemKind != SpecItemAT && args.ItemKind != SpecItemEC {
		return fmt.Errorf("DeprecateSpecItem: ItemKind must be 'at' or 'ec' (got %q)", args.ItemKind)
	}
	rationale := strings.TrimSpace(args.Rationale)
	if len(rationale) < minDeprecationRationaleLen {
		return fmt.Errorf("DeprecateSpecItem: rationale must be ≥ %d chars (got %d)", minDeprecationRationaleLen, len(rationale))
	}
	if !allowedRemovalKinds[args.RemovalKind] {
		return fmt.Errorf("DeprecateSpecItem: removal_kind must be one of mistake|superseded|satisfied|out_of_scope (got %q)", args.RemovalKind)
	}
	if strings.TrimSpace(args.RemovedByEmail) == "" {
		// Pattern P21 runtime guard — agent paths cannot synthesise an
		// operator email. The dashboard handler is the only legitimate
		// caller; it pulls operator_email from the request body.
		return fmt.Errorf("DeprecateSpecItem: RemovedByEmail required (operator-only flow)")
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("DeprecateSpecItem: begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var raw string
	if err := tx.QueryRow(
		`SELECT IFNULL(verification_spec_json, '') FROM Convoys WHERE id = ?`,
		args.ConvoyID,
	).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("DeprecateSpecItem: convoy %d not found", args.ConvoyID)
		}
		return fmt.Errorf("DeprecateSpecItem: load spec: %w", err)
	}

	// Use a generic map so unknown fields round-trip rather than getting
	// silently dropped on rewrite.
	specObj := map[string]any{}
	if t := strings.TrimSpace(raw); t != "" && t != "{}" {
		if err := json.Unmarshal([]byte(t), &specObj); err != nil {
			return fmt.Errorf("DeprecateSpecItem: parse spec: %w", err)
		}
	}

	listKey := "ats"
	if args.ItemKind == SpecItemEC {
		listKey = "exit_criteria"
	}

	// 1. Find + remove the entry from the active list.
	var foundEntry map[string]any
	if rawList, ok := specObj[listKey].([]any); ok {
		newList := make([]any, 0, len(rawList))
		for _, entry := range rawList {
			if m, ok := entry.(map[string]any); ok {
				idStr, _ := m["id"].(string)
				if idStr == args.ItemID {
					foundEntry = m
					continue // drop from active list
				}
			}
			newList = append(newList, entry)
		}
		specObj[listKey] = newList
	}

	// 2. Check if already deprecated. If so, no-op (idempotent).
	depRaw, _ := specObj["deprecated"].([]any)
	for _, d := range depRaw {
		if m, ok := d.(map[string]any); ok {
			if existing, _ := m["at_id"].(string); existing == args.ItemID {
				// Already deprecated — silent success.
				return tx.Commit()
			}
		}
	}

	// 3. If the entry wasn't in active and wasn't in deprecated, error
	// (operator UI surface: "unknown AT").
	if foundEntry == nil {
		return fmt.Errorf("DeprecateSpecItem: %s %q not found in active or deprecated list for convoy %d",
			args.ItemKind, args.ItemID, args.ConvoyID)
	}

	// 4. Build the deprecation entry and append.
	depEntry := map[string]any{
		"at_id":            args.ItemID,
		"item_kind":        string(args.ItemKind),
		"removed_at":       NowSQLite(),
		"removed_by_email": args.RemovedByEmail,
		"rationale":        rationale,
		"removal_kind":     args.RemovalKind,
		"prior_entry":      foundEntry, // preserve description/evaluator for audit
	}
	if args.SupersededByRef != "" {
		depEntry["superseded_by"] = map[string]any{
			"kind": args.SupersededByKnd,
			"ref":  args.SupersededByRef,
		}
	}
	specObj["deprecated"] = append(depRaw, depEntry)

	// 5. Persist.
	out, err := json.Marshal(specObj)
	if err != nil {
		return fmt.Errorf("DeprecateSpecItem: marshal spec: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE Convoys SET verification_spec_json = ? WHERE id = ?`,
		string(out), args.ConvoyID,
	); err != nil {
		return fmt.Errorf("DeprecateSpecItem: write spec: %w", err)
	}

	// 6. Append a spec_history_json entry so the deprecation is auditable
	// (concern #9 / roadmap line 1191).
	if err := appendSpecHistoryDeprecation(tx, args.ConvoyID, args, rationale); err != nil {
		return err
	}

	return tx.Commit()
}

func appendSpecHistoryDeprecation(tx *sql.Tx, convoyID int, args DeprecateSpecItemArgs, rationale string) error {
	var raw string
	if err := tx.QueryRow(
		`SELECT IFNULL(spec_history_json, '[]') FROM Convoys WHERE id = ?`, convoyID,
	).Scan(&raw); err != nil {
		return fmt.Errorf("DeprecateSpecItem: load spec_history_json: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		raw = "[]"
	}
	var history []map[string]any
	if err := json.Unmarshal([]byte(raw), &history); err != nil {
		// Treat malformed history as empty to avoid blocking the operator
		// path; the entry below is still appended for the new audit row.
		history = nil
	}
	history = append(history, map[string]any{
		"kind":             "deprecate",
		"at_id":            args.ItemID,
		"item_kind":        string(args.ItemKind),
		"rationale":        rationale,
		"removal_kind":     args.RemovalKind,
		"proposed_by":      "operator",
		"ratified_at":      NowSQLite(),
		"ratified_by_email": args.RemovedByEmail,
	})
	out, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("DeprecateSpecItem: marshal spec_history: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE Convoys SET spec_history_json = ? WHERE id = ?`,
		string(out), convoyID,
	); err != nil {
		return fmt.Errorf("DeprecateSpecItem: write spec_history_json: %w", err)
	}
	return nil
}

// IsDeprecated returns true if the parsed verification spec carries a
// deprecation entry for the given AT id. Mirrors agents.IsATDeprecated but
// operates over the raw spec JSON so callers outside the agents package
// can re-use it.
func IsDeprecated(specJSON string, atID string) bool {
	t := strings.TrimSpace(specJSON)
	if t == "" || t == "{}" || atID == "" {
		return false
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(t), &spec); err != nil {
		return false
	}
	depRaw, _ := spec["deprecated"].([]any)
	for _, d := range depRaw {
		if m, ok := d.(map[string]any); ok {
			if existing, _ := m["at_id"].(string); existing == atID {
				return true
			}
		}
	}
	return false
}

// InflightTasksForAT returns the IDs + payload prefixes of any non-terminal
// BountyBoard rows that were spawned by an AT-driven ConvoyReview cycle.
// Used by the operator UI to surface "this AT is deprecated; close fix-task
// or proceed?" before the deprecation lands.
//
// Compound-key invariant (concern #8 / Pattern P20): the WHERE clause uses
// BOTH convoy_id AND spawning_at_id. Bare at_id matches across convoys are
// forbidden — see CLAUDE.md "Cross-convoy AT-id collision invariant".
func InflightTasksForAT(db *sql.DB, convoyID int, atID string) ([]int, error) {
	if convoyID <= 0 || atID == "" {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT id FROM BountyBoard
		 WHERE convoy_id = ?
		   AND spawning_at_id = ?
		   AND status NOT IN ('Completed','Cancelled','Failed')`,
		convoyID, atID,
	)
	if err != nil {
		return nil, fmt.Errorf("InflightTasksForAT: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

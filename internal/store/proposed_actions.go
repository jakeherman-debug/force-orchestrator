// D3 fix-loop-1 β1 — Captain proposal pipeline storage helpers.
//
// `BountyBoard.proposed_action_json` carries Captain's structured ruling
// payload for unmapped spawns: action, cited evidence (ATs + FleetRules),
// classification confidence, and human-prose rationale. Per roadmap
// concern #1 / exit criterion 7 (lines 1193-1198), every cited reference
// must resolve to a real spec/FleetRule row at proposal-emit time and
// every prose `AT-NNN` reference must appear in `cited_ats`.
//
// `ValidateProposedAction` is the mechanical-validator boundary. The
// LLM-judge layer (internal/agents/captain_proposal_judge.go) lives one
// layer up; it consumes a payload that already passed mechanical
// validation.
//
// Pattern P23 contract (proposer write discipline): every Captain write
// that goes through `SetProposedAction` MUST include cited_ats[] +
// cited_fleet_rules[]. Empty-arrays are fine; nil is rejected at the
// helper boundary.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ProposedAction is the canonical structured payload Captain writes to
// `BountyBoard.proposed_action_json`. The shape mirrors the roadmap
// schema for Captain proposals (lines 1193-1198).
type ProposedAction struct {
	// Action is the ruling kind. One of: "approve", "reject", "fix",
	// "escalate". Empty rejects at validation.
	Action string `json:"action"`

	// CitedATs are convoy-scoped AT references the Captain cited as
	// evidence. Each entry MUST be a real `(convoy_id, at_id)` tuple
	// in the convoy's `verification_spec_json` history (concern #8 /
	// Pattern P20 — bare AT-id without convoy scope is forbidden).
	CitedATs []CitedAT `json:"cited_ats"`

	// CitedFleetRules is the list of FleetRules `rule_key` values the
	// Captain cited. Validated against the live FleetRules table at
	// proposal-emit.
	CitedFleetRules []string `json:"cited_fleet_rules"`

	// SpecLink is an optional pointer to the spec section the Captain
	// thinks the spawn maps to. Free-form prose is acceptable — the
	// operator UI fetches and renders it.
	SpecLink string `json:"spec_link,omitempty"`

	// ClassificationConfidence is in [0.0, 1.0]. Validation rejects
	// out-of-range values to catch LLM model drift.
	ClassificationConfidence float64 `json:"classification_confidence"`

	// Rationale is Captain's free-form reasoning. Pattern P18 / concern
	// #1 anti-cheat asserts: every prose `AT-NNN` token in this string
	// must appear in CitedATs (no orphan references). The LLM-judge
	// layer additionally asks Haiku whether the rationale actually
	// supports the cited evidence.
	Rationale string `json:"rationale"`

	// DraftAmendment is the Captain's proposed spec amendment (if any).
	// Optional; empty when the Captain isn't proposing a spec change.
	DraftAmendment string `json:"draft_amendment,omitempty"`

	// Alternative is an operator-facing alternative path the Captain
	// would suggest if the proposal is rejected. Optional.
	Alternative string `json:"alternative,omitempty"`
}

// CitedAT is a convoy-scoped AT reference. Both fields are mandatory at
// the helper boundary; bare AT-id without convoy_id rejects (P20).
type CitedAT struct {
	ConvoyID int    `json:"convoy_id"`
	ATID     string `json:"at_id"`
}

// validProposedActions enumerates the legal `action` strings.
var validProposedActions = map[string]bool{
	"approve":  true,
	"reject":   true,
	"fix":      true,
	"escalate": true,
}

// atProseRefPattern matches `AT-` followed by 1+ digits or alphanums,
// case-insensitive. Used by ValidateProposedAction to enforce that
// every prose reference in the rationale resolves through CitedATs
// (concern #1 anti-cheat / Pattern P18).
var atProseRefPattern = regexp.MustCompile(`(?i)\bAT-[A-Z0-9_-]+\b`)

// ValidateProposedAction is the mechanical validator. Returns nil iff
// the payload satisfies:
//
//   - action ∈ {approve, reject, fix, escalate}
//   - 0.0 ≤ classification_confidence ≤ 1.0
//   - cited_ats and cited_fleet_rules are non-nil (empty slice is fine)
//   - every cited AT carries both convoy_id and at_id (Pattern P20)
//   - every prose `AT-NNN` token in rationale is also listed in
//     cited_ats (concern #1 — no orphan references)
//
// Validation is structural only — does NOT verify that cited references
// resolve in the DB. The DB-resolution check is the caller's
// responsibility (it requires a `*sql.DB` and a convoy context).
func ValidateProposedAction(p ProposedAction) error {
	if !validProposedActions[p.Action] {
		return fmt.Errorf("proposed_action: invalid action %q (want approve|reject|fix|escalate)", p.Action)
	}
	if p.ClassificationConfidence < 0.0 || p.ClassificationConfidence > 1.0 {
		return fmt.Errorf("proposed_action: classification_confidence %f out of range [0.0, 1.0]", p.ClassificationConfidence)
	}
	// P23: nil slices reject; empty is OK.
	if p.CitedATs == nil {
		return fmt.Errorf("proposed_action: cited_ats must not be nil (P23 — proposer write discipline)")
	}
	if p.CitedFleetRules == nil {
		return fmt.Errorf("proposed_action: cited_fleet_rules must not be nil (P23 — proposer write discipline)")
	}
	// P20: bare AT-id without convoy_id is forbidden.
	for i, at := range p.CitedATs {
		if at.ATID == "" {
			return fmt.Errorf("proposed_action: cited_ats[%d].at_id is empty", i)
		}
		if at.ConvoyID <= 0 {
			return fmt.Errorf("proposed_action: cited_ats[%d].convoy_id must be > 0 (P20 — convoy-scoped lookup)", i)
		}
	}
	// Prose-vs-cited consistency. Build the set of cited AT-ids; every
	// prose token must hit.
	citedSet := make(map[string]bool, len(p.CitedATs))
	for _, at := range p.CitedATs {
		citedSet[strings.ToUpper(at.ATID)] = true
	}
	for _, m := range atProseRefPattern.FindAllString(p.Rationale, -1) {
		key := strings.ToUpper(strings.TrimSpace(m))
		if !citedSet[key] {
			return fmt.Errorf("proposed_action: rationale references %q but it is not in cited_ats (concern #1 — no orphan references)", m)
		}
	}
	return nil
}

// SetProposedAction validates `payload` and writes the JSON-encoded
// shape to `BountyBoard.proposed_action_json` for `taskID`. Returns an
// error on validation failure or DB write failure (no silent failures
// per CLAUDE.md). The write is idempotent: calling twice with the same
// payload is a no-op at the row level (the JSON contents replace).
func SetProposedAction(db *sql.DB, taskID int, payload ProposedAction) error {
	if err := ValidateProposedAction(payload); err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("SetProposedAction: marshal: %w", err)
	}
	res, err := db.Exec(
		`UPDATE BountyBoard SET proposed_action_json = ? WHERE id = ?`,
		string(encoded), taskID)
	if err != nil {
		return fmt.Errorf("SetProposedAction: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("SetProposedAction: task %d not found", taskID)
	}
	return nil
}

// GetProposedAction reads the structured payload back from
// `BountyBoard.proposed_action_json`. Returns (zero-value, nil) if the
// row exists but the column is empty (i.e. the Captain has not yet
// emitted a proposal for this task).
func GetProposedAction(db *sql.DB, taskID int) (ProposedAction, bool, error) {
	var raw string
	err := db.QueryRow(
		`SELECT IFNULL(proposed_action_json, '') FROM BountyBoard WHERE id = ?`,
		taskID).Scan(&raw)
	if err == sql.ErrNoRows {
		return ProposedAction{}, false, nil
	}
	if err != nil {
		return ProposedAction{}, false, fmt.Errorf("GetProposedAction: query: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return ProposedAction{}, false, nil
	}
	var p ProposedAction
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ProposedAction{}, false, fmt.Errorf("GetProposedAction: unmarshal: %w", err)
	}
	return p, true, nil
}

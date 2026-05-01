// D3 fix-loop-1 β2 — ProposedFeatures pipeline storage helpers.
//
// Per roadmap concern #10 / exit criterion 14 (lines 1233-1245):
//
//   - Investigator detects patterns and emits ProposedFeatures rows.
//   - Each emit includes title (observation_summary), category,
//     fingerprint (deterministic SHA256 of normalized canonical input),
//     value_score, complexity_score, source observations.
//   - Suppression check: if a matching ProposedFeatureSuppression
//     exists for this fingerprint AND is not expired, suppress (no-op).
//   - Dedup via ON CONFLICT on the partial-unique fingerprint index:
//     bump occurrence_count + last_seen_at + evidence_history_json
//     instead of inserting duplicate.
//   - Score-aware auto-archive (housekeeping dog) decays scores so old
//     features don't permanently rank highest.
//
// Pattern P22 (fingerprint determinism): Fingerprint MUST be a pure
// function of the canonical input — no timestamps, no run IDs, no
// random salts. Two calls with the same input MUST return byte-equal
// hashes. Tests assert this.
//
// Pattern P23 (proposer write discipline): proposers (Investigator,
// Captain mid-cycle, EC, ConvoyReview) only INSERT or use the dedup
// ON CONFLICT path. Direct writes to archived_at / archive_reason / any
// ProposedFeatureSuppressions column from a proposer code path fail.
// Only operator-routed handlers and the housekeeping dog may write
// archive state.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProposedFeaturePayload is the structured input proposers hand to
// EmitProposedFeature. The fingerprint is computed inside the helper —
// callers MUST NOT precompute it themselves so the canonical-input
// shape stays in one place.
type ProposedFeaturePayload struct {
	// ObservationSummary — short title-line description of the pattern.
	ObservationSummary string `json:"observation_summary"`

	// Category — bucket label ("missing_test", "duplicate_logic",
	// "cross_convoy_pattern", etc.). Free-form prose; no enum constraint
	// at the schema layer.
	Category string `json:"category"`

	// Source — which proposer raised this. One of: "investigator",
	// "captain", "engineering-corps", "convoy-review", "operator".
	Source string `json:"source"`

	// SourceObservations — JSON array of structured citations the
	// proposer attached. Each entry references a real DB row (task ID,
	// convoy ID, transcript ID, etc.) — Pattern P29 lineage.
	SourceObservations []SourceObservation `json:"source_observations"`

	// CodePaths — sorted list of file paths the pattern touches. Used
	// in the canonical fingerprint input.
	CodePaths []string `json:"code_paths"`

	// ATRefs — sorted convoy-scoped AT references. Used in the
	// canonical fingerprint input.
	ATRefs []string `json:"at_refs"`

	// FleetRuleRefs — sorted FleetRules rule_keys. Used in the
	// canonical fingerprint input.
	FleetRuleRefs []string `json:"fleet_rule_refs"`

	// Topic — one-line normalised pattern label, included in the
	// fingerprint input. Lets two proposers with the same code paths
	// but different conceptual frames stay distinct.
	Topic string `json:"topic"`

	// ValueScore in {low, medium, high}. Empty defaults to "medium" at
	// the helper boundary.
	ValueScore string `json:"value_score"`

	// ComplexityScore in {low, medium, high}. Empty defaults to "medium".
	ComplexityScore string `json:"complexity_score"`

	// ValueRationale, ComplexityRationale — proposer's one-line
	// justifications for each score. Free prose, optional.
	ValueRationale      string `json:"value_rationale"`
	ComplexityRationale string `json:"complexity_rationale"`

	// ScoredBy — model + version label that produced the scores.
	// e.g. "haiku-2026" / "investigator-v1". Stamped on the row.
	ScoredBy string `json:"scored_by"`
}

// SourceObservation is one structured citation attached to a
// ProposedFeature. The schema is intentionally loose — proposers
// can attach any shape — but every entry MUST carry a kind + ref so
// downstream UIs can hyperlink it.
type SourceObservation struct {
	Kind string `json:"kind"` // "task", "convoy", "transcript", "promotion_proposal", ...
	Ref  string `json:"ref"`  // stringified ID or composite key
	Note string `json:"note"` // optional one-line context
}

// EmitResult describes what EmitProposedFeature did.
type EmitResult struct {
	// FeatureID is the row id (whether newly inserted or merged).
	FeatureID int64
	// Inserted is true when this was a fresh row; false when the
	// dedup path bumped occurrence_count on an existing row.
	Inserted bool
	// Suppressed is true when an active ProposedFeatureSuppression
	// blocked the write entirely. FeatureID is 0 in that case.
	Suppressed bool
}

// validScores enumerates the legal value_score / complexity_score
// strings (matching the CHECK constraint in schema.go).
var validScores = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
}

// Fingerprint is the canonical SHA256 of the normalised proposer
// input. Pattern P22 contract: pure function — same input always
// returns byte-equal output. Inputs are normalised (lowercased,
// whitespace-collapsed) and the variadic slices are sorted before
// hashing so caller ordering doesn't matter.
//
// Fingerprint inputs (the ONLY legal fields):
//   - source     — which proposer
//   - topic      — one-line conceptual label
//   - codePaths  — sorted list of file paths
//   - atRefs     — sorted list of convoy-scoped AT references
//   - fleetRuleRefs — sorted list of FleetRules rule_keys
//
// Excluded by design: timestamps, run IDs, random salts, occurrence
// counts. Pattern P22's audit asserts these stay excluded.
func Fingerprint(source, topic string, codePaths, atRefs, fleetRuleRefs []string) string {
	cps := normaliseAndSort(codePaths)
	ats := normaliseAndSort(atRefs)
	frs := normaliseAndSort(fleetRuleRefs)

	canonical := struct {
		Source        string   `json:"source"`
		Topic         string   `json:"topic"`
		CodePaths     []string `json:"code_paths"`
		ATRefs        []string `json:"at_refs"`
		FleetRuleRefs []string `json:"fleet_rule_refs"`
	}{
		Source:        normaliseScalar(source),
		Topic:         normaliseScalar(topic),
		CodePaths:     cps,
		ATRefs:        ats,
		FleetRuleRefs: frs,
	}
	encoded, _ := json.Marshal(canonical) // deterministic field-order Go marshal
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func normaliseScalar(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normaliseAndSort(in []string) []string {
	if in == nil {
		return []string{}
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := normaliseScalar(s)
		if t != "" {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// EmitProposedFeature is the single ingress point every proposer must
// route through. Computes the fingerprint, checks ProposedFeatureSuppressions
// for an active match, and either inserts a fresh row or merges into
// an existing fingerprint via ON CONFLICT (bumping occurrence_count
// and last_seen_at). Returns the row id and a status flag describing
// what happened.
//
// P23 invariant: this helper NEVER writes to archived_at /
// archive_reason / ProposedFeatureSuppressions. Operator-routed
// handlers + the housekeeping dog own those columns.
func EmitProposedFeature(db *sql.DB, payload ProposedFeaturePayload) (EmitResult, error) {
	if strings.TrimSpace(payload.ObservationSummary) == "" {
		return EmitResult{}, fmt.Errorf("EmitProposedFeature: observation_summary is required")
	}
	if strings.TrimSpace(payload.Source) == "" {
		return EmitResult{}, fmt.Errorf("EmitProposedFeature: source is required")
	}
	if strings.TrimSpace(payload.Category) == "" {
		// Default to "uncategorised" rather than reject — proposer
		// prompts may not always tag a category and we'd rather have
		// the row than lose the signal.
		payload.Category = "uncategorised"
	}
	value := strings.ToLower(strings.TrimSpace(payload.ValueScore))
	if value == "" {
		value = "medium"
	}
	complexity := strings.ToLower(strings.TrimSpace(payload.ComplexityScore))
	if complexity == "" {
		complexity = "medium"
	}
	if !validScores[value] {
		return EmitResult{}, fmt.Errorf("EmitProposedFeature: value_score %q must be low|medium|high", payload.ValueScore)
	}
	if !validScores[complexity] {
		return EmitResult{}, fmt.Errorf("EmitProposedFeature: complexity_score %q must be low|medium|high", payload.ComplexityScore)
	}

	fp := Fingerprint(payload.Source, payload.Topic, payload.CodePaths, payload.ATRefs, payload.FleetRuleRefs)

	// Suppression check at ingress (concern #10).
	suppressed, sErr := isFingerprintSuppressed(db, fp)
	if sErr != nil {
		return EmitResult{}, fmt.Errorf("EmitProposedFeature: suppression check: %w", sErr)
	}
	if suppressed {
		return EmitResult{Suppressed: true}, nil
	}

	obs := payload.SourceObservations
	if obs == nil {
		obs = []SourceObservation{}
	}
	obsJSON, _ := json.Marshal(obs)

	// Evidence-history seed: a single-element array carrying this emit's
	// observations + a sentinel "first_seen" entry. Future merges append
	// (handled in the ON CONFLICT path below).
	historyJSON, _ := json.Marshal([]map[string]any{{
		"seen_at":      NowSQLite(),
		"source":       payload.Source,
		"observations": obs,
	}})

	// INSERT ... ON CONFLICT bumps occurrence_count + last_seen_at on a
	// matching active fingerprint. The partial unique index in schema.go
	// scopes the conflict resolution to active rows; archived rows do
	// not block re-emission (a fresh insert lands).
	const stmt = `
		INSERT INTO ProposedFeatures
			(observation_summary, category, source, source_observations, fingerprint,
			 occurrence_count, first_seen_at, last_seen_at, evidence_history_json,
			 value_score, complexity_score, value_rationale, complexity_rationale,
			 scored_by, status)
		VALUES (?, ?, ?, ?, ?, 1, datetime('now'), datetime('now'), ?, ?, ?, ?, ?, ?, 'pending')
		ON CONFLICT(fingerprint) WHERE archived_at = '' AND fingerprint != ''
		DO UPDATE SET
			occurrence_count = occurrence_count + 1,
			last_seen_at = datetime('now'),
			evidence_history_json = json_insert(
				IFNULL(evidence_history_json, '[]'),
				'$[#]',
				json_object(
					'seen_at', datetime('now'),
					'source', excluded.source,
					'observation_summary', excluded.observation_summary
				)
			)
		RETURNING id, occurrence_count
	`
	var (
		id      int64
		count   int
	)
	err := db.QueryRow(stmt,
		payload.ObservationSummary, payload.Category, payload.Source, string(obsJSON), fp,
		string(historyJSON),
		value, complexity, payload.ValueRationale, payload.ComplexityRationale,
		payload.ScoredBy,
	).Scan(&id, &count)
	if err != nil {
		return EmitResult{}, fmt.Errorf("EmitProposedFeature: insert: %w", err)
	}
	return EmitResult{
		FeatureID: id,
		Inserted:  count == 1,
	}, nil
}

// isFingerprintSuppressed returns true iff there exists at least one
// active (non-expired) ProposedFeatureSuppressions row for this
// fingerprint. Caller logs the suppression — silent suppression is OK
// here because the operator deliberately installed the rule.
func isFingerprintSuppressed(db *sql.DB, fingerprint string) (bool, error) {
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM ProposedFeatureSuppressions
		WHERE fingerprint = ?
		  AND (suppressed_until = '' OR suppressed_until > datetime('now'))
	`, fingerprint).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SuppressProposedFeature installs an operator-only suppression rule.
// Intended for the operator-UI handler — proposers must NEVER call
// this (P23 enforces).
//
// `until` is the wall-clock expiry. Pass time.Time{} for "no expiry"
// (rendered as empty string per schema). Rationale is required (≥ 20
// chars enforced by the schema CHECK constraint).
func SuppressProposedFeature(db *sql.DB, fingerprint, rationale string, until time.Time, byEmail string) (int64, error) {
	if strings.TrimSpace(fingerprint) == "" {
		return 0, fmt.Errorf("SuppressProposedFeature: fingerprint required")
	}
	if len(strings.TrimSpace(rationale)) < 20 {
		return 0, fmt.Errorf("SuppressProposedFeature: rationale must be ≥ 20 chars")
	}
	if strings.TrimSpace(byEmail) == "" {
		return 0, fmt.Errorf("SuppressProposedFeature: created_by_email required")
	}
	untilStr := ""
	if !until.IsZero() {
		untilStr = until.UTC().Format("2006-01-02 15:04:05")
	}
	res, err := db.Exec(`
		INSERT INTO ProposedFeatureSuppressions
			(fingerprint, rationale, suppressed_until, created_by_email)
		VALUES (?, ?, ?, ?)
	`, fingerprint, rationale, untilStr, byEmail)
	if err != nil {
		return 0, fmt.Errorf("SuppressProposedFeature: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// OverrideProposedFeatureScore writes the operator's score change
// (P23 — operator-routed only). Atomically: writes the audit row +
// the score update inside one tx so the score can never drift from
// the audit history.
func OverrideProposedFeatureScore(db *sql.DB, featureID int64, newValue, newComplexity, rationale, byEmail string) error {
	if len(strings.TrimSpace(rationale)) < 1 {
		return fmt.Errorf("OverrideProposedFeatureScore: rationale required")
	}
	if strings.TrimSpace(byEmail) == "" {
		return fmt.Errorf("OverrideProposedFeatureScore: by_email required")
	}
	value := strings.ToLower(strings.TrimSpace(newValue))
	complexity := strings.ToLower(strings.TrimSpace(newComplexity))
	if value != "" && !validScores[value] {
		return fmt.Errorf("OverrideProposedFeatureScore: value_score %q must be low|medium|high", newValue)
	}
	if complexity != "" && !validScores[complexity] {
		return fmt.Errorf("OverrideProposedFeatureScore: complexity_score %q must be low|medium|high", newComplexity)
	}
	if value == "" && complexity == "" {
		return fmt.Errorf("OverrideProposedFeatureScore: at least one score must be provided")
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("OverrideProposedFeatureScore: begin: %w", err)
	}
	defer tx.Rollback()

	var priorValue, priorComplexity string
	err = tx.QueryRow(`
		SELECT value_score, complexity_score FROM ProposedFeatures WHERE id = ?
	`, featureID).Scan(&priorValue, &priorComplexity)
	if err == sql.ErrNoRows {
		return fmt.Errorf("OverrideProposedFeatureScore: feature %d not found", featureID)
	}
	if err != nil {
		return fmt.Errorf("OverrideProposedFeatureScore: select prior: %w", err)
	}

	// Audit row first (P23 — every score mutation has a paired audit).
	_, err = tx.Exec(`
		INSERT INTO ProposedFeatureScoreOverrides
			(proposed_feature_id, prior_value_score, prior_complexity_score,
			 new_value_score, new_complexity_score, rationale, overridden_by_email)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, featureID, priorValue, priorComplexity, value, complexity, rationale, byEmail)
	if err != nil {
		return fmt.Errorf("OverrideProposedFeatureScore: insert audit: %w", err)
	}

	// Coalesce empty-string updates to "keep prior".
	effectiveValue := value
	if effectiveValue == "" {
		effectiveValue = priorValue
	}
	effectiveComplexity := complexity
	if effectiveComplexity == "" {
		effectiveComplexity = priorComplexity
	}
	_, err = tx.Exec(`
		UPDATE ProposedFeatures SET value_score = ?, complexity_score = ? WHERE id = ?
	`, effectiveValue, effectiveComplexity, featureID)
	if err != nil {
		return fmt.Errorf("OverrideProposedFeatureScore: update score: %w", err)
	}
	return tx.Commit()
}

// DecayProposedFeatureScores is the housekeeping helper called by the
// `proposed-features-decay` dog. It demotes value_score by one tier
// (high → medium → low) on rows whose last_seen_at is older than
// `staleAfter` AND that are still pending (not promoted, not archived,
// no operator decision). Returns count of rows decayed.
//
// Decay is conservative: it never demotes complexity_score (operator
// signal — manually adjusted scores feed calibration, mutating them
// would corrupt the meta-metric per concern #10), and it only fires
// once per row per dog cycle (the WHERE clause excludes already-low
// rows).
//
// P23 contract: this helper IS the housekeeping dog's path; proposers
// must not call it. Audit row is written for each decay so the
// distribution-shift signal stays auditable.
func DecayProposedFeatureScores(db *sql.DB, staleAfter time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-staleAfter).Format("2006-01-02 15:04:05")

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("DecayProposedFeatureScores: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id, value_score FROM ProposedFeatures
		WHERE last_seen_at < ?
		  AND value_score IN ('high', 'medium')
		  AND archived_at = ''
		  AND IFNULL(promoted_at, '') = ''
		  AND status = 'pending'
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("DecayProposedFeatureScores: select: %w", err)
	}

	type decay struct {
		id       int64
		oldValue string
		newValue string
	}
	var decays []decay
	for rows.Next() {
		var d decay
		if err := rows.Scan(&d.id, &d.oldValue); err != nil {
			rows.Close()
			return 0, fmt.Errorf("DecayProposedFeatureScores: scan: %w", err)
		}
		switch d.oldValue {
		case "high":
			d.newValue = "medium"
		case "medium":
			d.newValue = "low"
		}
		if d.newValue != "" {
			decays = append(decays, d)
		}
	}
	rows.Close()
	if rErr := rows.Err(); rErr != nil {
		return 0, fmt.Errorf("DecayProposedFeatureScores: iter: %w", rErr)
	}

	for _, d := range decays {
		_, err := tx.Exec(`
			INSERT INTO ProposedFeatureScoreOverrides
				(proposed_feature_id, prior_value_score, prior_complexity_score,
				 new_value_score, new_complexity_score, rationale, overridden_by_email)
			VALUES (?, ?, '', ?, '', 'auto-decay: stale row', 'system:decay-dog')
		`, d.id, d.oldValue, d.newValue)
		if err != nil {
			return 0, fmt.Errorf("DecayProposedFeatureScores: audit insert: %w", err)
		}
		_, err = tx.Exec(`
			UPDATE ProposedFeatures SET value_score = ? WHERE id = ?
		`, d.newValue, d.id)
		if err != nil {
			return 0, fmt.Errorf("DecayProposedFeatureScores: update: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("DecayProposedFeatureScores: commit: %w", err)
	}
	return len(decays), nil
}

// ProposedFeatureRow is the read shape for ListProposedFeatures.
type ProposedFeatureRow struct {
	ID                  int64               `json:"id"`
	ObservationSummary  string              `json:"observation_summary"`
	Category            string              `json:"category"`
	Source              string              `json:"source"`
	SourceObservations  []SourceObservation `json:"source_observations"`
	Fingerprint         string              `json:"fingerprint"`
	OccurrenceCount     int                 `json:"occurrence_count"`
	FirstSeenAt         string              `json:"first_seen_at"`
	LastSeenAt          string              `json:"last_seen_at"`
	ValueScore          string              `json:"value_score"`
	ComplexityScore     string              `json:"complexity_score"`
	ValueRationale      string              `json:"value_rationale"`
	ComplexityRationale string              `json:"complexity_rationale"`
	ScoredBy            string              `json:"scored_by"`
	PromotedAt          string              `json:"promoted_at"`
	PromotionDeadline   string              `json:"promotion_deadline"`
	Status              string              `json:"status"`
	DecidedAt           string              `json:"decided_at"`
	DecidedBy           string              `json:"decided_by"`
	DecisionAction      string              `json:"decision_action"`
	ArchivedAt          string              `json:"archived_at"`
	ArchiveReason       string              `json:"archive_reason"`
}

// ListProposedFeatures returns rows matching the filter. statusFilter
// of "" means all-non-archived; "archived" means only archived rows;
// other values pass through as exact-match on ProposedFeatures.status.
func ListProposedFeatures(db *sql.DB, statusFilter string) ([]ProposedFeatureRow, error) {
	q := `
		SELECT id, observation_summary, category, source,
		       IFNULL(source_observations,'[]'), IFNULL(fingerprint,''),
		       IFNULL(occurrence_count, 1),
		       IFNULL(first_seen_at,''), IFNULL(last_seen_at,''),
		       IFNULL(value_score,'medium'), IFNULL(complexity_score,'medium'),
		       IFNULL(value_rationale,''), IFNULL(complexity_rationale,''),
		       IFNULL(scored_by,''),
		       IFNULL(promoted_at,''), IFNULL(promotion_deadline,''),
		       IFNULL(status,'pending'),
		       IFNULL(decided_at,''), IFNULL(decided_by,''), IFNULL(decision_action,''),
		       IFNULL(archived_at,''), IFNULL(archive_reason,'')
		FROM ProposedFeatures
		WHERE 1=1
	`
	args := []any{}
	switch statusFilter {
	case "":
		q += " AND IFNULL(archived_at,'') = ''"
	case "archived":
		q += " AND IFNULL(archived_at,'') != ''"
	default:
		q += " AND status = ?"
		args = append(args, statusFilter)
	}
	q += " ORDER BY occurrence_count DESC, last_seen_at DESC, id DESC"

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListProposedFeatures: query: %w", err)
	}
	defer rows.Close()

	var out []ProposedFeatureRow
	for rows.Next() {
		var r ProposedFeatureRow
		var obsRaw string
		if err := rows.Scan(
			&r.ID, &r.ObservationSummary, &r.Category, &r.Source,
			&obsRaw, &r.Fingerprint, &r.OccurrenceCount,
			&r.FirstSeenAt, &r.LastSeenAt,
			&r.ValueScore, &r.ComplexityScore,
			&r.ValueRationale, &r.ComplexityRationale, &r.ScoredBy,
			&r.PromotedAt, &r.PromotionDeadline, &r.Status,
			&r.DecidedAt, &r.DecidedBy, &r.DecisionAction,
			&r.ArchivedAt, &r.ArchiveReason,
		); err != nil {
			return nil, fmt.Errorf("ListProposedFeatures: scan: %w", err)
		}
		_ = json.Unmarshal([]byte(obsRaw), &r.SourceObservations)
		if r.SourceObservations == nil {
			r.SourceObservations = []SourceObservation{}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListProposedFeatures: iter: %w", err)
	}
	return out, nil
}

// PromoteProposedFeature flips a row from "pending" to "promoted" with
// an operator-supplied deadline. Returns an error if the feature row
// is already promoted, archived, or otherwise terminal.
//
// P23: operator-only path (the dashboard handler calls this; proposers
// must not).
func PromoteProposedFeature(db *sql.DB, featureID int64, deadline string, byEmail string) error {
	if strings.TrimSpace(byEmail) == "" {
		return fmt.Errorf("PromoteProposedFeature: by_email required")
	}
	res, err := db.Exec(`
		UPDATE ProposedFeatures
		SET status = 'promoted',
		    promoted_at = datetime('now'),
		    promotion_deadline = ?,
		    decided_at = datetime('now'),
		    decided_by = ?,
		    decision_action = 'promote'
		WHERE id = ?
		  AND IFNULL(promoted_at,'') = ''
		  AND IFNULL(archived_at,'') = ''
	`, deadline, byEmail, featureID)
	if err != nil {
		return fmt.Errorf("PromoteProposedFeature: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("PromoteProposedFeature: feature %d not promotable (already promoted/archived/missing)", featureID)
	}
	return nil
}

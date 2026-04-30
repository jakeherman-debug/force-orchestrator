package engineering_corps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/store"
)

// DemotionAuthor — author a demotion PromotionProposals row when a
// previously-promoted rule shows decayed downstream benefit.
//
// Per docs/paired-runs.md § Demotion authority, full retention scoring
// (Tier 2 reports + post-ship monitor experiments showing P(regression
// > practical) > 0.8) lives in P4/P5 of D3. The Phase 3 scope here
// is the plumbing:
//
//   1. Read PromotionProposals with kind='promote' that ratified more
//      than `staleDays` (default 30) ago — the candidate set for
//      retention review.
//   2. For each, write a placeholder demotion proposal with
//      kind='demote', authored_by='engineering-corps', and an
//      evidence summary marking it as a P3 placeholder ("retention
//      window elapsed; full scoring deferred to P4/P5").
//   3. Operator ratifies (or rejects) the demotion via the dashboard.
//
// Operator-routing invariant: the demotion row is unratified
// (ratified_at='') and carries a 14-day TTL just like a promote
// proposal. P4/P5 will replace the placeholder evidence with real
// retention scores, but the operator-routing shape stays identical.
//
// Idempotence: if a non-rejected demotion proposal already exists for
// the same source promotion, this handler skips it. Re-running on the
// same window does not produce duplicates.
//
// SQL-only — no LLM call. Phase 4/5 may add a critic-LLM pass over
// the retention evidence; Phase 3 deliberately keeps this minimal so
// the dispatcher routes to a non-stub for every task type.
//
// Inputs (BountyBoard.payload JSON):
//   {} (or empty) — scan; "stale_days" optional override
type demotionAuthorPayload struct {
	StaleDays int `json:"stale_days"`
}

// defaultDemotionStaleDays is the post-ratification window after
// which a promote proposal is eligible for retention review. 30 days
// roughly aligns with the "30-day revert" signal in roadmap.md
// concerns #2/#5.
const defaultDemotionStaleDays = 30

func handleDemotionAuthor(
	ctx context.Context,
	cfg EngineeringCorpsConfig,
	_ *capabilities.Profile,
	agentName string,
	bounty *store.Bounty,
	logger *log.Logger,
) error {
	db := cfg.DB

	var payload demotionAuthorPayload
	if err := strictDecode(bounty.Payload, &payload); err != nil {
		return fmt.Errorf("DemotionAuthor: parse payload: %w", err)
	}
	stale := payload.StaleDays
	if stale <= 0 {
		stale = defaultDemotionStaleDays
	}

	candidates, err := loadStalePromotedProposals(db, stale)
	if err != nil {
		return fmt.Errorf("DemotionAuthor: load candidates: %w", err)
	}

	authored := 0
	skipped := 0
	for _, c := range candidates {
		// Skip if a non-rejected demotion already exists for this
		// promotion's experiment_id.
		exists, err := openDemotionExistsForExperiment(db, c.ExperimentID)
		if err != nil {
			return fmt.Errorf("DemotionAuthor: idempotence check exp=%d: %w", c.ExperimentID, err)
		}
		if exists {
			skipped++
			continue
		}
		evidence := map[string]any{
			"source_proposal_id":    c.ID,
			"source_experiment_id":  c.ExperimentID,
			"source_rule_key":       c.RuleKey,
			"ratified_at":           c.RatifiedAt,
			"phase":                 "P3-placeholder",
			"note":                  "retention window elapsed; full scoring deferred to P4/P5 retention reports + post-ship monitor experiments",
			"trigger":               "stale-promotion-window",
			"stale_days_threshold":  stale,
		}
		evidenceJSON, _ := json.Marshal(evidence)
		var demotionID int
		err = db.QueryRowContext(ctx, `
			INSERT INTO PromotionProposals
				(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
				 authored_by, authored_at, ttl_expires_at)
			VALUES (?, 'demote', ?, '', ?, 'engineering-corps', datetime('now'), datetime('now', '+14 days'))
			RETURNING id
		`, c.ExperimentID, c.RuleKey, string(evidenceJSON)).Scan(&demotionID)
		if err != nil {
			return fmt.Errorf("DemotionAuthor: insert demotion exp=%d: %w", c.ExperimentID, err)
		}
		authored++
		logger.Printf("[%s] DemotionAuthor #%d: authored demotion proposal #%d for rule_key=%q (source promotion #%d, exp %d)",
			agentName, bounty.ID, demotionID, c.RuleKey, c.ID, c.ExperimentID)
	}

	logger.Printf("[%s] DemotionAuthor #%d: %d stale promotion(s) reviewed; authored=%d skipped=%d",
		agentName, bounty.ID, len(candidates), authored, skipped)

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		return fmt.Errorf("DemotionAuthor: complete bounty: %w", err)
	}
	return nil
}

type stalePromotion struct {
	ID           int
	ExperimentID int
	RuleKey      string
	RatifiedAt   string
}

// loadStalePromotedProposals returns ratified-promote proposals whose
// ratified_at is older than `staleDays`. Joining against the active
// FleetRules row would be a P4/P5 enrichment; in P3 the placeholder
// ratified_at signal alone suffices for the plumbing.
func loadStalePromotedProposals(db *sql.DB, staleDays int) ([]stalePromotion, error) {
	rows, err := db.Query(`
		SELECT id, experiment_id, IFNULL(rule_key,''), IFNULL(ratified_at,'')
		FROM PromotionProposals
		WHERE kind = 'promote'
		  AND IFNULL(ratified_at,'') != ''
		  AND IFNULL(rejected_at,'') = ''
		  AND ratified_at <= datetime('now', '-' || ? || ' days')
		ORDER BY id
	`, staleDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stalePromotion
	for rows.Next() {
		var c stalePromotion
		if err := rows.Scan(&c.ID, &c.ExperimentID, &c.RuleKey, &c.RatifiedAt); err != nil {
			return nil, err
		}
		// Defensive empty-rule-key trim — keep our SQL output sane.
		c.RuleKey = strings.TrimSpace(c.RuleKey)
		out = append(out, c)
	}
	return out, rows.Err()
}

// openDemotionExistsForExperiment returns true if a kind='demote'
// PromotionProposals row exists for this experiment that is neither
// ratified nor rejected (operator hasn't acted yet).
func openDemotionExistsForExperiment(db *sql.DB, experimentID int) (bool, error) {
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM PromotionProposals
		WHERE experiment_id = ? AND kind = 'demote'
		  AND IFNULL(ratified_at,'') = ''
		  AND IFNULL(rejected_at,'') = ''
	`, experimentID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

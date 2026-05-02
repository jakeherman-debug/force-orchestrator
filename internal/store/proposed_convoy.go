package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
)

// StoreProposedConvoy upserts a Commander's plan for Chancellor review.
// Returns the proposal ID.
func StoreProposedConvoy(db *sql.DB, featureID int, tasks []TaskPlan) (int, error) {
	planJSON, err := json.Marshal(tasks)
	if err != nil {
		return 0, err
	}
	return storeProposedConvoyJSON(db, featureID, string(planJSON))
}

// StoreProposedConvoyRaw upserts a pre-marshalled plan envelope. Used by
// the D5.5 P2 staged-convoy path to persist the full staging envelope
// (`{"staging_mode":"staged",...}`) under the same ProposedConvoys.plan_json
// column without forcing it through the legacy []TaskPlan shape.
//
// rawPlanJSON must already be valid JSON; the caller is responsible for
// marshalling. The same upsert-on-feature_id semantics apply — re-running
// the Commander on the same Feature replaces the prior proposal.
func StoreProposedConvoyRaw(db *sql.DB, featureID int, rawPlanJSON string) (int, error) {
	if rawPlanJSON == "" {
		return 0, fmt.Errorf("StoreProposedConvoyRaw: rawPlanJSON must be non-empty")
	}
	return storeProposedConvoyJSON(db, featureID, rawPlanJSON)
}

// parseProposedPlanFlexible decodes a ProposedConvoys.plan_json blob that
// may be either the legacy bare-array shape or the D5.5 P2 staged envelope.
// Returns the flattened []TaskPlan; the staged-envelope metadata (mode,
// strategy, per-stage gates) is read separately by GetProposedStagingPlan
// when the Chancellor approval path needs to create ConvoyStages rows.
//
// Errors only on JSON syntax errors. An empty/null plan_json yields an
// empty TaskPlan slice — the existing single-stage callers treat that as
// a benign "no proposal yet."
func parseProposedPlanFlexible(planJSON string) []TaskPlan {
	trimmed := planJSON
	if trimmed == "" {
		return nil
	}
	// Bare-array shape (legacy single-mode): unmarshal directly.
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var tasks []TaskPlan
		_ = json.Unmarshal([]byte(trimmed), &tasks)
		return tasks
	}
	// Staged envelope: peek at staging_mode, then flatten per-stage tasks.
	var envelope struct {
		StagingMode string `json:"staging_mode"`
		Stages      []struct {
			Tasks []TaskPlan `json:"tasks"`
		} `json:"stages"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return nil
	}
	var out []TaskPlan
	for _, s := range envelope.Stages {
		out = append(out, s.Tasks...)
	}
	return out
}

// GetProposedStagingPlan returns the raw plan_json for a feature so callers
// that need the full staged envelope (mode, strategy, per-stage gates) can
// re-parse via internal/agents/commander.ParseStagingPlan. Returns "" if no
// pending proposal exists. The store package itself does not depend on the
// commander package, so the typed StagingPlan struct lives there; this
// helper is the byte bridge.
func GetProposedStagingPlan(db *sql.DB, featureID int) (string, error) {
	var planJSON string
	err := db.QueryRow(`SELECT plan_json FROM ProposedConvoys WHERE feature_id = ? AND status = 'pending'`, featureID).Scan(&planJSON)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetProposedStagingPlan: query feature %d: %w", featureID, err)
	}
	return planJSON, nil
}

func storeProposedConvoyJSON(db *sql.DB, featureID int, planJSON string) (int, error) {
	var id int
	err := db.QueryRow(
		`INSERT INTO ProposedConvoys (feature_id, plan_json)
		 VALUES (?, ?)
		 ON CONFLICT(feature_id) DO UPDATE SET plan_json = excluded.plan_json, status = 'pending'
		 RETURNING id`,
		featureID, planJSON).Scan(&id)
	return id, err
}

// ClaimChancellorTask atomically claims one AwaitingChancellorReview Feature task
// and returns it along with its parsed task plan.
func ClaimChancellorTask(db *sql.DB, agentName string) (*Bounty, []TaskPlan, bool) {
	var b Bounty
	var planJSON string
	err := db.QueryRow(`
		SELECT bb.id, bb.parent_id, bb.target_repo, bb.type, bb.status, bb.payload,
		       bb.convoy_id, bb.checkpoint, bb.priority, IFNULL(bb.task_timeout, 0),
		       pc.plan_json
		FROM BountyBoard bb
		JOIN ProposedConvoys pc ON pc.feature_id = bb.id AND pc.status = 'pending'
		WHERE bb.status = 'AwaitingChancellorReview' AND bb.type = 'Feature'
		ORDER BY bb.priority DESC, bb.id ASC
		LIMIT 1`).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Type, &b.Status, &b.Payload,
			&b.ConvoyID, &b.Checkpoint, &b.Priority, &b.TaskTimeout, &planJSON)
	if err != nil {
		return nil, nil, false
	}

	res, _ := db.Exec(
		`UPDATE BountyBoard SET status = 'Locked', owner = ?, locked_at = datetime('now')
		 WHERE id = ? AND status = 'AwaitingChancellorReview'`,
		agentName, b.ID)
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, nil, false
	}
	b.Status = "Locked"
	b.Owner = agentName

	// D5.5 P2: tolerate either the legacy bare-array plan_json shape or
	// the staged envelope. parseProposedPlanFlexible flattens per-stage
	// tasks; staged metadata is fetched separately via
	// GetProposedStagingPlan when the Chancellor approval path needs it.
	tasks := parseProposedPlanFlexible(planJSON)
	return &b, tasks, true
}

// ClaimMergeTarget atomically locks a second AwaitingChancellorReview Feature task
// so the Chancellor can merge it with the currently-held task.
func ClaimMergeTarget(db *sql.DB, featureID int, agentName string) (*Bounty, []TaskPlan, bool) {
	var b Bounty
	var planJSON string
	err := db.QueryRow(`
		SELECT bb.id, bb.parent_id, bb.target_repo, bb.type, bb.status, bb.payload,
		       bb.convoy_id, bb.checkpoint, bb.priority, IFNULL(bb.task_timeout, 0),
		       pc.plan_json
		FROM BountyBoard bb
		JOIN ProposedConvoys pc ON pc.feature_id = bb.id AND pc.status = 'pending'
		WHERE bb.id = ? AND bb.status = 'AwaitingChancellorReview'`, featureID).
		Scan(&b.ID, &b.ParentID, &b.TargetRepo, &b.Type, &b.Status, &b.Payload,
			&b.ConvoyID, &b.Checkpoint, &b.Priority, &b.TaskTimeout, &planJSON)
	if err != nil {
		return nil, nil, false
	}

	res, _ := db.Exec(
		`UPDATE BountyBoard SET status = 'Locked', owner = ?, locked_at = datetime('now')
		 WHERE id = ? AND status = 'AwaitingChancellorReview'`,
		agentName, b.ID)
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, nil, false
	}
	b.Status = "Locked"
	b.Owner = agentName

	tasks := parseProposedPlanFlexible(planJSON)
	return &b, tasks, true
}

// GetActiveConvoyContext returns a summary of all active convoys for Chancellor context.
func GetActiveConvoyContext(db *sql.DB) []ActiveConvoyInfo {
	rows, err := db.Query(`
		SELECT c.id, c.name FROM Convoys c
		WHERE c.status = 'Active'
		ORDER BY c.created_at DESC LIMIT 20`)
	if err != nil {
		return nil
	}
	var convoys []ActiveConvoyInfo
	for rows.Next() {
		var c ActiveConvoyInfo
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			log.Printf("GetActiveConvoyContext: scan failed: %v", err)
			continue
		}
		convoys = append(convoys, c)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("proposed_convoy.go:GetActiveConvoyContext: rows iter error: %v", rErr)
	}
	rows.Close()

	for i := range convoys {
		taskRows, err := db.Query(`
			SELECT payload FROM BountyBoard
			WHERE convoy_id = ? AND status NOT IN ('Completed', 'Cancelled', 'Failed')
			ORDER BY id ASC LIMIT 10`, convoys[i].ID)
		if err != nil {
			continue
		}
		for taskRows.Next() {
			var payload string
			if err := taskRows.Scan(&payload); err != nil {
				log.Printf("GetActiveConvoyContext: taskRows scan failed: %v", err)
				continue
			}
			if len(payload) > 100 {
				payload = payload[:100] + "…"
			}
			convoys[i].Tasks = append(convoys[i].Tasks, payload)
		}
		if rErr := taskRows.Err(); rErr != nil {
			log.Printf("proposed_convoy.go:GetActiveConvoyContext: rows iter error: %v", rErr)
		}
		taskRows.Close()
	}
	return convoys
}

// GetPendingProposals returns pending ProposedConvoys excluding the one being reviewed.
func GetPendingProposals(db *sql.DB, excludeFeatureID int) []PendingProposalInfo {
	rows, err := db.Query(`
		SELECT pc.feature_id, bb.payload, pc.plan_json
		FROM ProposedConvoys pc
		JOIN BountyBoard bb ON bb.id = pc.feature_id
		WHERE pc.status = 'pending' AND pc.feature_id != ?
		ORDER BY pc.created_at ASC LIMIT 10`, excludeFeatureID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var proposals []PendingProposalInfo
	for rows.Next() {
		var p PendingProposalInfo
		if err := rows.Scan(&p.FeatureID, &p.Payload, &p.PlanJSON); err != nil {
			log.Printf("GetPendingProposals: scan failed: %v", err)
			continue
		}
		proposals = append(proposals, p)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("proposed_convoy.go:GetPendingProposals: rows iter error: %v", rErr)
	}
	return proposals
}

// SetProposedConvoyStatus updates the status of a proposal.
func SetProposedConvoyStatus(db *sql.DB, featureID int, status string) {
	db.Exec(`UPDATE ProposedConvoys SET status = ? WHERE feature_id = ?`, status, featureID)
}

// GetPendingFeatures returns Feature tasks not yet planned by Commander so the
// Chancellor can reason about upcoming work when reviewing a proposal.
func GetPendingFeatures(db *sql.DB, excludeFeatureID int) []PendingFeatureInfo {
	rows, err := db.Query(`
		SELECT id, payload FROM BountyBoard
		WHERE type = 'Feature' AND status IN ('Pending', 'Classifying')
		  AND id != ?
		ORDER BY id ASC LIMIT 20`, excludeFeatureID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var features []PendingFeatureInfo
	for rows.Next() {
		var f PendingFeatureInfo
		if err := rows.Scan(&f.FeatureID, &f.Payload); err != nil {
			log.Printf("GetPendingFeatures: scan failed: %v", err)
			continue
		}
		features = append(features, f)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("proposed_convoy.go:GetPendingFeatures: rows iter error: %v", rErr)
	}
	return features
}

// GetConvoyTailTaskIDs returns the IDs of tasks in the given convoy that no other
// task in the same convoy depends on — i.e., the last tasks in the execution graph.
// These are used as blocking dependencies when sequencing a new convoy after this one.
func GetConvoyTailTaskIDs(db *sql.DB, convoyID int) []int {
	rows, err := db.Query(`
		SELECT id FROM BountyBoard
		WHERE convoy_id = ?
		  AND status NOT IN ('Completed', 'Cancelled', 'Failed')
		  AND id NOT IN (
		      SELECT td.depends_on
		      FROM TaskDependencies td
		      INNER JOIN BountyBoard bb2 ON bb2.id = td.task_id AND bb2.convoy_id = ?
		  )`, convoyID, convoyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			log.Printf("GetConvoyTailTaskIDs: scan failed: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("proposed_convoy.go:GetConvoyTailTaskIDs: rows iter error: %v", rErr)
	}
	return ids
}

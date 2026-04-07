package store

import (
	"database/sql"
	"encoding/json"
)

// StoreProposedConvoy upserts a Commander's plan for Chancellor review.
// Returns the proposal ID.
func StoreProposedConvoy(db *sql.DB, featureID int, tasks []TaskPlan) (int, error) {
	planJSON, err := json.Marshal(tasks)
	if err != nil {
		return 0, err
	}
	var id int
	err = db.QueryRow(
		`INSERT INTO ProposedConvoys (feature_id, plan_json)
		 VALUES (?, ?)
		 ON CONFLICT(feature_id) DO UPDATE SET plan_json = excluded.plan_json, status = 'pending'
		 RETURNING id`,
		featureID, string(planJSON)).Scan(&id)
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

	var tasks []TaskPlan
	json.Unmarshal([]byte(planJSON), &tasks)
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

	var tasks []TaskPlan
	json.Unmarshal([]byte(planJSON), &tasks)
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
		rows.Scan(&c.ID, &c.Name)
		convoys = append(convoys, c)
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
			taskRows.Scan(&payload)
			if len(payload) > 100 {
				payload = payload[:100] + "…"
			}
			convoys[i].Tasks = append(convoys[i].Tasks, payload)
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
		rows.Scan(&p.FeatureID, &p.Payload, &p.PlanJSON)
		proposals = append(proposals, p)
	}
	return proposals
}

// SetProposedConvoyStatus updates the status of a proposal.
func SetProposedConvoyStatus(db *sql.DB, featureID int, status string) {
	db.Exec(`UPDATE ProposedConvoys SET status = ? WHERE feature_id = ?`, status, featureID)
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
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

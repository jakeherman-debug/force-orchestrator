package store

import "database/sql"

// DigestStats holds the fleet health summary for the last 24 hours.
type DigestStats struct {
	Completed    int
	Failed       int
	Escalated    int
	Pending      int
	Locked       int
	TopAgents    []AgentCount
	StaleConvoys []StaleConvoy
}

// AgentCount holds an agent name and their task completion count.
type AgentCount struct {
	Agent string
	Count int
}

// StaleConvoy holds minimal info about a convoy that has been Active for more than 48 hours.
type StaleConvoy struct {
	ID        int
	Name      string
	CreatedAt string
}

// FetchDigestStats returns a fleet health summary for use in the daily digest mail.
func FetchDigestStats(db *sql.DB) (DigestStats, error) {
	var stats DigestStats

	// (1) Task counts by terminal status in the last 24 hours.
	// BountyBoard has no updated_at; use TaskHistory.created_at, which records
	// the timestamp of each attempt outcome including terminal transitions.
	rows, err := db.Query(`
		SELECT b.status, COUNT(DISTINCT b.id)
		FROM BountyBoard b
		JOIN TaskHistory h ON h.task_id = b.id
		WHERE b.status IN ('Completed', 'Failed', 'Escalated')
		  AND h.outcome IN ('Completed', 'Failed', 'Escalated')
		  AND h.created_at >= datetime('now', '-24 hours')
		GROUP BY b.status`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return stats, err
		}
		switch status {
		case "Completed":
			stats.Completed = count
		case "Failed":
			stats.Failed = count
		case "Escalated":
			stats.Escalated = count
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	// (2) Current Pending and Locked counts.
	if err = db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status = 'Pending'`).Scan(&stats.Pending); err != nil {
		return stats, err
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status = 'Locked'`).Scan(&stats.Locked); err != nil {
		return stats, err
	}

	// (3) Top 3 agents by tasks completed.
	agentRows, err := db.Query(`
		SELECT agent, COUNT(*) AS cnt FROM TaskHistory
		WHERE outcome = 'Completed'
		GROUP BY agent
		ORDER BY cnt DESC
		LIMIT 3`)
	if err != nil {
		return stats, err
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var ac AgentCount
		if err := agentRows.Scan(&ac.Agent, &ac.Count); err != nil {
			return stats, err
		}
		stats.TopAgents = append(stats.TopAgents, ac)
	}
	if err := agentRows.Err(); err != nil {
		return stats, err
	}

	// (4) Stale active convoys (Active for more than 48 hours).
	convoyRows, err := db.Query(`
		SELECT id, name, created_at FROM Convoys
		WHERE status = 'Active'
		  AND created_at <= datetime('now', '-48 hours')`)
	if err != nil {
		return stats, err
	}
	defer convoyRows.Close()
	for convoyRows.Next() {
		var sc StaleConvoy
		if err := convoyRows.Scan(&sc.ID, &sc.Name, &sc.CreatedAt); err != nil {
			return stats, err
		}
		stats.StaleConvoys = append(stats.StaleConvoys, sc)
	}
	if err := convoyRows.Err(); err != nil {
		return stats, err
	}

	return stats, nil
}

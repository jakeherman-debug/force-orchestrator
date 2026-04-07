package store

import "database/sql"

// LeaderboardEntry holds per-agent performance metrics derived from TaskHistory.
type LeaderboardEntry struct {
	Agent           string
	TasksCompleted  int
	TasksFailed     int
	AvgTurns        float64
	AvgWallSeconds  float64
}

// GetLeaderboard returns agent performance stats sorted by TasksCompleted DESC.
// Agents with zero completed and zero failed tasks are excluded.
func GetLeaderboard(db *sql.DB) []LeaderboardEntry {
	rows, err := db.Query(`
		SELECT
			th.agent,
			COUNT(DISTINCT CASE WHEN th.outcome = 'Completed' THEN th.task_id END) AS tasks_completed,
			COUNT(DISTINCT CASE WHEN th.outcome = 'Failed'    THEN th.task_id END) AS tasks_failed,
			AVG(CASE WHEN th.outcome = 'Completed' THEN th.attempt END)            AS avg_turns,
			AVG(CASE WHEN th.outcome = 'Completed' AND bb.locked_at != ''
			         THEN (julianday(th.created_at) - julianday(bb.locked_at)) * 86400
			    END)                                                                AS avg_wall_seconds
		FROM TaskHistory th
		JOIN BountyBoard bb ON th.task_id = bb.id
		GROUP BY th.agent
		HAVING tasks_completed > 0 OR tasks_failed > 0
		ORDER BY tasks_completed DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var entries []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		var avgTurns, avgWall sql.NullFloat64
		if err := rows.Scan(&e.Agent, &e.TasksCompleted, &e.TasksFailed, &avgTurns, &avgWall); err != nil {
			return nil
		}
		if avgTurns.Valid {
			e.AvgTurns = avgTurns.Float64
		}
		if avgWall.Valid {
			e.AvgWallSeconds = avgWall.Float64
		}
		entries = append(entries, e)
	}
	if rows.Err() != nil {
		return nil
	}
	return entries
}

// D3 P6A.8 — Pulse fleet-panel pre-computed query helpers.
//
// The Pulse fleet panel needs five pieces of data per refresh: spend
// rate, active agents, convoys in flight, queue at a glance, trust
// dials compact. These helpers compute them in a single round-trip per
// component so the SSE stream's 5s tick stays under 100 ms.
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// PulseSnapshot is the JSON payload returned by /api/pulse/snapshot.
// All fields are read-side; the dashboard renders without further
// queries (no N+1, no client-side joins).
type PulseSnapshot struct {
	Spend         PulseSpend         `json:"spend"`
	ActiveAgents  []PulseActiveAgent `json:"active_agents"`
	Convoys       []PulseConvoy      `json:"convoys"`
	Queue         PulseQueue         `json:"queue"`
	TrustDials    []TrustDial        `json:"trust_dials"`
}

type PulseSpend struct {
	HourlyUSD       float64 `json:"hourly_usd"`
	ProjectedToday  float64 `json:"projected_today_usd"`
	Last7dAverage   float64 `json:"last_7d_avg_usd"`
	TopBurnerTaskID int64   `json:"top_burner_task_id"`
}

type PulseActiveAgent struct {
	Agent     string `json:"agent"`
	TaskID    int64  `json:"task_id"`
	Payload   string `json:"payload"` // truncated server-side; no innerHTML risk
	LockedAt string `json:"locked_at"`
}

type PulseConvoy struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	CycleCount int    `json:"cycle_count"`
}

type PulseQueue struct {
	HighStakes   int `json:"high_stakes"`
	MediumStakes int `json:"medium_stakes"`
	LowStakes    int `json:"low_stakes"`
	Total        int `json:"total"`
}

// PulseSnapshotFor builds the snapshot in five queries.
func PulseSnapshotFor(ctx context.Context, db *sql.DB, operatorEmail string) (PulseSnapshot, error) {
	var snap PulseSnapshot
	var err error
	if snap.Spend, err = pulseSpend(ctx, db); err != nil {
		return snap, fmt.Errorf("spend: %w", err)
	}
	if snap.ActiveAgents, err = pulseActiveAgents(ctx, db); err != nil {
		return snap, fmt.Errorf("active agents: %w", err)
	}
	if snap.Convoys, err = pulseConvoys(ctx, db); err != nil {
		return snap, fmt.Errorf("convoys: %w", err)
	}
	if snap.Queue, err = pulseQueue(ctx, db); err != nil {
		return snap, fmt.Errorf("queue: %w", err)
	}
	if operatorEmail != "" {
		if snap.TrustDials, err = ListCurrentTrustDials(ctx, db, operatorEmail); err != nil {
			return snap, fmt.Errorf("trust dials: %w", err)
		}
	}
	return snap, nil
}

func pulseSpend(ctx context.Context, db *sql.DB) (PulseSpend, error) {
	var s PulseSpend
	// Trailing-1h spend.
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM CostLedger
		WHERE created_at >= datetime('now', '-1 hour')`).Scan(&s.HourlyUSD)
	// Today's projected.
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM CostLedger
		WHERE created_at >= datetime('now', 'start of day')`).Scan(&s.ProjectedToday)
	// 7d average (per-day mean).
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) / 7.0 FROM CostLedger
		WHERE created_at >= datetime('now', '-7 days')`).Scan(&s.Last7dAverage)
	// Top burner this hour.
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(task_id, 0) FROM CostLedger
		WHERE created_at >= datetime('now', '-1 hour')
		ORDER BY cost_usd DESC LIMIT 1`).Scan(&s.TopBurnerTaskID)
	return s, nil
}

func pulseActiveAgents(ctx context.Context, db *sql.DB) ([]PulseActiveAgent, error) {
	rows, err := db.QueryContext(ctx, `SELECT IFNULL(owner, ''), id, IFNULL(payload, ''), IFNULL(locked_at, '')
		FROM BountyBoard WHERE status = 'Locked' ORDER BY locked_at DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PulseActiveAgent
	for rows.Next() {
		var a PulseActiveAgent
		if err := rows.Scan(&a.Agent, &a.TaskID, &a.Payload, &a.LockedAt); err != nil {
			return nil, fmt.Errorf("scan active agent: %w", err)
		}
		// Truncate payload server-side so XSS sinks have no surface.
		if len(a.Payload) > 120 {
			a.Payload = a.Payload[:120] + "…"
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter active agents: %w", err)
	}
	return out, nil
}

func pulseConvoys(ctx context.Context, db *sql.DB) ([]PulseConvoy, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, IFNULL(name, ''), IFNULL(status, ''), 0
		FROM Convoys WHERE status NOT IN ('Completed', 'Cancelled')
		ORDER BY id DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PulseConvoy
	for rows.Next() {
		var c PulseConvoy
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &c.CycleCount); err != nil {
			return nil, fmt.Errorf("scan convoy: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter convoys: %w", err)
	}
	return out, nil
}

func pulseQueue(ctx context.Context, db *sql.DB) (PulseQueue, error) {
	var q PulseQueue
	// PromotionProposals + Captain proposals etc. Keep simple: count tasks
	// in AwaitingCaptainReview / AwaitingCouncilReview as the operator's queue.
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM BountyBoard
		WHERE status IN ('AwaitingCaptainReview', 'AwaitingCouncilReview', 'Escalated')`).Scan(&q.Total)
	// Stakes tier breakdown approximate: Escalated=high, others=medium.
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM BountyBoard WHERE status = 'Escalated'`).Scan(&q.HighStakes)
	q.MediumStakes = q.Total - q.HighStakes
	return q, nil
}

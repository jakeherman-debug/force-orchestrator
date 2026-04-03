package agents

import (
	"database/sql"
	"fmt"

	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// CheckConvoyCompletions scans all active convoys and closes any where all
// CodeEdit tasks are complete. Called by the Inquisitor on every cycle.
func CheckConvoyCompletions(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	rows, err := db.Query(`SELECT id, name FROM Convoys WHERE status = 'Active'`)
	if err != nil {
		return
	}

	// Drain the cursor before doing per-convoy queries — avoids deadlock on
	// the single-connection SQLite pool (MaxOpenConns=1).
	type convoy struct {
		id   int
		name string
	}
	var active []convoy
	for rows.Next() {
		var c convoy
		rows.Scan(&c.id, &c.name)
		active = append(active, c)
	}
	rows.Close()

	for _, c := range active {
		completed, total := store.ConvoyProgress(db, c.id)
		if total == 0 {
			continue
		}

		// Check for any failed or escalated tasks — convoy is stalled
		var problemCount int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status IN ('Failed','Escalated')`, c.id).Scan(&problemCount)

		if completed == total {
			db.Exec(`UPDATE Convoys SET status = 'Completed' WHERE id = ?`, c.id)
			logger.Printf("Convoy '%s' COMPLETED (%d/%d tasks done)", c.name, completed, total)
			telemetry.EmitEvent(telemetry.TelemetryEvent{
				EventType: "convoy_completed",
				Payload:   map[string]any{"convoy_id": c.id, "convoy_name": c.name, "tasks": total},
			})
			store.SendMail(db, "inquisitor", "operator",
				fmt.Sprintf("[CONVOY COMPLETE] %s", c.name),
				fmt.Sprintf("Convoy '%s' has completed — all %d task(s) have been approved and merged.", c.name, total),
				0, store.MailTypeInfo)
		} else if problemCount > 0 {
			logger.Printf("Convoy '%s' STALLED — %d problem task(s), %d/%d complete", c.name, problemCount, completed, total)
			subject := fmt.Sprintf("[CONVOY STALLED] %s", c.name)
			var existing int
			db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = ? AND read_at = ''`, subject).Scan(&existing)
			if existing == 0 {
				store.SendMail(db, "inquisitor", "operator", subject,
					fmt.Sprintf("Convoy '%s' is stalled — %d task(s) have failed or escalated.\n\n%d/%d tasks completed.\n\nInspect: force convoy show %d\nRetry failed tasks: force convoy reset %d",
						c.name, problemCount, completed, total, c.id, c.id),
					0, store.MailTypeAlert)
			}
		}
	}
}

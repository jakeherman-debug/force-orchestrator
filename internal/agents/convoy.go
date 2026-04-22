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
			// No tasks were ever added — close as Completed so it doesn't sit Active forever.
			db.Exec(`UPDATE Convoys SET status = 'Completed' WHERE id = ?`, c.id)
			logger.Printf("Convoy '%s' closed — no tasks were added", c.name)
			continue
		}

		// Check for any failed or escalated tasks — convoy is stalled.
		// Cancelled tasks are not problems: they represent intentional operator removal of scope.
		var problemCount int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status IN ('Failed','Escalated')`, c.id).Scan(&problemCount)

		if completed == total {
			// PR flow branch: if the convoy has any ConvoyAskBranch rows, the
			// work is "complete at the sub-PR level" but a human still needs to
			// ship the draft PR(s) to main. Transition to AwaitingDraftPR and
			// enqueue a Diplomat ShipConvoy task.
			askBranches := store.ListConvoyAskBranches(db, c.id)
			if len(askBranches) > 0 {
				// Don't duplicate if one is already queued or in flight.
				// Boundary-match on JSON token so convoy id=1 doesn't dedup against 10, 100.
				var existing int
				db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
					WHERE type = 'ShipConvoy' AND status IN ('Pending', 'Locked')
					  AND (payload LIKE '%"convoy_id":' || ? || ',%'
					    OR payload LIKE '%"convoy_id":' || ? || '}%')`,
					c.id, c.id).Scan(&existing)
				if existing == 0 {
					db.Exec(`UPDATE Convoys SET status = 'AwaitingDraftPR' WHERE id = ?`, c.id)
					if _, err := QueueShipConvoy(db, c.id); err != nil {
						logger.Printf("Convoy '%s': failed to queue ShipConvoy: %v", c.name, err)
					} else {
						logger.Printf("Convoy '%s' all sub-PRs merged — enqueued Diplomat ShipConvoy", c.name)
					}
				}
				continue
			}

			// Legacy path (no PR flow): mark convoy Completed outright.
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
			db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, c.id)
			logger.Printf("Convoy '%s' STALLED — %d problem task(s), %d/%d complete", c.name, problemCount, completed, total)
			subject := fmt.Sprintf("[CONVOY STALLED] %s", c.name)
			var existing int
			db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = ? AND read_at = ''`, subject).Scan(&existing)
			if existing == 0 {
				var taskErr string
				db.QueryRow(`SELECT error_log FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status IN ('Failed','Escalated') ORDER BY id ASC LIMIT 1`, c.id).Scan(&taskErr)
				body := fmt.Sprintf("Convoy '%s' is stalled — %d task(s) have failed or escalated.\n\n%d/%d tasks completed.\n\nInspect: force convoy show %d\nRetry failed tasks: force convoy reset %d",
					c.name, problemCount, completed, total, c.id, c.id)
				if taskErr != "" {
					body += "\n\nFirst failure:\n" + taskErr
				}
				store.SendMail(db, "inquisitor", "operator", subject, body, 0, store.MailTypeAlert)
			}
		}
	}
}

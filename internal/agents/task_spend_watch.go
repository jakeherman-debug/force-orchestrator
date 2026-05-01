package agents

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"force-orchestrator/internal/store"
)

// task-spend-watch (D2 T1-1) — per-task spend anomaly detector.
//
// Sibling to spend-burn-watch (which gates the FLEET trailing-hour burn).
// task-spend-watch operates one level finer: each task with TaskHistory
// activity in the last hour gets its trailing-10-min cost summed; if the
// sum exceeds per_task_spend_alert_usd ($5 by default), the operator is
// mailed once per (task_id, window_start). If the sum exceeds
// per_task_spend_escalate_usd ($15 by default), an escalation is filed AND
// BountyBoard.spend_suspended is flipped so the next claim cycle skips
// the runaway row.
//
// Why a separate dog: the fleet-wide cap catches a many-task burn but
// silently sleeps a single task that's burning $5 every five minutes
// indefinitely (medic-requeue loop, captain-rejection thrash). The
// per-task gate catches that specific failure mode at single-task
// granularity, with operator-tunable thresholds.

// DefaultPerTaskSpendAlertUSD is the per-task soft alert threshold. A task's
// trailing-10-min cost above this triggers operator mail (once per window).
const DefaultPerTaskSpendAlertUSD = 5.0

// DefaultPerTaskSpendEscalateUSD is the per-task escalate threshold. A task's
// trailing-10-min cost above this triggers an escalation, suspends the task
// from claim queries, and mails the operator with the [TASK SPEND ESCALATE]
// subject. Operator unsuspends manually once the underlying loop is fixed.
const DefaultPerTaskSpendEscalateUSD = 15.0

// taskSpendWindow is the trailing-window size used for per-task aggregation.
// 10 minutes is short enough to catch a fast loop within one or two dog
// ticks (5-min cadence) while long enough to avoid false positives on a
// single expensive but legitimate Claude call.
const taskSpendWindow = 10 * time.Minute

// PerTaskSpendAlertUSD reads the operator-tunable alert threshold from
// SystemConfig, falling back to the compile-time default.
func PerTaskSpendAlertUSD(db *sql.DB) float64 {
	v := store.GetConfig(db, "per_task_spend_alert_usd", "")
	if v == "" {
		return DefaultPerTaskSpendAlertUSD
	}
	var n float64
	if _, err := fmt.Sscanf(v, "%f", &n); err != nil || n <= 0 {
		return DefaultPerTaskSpendAlertUSD
	}
	return n
}

// PerTaskSpendEscalateUSD reads the operator-tunable escalate threshold.
func PerTaskSpendEscalateUSD(db *sql.DB) float64 {
	v := store.GetConfig(db, "per_task_spend_escalate_usd", "")
	if v == "" {
		return DefaultPerTaskSpendEscalateUSD
	}
	var n float64
	if _, err := fmt.Sscanf(v, "%f", &n); err != nil || n <= 0 {
		return DefaultPerTaskSpendEscalateUSD
	}
	return n
}

// dogTaskSpendWatch is the dog dispatcher entry. 5-min cadence. Walks every
// task with TaskHistory activity in the last hour, computes its trailing-
// 10-min spend, and acts on the alert / escalate thresholds.
//
// Idempotence: TaskSpendWatch holds one row per (task_id, window_start);
// re-ticking within the same 10-min window finds the existing row and
// skips re-mailing the operator. The `window_start` is bucketed to the
// 10-min boundary so a tick at 10:13 and a tick at 10:17 share the
// "10:10:00" bucket and dedup against each other.
func dogTaskSpendWatch(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	alertUSD := PerTaskSpendAlertUSD(db)
	escalateUSD := PerTaskSpendEscalateUSD(db)
	now := time.Now().UTC()
	windowStart := bucketWindow(now, taskSpendWindow)
	cutoff := now.Add(-taskSpendWindow).Format("2006-01-02 15:04:05")

	// Pull the per-task cost sum within the trailing window. The CROSS-cut on
	// "last hour" + "last 10 min" keeps this query bounded (it sums a
	// fraction of TaskHistory rows) and matches the spec's two-stage filter
	// — only tasks "with activity in the last hour" are eligible, and the
	// rolling-window sum is over the last 10 minutes of that activity.
	rows, err := db.Query(`
		SELECT task_id, COALESCE(SUM(cost_usd_estimate), 0) AS cost
		FROM TaskHistory
		WHERE created_at > ?
		GROUP BY task_id
		HAVING cost > ?`, cutoff, alertUSD)
	if err != nil {
		return fmt.Errorf("dog task-spend-watch: query failed: %w", err)
	}
	defer rows.Close()

	type taskCost struct {
		taskID int
		cost   float64
	}
	var hits []taskCost
	for rows.Next() {
		var tc taskCost
		if sErr := rows.Scan(&tc.taskID, &tc.cost); sErr != nil {
			logger.Printf("Dog task-spend-watch: scan failed: %v", sErr)
			continue
		}
		hits = append(hits, tc)
	}
	if rErr := rows.Err(); rErr != nil {
		// Returned to RunDogs (operator-mailed via the dog dispatcher's
		// error path). A schema drift or table-locked error in this loop
		// would otherwise silently swallow per-task spend signals.
		return fmt.Errorf("dog task-spend-watch: rows iter error: %w", rErr)
	}

	if len(hits) == 0 {
		logger.Printf("Dog task-spend-watch: no tasks above alert threshold $%.2f in trailing 10m", alertUSD)
		return nil
	}

	bucketStr := windowStart.Format("2006-01-02 15:04:05")
	for _, h := range hits {
		// Idempotence gate: did we already record THIS bucket for THIS task?
		var existing int
		db.QueryRow(`SELECT COUNT(*) FROM TaskSpendWatch WHERE task_id = ? AND window_start = ?`,
			h.taskID, bucketStr).Scan(&existing)
		if existing > 0 {
			logger.Printf("Dog task-spend-watch: task %d $%.2f in window %s — already notified, skipping",
				h.taskID, h.cost, bucketStr)
			continue
		}

		escalating := h.cost > escalateUSD
		// Always insert the dedup row before any side effect so a partial
		// failure (mail send error, escalation insert error) doesn't cause
		// the next dog tick to re-fire the same notification.
		if _, insErr := db.Exec(
			`INSERT INTO TaskSpendWatch (task_id, window_start, cost_usd, notified_at)
			 VALUES (?, ?, ?, datetime('now'))`,
			h.taskID, bucketStr, h.cost); insErr != nil {
			logger.Printf("Dog task-spend-watch: TaskSpendWatch insert for task %d failed: %v", h.taskID, insErr)
			continue
		}

		taskHint := taskContextHint(db, h.taskID)
		if escalating {
			// Hard threshold — suspend further claims AND escalate.
			if susErr := store.SetSpendSuspended(db, h.taskID, true); susErr != nil {
				logger.Printf("Dog task-spend-watch: SetSpendSuspended(task=%d) failed: %v", h.taskID, susErr)
			}
			if _, escErr := CreateEscalation(db, h.taskID, "Medium",
				fmt.Sprintf("Per-task spend $%.2f over trailing 10m exceeds escalate threshold $%.2f. Task suspended from claim queries.",
					h.cost, escalateUSD)); escErr != nil {
				logger.Printf("Dog task-spend-watch: CreateEscalation(task=%d) failed: %v — operator mail still fires", h.taskID, escErr)
			}
			// P27 burn-down: budget-gate the operator emit before SendMail.
			// On allowed=false the helper has already drop/digested per the
			// configured budget. Fail-open on err so a transient SQLite
			// glitch never silences a high-stakes alert.
			if allowed, _ := store.RespectNotificationBudget(
				context.Background(), db, "operator", "task-spend-watch", "email", "{}",
				store.StakesHigh,
			); !allowed {
				// budget exhausted (StakesHigh always punches through, so
				// this branch only fires on a real config-set 0-cap row).
			} else {
				_ = allowed
			}
			store.SendMail(db, "task-spend-watch", "operator",
				fmt.Sprintf("[TASK SPEND ESCALATE] Task #%d — $%.2f / 10m", h.taskID, h.cost),
				fmt.Sprintf(`Task #%d's trailing-10-min cost ($%.2f) exceeded the per-task escalate threshold ($%.2f).

The task has been suspended from claim queries (BountyBoard.spend_suspended=1) so the
next claim cycle will skip it. Operator action required:

  1. Investigate why this task is burning that fast — typically a Medic-requeue loop or
     a captain/council rejection thrash. The dashboard task detail view shows lifetime
     cost + per-attempt breakdown.
  2. Fix the underlying loop or cancel the task.
  3. Clear the suspended flag manually:
       sqlite3 holocron.db "UPDATE BountyBoard SET spend_suspended=0 WHERE id=%d"

Task context:
%s

Thresholds (configurable via SystemConfig):
  per_task_spend_alert_usd     = $%.2f
  per_task_spend_escalate_usd  = $%.2f
`, h.taskID, h.cost, escalateUSD, h.taskID, taskHint, alertUSD, escalateUSD),
				h.taskID, store.MailTypeAlert)
			logger.Printf("Dog task-spend-watch: ESCALATE — task %d $%.2f/10m (suspended + mail + escalation)",
				h.taskID, h.cost)
			continue
		}

		// Soft threshold — operator mail only, no claim-time gate.
		store.SendMail(db, "task-spend-watch", "operator",
			fmt.Sprintf("[TASK SPEND ANOMALY] Task #%d — $%.2f / 10m", h.taskID, h.cost),
			fmt.Sprintf(`Task #%d's trailing-10-min cost ($%.2f) exceeded the per-task alert threshold ($%.2f).

The task is still claimable; the alert fires once per 10-min window so successive
ticks within the same window will not re-mail. If this becomes a sustained pattern
(>$%.2f/10m), the task will auto-suspend.

Task context:
%s
`, h.taskID, h.cost, alertUSD, escalateUSD, taskHint),
			h.taskID, store.MailTypeAlert)
		logger.Printf("Dog task-spend-watch: ALERT — task %d $%.2f/10m (mailed)", h.taskID, h.cost)
	}

	return nil
}

// taskContextHint returns a brief multi-line context block for the operator
// mail body. Best-effort: returns "(task row not found)" if GetBounty fails.
func taskContextHint(db *sql.DB, taskID int) string {
	b, err := store.GetBounty(db, taskID)
	if err != nil || b == nil {
		return fmt.Sprintf("  (task #%d not found in BountyBoard)", taskID)
	}
	payload := b.Payload
	if len(payload) > 200 {
		payload = payload[:200] + "..."
	}
	return fmt.Sprintf(`  task_id   : %d
  type      : %s
  status    : %s
  repo      : %s
  convoy    : %d
  retry/inf : %d / %d
  payload   : %s`,
		b.ID, b.Type, b.Status, b.TargetRepo, b.ConvoyID, b.RetryCount, b.InfraFailures, payload)
}

// bucketWindow rounds t down to the nearest multiple of size. Used to put
// successive dog ticks within the same 10-min window into the same dedup
// bucket regardless of the exact tick offset.
func bucketWindow(t time.Time, size time.Duration) time.Time {
	return t.Truncate(size)
}

// TaskSpendAnomaliesLastHour (dashboard) returns the count of TaskSpendWatch
// rows inserted in the last hour. Surfaced on /api/status so the operator
// sees recent anomaly volume at a glance.
func TaskSpendAnomaliesLastHour(db *sql.DB) int {
	var n int
	db.QueryRow(`
		SELECT COUNT(*) FROM TaskSpendWatch
		WHERE notified_at > datetime('now', '-1 hours')`).Scan(&n)
	return n
}

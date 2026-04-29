package agents

import (
	"database/sql"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// seedTaskHistory inserts n rows into TaskHistory for taskID, evenly spaced
// over the trailing windowMinutes; each row carries costPerRowUSD. Returns
// nothing — the helper exists so each test reads as a single setup line.
//
// Uses the SQLite "datetime('now', '-N minutes')" form so created_at lands
// exactly where the dog's "trailing 10m" filter expects.
func seedTaskHistory(t *testing.T, db *sql.DB, taskID, n, windowMinutes int, costPerRowUSD float64) {
	t.Helper()
	if n <= 0 {
		return
	}
	// Spread rows across the window. windowMinutes/n determines the offset
	// per row so a 12-row / 10-minute seed lands every ~50 seconds. We add
	// the rows in reverse-time order so the most-recent row is the LAST
	// insert (deterministic ordering for any future test that asserts on
	// recency — none today).
	stepSec := (windowMinutes * 60) / n
	if stepSec < 1 {
		stepSec = 1
	}
	for i := 0; i < n; i++ {
		offsetSec := i * stepSec
		// "-Ns" = N seconds ago.
		modifier := "-" + strconv.Itoa(offsetSec) + " seconds"
		_, err := db.Exec(`
			INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome, tokens_in, tokens_out, cost_usd_estimate, created_at)
			VALUES (?, ?, 'astromech', 'test-session', '', 'Completed', 0, 0, ?, datetime('now', ?))`,
			taskID, i+1, costPerRowUSD, modifier)
		if err != nil {
			t.Fatalf("seedTaskHistory: insert row %d failed: %v", i, err)
		}
	}
}

// seedBounty inserts a Pending CodeEdit row so the dog has a real task to
// reference when generating its operator-mail body.
func seedBounty(t *testing.T, db *sql.DB) int {
	t.Helper()
	id := store.AddBounty(db, 0, "CodeEdit", "test task body")
	if id == 0 {
		t.Fatalf("seedBounty: AddBounty returned 0")
	}
	return id
}

// captureMail returns Fleet_Mail rows whose subject matches a substring,
// scoped to the task. Test helper — no production code path uses this.
func captureMail(t *testing.T, db *sql.DB, taskID int, subjectSubstr string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT subject FROM Fleet_Mail WHERE task_id = ? AND subject LIKE ?`,
		taskID, "%"+subjectSubstr+"%")
	if err != nil {
		t.Fatalf("captureMail: query failed: %v", err)
	}
	defer rows.Close()
	var subjects []string
	for rows.Next() {
		var s string
		if sErr := rows.Scan(&s); sErr != nil {
			t.Fatalf("captureMail: scan failed: %v", sErr)
		}
		subjects = append(subjects, s)
	}
	return subjects
}

// TestTaskSpendWatch_AlertsAtSoftThreshold seeds 12 rows totaling $6 in 10
// min, runs the dog, and asserts: (a) operator mail emitted with the
// [TASK SPEND ANOMALY] subject, (b) one TaskSpendWatch row inserted.
// Default threshold ($5) is exceeded; default escalate threshold ($15)
// is NOT, so the row does NOT get spend_suspended.
func TestTaskSpendWatch_AlertsAtSoftThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := seedBounty(t, db)
	// 12 rows × $0.50 = $6.00 in trailing 10m — above $5 alert, below $15 escalate.
	seedTaskHistory(t, db, taskID, 12, 10, 0.50)

	logger := log.New(io.Discard, "", 0)
	if err := dogTaskSpendWatch(db, logger); err != nil {
		t.Fatalf("dogTaskSpendWatch failed: %v", err)
	}

	// (a) operator mail with the alert subject.
	mails := captureMail(t, db, taskID, "[TASK SPEND ANOMALY]")
	if len(mails) != 1 {
		t.Fatalf("expected 1 [TASK SPEND ANOMALY] mail, got %d (subjects=%v)", len(mails), mails)
	}
	if !strings.Contains(mails[0], "$6.00") {
		t.Errorf("mail subject should reflect $6.00 cost; got %q", mails[0])
	}

	// (b) TaskSpendWatch row inserted.
	var rowCount int
	db.QueryRow(`SELECT COUNT(*) FROM TaskSpendWatch WHERE task_id = ?`, taskID).Scan(&rowCount)
	if rowCount != 1 {
		t.Errorf("expected 1 TaskSpendWatch row, got %d", rowCount)
	}

	// (c) NOT escalating — spend_suspended must remain 0.
	if store.GetSpendSuspended(db, taskID) {
		t.Errorf("task should NOT be spend-suspended at $6 (escalate threshold $15)")
	}

	// (d) Escalation row should NOT exist for this task.
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount != 0 {
		t.Errorf("expected 0 escalation rows at soft threshold; got %d", escCount)
	}
}

// TestTaskSpendWatch_Idempotence asserts that a second invocation of the dog
// in the same window produces no duplicate mail and no duplicate
// TaskSpendWatch row. This is the load-bearing dedup property — dog ticks
// every 5 min within the same 10-min bucket would otherwise mail the
// operator twice for the same anomaly.
func TestTaskSpendWatch_Idempotence(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := seedBounty(t, db)
	seedTaskHistory(t, db, taskID, 12, 10, 0.50)

	logger := log.New(io.Discard, "", 0)

	// First run.
	if err := dogTaskSpendWatch(db, logger); err != nil {
		t.Fatalf("first dogTaskSpendWatch failed: %v", err)
	}
	// Second run — same DB, same window, no new TaskHistory rows.
	if err := dogTaskSpendWatch(db, logger); err != nil {
		t.Fatalf("second dogTaskSpendWatch failed: %v", err)
	}

	// (a) Still exactly 1 mail.
	mails := captureMail(t, db, taskID, "[TASK SPEND ANOMALY]")
	if len(mails) != 1 {
		t.Fatalf("expected 1 mail after 2 runs, got %d", len(mails))
	}
	// (b) Still exactly 1 TaskSpendWatch row.
	var rowCount int
	db.QueryRow(`SELECT COUNT(*) FROM TaskSpendWatch WHERE task_id = ?`, taskID).Scan(&rowCount)
	if rowCount != 1 {
		t.Errorf("expected 1 TaskSpendWatch row after 2 runs, got %d", rowCount)
	}
}

// TestTaskSpendWatch_EscalatesAtHardThreshold seeds rows totaling $20 in 10
// min — above the default $15 escalate threshold. Assertions:
//   - operator mail with [TASK SPEND ESCALATE] subject
//   - TaskSpendWatch row inserted
//   - BountyBoard.spend_suspended = 1
//   - Open Escalations row created (severity Medium)
//   - claim queries SKIP the suspended row
func TestTaskSpendWatch_EscalatesAtHardThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := seedBounty(t, db)
	// 10 rows × $2 = $20 — over $15 escalate threshold.
	seedTaskHistory(t, db, taskID, 10, 10, 2.0)

	logger := log.New(io.Discard, "", 0)
	if err := dogTaskSpendWatch(db, logger); err != nil {
		t.Fatalf("dogTaskSpendWatch failed: %v", err)
	}

	// Mail.
	mails := captureMail(t, db, taskID, "[TASK SPEND ESCALATE]")
	if len(mails) != 1 {
		t.Fatalf("expected 1 [TASK SPEND ESCALATE] mail, got %d", len(mails))
	}

	// Spend suspended.
	if !store.GetSpendSuspended(db, taskID) {
		t.Errorf("task should be spend-suspended at $20 / 10m")
	}

	// Escalation row.
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("expected 1 Open escalation, got %d", escCount)
	}

	// CreateEscalation also flips status → 'Escalated', so the row is no
	// longer Pending regardless of spend_suspended. Reset status to Pending
	// so we can isolate the spend_suspended gate. (This is a unit-test
	// surgery — operators in production unsuspend after acknowledging the
	// escalation, then the task moves through Medic / cancel / etc.)
	if _, err := db.Exec(`UPDATE BountyBoard SET status='Pending' WHERE id=?`, taskID); err != nil {
		t.Fatalf("reset status to Pending failed: %v", err)
	}
	// ClaimBounty must STILL skip the suspended row even though it's Pending.
	if got, ok := store.ClaimBounty(db, "CodeEdit", "astromech-test"); ok || got != nil {
		t.Errorf("expected ClaimBounty to skip spend_suspended task; got id=%v ok=%v", got, ok)
	}

	// After unsuspending, the task should be claimable again.
	if err := store.SetSpendSuspended(db, taskID, false); err != nil {
		t.Fatalf("SetSpendSuspended(false) failed: %v", err)
	}
	if got, ok := store.ClaimBounty(db, "CodeEdit", "astromech-test"); !ok || got == nil {
		t.Errorf("expected ClaimBounty to succeed after unsuspending; got id=%v ok=%v", got, ok)
	}
}

// TestTaskSpendWatch_NoOpUnderThreshold asserts that no mail / no row is
// emitted when every task's trailing-10-min cost is below the alert
// threshold. The "happy path" / quiet fleet case.
func TestTaskSpendWatch_NoOpUnderThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := seedBounty(t, db)
	// 4 rows × $0.50 = $2, well under $5 alert.
	seedTaskHistory(t, db, taskID, 4, 10, 0.50)

	logger := log.New(io.Discard, "", 0)
	if err := dogTaskSpendWatch(db, logger); err != nil {
		t.Fatalf("dogTaskSpendWatch failed: %v", err)
	}

	mails := captureMail(t, db, taskID, "[TASK SPEND")
	if len(mails) != 0 {
		t.Errorf("expected 0 mails under alert threshold, got %d (subjects=%v)", len(mails), mails)
	}
	var rowCount int
	db.QueryRow(`SELECT COUNT(*) FROM TaskSpendWatch`).Scan(&rowCount)
	if rowCount != 0 {
		t.Errorf("expected 0 TaskSpendWatch rows, got %d", rowCount)
	}
}

// TestTaskSpendWatch_OperatorThresholdOverride asserts that a SystemConfig
// override moves both alert and escalate thresholds.
func TestTaskSpendWatch_OperatorThresholdOverride(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Lower the alert threshold so a $2 task trips it; raise the escalate
	// threshold so a $2 task does NOT escalate.
	store.SetConfig(db, "per_task_spend_alert_usd", "1.5")
	store.SetConfig(db, "per_task_spend_escalate_usd", "100")

	taskID := seedBounty(t, db)
	seedTaskHistory(t, db, taskID, 4, 10, 0.50) // $2 total

	logger := log.New(io.Discard, "", 0)
	if err := dogTaskSpendWatch(db, logger); err != nil {
		t.Fatalf("dogTaskSpendWatch failed: %v", err)
	}

	mails := captureMail(t, db, taskID, "[TASK SPEND ANOMALY]")
	if len(mails) != 1 {
		t.Fatalf("expected 1 anomaly mail with overridden $1.5 threshold, got %d", len(mails))
	}
	if store.GetSpendSuspended(db, taskID) {
		t.Errorf("task should NOT be suspended at $2 with $100 escalate threshold")
	}
}

// TestTaskSpendWatch_TaskSpendAnomaliesLastHour asserts the dashboard
// counter helper picks up freshly-recorded rows.
func TestTaskSpendWatch_TaskSpendAnomaliesLastHour(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if got := TaskSpendAnomaliesLastHour(db); got != 0 {
		t.Errorf("starting state: expected 0, got %d", got)
	}

	taskID := seedBounty(t, db)
	seedTaskHistory(t, db, taskID, 12, 10, 0.50)

	logger := log.New(io.Discard, "", 0)
	if err := dogTaskSpendWatch(db, logger); err != nil {
		t.Fatalf("dogTaskSpendWatch failed: %v", err)
	}
	if got := TaskSpendAnomaliesLastHour(db); got != 1 {
		t.Errorf("expected 1 anomaly in last hour, got %d", got)
	}
}

// TestBucketWindow asserts the time-bucket helper rounds cleanly on the
// 10-minute boundary.
func TestBucketWindow(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2026-04-29T10:13:42Z", "2026-04-29T10:10:00Z"},
		{"2026-04-29T10:17:01Z", "2026-04-29T10:10:00Z"},
		{"2026-04-29T10:20:00Z", "2026-04-29T10:20:00Z"},
		{"2026-04-29T10:09:59Z", "2026-04-29T10:00:00Z"},
	}
	for _, tc := range cases {
		in, _ := time.Parse(time.RFC3339, tc.in)
		got := bucketWindow(in, 10*time.Minute).Format(time.RFC3339)
		if got != tc.want {
			t.Errorf("bucketWindow(%s) = %s; want %s", tc.in, got, tc.want)
		}
	}
}

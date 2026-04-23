package agents

import (
	"database/sql"
	"fmt"
	"time"

	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// Spend-cap invariants (see CLAUDE.md):
//
//   hourly_spend_cap_usd  — trailing-hour spend ceiling. Default $25/h.
//     When exceeded, every agent claim loop skip-and-sleeps instead of
//     spawning. This is the soft ceiling — operator mail is sent the first
//     time per hour it trips.
//
//   hourly_spend_estop_usd  — trailing-hour spend kill-switch. Default $200/h.
//     When exceeded, the spend-burn-watch dog flips e-stop unconditionally.
//     This is the hard ceiling — the $300-burn backstop.
//
// Both ceilings look at the SAME trailing-hour sum (SpendRateDollars(db,"1 hours")).
// A task that has just started running (its tokens not yet flushed to
// TaskHistory) is NOT counted — this is intentional, since the in-flight work
// will land on the next refresh and be visible to the next tick.

// DefaultHourlySpendCapUSD is the per-hour spend ceiling for agent claim loops.
// Agents skip-and-sleep when trailing-hour spend exceeds this value. Default
// is deliberately conservative: $25/h ≈ $600/day which is easily an order of
// magnitude above normal fleet operation. The $300 observed burn happened in
// under 3 hours so this cap would have bounded it at ~$75.
const DefaultHourlySpendCapUSD = 25.0

// DefaultHourlySpendEstopUSD is the auto-e-stop trigger. The spend-burn-watch
// dog flips estop=true the moment trailing-hour spend crosses this value.
// Chosen 8x above the soft cap so a single noisy hour doesn't halt the fleet,
// but a runaway loop trips within one 5-min dog cycle.
const DefaultHourlySpendEstopUSD = 200.0

// HourlySpendCapUSD returns the configured soft-cap, or the default if unset.
func HourlySpendCapUSD(db *sql.DB) float64 {
	v := store.GetConfig(db, "hourly_spend_cap_usd", "")
	if v == "" {
		return DefaultHourlySpendCapUSD
	}
	var n float64
	if _, err := fmt.Sscanf(v, "%f", &n); err != nil || n <= 0 {
		return DefaultHourlySpendCapUSD
	}
	return n
}

// HourlySpendEstopUSD returns the configured auto-e-stop threshold, or the default.
func HourlySpendEstopUSD(db *sql.DB) float64 {
	v := store.GetConfig(db, "hourly_spend_estop_usd", "")
	if v == "" {
		return DefaultHourlySpendEstopUSD
	}
	var n float64
	if _, err := fmt.Sscanf(v, "%f", &n); err != nil || n <= 0 {
		return DefaultHourlySpendEstopUSD
	}
	return n
}

// SpendCapExceeded returns true when the trailing-hour spend has crossed the
// configured soft cap. Called at the top of every agent claim loop. If true,
// the agent skip-and-sleeps rather than claiming new work — the only way out
// of cap-exceeded is for existing work to finish (reducing the windowed sum as
// old TaskHistory rows age out of the hour) OR the operator raising the cap.
func SpendCapExceeded(db *sql.DB) bool {
	return store.SpendRateDollars(db, "1 hours") > HourlySpendCapUSD(db)
}

// SleepUnlessEstopped sleeps for up to d, but wakes early if e-stop is flipped.
// Polls IsEstopped every pollInterval. Returns true if e-stop interrupted the
// sleep, false if the full duration elapsed.
//
// This is the mandatory replacement for any long `time.Sleep` inside an agent
// loop — most notably the rate-limit backoff, which can be 60s-600s long.
// Short sleeps (< pollInterval) just call time.Sleep directly.
//
// Test budget constraint: with a 1s poll interval, e-stop response is ≤ 1s
// regardless of how long the caller asked for. The Pattern P11 test (AUDIT-107)
// has a 3-second hard budget, so pollInterval must stay well under 3s.
func SleepUnlessEstopped(db *sql.DB, d time.Duration) bool {
	return sleepUnlessEstopped(db, d, 1*time.Second)
}

// sleepUnlessEstopped is the testable core of SleepUnlessEstopped with an
// injected poll interval for fast test runs.
func sleepUnlessEstopped(db *sql.DB, d time.Duration, pollInterval time.Duration) bool {
	if d <= 0 {
		return IsEstopped(db)
	}
	if d <= pollInterval {
		time.Sleep(d)
		return IsEstopped(db)
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if IsEstopped(db) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining > pollInterval {
			time.Sleep(pollInterval)
		} else {
			time.Sleep(remaining)
		}
	}
	return IsEstopped(db)
}

// ReportSpendBurn is called by the spend-burn-watch dog on each tick. It
// queries trailing-hour spend, mails the operator the first time per hour it
// crosses the soft cap, and flips auto-e-stop when it crosses the hard cap.
// Returns (hourlySpend, capExceeded, estopped).
//
// The "once per hour" dedup uses SystemConfig[spend_cap_last_alert_hour] which
// stores the ISO-truncated hour-of-last-alert. A new alert only fires when the
// current hour differs from the stored value — so the operator doesn't get
// spammed every 5 minutes during a sustained burn.
func ReportSpendBurn(db *sql.DB) (float64, bool, bool) {
	hourly := store.SpendRateDollars(db, "1 hours")
	cap := HourlySpendCapUSD(db)
	estopThreshold := HourlySpendEstopUSD(db)

	// Hard ceiling: flip e-stop and mail operator once per breach.
	if hourly > estopThreshold {
		// Only flip if not already estopped (avoid redundant mail on subsequent ticks).
		if !IsEstopped(db) {
			SetEstop(db, true)
			telemetry.EmitEvent(telemetry.EventSpendCapExceeded(hourly, estopThreshold, "auto_estop"))
			store.SendMail(db, "spend-burn-watch", "operator",
				fmt.Sprintf("[AUTO-ESTOP] Trailing-hour spend $%.2f exceeded estop threshold $%.2f", hourly, estopThreshold),
				fmt.Sprintf(`The fleet has auto-activated e-stop because trailing-hour spend ($%.2f) exceeded the hard ceiling ($%.2f).

All agent claim loops have halted. Dogs will also pause on their next cycle.

To investigate:
  force tail <task_id>       # show in-flight Claude output
  force mail                  # look for escalations or infra failures

To resume after resolving:
  force estop off

If you believe the cap is set too low, raise it first:
  force config set hourly_spend_estop_usd 300
`, hourly, estopThreshold),
				0, store.MailTypeAlert)
		}
		return hourly, true, true
	}

	// Soft ceiling: mail operator once per hour while over cap.
	if hourly > cap {
		hourKey := time.Now().UTC().Format("2006-01-02T15")
		lastAlertHour := store.GetConfig(db, "spend_cap_last_alert_hour", "")
		if lastAlertHour != hourKey {
			store.SetConfig(db, "spend_cap_last_alert_hour", hourKey)
			telemetry.EmitEvent(telemetry.EventSpendCapExceeded(hourly, cap, "soft_cap"))
			store.SendMail(db, "spend-burn-watch", "operator",
				fmt.Sprintf("[SPEND WARNING] Trailing-hour spend $%.2f exceeded cap $%.2f", hourly, cap),
				fmt.Sprintf(`Trailing-hour spend ($%.2f) has exceeded the soft cap ($%.2f).

Agent claim loops are skip-and-sleeping; NO new work is being spawned while over cap.
Dogs continue to run. Auto-e-stop triggers at $%.2f/h.

To investigate:
  force tail <task_id>       # show in-flight Claude output
  force status                # see current pending/locked counts

This is a warning, not a halt — the fleet resumes spawning automatically when
the trailing-hour spend drops back under the cap.
`, hourly, cap, estopThreshold),
				0, store.MailTypeAlert)
		}
		return hourly, true, false
	}

	return hourly, false, false
}

// dogSpendBurnWatch is the dog dispatcher entry. 5-min cadence. Cheap
// (one SUM query + possibly one mail) — no agent work spawned.
func dogSpendBurnWatch(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	hourly, capExceeded, estopped := ReportSpendBurn(db)
	if estopped {
		logger.Printf("Dog spend-burn-watch: AUTO-ESTOP — $%.2f/h exceeds estop threshold", hourly)
	} else if capExceeded {
		logger.Printf("Dog spend-burn-watch: $%.2f/h exceeds soft cap (warning mailed)", hourly)
	} else {
		logger.Printf("Dog spend-burn-watch: trailing-hour spend $%.2f (under cap)", hourly)
	}
	return nil
}

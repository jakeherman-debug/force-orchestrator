package agents

import (
	"database/sql"
	"fmt"
	"time"

	"force-orchestrator/internal/store"
)

// E-stop and pressure-gating are intentionally separate mechanisms:
//
//	IsEstopped   — hard stop. NO tasks should be processed by ANY agent, regardless
//	               of capacity. Activated manually by the operator. The daemon
//	               continues running but all agent loops skip their claim step.
//
//	IsOverCapacity — soft throttle. Agents defer claiming NEW tasks when the system
//	                 is already processing as many tasks as configured. Resets
//	                 automatically as tasks complete.
//
// They compose in the agent claim loop as independent sequential checks:
//
//	if IsEstopped(db)      { sleep; continue }   // hard wall first
//	if IsOverCapacity(db)  { sleep; continue }   // soft throttle second
//	ClaimBounty(...)                              // only now claim work
//
// Never implement one in terms of the other. Setting max_concurrent=0 to simulate
// an e-stop conflates the two, making it impossible to resume capacity-aware
// throttling independently of the operator-controlled stop.

const DefaultMaxConcurrent = 0 // 0 = unlimited (operator must explicitly set max_concurrent to throttle)
const MaxInfraFailures = 5    // circuit-break a task after this many infra failures
const MaxRetries = 3

// StallWarnTimeout is the duration after which a locked task with no commits is
// flagged as stalled in status output. Also used by the Inquisitor for triage.
const StallWarnTimeout = 20 * time.Minute

// IsEstopped returns true if the operator has activated the emergency stop.
func IsEstopped(db *sql.DB) bool {
	return store.GetConfig(db, "estop", "false") == "true"
}

// SetEstop activates or deactivates the emergency stop.
func SetEstop(db *sql.DB, active bool) {
	val := "false"
	if active {
		val = "true"
	}
	store.SetConfig(db, "estop", val)
}

// IsOverCapacity returns true if the number of currently running CodeEdit tasks
// is at or above the configured maximum. Only CodeEdit tasks count — Commander
// and Jedi Council are lightweight and not subject to capacity limits.
func IsOverCapacity(db *sql.DB) bool {
	maxStr := store.GetConfig(db, "max_concurrent", "")
	max := DefaultMaxConcurrent
	if maxStr != "" {
		fmt.Sscanf(maxStr, "%d", &max)
	}
	if max <= 0 {
		return false // 0 = unlimited
	}
	var active int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status IN ('Locked', 'UnderReview') AND type = 'CodeEdit'`).Scan(&active)
	return active >= max
}

// InfraBackoff returns a linear backoff duration for infra failure retries:
// 10s × count, capped at 60s. Prevents tight retry loops on broken infrastructure.
func InfraBackoff(count int) time.Duration {
	d := time.Duration(count) * 10 * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// RateLimitBackoff returns an exponential backoff for rate-limit errors:
// 60s, 120s, 240s … capped at 10 minutes. Longer than infra backoff because
// rate limits are external quota events, not transient infrastructure hiccups.
func RateLimitBackoff(count int) time.Duration {
	d := 60 * time.Second
	for i := 0; i < count; i++ {
		d *= 2
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// SpawnDelayDuration returns the configured inter-claim delay for agents.
// Agents sleep this long after successfully claiming a task to prevent
// thundering-herd when many tasks become Pending simultaneously.
func SpawnDelayDuration(db *sql.DB) time.Duration {
	v := store.GetConfig(db, "spawn_delay_ms", "0")
	var ms int
	fmt.Sscanf(v, "%d", &ms)
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// IsThrottledByBatchSize returns true when the number of tasks claimed in the
// last 60 seconds has reached the configured batch_size limit. A value of 0
// (default) disables throttling entirely.
func IsThrottledByBatchSize(db *sql.DB) bool {
	v := store.GetConfig(db, "batch_size", "0")
	var max int
	fmt.Sscanf(v, "%d", &max)
	if max <= 0 {
		return false
	}
	var recent int
	// Only 'Locked', 'UnderReview', and 'UnderCaptainReview' retain a non-empty
	// locked_at; UpdateBountyStatus clears locked_at for all other statuses.
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE locked_at >= datetime('now', '-60 seconds')
		  AND status IN ('Locked','UnderReview','UnderCaptainReview')`).Scan(&recent)
	return recent >= max
}

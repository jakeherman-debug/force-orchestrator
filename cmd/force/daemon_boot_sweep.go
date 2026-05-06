package main

// D12 P3 — Boot-time recovery sweep.
//
// runBootSweep is invoked from cmdDaemon AFTER the crash-budget check
// passes and BEFORE the agent spawn loop. It puts the fleet in a clean
// state by sweeping four classes of stale state that survive a daemon
// restart:
//
//  1. Stale Locked / UnderReview / UnderCaptainReview tasks — released
//     back to Pending via store.ReleaseInFlightTasks. Without this the
//     first agent to claim would see a stuck row owned by a vanished
//     daemon process.
//  2. Stale Dogs.heartbeat_at — dogs that never got to write their
//     "I finished" tick before the daemon vanished. Cleared so the
//     liveness banner stops showing them as alive.
//  3. Half-baked DraftPROpen convoys — convoys that have a draft_pr_url
//     populated but no PRHandoffSyntheses row recording the operator
//     notification. We log a warning so the operator can re-trigger the
//     handoff synthesis.
//  4. Mid-update binary — if the live binary's SHA doesn't match the
//     most recent DaemonUpdateHistory.outcome='success' row's
//     new_binary_sha, log a warning. This catches the case where an
//     update partially failed and the operator restored the old binary
//     by hand without recording the rollback.
//
// Failures in any individual step are logged, not fatal — the daemon
// must boot even if one of these probes returns an error (e.g. a
// missing column on a freshly-migrated DB the first time after upgrade).
// A non-nil return signals "the operator should look at this", but
// callers continue past it.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"force-orchestrator/internal/daemon/trust"
	"force-orchestrator/internal/store"
)

// runBootSweep walks the four boot-time recovery checks. Each step's
// outcome is logged; the function returns a wrapped error summarising
// any individual failure, but does NOT short-circuit (one stale row
// shouldn't block the daemon from booting).
func runBootSweep(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("runBootSweep: nil db")
	}
	var errs []string

	// (1) Release stale in-flight tasks. ReleaseInFlightTasks already
	// scrubs secrets from the reason string (Fix #10).
	if released := store.ReleaseInFlightTasks(db, "boot sweep — daemon restart recovery"); released > 0 {
		log.Printf("[BOOT-SWEEP] released %d stale in-flight task(s) back to Pending", released)
	}

	// (2) Clear stale Dogs heartbeats. A heartbeat older than 10 minutes
	// is treated as stale (the dog scheduler ticks every ~30s; 10 min
	// threshold means we don't blow away dogs that just paused).
	if cleared, err := clearStaleDogHeartbeats(db, 10*time.Minute); err != nil {
		errs = append(errs, fmt.Sprintf("dog heartbeats: %v", err))
		log.Printf("[BOOT-SWEEP] clearStaleDogHeartbeats: %v", err)
	} else if cleared > 0 {
		log.Printf("[BOOT-SWEEP] cleared %d stale dog heartbeat(s)", cleared)
	}

	// (3) Detect half-baked DraftPROpen convoys.
	if half, err := findHalfBakedDraftPROpenConvoys(db); err != nil {
		errs = append(errs, fmt.Sprintf("half-baked convoys: %v", err))
		log.Printf("[BOOT-SWEEP] findHalfBakedDraftPROpenConvoys: %v", err)
	} else if len(half) > 0 {
		log.Printf("[BOOT-SWEEP] %d half-baked DraftPROpen convoy(s) without handoff synthesis: %v",
			len(half), half)
	}

	// (4) Mid-update binary check.
	if err := checkBinarySHADrift(db); err != nil {
		// Drift detection is informational; treat the error itself as
		// "couldn't determine" rather than fatal.
		log.Printf("[BOOT-SWEEP] binary SHA drift check: %v", err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("runBootSweep: %s", strings.Join(errs, "; "))
	}
	return nil
}

// clearStaleDogHeartbeats clears Dogs.heartbeat_at where the recorded
// timestamp is older than `threshold`. Returns the number of rows
// updated. A dog that was running when the daemon vanished should be
// re-scheduled fresh, not show up as "still alive" in the banner.
func clearStaleDogHeartbeats(db *sql.DB, threshold time.Duration) (int, error) {
	cutoff := fmt.Sprintf("-%d seconds", int(threshold.Seconds()))
	res, err := db.Exec(`UPDATE Dogs
		SET heartbeat_at = ''
		WHERE heartbeat_at != ''
		  AND heartbeat_at < datetime('now', ?)`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// findHalfBakedDraftPROpenConvoys returns convoy ids whose draft_pr_url
// is populated but no PRHandoffSyntheses row exists. These are the
// "operator never saw the mail" cases — the convoy reached DraftPROpen
// state but the synthesis-handoff was never posted (daemon vanished
// between draft-PR creation and handoff post).
//
// The query uses a NOT EXISTS subquery so we don't fabricate a join
// when the PRHandoffSyntheses table is empty.
func findHalfBakedDraftPROpenConvoys(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT c.id FROM Convoys c
		WHERE c.status = 'DraftPROpen'
		  AND IFNULL(c.draft_pr_url, '') != ''
		  AND NOT EXISTS (SELECT 1 FROM PRHandoffSyntheses h WHERE h.convoy_id = c.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			log.Printf("findHalfBakedDraftPROpenConvoys: scan: %v", scanErr)
			continue
		}
		out = append(out, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return out, rerr
	}
	return out, nil
}

// checkBinarySHADrift compares the current binary's SHA against the
// most recent DaemonUpdateHistory row with outcome='success'. A
// mismatch means the live binary doesn't match what we last recorded
// as a successful update — e.g. the operator hand-rolled the binary
// without going through `force daemon update`, or an update succeeded
// at the file level but failed to record (and the operator restored
// .previous manually). Logged as a warning so the operator can sync
// the trust file.
func checkBinarySHADrift(db *sql.DB) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	live, err := trust.HashFile(exe)
	if err != nil {
		return fmt.Errorf("hash live binary: %w", err)
	}
	var lastNew string
	row := db.QueryRow(`SELECT new_binary_sha FROM DaemonUpdateHistory
		WHERE outcome = 'success'
		ORDER BY id DESC LIMIT 1`)
	switch err := row.Scan(&lastNew); err {
	case nil:
		// fall through
	case sql.ErrNoRows:
		// No update history yet — fresh install or never updated through
		// the gate. Not an error.
		return nil
	default:
		return fmt.Errorf("query: %w", err)
	}
	if !strings.EqualFold(live, lastNew) {
		log.Printf("[BOOT-SWEEP] WARNING: live binary SHA (%s) does not match the most recent successful DaemonUpdateHistory entry (%s) — check trust file or run `force daemon history`",
			live, lastNew)
	}
	return nil
}

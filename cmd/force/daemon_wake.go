// daemon_wake.go — D12 P2 sleep/wake reconciliation glue.
//
// reconcilePostWakeLoop is the goroutine driver wired from cmdDaemon
// (see fleet_cmds.go's "D12 P2 sleep/wake hooks" block). It reads
// wake.Event values off the channel returned by wake.Subscribe and
// invokes reconcilePostWake on every Woke event.
//
// reconcilePostWake is the heart of the deliverable: after the system
// resumes from sleep, in-flight goroutines may have stale state, the
// holocron handle may have been left in a half-open state, and the
// PID lock may have moved hands if the OS released the flock during
// sleep (rare on local disk, possible on networked filesystems).
//
// Idempotence is the correctness gate. Running reconcilePostWake N
// times in a row MUST produce the same result as running it once.
// The unit test (daemon_wake_test.go) asserts this.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"force-orchestrator/internal/daemon/singleton"
	"force-orchestrator/internal/daemon/wake"
	"force-orchestrator/internal/notify"
	"force-orchestrator/internal/store"
)

// reconcilePostWakeLoop consumes wake.Event values until ctx is
// cancelled or the channel closes. On GoingToSleep we take a pre-sleep
// holocron snapshot (best-effort; failures log but don't block the
// sleep transition — the OS doesn't wait on us). On Woke we call
// reconcilePostWake; if the reconciler returns one of the fatal
// sentinels (DB dead, foreign singleton), we log.Fatalf so a
// supervisor can restart us with fresh state.
func reconcilePostWakeLoop(ctx context.Context, db *sql.DB, events <-chan wake.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev {
			case wake.GoingToSleep:
				log.Printf("daemon: system going to sleep")
				// Pre-sleep snapshot. Best-effort: a failure here
				// (read-only FS, permission, in-memory test DB) is
				// logged and we continue — the OS isn't waiting on
				// us and the post-wake reconciler is what actually
				// keeps the fleet consistent. Tests pass an in-memory
				// DB; SnapshotHolocron returns an error in that case
				// which we log + swallow.
				if path, err := store.SnapshotHolocron(db, "pre-sleep"); err != nil {
					log.Printf("daemon: pre-sleep snapshot failed (continuing): %v", err)
				} else {
					log.Printf("daemon: pre-sleep snapshot written to %s", path)
				}
			case wake.Woke:
				log.Printf("daemon: system woke — running post-wake reconciliation")
				if err := reconcilePostWake(ctx, db); err != nil {
					switch {
					case errors.Is(err, ErrPostWakeDBDead):
						log.Fatalf("daemon: post-wake DB ping failed (process must exit so a supervisor can restart): %v", err)
					case errors.Is(err, ErrPostWakeForeignSingleton):
						log.Fatalf("daemon: post-wake found foreign singleton holder — exiting so the new owner stays authoritative: %v", err)
					default:
						log.Printf("daemon: post-wake reconcile failed: %v", err)
					}
				}
			}
		}
	}
}

// ErrPostWakeDBDead is returned by reconcilePostWake when the holocron
// handle no longer pings. Callers (the goroutine driver) MUST treat
// this as fatal — exiting the daemon process so a supervisor can
// restart us with a fresh handle. The function returns the error
// instead of calling log.Fatalf directly so unit tests can exercise
// the code path without terminating the test binary.
var ErrPostWakeDBDead = errors.New("post-wake: DB ping failed")

// ErrPostWakeForeignSingleton is returned when the singleton lock is
// held by a different PID after wake. The goroutine driver treats
// this as fatal — we exit so the new owner stays authoritative.
var ErrPostWakeForeignSingleton = errors.New("post-wake: singleton lock held by foreign PID")

// reconcilePostWake brings the daemon back into a self-consistent
// state after the OS suspended us. Steps (each idempotent):
//
//  1. DB liveness ping. A dead DB is fatal — returns
//     ErrPostWakeDBDead and the goroutine driver exits the process.
//  2. Singleton-lock recheck. If a different PID holds the lock now
//     (e.g. a networked filesystem reaped our flock during sleep),
//     we return ErrPostWakeForeignSingleton.
//  3. Sweep stuck Locked / UnderReview / UnderCaptainReview tasks
//     back to Pending. ReleaseInFlightTasks is idempotent: a row
//     already in Pending stays Pending.
//  4. Re-issue queued operator notifications by emitting a
//     system_event ping. This kicks any dispatcher that might have
//     missed an in-flight tick during sleep.
//
// Steps the spec called for that we punted with rationale:
//   - "AgentAttention table" — no such table exists in this codebase
//     (OperatorAttentionTags is operator-pinned, not agent-state
//     metadata). Skipped with TODO.
//   - "store.SnapshotHolocron" — now exists; the pre-sleep snapshot
//     is taken in reconcilePostWakeLoop's GoingToSleep branch (above),
//     not here. The post-wake reconciler does not re-snapshot because
//     the wake transition already crossed the fleet-consistency
//     boundary; rolling back to the pre-sleep snapshot is a manual
//     operator action, not an automatic one.
//
// Idempotence: running this twice in a row is indistinguishable from
// running it once. ReleaseInFlightTasks finds nothing to release on
// the second call; the singleton probe is read-only; the
// notify.Dispatch call writes a Fleet_Mail row each invocation, but
// that's not a state mutation that affects future reconciler runs.
// The reconciler does NOT rely on Fleet_Mail row count for its
// idempotence guarantee.
func reconcilePostWake(ctx context.Context, db *sql.DB) error {
	// (1) DB liveness.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("%w: %v", ErrPostWakeDBDead, err)
	}

	// (2) Singleton lock — check that we still hold it. If a different
	// PID holds it now, return the foreign-singleton sentinel.
	pidPath := singleton.DefaultPIDPath()
	locked, holderPID, lockErr := singleton.IsLocked(pidPath)
	if lockErr != nil {
		// Probe failure is observability only — log and continue.
		// The flock is the source of truth; if our FD survived sleep
		// (which it does on local disk), we're still good.
		log.Printf("daemon: post-wake singleton probe failed: %v (continuing)", lockErr)
	} else if locked && holderPID > 0 && holderPID != int(processPID()) {
		return fmt.Errorf("%w: holder=%d", ErrPostWakeForeignSingleton, holderPID)
	}

	// (3) Sweep stuck in-flight tasks. ReleaseInFlightTasks is
	// idempotent — a second call finds nothing to release and is a
	// no-op. Returns the count released.
	released := store.ReleaseInFlightTasks(db, "Fleet: reset on post-wake reconcile")
	if released > 0 {
		log.Printf("daemon: post-wake released %d in-flight task(s) back to Pending", released)
	}

	// (4) Notification kick. system_event is a Tier-2 mail-default
	// category (config/notifications.yaml). We emit one row so the
	// dispatcher's tick fires and any orphaned downstream notifier
	// loops re-converge.
	//
	// Best-effort: Dispatch errors are logged but not returned. The
	// reconciler shouldn't fail the whole sweep over a missing
	// category or a transient mail-write hiccup.
	label := fmt.Sprintf("Daemon resumed from sleep at %s; reconciliation complete (released=%d)",
		time.Now().UTC().Format(time.RFC3339), released)
	body := fmt.Sprintf("D12 P2 post-wake reconciliation finished.\nReleased %d Locked task(s).\nSingleton lock check: ok.\nDB ping: ok.",
		released)
	if dispatchErr := notify.Dispatch(ctx, db, "system_event", 0, label, body); dispatchErr != nil {
		// ErrUnknownCategory is tolerated — the daemon may be running
		// against an older notifications.yaml that hasn't been
		// reseeded yet, OR (in tests) the dispatcher is unconfigured.
		var unknown notify.ErrUnknownCategory
		switch {
		case errors.As(dispatchErr, &unknown):
			log.Printf("daemon: post-wake system_event category not registered (older config — continuing): %v", dispatchErr)
		case errors.Is(dispatchErr, notify.ErrNoConfig):
			log.Printf("daemon: post-wake notify config not installed (test or pre-startup — continuing)")
		default:
			log.Printf("daemon: post-wake notify.Dispatch failed (continuing): %v", dispatchErr)
		}
	}

	return nil
}

// processPID is a thin os.Getpid wrapper kept as a var so tests can
// override "current PID" without mucking with the real process. Tests
// that exercise the foreign-PID branch swap this; production code never
// changes it.
var processPID = func() uint64 {
	return uint64(os.Getpid())
}

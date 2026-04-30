package engineering_corps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/clients/metrics"
	"force-orchestrator/internal/store"
)

// ── Phase 1 stub guard ───────────────────────────────────────────────

// ErrNotImplemented is returned by every handler stub the dispatcher
// routes to in Phase 1. Phase 3 sub-agent A replaces these with real
// handler bodies. Tests assert each handler returns this error so
// regression catches a sub-agent silently dropping a task type.
var ErrNotImplemented = errors.New("engineering_corps: handler not implemented (Phase 1 stub)")

// ── Configuration ────────────────────────────────────────────────────

// EngineeringCorpsConfig wires the dependencies the corps needs.
//
// Cross-agent service boundaries route through Client interfaces only
// (CLAUDE.md cross-agent service interface invariant). Concrete struct
// types from the clients packages are forbidden; constructor injection
// of the Client interface is the only legal shape.
type EngineeringCorpsConfig struct {
	// Name is the agent name used for ClaimBounty owner stamping and
	// log prefixing. Empty defaults to "EngineeringCorps-1".
	Name string

	// DB is the holocron handle. Required.
	DB *sql.DB

	// Librarian is the in-process Librarian client. Required for the
	// ExperimentAuthor handler that consumes candidate proposals (the
	// Phase 3 sub-agent B handoff plumbing).
	Librarian librarian.Client

	// Metrics is the in-process metrics client. Used by the
	// MetricAuthor handler to register new metric versions.
	Metrics metrics.Client
}

// validate returns nil iff the config has every required dependency.
// Called once at Spawn time; missing deps fail closed (the Spawn loop
// returns immediately rather than spinning on nil pointer panics).
func (c EngineeringCorpsConfig) validate() error {
	if c.DB == nil {
		return fmt.Errorf("engineering_corps: DB is required")
	}
	if c.Librarian == nil {
		return fmt.Errorf("engineering_corps: Librarian client is required")
	}
	if c.Metrics == nil {
		return fmt.Errorf("engineering_corps: Metrics client is required")
	}
	return nil
}

// ── Spawn loop ───────────────────────────────────────────────────────

// SpawnEngineeringCorps runs the EC claim loop. Mirrors Diplomat's
// shape (separate goroutine, separate queue, claims multiple task
// types in a fixed priority order).
//
// ctx is the daemon's cancellation context (CLAUDE.md
// daemon-context-threading invariant). On SIGINT/SIGTERM, cmdDaemon
// cancels ctx BEFORE the drain loop so this function exits and
// ReleaseInFlightTasks can sweep claimed-but-uncompleted rows.
//
// Honors IsEstopped + SpendCapExceeded at the top of every iteration
// (CLAUDE.md throttle conventions); both are cheap reads against
// SystemConfig.
func SpawnEngineeringCorps(ctx context.Context, cfg EngineeringCorpsConfig) {
	name := cfg.Name
	if name == "" {
		name = "EngineeringCorps-1"
	}
	logger := agents.NewLogger(name)

	if err := cfg.validate(); err != nil {
		logger.Printf("EngineeringCorps %s cannot start: %v", name, err)
		return
	}

	// Pattern P13: capability profile is sourced from YAML, never a
	// hardcoded literal. LoadProfile fails closed on missing file,
	// unknown tool, or blocklisted grant — the agent cannot start.
	profile, err := capabilities.LoadProfile("engineering-corps")
	if err != nil {
		logger.Printf("EngineeringCorps %s cannot start: capability profile load failed: %v", name, err)
		return
	}

	logger.Printf("EngineeringCorps %s coming online", name)

	for {
		// ctx-cancel check: ordered FIRST so a cancelled context
		// short-circuits the e-stop / spend-cap reads. Mirrors
		// Diplomat's shape exactly.
		if ctx.Err() != nil {
			logger.Printf("EngineeringCorps %s exiting: %v", name, ctx.Err())
			return
		}
		if agents.IsEstopped(cfg.DB) {
			time.Sleep(5 * time.Second)
			continue
		}
		if agents.SpendCapExceeded(cfg.DB) {
			time.Sleep(10 * time.Second)
			continue
		}

		if dispatched := claimAndDispatch(ctx, cfg, profile, name, logger); dispatched {
			continue
		}

		// No claimable work — back off with jitter so multiple EC
		// goroutines (in the unlikely future where the operator runs
		// >1) don't lockstep against the DB.
		time.Sleep(time.Duration(3000+rand.Intn(1000)) * time.Millisecond)
	}
}

// ── Dispatcher ───────────────────────────────────────────────────────

// claimAndDispatch attempts to claim each EC task type in AllTaskTypes
// order; returns true on the first claim. The fixed priority order
// keeps the loop deterministic — sub-agent A's handlers can assume a
// stable claim order when reasoning about backpressure.
func claimAndDispatch(
	ctx context.Context,
	cfg EngineeringCorpsConfig,
	profile *capabilities.Profile,
	name string,
	logger *log.Logger,
) bool {
	for _, taskType := range AllTaskTypes {
		bounty, claimed := store.ClaimBounty(cfg.DB, taskType, name)
		if !claimed {
			continue
		}
		dispatch(ctx, cfg, profile, name, taskType, bounty, logger)
		return true
	}
	return false
}

// dispatch routes a claimed bounty to the handler for its task type.
//
// The default branch routes through handleUnknownTaskType which fails
// the bounty cleanly via store.FailBounty (CLAUDE.md no-silent-failures
// invariant + the Captain pattern P12 fail-closed-on-unknown-decision
// shape). An unknown type can only land here via direct DB write or a
// regression that adds a const to AllTaskTypes without adding a switch
// case — both should surface to the operator, not silently no-op.
func dispatch(
	ctx context.Context,
	cfg EngineeringCorpsConfig,
	profile *capabilities.Profile,
	agentName string,
	taskType string,
	bounty *store.Bounty,
	logger *log.Logger,
) {
	var err error
	switch taskType {
	case TaskTypeExperimentAuthor:
		err = handleExperimentAuthor(ctx, cfg, profile, agentName, bounty, logger)
	case TaskTypeExperimentMonitor:
		err = handleExperimentMonitor(ctx, cfg, profile, agentName, bounty, logger)
	case TaskTypePromotionAuthor:
		err = handlePromotionAuthor(ctx, cfg, profile, agentName, bounty, logger)
	case TaskTypeDemotionAuthor:
		err = handleDemotionAuthor(ctx, cfg, profile, agentName, bounty, logger)
	case TaskTypeMetricAuthor:
		err = handleMetricAuthor(ctx, cfg, profile, agentName, bounty, logger)
	case TaskTypeHoldoutMonitor:
		err = handleHoldoutMonitor(ctx, cfg, profile, agentName, bounty, logger)
	default:
		handleUnknownTaskType(cfg.DB, agentName, taskType, bounty, logger)
		return
	}
	if err != nil {
		// Phase 1 stub: Phase 3 sub-agent A replaces this with the
		// real handleInfraFailure-driven retry shape. For the skeleton
		// we surface ErrNotImplemented as a clean failure so the
		// claim loop releases the row and operator mail surfaces the
		// missing handler. This is safe to keep visible — the
		// dispatcher_test.go fixture seeds rows just to exercise
		// routing; production never enqueues an EC task until sub-
		// agent A's handlers land.
		failBountyOrLog(cfg.DB, bounty.ID, fmt.Sprintf("EC %s handler stub: %v", taskType, err), logger)
	}
}

// handleUnknownTaskType is the dispatcher default branch. An unknown
// taskType can only reach here via direct DB write or a stale const —
// either way the row is failed and the operator surfaces the failure
// via the standard FailBounty path.
func handleUnknownTaskType(
	db *sql.DB,
	agentName string,
	taskType string,
	bounty *store.Bounty,
	logger *log.Logger,
) {
	msg := fmt.Sprintf("engineering_corps: unknown task type %q (bounty #%d) — refusing to no-op", taskType, bounty.ID)
	logger.Printf("%s [%s]", msg, agentName)
	failBountyOrLog(db, bounty.ID, msg, logger)
}

// failBountyOrLog wraps store.FailBounty so the no-silent-failures
// invariant is honored even when FailBounty itself errors. The
// stale-lock detector eventually sweeps a row that stuck in Locked
// after this path runs.
func failBountyOrLog(db *sql.DB, bountyID int, msg string, logger *log.Logger) {
	if err := store.FailBounty(db, bountyID, msg); err != nil {
		logger.Printf("engineering_corps: FailBounty(#%d) failed: %v — stale-lock detector will recover", bountyID, err)
	}
}

// ── Handler stubs ────────────────────────────────────────────────────
//
// Each handler lives in its own file (handler_<task>.go) — Phase 3
// sub-agent A fills in the bodies. The stub bodies below return
// ErrNotImplemented so dispatcher_test.go can verify routing without
// requiring real LLM plumbing.
//
// Phase 1 keeps the stubs INLINE here (rather than in separate files)
// so sub-agent A can replace each one in a single commit. Sub-agent A
// is expected to MOVE each handler into its own file as it lands.

func handleExperimentAuthor(
	_ context.Context,
	_ EngineeringCorpsConfig,
	_ *capabilities.Profile,
	_ string,
	_ *store.Bounty,
	_ *log.Logger,
) error {
	return ErrNotImplemented
}

// handleExperimentMonitor lives in experiment_monitor.go.

// handlePromotionAuthor lives in promotion_author.go.

// handleDemotionAuthor lives in demotion_author.go.

func handleMetricAuthor(
	_ context.Context,
	_ EngineeringCorpsConfig,
	_ *capabilities.Profile,
	_ string,
	_ *store.Bounty,
	_ *log.Logger,
) error {
	return ErrNotImplemented
}

// handleHoldoutMonitor lives in holdout_monitor.go.

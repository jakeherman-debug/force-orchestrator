package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/daemon/provenance"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

// Build-time provenance — injected by the Makefile via -ldflags.
// See `make build` and provenance.Set wiring below. A binary built
// outside the Makefile keeps the "unknown" defaults; `force version`
// and `force daemon status` surface those as a hint to the operator
// that the binary's history is unverified.
var (
	GitSHA    = "unknown"
	BuildTime = "unknown"
	GitBranch = "unknown"
)

func init() {
	// Mirror the -ldflags-injected vars into the provenance package
	// so non-main code (dashboard /api/version, daemon status, etc.)
	// can read them without importing main.
	provenance.Set(GitSHA, BuildTime, GitBranch)
}

func mustParseID(s string) int {
	id, err := strconv.Atoi(s)
	if err != nil {
		fmt.Printf("Invalid ID: %s\n", s)
		os.Exit(1)
	}
	return id
}

func main() {
	command := "status"
	if len(os.Args) >= 2 {
		command = os.Args[1]
	}

	// Fix #8e: top-level CLI ctx cancelled on SIGINT/SIGTERM so subprocess
	// invocations launched by force subcommands (foreground astromech, dog
	// runs, etc.) terminate cleanly instead of running to their fabricated
	// timeout. The daemon command has its own deeper ctx wiring that takes
	// precedence; this is the catch-all for one-shot CLI commands.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db := store.InitHolocron()
	defer db.Close()
	telemetry.InitTelemetry()

	// D1 T0-2: wire the inbound-redact alerter to the live DB so the
	// claude-package wrappers can record redaction events and emit
	// throttled operator mail when the threshold is crossed. Tests
	// that exercise ScrubInbound in isolation pass nil to disable
	// alerting.
	claude.SetInboundRedactDB(db)

	switch command {
	case "help", "--help", "-h":
		printUsage()

	case "version", "--version", "-v":
		fmt.Println("force-orchestrator — Galactic Fleet Command System")
		fmt.Printf("Built with %s\n", runtime.Version())
		// D12 P1 — provenance from -ldflags. "unknown" means the binary
		// was not built via `make build`; operators should rebuild via
		// the Makefile so trust-file entries are auditable.
		fmt.Printf("git-sha    %s\n", GitSHA)
		fmt.Printf("git-branch %s\n", GitBranch)
		fmt.Printf("build-time %s\n", BuildTime)

	case "daemon":
		// D12 P1 — `force daemon <subcommand>` family. Bare `force daemon`
		// (no subcommand) routes through dispatchDaemon and falls back to
		// cmdDaemon (legacy foreground) so backwards compat is preserved.
		dispatchDaemon(db, os.Args[2:])

	case "add":
		cmdAdd(db, os.Args[2:])

	case "add-task":
		fmt.Fprintf(os.Stderr, "error: 'force add-task' has been removed.\nAll code changes now flow through Commander → Chancellor for conflict review.\nUse: force add --type Feature <description>\n  Or omit --type to auto-classify.\n")
		os.Exit(1)

	case "investigate", "add-investigate":
		cmdAddInvestigate(db, os.Args[2:])

	case "scan", "add-audit":
		cmdAddAudit(db, os.Args[2:])

	case "add-jira":
		cmdAddJira(db, os.Args[2:])

	case "repos":
		cmdRepos(db, os.Args[2:])

	case "repo":
		cmdRepo(db, os.Args[2:],
			func(a []string) { cmdRepoSync(ctx, db, a) },
			func(a []string) { cmdRepoSetPRFlow(db, a) })

	case "migrate":
		cmdMigrate(ctx, db, os.Args[2:])

	case "add-repo":
		cmdAddRepo(db, os.Args[2:])

	case "add-repos":
		cmdAddRepos(db, os.Args[2:])

	case "reset":
		cmdReset(db, "reset", "manual reset via CLI", os.Args[2:])

	case "retry":
		cmdReset(db, "retry", "retry via CLI", os.Args[2:])

	case "cancel":
		cmdCancel(db, os.Args[2:])

	case "block":
		cmdBlock(db, os.Args[2:])

	case "unblock":
		cmdUnblock(db, os.Args[2:])

	case "unblock-dependents":
		cmdUnblockDependents(db, os.Args[2:])

	case "tree":
		cmdTree(db, os.Args[2:])

	case "diff":
		cmdDiff(ctx, db, os.Args[2:])

	case "approve":
		// Operator manually approves a task, bypassing the Jedi Council
		cmdApproveTask(ctx, db, os.Args[2:])

	case "reject":
		// Operator manually rejects a task, sending it back with feedback
		cmdRejectTask(db, os.Args[2:])

	case "prioritize":
		cmdPrioritize(db, os.Args[2:])

	case "retry-all-failed":
		cmdRetryAllFailed(db, os.Args[2:])

	case "list":
		cmdList(db, os.Args[2:])

	case "logs":
		cmdLogs(db, os.Args[2:])

	case "history":
		cmdHistory(db, os.Args[2:])

	case "agents":
		cmdAgents(db, os.Args[2:])

	case "status":
		cmdStatus(db, os.Args[2:])

	case "who":
		cmdWho(db, os.Args[2:])

	case "stats":
		cmdStats(db, os.Args[2:])

	case "logs-fleet":
		cmdLogsFleet(db, os.Args[2:])

	case "tail":
		cmdTailTask(db, os.Args[2:])

	case "holonet":
		cmdHolonet(db, os.Args[2:])

	case "export":
		cmdExport(db, os.Args[2:])

	case "import":
		cmdImport(db, os.Args[2:])

	case "search":
		cmdSearch(db, os.Args[2:])

	case "audit":
		cmdAudit(db, os.Args[2:])

	case "prune":
		cmdPrune(db, os.Args[2:])

	case "purge":
		cmdPurge(ctx, db, os.Args[2:])

	case "hard-reset":
		cmdHardReset(ctx, db, os.Args[2:])

	case "scale":
		cmdScale(db, os.Args[2:])

	case "estop":
		cmdEstop(db, os.Args[2:])

	case "resume":
		cmdResume(db, os.Args[2:])

	case "escalations":
		cmdEscalations(db, os.Args[2:])

	case "dogs":
		cmdDogs(ctx, db, os.Args[2:])

	case "cleanup":
		cmdCleanup(ctx, db, os.Args[2:])

	case "doctor":
		cmdDoctor(db, os.Args[2:])

	case "leaderboard":
		cmdLeaderboard(db, os.Args[2:])

	case "costs":
		cmdCosts(db, os.Args[2:])

	case "notifications":
		// D3 P6A.4 — `force notifications budget ...` parity with PUT
		// /api/notifications/budgets/:source/:channel.
		os.Exit(cmdNotifications(db, os.Args[2:]))

	case "session":
		os.Exit(cmdSession(db, os.Args[2:]))

	case "trust":
		os.Exit(cmdTrust(db, os.Args[2:]))

	case "attention":
		os.Exit(cmdAttention(db, os.Args[2:]))

	case "cooldown":
		os.Exit(cmdCooldown(db, os.Args[2:]))

	case "decide":
		os.Exit(cmdDecide(db, os.Args[2:]))

	case "briefing-reject":
		os.Exit(cmdReject(db, os.Args[2:]))

	case "learning":
		// D3 P6B.12 — fleet learning panel CLI parity
		// (refresh / show) for /api/reflection/learning.
		os.Exit(cmdLearning(db, os.Args[2:]))

	case "annotate":
		// D3 P6B.8 — operator annotation CLI parity.
		os.Exit(cmdAnnotate(db, os.Args[2:]))

	case "replay":
		// D3 P6B.7 — drill replay CLI parity.
		os.Exit(cmdReplay(db, os.Args[2:]))

	case "ask":
		// D3 P6B.10 — Ask `/` shortcut CLI parity.
		os.Exit(cmdAsk(db, os.Args[2:]))

	case "retro":
		// D3 P6B.13 — 5-min retro CLI parity.
		os.Exit(cmdRetro(db, os.Args[2:]))

	case "proposed-features":
		// D3 fix-loop-1 β2 — ProposedFeatures CLI parity (Pattern P25)
		// for /api/proposed-features endpoints. Subcommands:
		// list / suppress / score / promote.
		os.Exit(cmdProposedFeatures(db, os.Args[2:]))

	case "dashboard":
		// `force dashboard status` reads the latest heartbeat (handled
		// inside cmdDashboard) and exits 0/1. `force dashboard [--port N]`
		// starts the server.
		cmdDashboard(db, os.Args[2:])

	case "watch":
		cmdWatch(db, os.Args[2:])

	case "run":
		cmdRunForeground(db, os.Args[2:], func(id int) {
			agents.RunTaskForeground(ctx, db, id)
		})

	case "bounty":
		cmdBounty(db, os.Args[2:])

	case "convoy":
		cmdConvoy(db, os.Args[2:])

	case "config":
		cmdConfig(db, os.Args[2:])

	case "memories":
		cmdMemories(db, os.Args[2:])

	case "directive":
		agents.CmdDirective(os.Args[2:])

	case "mail":
		cmdMail(db, os.Args[2:])

	case "task":
		cmdTask(db, os.Args[2:])

	case "render-rules":
		cmdRenderRules(ctx, db, os.Args[2:])

	case "onboard":
		cmdOnboard(ctx, db, os.Args[2:])

	case "experiment":
		cmdExperiment(ctx, db, os.Args[2:])

	case "ec":
		cmdEC(ctx, db, os.Args[2:])

	case "install-sleep-hook":
		os.Exit(cmdInstallSleepHook(ctx, db, os.Args[2:]))

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\nRun 'force help' for usage.\n", command)
		os.Exit(1)
	}
}

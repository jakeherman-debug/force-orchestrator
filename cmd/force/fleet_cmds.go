package main

import (
	"log"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"path/filepath"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/agents/engineering_corps"
	"force-orchestrator/internal/analysis"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/clients/metrics"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/holdout"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/treatments"
)

// readDaemonPID checks if the PID in fleet.pid refers to a running process.
// Returns (pid, true) if alive, (pid, false) if stale/missing.
func readDaemonPID() (int, bool) {
	data, err := os.ReadFile("fleet.pid")
	if err != nil {
		return 0, false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 0 {
		return 0, false
	}
	proc, procErr := os.FindProcess(pid)
	if procErr != nil {
		return pid, false
	}
	return pid, proc.Signal(syscall.Signal(0)) == nil
}

func cmdDaemon(db *sql.DB) {
	// Prevent double-daemon: write PID file, but verify if the existing one is still alive
	pidFile := "fleet.pid"
	if existing, err := os.ReadFile(pidFile); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(existing)))
		if pid > 0 {
			proc, procErr := os.FindProcess(pid)
			if procErr == nil && proc.Signal(syscall.Signal(0)) == nil {
				fmt.Printf("Daemon already running (PID %d). Run 'force estop' to halt agents.\n", pid)
				os.Exit(1)
			}
		}
		fmt.Printf("Stale fleet.pid found (PID %s) — previous daemon appears dead, restarting.\n",
			strings.TrimSpace(string(existing)))
	}
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
	defer os.Remove(pidFile)

	numAgents := 2
	if n := store.GetConfig(db, "num_astromechs", ""); n != "" {
		fmt.Sscanf(n, "%d", &numAgents)
	}
	if numAgents < 1 {
		numAgents = 1
	}
	numCouncil := 1
	if n := store.GetConfig(db, "num_council", ""); n != "" {
		fmt.Sscanf(n, "%d", &numCouncil)
	}

	numCaptain := 1
	if n := store.GetConfig(db, "num_captain", ""); n != "" {
		fmt.Sscanf(n, "%d", &numCaptain)
	}

	numInvestigators := 1
	if n := store.GetConfig(db, "num_investigators", ""); n != "" {
		fmt.Sscanf(n, "%d", &numInvestigators)
	}
	numAuditors := 1
	if n := store.GetConfig(db, "num_auditors", ""); n != "" {
		fmt.Sscanf(n, "%d", &numAuditors)
	}
	numLibrarians := 1
	if n := store.GetConfig(db, "num_librarians", ""); n != "" {
		fmt.Sscanf(n, "%d", &numLibrarians)
	}
	numCommanders := 3
	if n := store.GetConfig(db, "num_commanders", ""); n != "" {
		fmt.Sscanf(n, "%d", &numCommanders)
	}
	if numCommanders < 1 {
		numCommanders = 1
	}

	astromechRoster    := []string{"R2-D2", "BB-8", "R5-D4", "K-2SO", "BD-1", "R7-A7", "R4-P17", "D-O", "C1-10P", "R3-S6"}
	councilRoster      := []string{"Council-Yoda", "Council-Mace", "Council-Ki-Adi", "Council-Kit-Fisto", "Council-Shaak-Ti"}
	captainRoster      := []string{"Captain-Rex", "Captain-Wolffe", "Captain-Bly", "Captain-Gree", "Captain-Ponds"}
	investigatorRoster := []string{"Ahsoka-Tano", "Kanan-Jarrus", "Ezra-Bridger", "Hera-Syndulla"}
	auditorRoster      := []string{"IG-11", "Zeb-Orrelios", "Sabine-Wren", "Chopper"}
	librarianRoster    := []string{"Jocasta-Nu", "Huyang", "Dexter-Jettster"}
	// Commander roster: disjoint from Captain (previously both had Rex/Wolffe/Bly/Gree,
	// which produced "Captain-Rex" and "Commander-Rex" simultaneously and confused
	// the operator). Current mix is clone commanders who never held captain rank
	// (Cody, Fox, Neyo, Bacara) + Jedi Padawans, keeping the "strategic planner"
	// archetype coherent. Any addition here must not appear in captainRoster or
	// investigatorRoster.
	commanderRoster    := []string{"Commander-Cody", "Commander-Fox", "Commander-Neyo", "Commander-Bacara",
		"Commander-Barriss", "Commander-Cal", "Commander-Depa", "Commander-Caleb"}

	numMedics := 1
	if n := store.GetConfig(db, "num_medics", ""); n != "" {
		fmt.Sscanf(n, "%d", &numMedics)
	}
	medicRoster := []string{"Bacta", "Kolto", "Stim"}

	numPilots := 1
	if n := store.GetConfig(db, "num_pilots", ""); n != "" {
		fmt.Sscanf(n, "%d", &numPilots)
	}
	if numPilots < 1 {
		numPilots = 1
	}
	pilotRoster := []string{"Poe-Dameron", "Wedge-Antilles", "Hera-Pilot"}

	numDiplomats := 1
	if n := store.GetConfig(db, "num_diplomats", ""); n != "" {
		fmt.Sscanf(n, "%d", &numDiplomats)
	}
	if numDiplomats < 1 {
		numDiplomats = 1
	}
	diplomatRoster := []string{"Leia-Organa", "Padme-Amidala", "Bail-Organa"}

	// Recover any Failed convoys whose tasks were manually reset (e.g. via `force reset` or
	// direct DB edits) without going through the normal task-completion path.
	store.RecoverStaleConvoys(db)

	// AUDIT-020 (Fix #1): threaded context for graceful shutdown. Every agent
	// Spawn loop exits cleanly when ctx is cancelled, which happens on
	// SIGINT/SIGTERM BEFORE the drain loop begins. This replaces the prior
	// behaviour where agents kept claiming fresh Pending tasks during the 30s
	// drain window and `claude -p` children orphaned on daemon exit.
	//
	// D3 polish-pass B4: ctx hoisted ABOVE runPRFlowStartup so the
	// preflight ls-remote / get-url ops route through igit.LogAndRun
	// with a real cancellable ctx (Pattern P11 forbids
	// context.Background() in agent code).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// PR-flow preflight + Layer B backfill. Fatal checks abort daemon startup;
	// per-repo failures mark the repo pr_flow_enabled=0 and continue.
	if err := runPRFlowStartup(ctx, db); err != nil {
		fmt.Fprintf(os.Stderr, "Daemon start aborted: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Starting the Fleet Daemon (%d astromech(s), %d captain(s), %d council member(s), %d commander(s), %d investigator(s), %d auditor(s), %d librarian(s), %d medic(s), %d pilot(s))...\n",
		numAgents, numCaptain, numCouncil, numCommanders, numInvestigators, numAuditors, numLibrarians, numMedics, numPilots)

	// D2 T1-0: crash-recovery + reconciliation, in this exact order, BEFORE
	// any agent spawns.
	//
	//   1. ReleaseInFlightTasks moves Locked / UnderReview / UnderCaptainReview
	//      rows back to Pending. The shutdown path runs the same call on a
	//      clean SIGINT, but a daemon that crashed (laptop sleep, kill -9,
	//      power loss) leaves rows wedged in Locked. Without this call the
	//      claim loops would skip those rows forever.
	//   2. ReconcileOnStartup cross-checks every remaining non-terminal row
	//      against actual disk/git state. A non-nil return is fatal — never
	//      proceed with an unreliable fleet view (CLAUDE.md "no silent
	//      failures"; AUDIT-020-class hazard).
	if n := store.ReleaseInFlightTasks(db, "Fleet: reset on daemon startup (crash recovery)"); n > 0 {
		fmt.Printf("Released %d in-flight task(s) from prior daemon (status reset to Pending).\n", n)
	}
	if err := agents.ReconcileOnStartup(ctx, db); err != nil {
		fmt.Fprintf(os.Stderr, "[RECONCILE FATAL] daemon start aborted: %v\n", err)
		os.Exit(1)
	}

	// D3 Phase 1 — populate FleetRules from the audit so per-agent prompt
	// injection (`AssemblePerAgentPrompt`) sees content at runtime.
	// Idempotent: a re-run finds (rule_key, version=1) already present and
	// no-ops. Pass empty string to skip the all-sections-covered check —
	// the daemon's CWD may not match the development tree (a release-
	// installed force binary can run from anywhere); the check is a
	// development guard exercised by `force render-rules` and the test
	// suite, not by every daemon start.
	if n, err := store.BootstrapFleetRules(ctx, db, ""); err != nil {
		fmt.Fprintf(os.Stderr, "[FLEET-RULES BOOTSTRAP] daemon start aborted: %v\n", err)
		os.Exit(1)
	} else if n > 0 {
		fmt.Printf("Seeded %d FleetRules row(s) from the bootstrap audit.\n", n)
	}

	// D3 Phase 2 — register the Bayesian Beta-Binomial analysis
	// framework so experiments authored later in this run can reference
	// it via Experiments.analysis_framework_version. Idempotent: a
	// re-run finds the (version) primary key already present and
	// returns nil. Must run BEFORE the holdout mint and the
	// treatments.Apply live flip — both can reference framework rows.
	if err := analysis.RegisterBayesianBetaBinomial(ctx, db); err != nil {
		fmt.Fprintf(os.Stderr, "[ANALYSIS-FRAMEWORK] daemon start aborted: %v\n", err)
		os.Exit(1)
	}

	// D3 Phase 2 — mint the baseline-2026 global holdout. Idempotent
	// on the UNIQUE name index. Must run AFTER the framework register
	// (so future code that wants to attach a fleet_state_hash from a
	// framework version can do so) and BEFORE the treatments.Apply
	// live flip (so Apply sees the holdout row when it queries
	// membership).
	if _, err := holdout.MintBaseline2026(ctx, db); err != nil {
		fmt.Fprintf(os.Stderr, "[HOLDOUT MINT] daemon start aborted: %v\n", err)
		os.Exit(1)
	}

	// D0-B: Construct the in-process Librarian client once at daemon
	// startup and inject it into every agent that produces WriteMemory
	// bounties (Jedi Council via JediCouncilConfig; Inquisitor → RunDogs
	// via InquisitorConfig). Subsequent deliverables (D1, D3, D8) will
	// add more clients to the same Spawn config structs.
	libClient := librarian.NewInProcess(db)

	// D2 T1-2 — wire the per-agent context-size guard. The DB handle
	// drives SystemConfig reads (per-agent caps) and persists
	// PromptByteAttribution rows; the summarizer is the librarian's
	// SummarizeForContextOverflow closure. Both must be installed
	// BEFORE any agent Spawn so the very first claim loop sees the
	// guard active.
	claude.SetContextSizeDB(db)
	claude.SetSummarizer(libClient.SummarizeForContextOverflow)

	// D3 P6B.1 — wire LLMCallTranscripts capture. Every Claude CLI
	// invocation routed through claude.CallWithTranscript* now records
	// a redacted row into LLMCallTranscripts. Forward-going code uses
	// the wrapper; the migration of pre-6B direct call sites is
	// recorded as a backlog in Pattern P31's allowlist.
	claude.SetTranscriptDB(db)

	// D3 P6B.2 — wire GitOperationLog capture. Every git/gh op routed
	// through internal/git's helpers (runGitCtx, runGitCtxOutput,
	// bestEffortRun) now records a redacted row in GitOperationLog
	// for Drill's git-op timeline + free-text search.
	igit.SetOpLogDB(db)

	// D3 Phase 1 — install the log-only treatments.Apply hook.
	// Every Claude CLI invocation now records to TreatmentApplyLog
	// (mode='log_only'). Phase 2 of D3 swaps this for live pass-through.
	claude.SetTreatmentApplyHook(func(hookCtx context.Context, agent string, taskID int) error {
		_, _, err := treatments.Apply(hookCtx, db, treatments.CallDescriptor{
			AgentName:       agent,
			NaturalUnitKind: "task",
			NaturalUnitID:   taskID,
		})
		// Phase 1 fail-open: a write failure to TreatmentApplyLog is
		// observability, not correctness. Log via the daemon's stdout
		// (no operator mail flood) and let the agent's call proceed.
		if err != nil {
			fmt.Fprintf(os.Stderr, "[TREATMENTS-APPLY] %s/task %d: %v\n", agent, taskID, err)
			return nil
		}
		return nil
	})

	go agents.SpawnChancellor(ctx, db)
	for i := 0; i < numCommanders; i++ {
		name := fmt.Sprintf("Commander-%d", i+1)
		if i < len(commanderRoster) {
			name = commanderRoster[i]
		}
		go agents.SpawnCommander(ctx, db, name)
	}
	for i := 0; i < numAgents; i++ {
		name := fmt.Sprintf("Astromech-%d", i+1)
		if i < len(astromechRoster) {
			name = astromechRoster[i]
		}
		go agents.SpawnAstromech(ctx, db, name)
	}
	for i := 0; i < numCaptain; i++ {
		name := fmt.Sprintf("Captain-%d", i+1)
		if i < len(captainRoster) {
			name = captainRoster[i]
		}
		go agents.SpawnCaptain(ctx, db, name)
	}
	for i := 0; i < numCouncil; i++ {
		name := fmt.Sprintf("Council-%d", i+1)
		if i < len(councilRoster) {
			name = councilRoster[i]
		}
		go agents.SpawnJediCouncil(ctx, db, agents.JediCouncilConfig{Name: name, Librarian: libClient})
	}
	for i := 0; i < numInvestigators; i++ {
		name := fmt.Sprintf("Investigator-%d", i+1)
		if i < len(investigatorRoster) {
			name = investigatorRoster[i]
		}
		go agents.SpawnInvestigator(ctx, db, name)
	}
	for i := 0; i < numAuditors; i++ {
		name := fmt.Sprintf("Auditor-%d", i+1)
		if i < len(auditorRoster) {
			name = auditorRoster[i]
		}
		go agents.SpawnAuditor(ctx, db, name)
	}
	for i := 0; i < numLibrarians; i++ {
		name := fmt.Sprintf("Librarian-%d", i+1)
		if i < len(librarianRoster) {
			name = librarianRoster[i]
		}
		go agents.SpawnLibrarian(ctx, db, name)
	}
	for i := 0; i < numMedics; i++ {
		name := fmt.Sprintf("Medic-%d", i+1)
		if i < len(medicRoster) {
			name = medicRoster[i]
		}
		go agents.SpawnMedic(ctx, db, name)
	}
	for i := 0; i < numPilots; i++ {
		name := fmt.Sprintf("Pilot-%d", i+1)
		if i < len(pilotRoster) {
			name = pilotRoster[i]
		}
		go agents.SpawnPilot(ctx, db, name)
	}
	for i := 0; i < numDiplomats; i++ {
		name := fmt.Sprintf("Diplomat-%d", i+1)
		if i < len(diplomatRoster) {
			name = diplomatRoster[i]
		}
		go agents.SpawnDiplomat(ctx, db, name)
	}
	go agents.SpawnInquisitor(ctx, db, agents.InquisitorConfig{Librarian: libClient})

	// D3 Phase 3 — Engineering Corps. Spawned AFTER the review-agent
	// roster is up and AFTER treatments.Apply is wired (above) so the
	// EC claim loop sees the same hook-driven enrollment behaviour
	// every other agent does. Phase 1 ships the skeleton + dispatcher
	// + ErrNotImplemented stubs; sub-agents A/B/C fill in the six
	// task type handlers in subsequent commits.
	go engineering_corps.SpawnEngineeringCorps(ctx, engineering_corps.EngineeringCorpsConfig{
		Name:      "EngineeringCorps-1",
		DB:        db,
		Librarian: libClient,
		Metrics:   metrics.NewInProcess(),
	})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	defer signal.Stop(sigChan)
	spawnedAgents := numAgents
	spawnedCaptains := numCaptain
	spawnedCouncil := numCouncil
	spawnedCommanders := numCommanders
	spawnedInvestigators := numInvestigators
	spawnedAuditors := numAuditors
	spawnedLibrarians := numLibrarians
	spawnedMedics := numMedics
	spawnedPilots := numPilots

	for {
		sig := <-sigChan
		switch sig {
		case syscall.SIGUSR1:
			// Dynamic scale-up: re-read agent counts and spawn any new agents.

			// Astromechs
			newTarget := spawnedAgents
			if n := store.GetConfig(db, "num_astromechs", ""); n != "" {
				fmt.Sscanf(n, "%d", &newTarget)
			}
			if newTarget < 1 {
				newTarget = 1
			}
			for spawnedAgents < newTarget {
				name := fmt.Sprintf("Astromech-%d", spawnedAgents+1)
				if spawnedAgents < len(astromechRoster) {
					name = astromechRoster[spawnedAgents]
				}
				fmt.Printf("Scaling: spawning %s (astromechs: %d → %d)\n", name, spawnedAgents, newTarget)
				go agents.SpawnAstromech(ctx, db, name)
				spawnedAgents++
			}
			if newTarget < spawnedAgents {
				fmt.Printf("Scale-down to %d astromech(s) requested (currently %d running) — takes effect on restart.\n", newTarget, spawnedAgents)
			}

			// Captains
			newCaptains := spawnedCaptains
			if n := store.GetConfig(db, "num_captain", ""); n != "" {
				fmt.Sscanf(n, "%d", &newCaptains)
			}
			if newCaptains < 1 {
				newCaptains = 1
			}
			for spawnedCaptains < newCaptains {
				name := fmt.Sprintf("Captain-%d", spawnedCaptains+1)
				if spawnedCaptains < len(captainRoster) {
					name = captainRoster[spawnedCaptains]
				}
				fmt.Printf("Scaling: spawning %s (captains: %d → %d)\n", name, spawnedCaptains, newCaptains)
				go agents.SpawnCaptain(ctx, db, name)
				spawnedCaptains++
			}
			if newCaptains < spawnedCaptains {
				fmt.Printf("Scale-down to %d captain(s) requested (currently %d running) — takes effect on restart.\n", newCaptains, spawnedCaptains)
			}

			// Council
			newCouncil := spawnedCouncil
			if n := store.GetConfig(db, "num_council", ""); n != "" {
				fmt.Sscanf(n, "%d", &newCouncil)
			}
			if newCouncil < 1 {
				newCouncil = 1
			}
			for spawnedCouncil < newCouncil {
				name := fmt.Sprintf("Council-%d", spawnedCouncil+1)
				if spawnedCouncil < len(councilRoster) {
					name = councilRoster[spawnedCouncil]
				}
				fmt.Printf("Scaling: spawning %s (council: %d → %d)\n", name, spawnedCouncil, newCouncil)
				go agents.SpawnJediCouncil(ctx, db, agents.JediCouncilConfig{Name: name, Librarian: libClient})
				spawnedCouncil++
			}
			if newCouncil < spawnedCouncil {
				fmt.Printf("Scale-down to %d council member(s) requested (currently %d running) — takes effect on restart.\n", newCouncil, spawnedCouncil)
			}

			// Commanders
			newCommanders := spawnedCommanders
			if n := store.GetConfig(db, "num_commanders", ""); n != "" {
				fmt.Sscanf(n, "%d", &newCommanders)
			}
			if newCommanders < 1 {
				newCommanders = 1
			}
			for spawnedCommanders < newCommanders {
				name := fmt.Sprintf("Commander-%d", spawnedCommanders+1)
				if spawnedCommanders < len(commanderRoster) {
					name = commanderRoster[spawnedCommanders]
				}
				fmt.Printf("Scaling: spawning %s (commanders: %d → %d)\n", name, spawnedCommanders, newCommanders)
				go agents.SpawnCommander(ctx, db, name)
				spawnedCommanders++
			}
			if newCommanders < spawnedCommanders {
				fmt.Printf("Scale-down to %d commander(s) requested (currently %d running) — takes effect on restart.\n", newCommanders, spawnedCommanders)
			}

			// Investigators
			newInvestigators := spawnedInvestigators
			if n := store.GetConfig(db, "num_investigators", ""); n != "" {
				fmt.Sscanf(n, "%d", &newInvestigators)
			}
			if newInvestigators < 1 {
				newInvestigators = 1
			}
			for spawnedInvestigators < newInvestigators {
				name := fmt.Sprintf("Investigator-%d", spawnedInvestigators+1)
				if spawnedInvestigators < len(investigatorRoster) {
					name = investigatorRoster[spawnedInvestigators]
				}
				fmt.Printf("Scaling: spawning %s (investigators: %d → %d)\n", name, spawnedInvestigators, newInvestigators)
				go agents.SpawnInvestigator(ctx, db, name)
				spawnedInvestigators++
			}
			if newInvestigators < spawnedInvestigators {
				fmt.Printf("Scale-down to %d investigator(s) requested (currently %d running) — takes effect on restart.\n", newInvestigators, spawnedInvestigators)
			}

			// Auditors
			newAuditors := spawnedAuditors
			if n := store.GetConfig(db, "num_auditors", ""); n != "" {
				fmt.Sscanf(n, "%d", &newAuditors)
			}
			if newAuditors < 1 {
				newAuditors = 1
			}
			for spawnedAuditors < newAuditors {
				name := fmt.Sprintf("Auditor-%d", spawnedAuditors+1)
				if spawnedAuditors < len(auditorRoster) {
					name = auditorRoster[spawnedAuditors]
				}
				fmt.Printf("Scaling: spawning %s (auditors: %d → %d)\n", name, spawnedAuditors, newAuditors)
				go agents.SpawnAuditor(ctx, db, name)
				spawnedAuditors++
			}
			if newAuditors < spawnedAuditors {
				fmt.Printf("Scale-down to %d auditor(s) requested (currently %d running) — takes effect on restart.\n", newAuditors, spawnedAuditors)
			}

			// Librarians
			newLibrarians := spawnedLibrarians
			if n := store.GetConfig(db, "num_librarians", ""); n != "" {
				fmt.Sscanf(n, "%d", &newLibrarians)
			}
			if newLibrarians < 1 {
				newLibrarians = 1
			}
			for spawnedLibrarians < newLibrarians {
				name := fmt.Sprintf("Librarian-%d", spawnedLibrarians+1)
				if spawnedLibrarians < len(librarianRoster) {
					name = librarianRoster[spawnedLibrarians]
				}
				fmt.Printf("Scaling: spawning %s (librarians: %d → %d)\n", name, spawnedLibrarians, newLibrarians)
				go agents.SpawnLibrarian(ctx, db, name)
				spawnedLibrarians++
			}
			if newLibrarians < spawnedLibrarians {
				fmt.Printf("Scale-down to %d librarian(s) requested (currently %d running) — takes effect on restart.\n", newLibrarians, spawnedLibrarians)
			}

			// Medics
			newMedics := spawnedMedics
			if n := store.GetConfig(db, "num_medics", ""); n != "" {
				fmt.Sscanf(n, "%d", &newMedics)
			}
			if newMedics < 1 {
				newMedics = 1
			}
			for spawnedMedics < newMedics {
				name := fmt.Sprintf("Medic-%d", spawnedMedics+1)
				if spawnedMedics < len(medicRoster) {
					name = medicRoster[spawnedMedics]
				}
				fmt.Printf("Scaling: spawning %s (medics: %d → %d)\n", name, spawnedMedics, newMedics)
				go agents.SpawnMedic(ctx, db, name)
				spawnedMedics++
			}
			if newMedics < spawnedMedics {
				fmt.Printf("Scale-down to %d medic(s) requested (currently %d running) — takes effect on restart.\n", newMedics, spawnedMedics)
			}

			// Pilots
			newPilots := spawnedPilots
			if n := store.GetConfig(db, "num_pilots", ""); n != "" {
				fmt.Sscanf(n, "%d", &newPilots)
			}
			if newPilots < 1 {
				newPilots = 1
			}
			for spawnedPilots < newPilots {
				name := fmt.Sprintf("Pilot-%d", spawnedPilots+1)
				if spawnedPilots < len(pilotRoster) {
					name = pilotRoster[spawnedPilots]
				}
				fmt.Printf("Scaling: spawning %s (pilots: %d → %d)\n", name, spawnedPilots, newPilots)
				go agents.SpawnPilot(ctx, db, name)
				spawnedPilots++
			}
			if newPilots < spawnedPilots {
				fmt.Printf("Scale-down to %d pilot(s) requested (currently %d running) — takes effect on restart.\n", newPilots, spawnedPilots)
			}

		default:
			// SIGINT / SIGTERM — cancel context to stop agents claiming new work,
			// then graceful drain, then exit.
			//
			// AUDIT-020 (Fix #1): cancel() is called BEFORE the drain loop, so
			// every Spawn goroutine sees ctx.Err() != nil on its next iteration
			// and returns cleanly. Prior behaviour let agents keep claiming
			// fresh Pending tasks during the 30s drain, which raced the
			// ReleaseInFlightTasks sweep and left orphaned `claude -p`
			// children. Now agent claim loops stop before the sweep runs.
			fmt.Printf("\nReceived %v — cancelling context, draining in-flight tasks (up to 30s)...\n", sig)
			cancel()
			drainDeadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(drainDeadline) {
				var active int
				db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status IN ('Locked', 'UnderCaptainReview', 'UnderReview')`).Scan(&active)
				if active == 0 {
					fmt.Println("All tasks drained cleanly.")
					break
				}
				fmt.Printf("  %d task(s) still running, waiting...\n", active)
				time.Sleep(2 * time.Second)
			}
			if n := store.ReleaseInFlightTasks(db, "Fleet: reset on daemon shutdown"); n > 0 {
				fmt.Printf("Force-released %d in-flight task(s) back to Pending.\n", n)
			}
			fmt.Println("Daemon shut down.")
			os.Exit(0)
		}
	}
}

func cmdScale(db *sql.DB, astromechs, council, captain, commanders, investigators, auditors, librarians int) {
	type update struct {
		key   string
		label string
		val   int
	}
	candidates := []update{
		{"num_astromechs", "astromechs", astromechs},
		{"num_council", "council", council},
		{"num_captain", "captain", captain},
		{"num_commanders", "commanders", commanders},
		{"num_investigators", "investigators", investigators},
		{"num_auditors", "auditors", auditors},
		{"num_librarians", "librarians", librarians},
	}

	var updated []string
	for _, u := range candidates {
		if u.val >= 0 {
			store.SetConfig(db, u.key, strconv.Itoa(u.val))
			updated = append(updated, fmt.Sprintf("%s=%d", u.label, u.val))
		}
	}

	if len(updated) == 0 {
		fmt.Println("Usage: force scale [--astromechs N] [--council N] [--captain N] [--investigators N] [--auditors N]")
		os.Exit(1)
	}

	fmt.Printf("Updated: %s\n", strings.Join(updated, ", "))

	pid, alive := readDaemonPID()
	if !alive {
		fmt.Println("No running daemon found — changes take effect on next start.")
		return
	}

	proc, findErr := os.FindProcess(pid)
	if findErr != nil {
		fmt.Printf("Cannot find daemon process (PID %d).\n", pid)
		return
	}
	if sigErr := proc.Signal(syscall.SIGUSR1); sigErr != nil {
		fmt.Printf("Signal failed: %v\n", sigErr)
	} else {
		fmt.Printf("Signaled daemon (PID %d) — new agents will start shortly.\n", pid)
	}
}

func cmdRepos(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "remove":
		if len(args) < 2 {
			fmt.Println("Usage: force repos remove <name>")
			os.Exit(1)
		}
		repoName := args[1]
		if store.RemoveRepo(db, repoName) {
			fmt.Printf("Repository '%s' removed.\n", repoName)
		} else {
			fmt.Printf("Repository '%s' not found.\n", repoName)
		}
	default:
		// list repos (default)
		rows, err := db.Query(`SELECT name, local_path, description FROM Repositories ORDER BY name`)
		if err != nil {
			fmt.Printf("DB error: %v\n", err)
			os.Exit(1)
		}
		defer rows.Close()
		fmt.Printf("%-20s %-35s %s\n", "NAME", "PATH", "DESCRIPTION")
		fmt.Println(strings.Repeat("-", 90))
		found := false
		for rows.Next() {
			found = true
			var name, path, desc string
			if err := rows.Scan(&name, &path, &desc); err != nil {
				fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
				continue
			}
			exists := ""
			if _, statErr := os.Stat(path); statErr != nil {
				exists = " [PATH MISSING]"
			}
			fmt.Printf("%-20s %-35s %s%s\n", name, truncate(path, 35), truncate(desc, 35), exists)
		}
		if rErr := rows.Err(); rErr != nil {
			log.Printf("fleet_cmds.go:cmdRepos: rows iter error: %v", rErr)
		}
		if !found {
			fmt.Println("No repositories registered. Run: force add-repo <name> <path> <desc>")
		}
	}
}

func cmdAddRepo(db *sql.DB, name, repoRegPath, desc string) {
	// D3 polish-pass iteration 2 (B4r): operator-invoked CLI subcommand.
	// The git probes here run BEFORE the daemon's holocron is wired,
	// but igit.LogAndRun degrades gracefully when no DB is attached.
	ctx := context.Background()
	// Fix #9: validate the path BEFORE any shell call. Leading `-` / `..` /
	// NUL / newline / non-absolute paths all fail here. Absolute form is
	// resolved via filepath.Abs so a caller that passes a relative path
	// from an unknown cwd still gets a meaningful check.
	absPath, absErr := filepath.Abs(repoRegPath)
	if absErr != nil {
		fmt.Printf("Cannot resolve path %q: %v\n", repoRegPath, absErr)
		os.Exit(1)
	}
	if err := igit.ValidateRepoPath(absPath, igit.RepoPathOptions{RejectSymlinks: false}); err != nil {
		fmt.Printf("Invalid repo path: %v\n", err)
		os.Exit(1)
	}
	// Verify the path exists and is a git repository.
	if _, statErr := os.Stat(absPath); statErr != nil {
		fmt.Printf("Path does not exist: %s\n", absPath)
		os.Exit(1)
	}
	// Trailing `--` keeps the arg positional (Fix #9 defence-in-depth;
	// absPath already passed the validator so this is belt-and-suspenders).
	if out, gitErr := igit.LogAndRun(ctx,
		igit.OpContext{Repo: absPath},
		"add-repo-rev-parse",
		"git", "-C", absPath, "rev-parse", "--git-dir", "--",
	); gitErr != nil {
		fmt.Printf("'%s' does not appear to be a git repository: %s\n", absPath, strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	store.AddRepo(db, name, absPath, desc)
	fmt.Printf("Repository '%s' registered at %s\n", name, absPath)

	// Eagerly populate PR-flow fields (remote_url, default_branch) and queue
	// FindPRTemplate so the repo is ready for the PR flow immediately. This
	// saves operators from having to remember to run `force repo sync` after
	// every add. A failure here is non-fatal — the repo is still registered,
	// and the daemon's startup Layer B will retry on next boot.
	if _, statErr := os.Stat(absPath); statErr == nil {
		remote, rErr := igit.LogAndRun(ctx,
			igit.OpContext{Repo: absPath},
			"add-repo-remote-get-url",
			"git", "-C", absPath, "remote", "get-url", "origin",
		)
		remoteURL := strings.TrimSpace(string(remote))
		// Fix #9: validate the URL from `git remote get-url origin` BEFORE
		// persisting. An attacker-crafted remote URL with embedded
		// `--upload-pack=` would otherwise flow through to `gh --repo` etc.
		urlErr := igit.ValidateRemoteURL(remoteURL)
		if rErr != nil || remoteURL == "" || urlErr != nil {
			reason := "no `origin` remote configured"
			if urlErr != nil {
				reason = fmt.Sprintf("origin URL rejected: %v", urlErr)
			}
			fmt.Printf("  (note) %s — PR flow will fall back to legacy local-merge for this repo.\n", reason)
			fmt.Printf("  Fix: `git -C %s remote add origin <url>` then `force repo sync`.\n", absPath)
			if err := store.SetRepoPRFlowEnabled(db, name, false); err != nil {
				fmt.Printf("  (warn) failed to persist pr_flow=false for %s: %v — re-run `force repo set-pr-flow %s off`\n", name, err, name)
			}
		} else {
			// Detect default branch via symbolic-ref, fall back to common names.
			var defaultBranch string
			if out, symErr := igit.LogAndRun(ctx,
				igit.OpContext{Repo: absPath},
				"add-repo-symbolic-ref",
				"git", "-C", absPath, "symbolic-ref", "--short", "--", "refs/remotes/origin/HEAD",
			); symErr == nil {
				parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
				if len(parts) == 2 {
					defaultBranch = parts[1]
				}
			}
			if defaultBranch == "" {
				for _, b := range []string{"main", "master", "develop"} {
					if _, vErr := igit.LogAndRun(ctx,
						igit.OpContext{Repo: absPath},
						"add-repo-verify-branch",
						"git", "-C", absPath, "rev-parse", "--verify", b, "--",
					); vErr == nil {
						defaultBranch = b
						break
					}
				}
			}
			if defaultBranch == "" {
				defaultBranch = "main"
			}
			if err := store.SetRepoRemoteInfo(db, name, remoteURL, defaultBranch); err == nil {
				fmt.Printf("  remote=%s default=%s\n", remoteURL, defaultBranch)
			}
			if _, err := agents.QueueFindPRTemplate(db, name, absPath); err == nil {
				fmt.Printf("  queued FindPRTemplate task to locate the repo's PR template.\n")
			}
		}
	}
}

func cmdEstop(db *sql.DB) {
	agents.SetEstop(db, true)
	telemetry.EmitEvent(telemetry.EventEstop(true))
	store.LogAudit(db, "operator", "estop", 0, "emergency stop activated")
	fmt.Println("E-STOP ACTIVATED. All agents will halt after their current sleep cycle.")
	fmt.Println("Run 'force resume' to re-enable agents.")
}

func cmdResume(db *sql.DB) {
	agents.SetEstop(db, false)
	telemetry.EmitEvent(telemetry.EventEstop(false))
	store.LogAudit(db, "operator", "resume", 0, "emergency stop cleared")
	fmt.Println("E-stop cleared. Agents will resume on their next cycle.")
}

// Fix #8e: ctx threads from main's signal-cancellation ctx.
func cmdCleanup(ctx context.Context, db *sql.DB) {
	runCleanup(ctx, db)
}

func cmdDoctor(db *sql.DB, clean bool) {
	runDoctor(db, clean)
}

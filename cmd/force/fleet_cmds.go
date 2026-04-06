package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
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

	astromechRoster   := []string{"R2-D2", "BB-8", "R5-D4", "K-2SO", "BD-1", "R7-A7", "R4-P17", "D-O", "C1-10P", "R3-S6"}
	councilRoster     := []string{"Council-Yoda", "Council-Mace", "Council-Ki-Adi", "Council-Kit-Fisto", "Council-Shaak-Ti"}
	captainRoster     := []string{"Captain-Rex", "Captain-Wolffe", "Captain-Bly", "Captain-Gree", "Captain-Ponds"}
	investigatorRoster := []string{"Ahsoka-Tano", "Kanan-Jarrus", "Ezra-Bridger", "Hera-Syndulla"}
	auditorRoster     := []string{"IG-11", "Zeb-Orrelios", "Sabine-Wren", "Chopper"}
	librarianRoster   := []string{"Jocasta-Nu", "Huyang", "Dexter-Jettster"}

	numMedics := 1
	if n := store.GetConfig(db, "num_medics", ""); n != "" {
		fmt.Sscanf(n, "%d", &numMedics)
	}
	medicRoster := []string{"Bacta", "Kolto", "Stim"}

	// Recover any Failed convoys whose tasks were manually reset (e.g. via `force reset` or
	// direct DB edits) without going through the normal task-completion path.
	store.RecoverStaleConvoys(db)

	fmt.Printf("Starting the Fleet Daemon (%d astromech(s), %d captain(s), %d council member(s), %d investigator(s), %d auditor(s), %d librarian(s), %d medic(s))...\n",
		numAgents, numCaptain, numCouncil, numInvestigators, numAuditors, numLibrarians, numMedics)
	go agents.SpawnCommander(db)
	for i := 0; i < numAgents; i++ {
		name := fmt.Sprintf("Astromech-%d", i+1)
		if i < len(astromechRoster) {
			name = astromechRoster[i]
		}
		go agents.SpawnAstromech(db, name)
	}
	for i := 0; i < numCaptain; i++ {
		name := fmt.Sprintf("Captain-%d", i+1)
		if i < len(captainRoster) {
			name = captainRoster[i]
		}
		go agents.SpawnCaptain(db, name)
	}
	for i := 0; i < numCouncil; i++ {
		name := fmt.Sprintf("Council-%d", i+1)
		if i < len(councilRoster) {
			name = councilRoster[i]
		}
		go agents.SpawnJediCouncil(db, name)
	}
	for i := 0; i < numInvestigators; i++ {
		name := fmt.Sprintf("Investigator-%d", i+1)
		if i < len(investigatorRoster) {
			name = investigatorRoster[i]
		}
		go agents.SpawnInvestigator(db, name)
	}
	for i := 0; i < numAuditors; i++ {
		name := fmt.Sprintf("Auditor-%d", i+1)
		if i < len(auditorRoster) {
			name = auditorRoster[i]
		}
		go agents.SpawnAuditor(db, name)
	}
	for i := 0; i < numLibrarians; i++ {
		name := fmt.Sprintf("Librarian-%d", i+1)
		if i < len(librarianRoster) {
			name = librarianRoster[i]
		}
		go agents.SpawnLibrarian(db, name)
	}
	for i := 0; i < numMedics; i++ {
		name := fmt.Sprintf("Medic-%d", i+1)
		if i < len(medicRoster) {
			name = medicRoster[i]
		}
		go agents.SpawnMedic(db, name)
	}
	go agents.SpawnInquisitor(db)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	spawnedAgents := numAgents
	spawnedCaptains := numCaptain
	spawnedCouncil := numCouncil
	spawnedInvestigators := numInvestigators
	spawnedAuditors := numAuditors
	spawnedLibrarians := numLibrarians
	spawnedMedics := numMedics

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
				go agents.SpawnAstromech(db, name)
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
				go agents.SpawnCaptain(db, name)
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
				go agents.SpawnJediCouncil(db, name)
				spawnedCouncil++
			}
			if newCouncil < spawnedCouncil {
				fmt.Printf("Scale-down to %d council member(s) requested (currently %d running) — takes effect on restart.\n", newCouncil, spawnedCouncil)
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
				go agents.SpawnInvestigator(db, name)
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
				go agents.SpawnAuditor(db, name)
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
				go agents.SpawnLibrarian(db, name)
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
				go agents.SpawnMedic(db, name)
				spawnedMedics++
			}
			if newMedics < spawnedMedics {
				fmt.Printf("Scale-down to %d medic(s) requested (currently %d running) — takes effect on restart.\n", newMedics, spawnedMedics)
			}

		default:
			// SIGINT / SIGTERM — graceful drain then exit.
			fmt.Printf("\nReceived %v — draining in-flight tasks (up to 30s)...\n", sig)
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

func cmdScale(db *sql.DB, astromechs, council, captain, investigators, auditors, librarians int) {
	type update struct {
		key   string
		label string
		val   int
	}
	candidates := []update{
		{"num_astromechs", "astromechs", astromechs},
		{"num_council", "council", council},
		{"num_captain", "captain", captain},
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
			rows.Scan(&name, &path, &desc)
			exists := ""
			if _, statErr := os.Stat(path); statErr != nil {
				exists = " [PATH MISSING]"
			}
			fmt.Printf("%-20s %-35s %s%s\n", name, truncate(path, 35), truncate(desc, 35), exists)
		}
		if !found {
			fmt.Println("No repositories registered. Run: force add-repo <name> <path> <desc>")
		}
	}
}

func cmdAddRepo(db *sql.DB, name, repoRegPath, desc string) {
	// Verify the path exists and is a git repository
	if _, statErr := os.Stat(repoRegPath); statErr != nil {
		fmt.Printf("Path does not exist: %s\n", repoRegPath)
		os.Exit(1)
	}
	if out, gitErr := exec.Command("git", "-C", repoRegPath, "rev-parse", "--git-dir").CombinedOutput(); gitErr != nil {
		fmt.Printf("'%s' does not appear to be a git repository: %s\n", repoRegPath, strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	store.AddRepo(db, name, repoRegPath, desc)
	fmt.Printf("Repository '%s' registered at %s\n", name, repoRegPath)
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

func cmdCleanup(db *sql.DB) {
	runCleanup(db)
}

func cmdDoctor(db *sql.DB, clean bool) {
	runDoctor(db, clean)
}

package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// ── `force migrate pr-flow [--dry-run] [--rollback]` ─────────────────────────
//
// The PR-flow migration is additive (Layer A ALTER + new table) and backfill-
// driven (Layer B remote_url/default_branch). It runs automatically on daemon
// startup, but operators may want to run it explicitly to see what will change,
// inspect preflight results, or take a manual snapshot.
//
// Snapshot semantics: every invocation that performs work creates a timestamped
// copy of holocron.db first. --rollback picks the most recent snapshot and
// restores it (refuses if the daemon is running).

const prFlowSnapshotPrefix = "holocron.db.pre-pr-flow."

// cmdMigrate dispatches `force migrate <subcommand>`.
func cmdMigrate(db *sql.DB, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: force migrate pr-flow [--dry-run] [--rollback --confirm]")
		os.Exit(1)
	}
	switch args[0] {
	case "pr-flow":
		cmdMigratePRFlow(db, args[1:])
	default:
		fmt.Printf("Unknown migration: %s\n", args[0])
		fmt.Println("Usage: force migrate pr-flow [--dry-run] [--rollback --confirm]")
		os.Exit(1)
	}
}

func cmdMigratePRFlow(db *sql.DB, args []string) {
	var dryRun, rollback, confirm bool
	for _, a := range args {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--rollback":
			rollback = true
		case "--confirm":
			confirm = true
		default:
			fmt.Printf("Unknown flag: %s\n", a)
			os.Exit(1)
		}
	}
	if dryRun && rollback {
		fmt.Println("Error: --dry-run and --rollback are mutually exclusive.")
		os.Exit(1)
	}
	if confirm && !rollback {
		fmt.Println("Error: --confirm is only valid with --rollback (it guards the destructive restore).")
		os.Exit(1)
	}

	if rollback {
		if !confirm {
			// Rollback is destructive — overwrites holocron.db, losing any
			// state changes since the snapshot (escalations, in-flight work,
			// fleet memory, etc.). Refuse without explicit --confirm.
			fmt.Fprintln(os.Stderr, "Error: --rollback is destructive and requires --confirm.")
			fmt.Fprintln(os.Stderr, "It will overwrite holocron.db with the latest pre-migration snapshot,")
			fmt.Fprintln(os.Stderr, "discarding any state changes since that snapshot was taken.")
			fmt.Fprintln(os.Stderr, "Re-run with `force migrate pr-flow --rollback --confirm` to proceed.")
			os.Exit(1)
		}
		runPRFlowRollback(db)
		return
	}

	if dryRun {
		runPRFlowDryRun(db)
		return
	}

	runPRFlowMigrate(db)
}

// runPRFlowStartup is the hook called from cmdDaemon before any agents spawn.
// It ensures schema migrations have run (Layer A, via InitHolocron), does the
// preflight checks (gh auth, per-repo origin), runs Layer B backfill, and
// enqueues FindPRTemplate for repos that still need it. Returns an error only
// when a FATAL preflight fails — per-repo issues disable pr_flow for that repo
// and log a warning but do not abort startup.
func runPRFlowStartup(db *sql.DB) error {
	ghClient := gh.NewClient()
	checks := agents.PRFlowPreflight(db, ghClient)

	var fatalFailures []string
	var perRepoFailures []string
	for _, c := range checks {
		if c.Passed {
			continue
		}
		if c.Fatal {
			fatalFailures = append(fatalFailures, fmt.Sprintf("[%s] %s", c.Name, c.Detail))
			continue
		}
		if c.RepoKey != "" {
			// Non-fatal per-repo failure — disable pr_flow for this repo so it
			// takes the legacy path rather than producing bad PRs.
			_ = store.SetRepoPRFlowEnabled(db, c.RepoKey, false)
			perRepoFailures = append(perRepoFailures, fmt.Sprintf("%s: %s", c.RepoKey, c.Detail))
		}
	}
	if len(fatalFailures) > 0 {
		return fmt.Errorf("fatal preflight failures:\n  - %s", strings.Join(fatalFailures, "\n  - "))
	}

	if summary := agents.BackfillRepoRemoteInfo(db); summary != "" {
		fmt.Printf("[migration] %s\n", summary)
	}
	queued, _ := agents.EnqueueMissingFindPRTemplate(db)
	if queued > 0 {
		fmt.Printf("[migration] enqueued %d FindPRTemplate task(s) (async)\n", queued)
	}
	if len(perRepoFailures) > 0 {
		fmt.Printf("[migration] pr_flow disabled for %d repo(s):\n", len(perRepoFailures))
		for _, f := range perRepoFailures {
			fmt.Printf("  - %s\n", f)
		}
	}

	// Active convoys without ask_branch need Layer C backfill. Phase 1 only
	// reports the count; Phase 2 adds the inquisitor check that queues
	// CreateAskBranch tasks.
	needsBackfill := store.ActiveConvoysMissingAskBranch(db)
	if len(needsBackfill) > 0 {
		fmt.Printf("[migration] %d Active convoy(s) need ask-branch backfill — will process on first tick\n",
			len(needsBackfill))
	}
	return nil
}

func runPRFlowDryRun(db *sql.DB) {
	fmt.Println("PR-flow migration — dry run")
	fmt.Println("===========================")

	repos := store.ListRepos(db)
	fmt.Printf("Registered repos: %d\n", len(repos))
	needsBackfillCount := 0
	needsTemplateCount := 0
	for _, r := range repos {
		status := "ok"
		detail := ""
		if r.LocalPath == "" {
			status = "no-path"
		} else if r.RemoteURL == "" || r.DefaultBranch == "" {
			status = "needs-backfill"
			needsBackfillCount++
		}
		if r.PRTemplatePath == "" && r.LocalPath != "" {
			needsTemplateCount++
		}
		if detail != "" {
			fmt.Printf("  - %s [%s] %s\n", r.Name, status, detail)
		} else {
			fmt.Printf("  - %s [%s]\n", r.Name, status)
		}
	}
	fmt.Printf("\nLayer B backfill would populate remote_url/default_branch for %d repo(s).\n", needsBackfillCount)
	fmt.Printf("FindPRTemplate would be enqueued for %d repo(s).\n", needsTemplateCount)

	needsBackfill := store.ActiveConvoysMissingAskBranch(db)
	fmt.Printf("\nActive convoys missing ask_branch: %d (Layer C backfill)\n", len(needsBackfill))

	fmt.Println("\nAvailable snapshots:")
	snapshots, _ := listPRFlowSnapshots(".")
	if len(snapshots) == 0 {
		fmt.Println("  (none — the first real migrate run will create one)")
	} else {
		for _, s := range snapshots {
			fmt.Printf("  - %s\n", s)
		}
	}
}

func runPRFlowMigrate(db *sql.DB) {
	// Take a snapshot before any work. Failing to snapshot aborts — we never
	// run the backfill without a rollback available.
	snapshot, err := takePRFlowSnapshot(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Snapshot failed: %v\nAborting migration.\n", err)
		os.Exit(1)
	}
	fmt.Printf("Snapshot: %s\n", snapshot)

	// Layer A has already run during InitHolocron (all ALTERs are idempotent).
	// We just need to do Layer B and report preflight state.
	ghClient := gh.NewClient()
	checks := agents.PRFlowPreflight(db, ghClient)
	fmt.Println("Preflight checks:")
	for _, c := range checks {
		mark := "PASS"
		if !c.Passed {
			mark = "FAIL"
		}
		if c.RepoKey != "" {
			fmt.Printf("  [%s] %s (%s): %s\n", mark, c.Name, c.RepoKey, c.Detail)
		} else {
			fmt.Printf("  [%s] %s: %s\n", mark, c.Name, c.Detail)
		}
	}
	summary := agents.BackfillRepoRemoteInfo(db)
	fmt.Printf("Layer B: %s\n", summary)

	queued, skipped := agents.EnqueueMissingFindPRTemplate(db)
	fmt.Printf("FindPRTemplate: %d queued, %d skipped\n", queued, skipped)

	needsBackfill := store.ActiveConvoysMissingAskBranch(db)
	fmt.Printf("Active convoys missing ask_branch: %d (Layer C runs on next daemon tick)\n", len(needsBackfill))

	fmt.Println("\nMigration complete.")
}

func runPRFlowRollback(db *sql.DB) {
	if _, alive := readDaemonPID(); alive {
		fmt.Fprintln(os.Stderr, "Error: daemon is running. Stop it before rolling back.")
		os.Exit(1)
	}

	snapshots, err := listPRFlowSnapshots(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list snapshots: %v\n", err)
		os.Exit(1)
	}
	if len(snapshots) == 0 {
		fmt.Fprintln(os.Stderr, "No snapshots found.")
		os.Exit(1)
	}
	latest := snapshots[0] // sort descending

	// Must close the DB before overwriting the file on some platforms.
	_ = db.Close()

	if err := copyFile(latest, "holocron.db"); err != nil {
		fmt.Fprintf(os.Stderr, "Rollback failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Rolled back holocron.db from %s\n", latest)
}

// takePRFlowSnapshot copies holocron.db to a timestamped snapshot in dir.
func takePRFlowSnapshot(dir string) (string, error) {
	src := filepath.Join(dir, "holocron.db")
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("holocron.db not found at %s: %v", src, err)
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s%s", prFlowSnapshotPrefix, time.Now().Format("20060102-150405")))
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// listPRFlowSnapshots returns snapshot filenames in dir, newest first.
func listPRFlowSnapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var snaps []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), prFlowSnapshotPrefix) {
			snaps = append(snaps, filepath.Join(dir, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(snaps)))
	return snaps, nil
}

// copyFile copies src to dst. Uses a simple Open/Create pair — holocron.db is
// small enough (MBs, not GBs) that we don't need to worry about buffering.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// ── `force repo sync` — populate remote info + enqueue FindPRTemplate ────────
//
// `force repo sync` is the same work runPRFlowStartup does at daemon start,
// but runnable as a one-shot so operators can refresh after adding repos or
// after a `git remote set-url` change.

func cmdRepoSync(db *sql.DB) {
	ghClient := gh.NewClient()
	checks := agents.PRFlowPreflight(db, ghClient)
	fmt.Println("Preflight:")
	for _, c := range checks {
		mark := "PASS"
		if !c.Passed {
			mark = "FAIL"
		}
		if c.RepoKey != "" {
			fmt.Printf("  [%s] %s (%s): %s\n", mark, c.Name, c.RepoKey, c.Detail)
		} else {
			fmt.Printf("  [%s] %s: %s\n", mark, c.Name, c.Detail)
		}
	}
	// Even if gh-auth failed, still run Layer B — it only needs git, not gh.
	fmt.Printf("\n%s\n", agents.BackfillRepoRemoteInfo(db))
	queued, skipped := agents.EnqueueMissingFindPRTemplate(db)
	fmt.Printf("FindPRTemplate: %d queued, %d skipped\n", queued, skipped)
}

// ── `force repo set-pr-flow <name> on|off` ───────────────────────────────────

func cmdRepoSetPRFlow(db *sql.DB, name, onOff string) {
	var enabled bool
	switch strings.ToLower(onOff) {
	case "on", "true", "1", "yes":
		enabled = true
	case "off", "false", "0", "no":
		enabled = false
	default:
		fmt.Printf("Invalid value %q — use 'on' or 'off'.\n", onOff)
		os.Exit(1)
	}
	if repo := store.GetRepo(db, name); repo == nil {
		fmt.Printf("Repository '%s' not found.\n", name)
		os.Exit(1)
	}
	if err := store.SetRepoPRFlowEnabled(db, name, enabled); err != nil {
		fmt.Printf("Failed to update: %v\n", err)
		os.Exit(1)
	}
	state := "off"
	if enabled {
		state = "on"
	}
	fmt.Printf("pr_flow_enabled for %s → %s\n", name, state)
}

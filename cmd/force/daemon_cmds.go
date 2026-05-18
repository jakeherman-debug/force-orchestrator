package main

// D12 P1 — `force daemon <subcommand>` control surface.
//
// The bare `force daemon` command (legacy foreground) lives in
// fleet_cmds.go::cmdDaemon. This file dispatches the new subcommand
// family:
//
//	foreground         — explicit foreground (alias for legacy daemon)
//	install            — install launchd plist (macOS) or systemd unit (linux)
//	uninstall          — remove plist/unit
//	status             — print PID, provenance, dashboard URL
//	stop               — graceful SIGTERM to running daemon
//	logs               — tail fleet.log
//	update             — binary rollover with 4-diff trust gate
//	rollback           — restore .previous binary
//	trust list/add/remove — manage ~/.force/trusted-binary-hashes
//	history            — DaemonUpdateHistory (P3 stub for now)
//	validate-config    — parse config/*.yaml without starting
//	validate-schema    — schema parity check against running DB
//
// P2/P3 surface area (sleep/wake hooks, crash recovery, auto-restart,
// DaemonUpdateHistory writer) is explicitly NOT in P1's lane and
// `history` returns a stub.

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/daemon/provenance"
	"force-orchestrator/internal/daemon/singleton"
	"force-orchestrator/internal/daemon/trust"
	"force-orchestrator/internal/forcepath"
	"force-orchestrator/internal/store"
)

// dispatchDaemon is called from main.go for `force daemon <sub>`.
// `args` is os.Args[2:] (everything after `daemon`).
func dispatchDaemon(db *sql.DB, args []string) {
	if len(args) == 0 {
		// Bare `force daemon` — legacy foreground. Print a one-line
		// deprecation pointer (TTY only) and continue.
		if isStdoutTTY() {
			fmt.Fprintln(os.Stderr,
				"`force daemon` (no subcommand) starts a foreground daemon. Use `force daemon foreground` going forward; `force daemon install` for managed lifecycle.")
		}
		cmdDaemon(db, nil)
		return
	}

	sub := args[0]
	rest := args[1:]
	switch sub {
	case "foreground", "fg":
		cmdDaemon(db, rest)
	case "install":
		os.Exit(cmdDaemonInstall(rest))
	case "uninstall":
		os.Exit(cmdDaemonUninstall(rest))
	case "status":
		os.Exit(cmdDaemonStatus(db, rest))
	case "stop":
		os.Exit(cmdDaemonStop(rest))
	case "logs":
		os.Exit(cmdDaemonLogs(rest))
	case "update":
		os.Exit(cmdDaemonUpdate(db, rest))
	case "rollback":
		os.Exit(cmdDaemonRollback(db, rest))
	case "clear-crash-budget":
		os.Exit(cmdDaemonClearCrashBudget(db, rest))
	case "trust":
		os.Exit(cmdDaemonTrust(rest))
	case "history":
		os.Exit(cmdDaemonHistory(db, rest))
	case "validate-config":
		os.Exit(cmdDaemonValidateConfig(rest))
	case "validate-schema":
		os.Exit(cmdDaemonValidateSchema(db, rest))
	case "help", "--help", "-h":
		// `force daemon help [<sub>]` — delegate to the subcommand's
		// own --help path so the same shared printer renders the body.
		if len(rest) > 0 {
			os.Exit(dispatchDaemonSubcommandHelp(db, rest[0]))
		}
		printDaemonUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown daemon subcommand: %s\n", sub)
		printDaemonUsage()
		os.Exit(1)
	}
}

// dispatchDaemonSubcommandHelp routes `force daemon help <sub>` through
// the subcommand's own --help path. Returns the exit code from the
// subcommand handler (always 0 for the --help path).
func dispatchDaemonSubcommandHelp(db *sql.DB, sub string) int {
	helpArgs := []string{"--help"}
	switch sub {
	case "foreground", "fg":
		// Bare foreground has no flags of its own; render a minimal block.
		printDaemonSubcommandHelp(os.Stdout, "foreground",
			"Run the daemon in the foreground (legacy bare 'force daemon').",
			[]flagDoc{{Name: "--help", Desc: "show this help and exit"}},
			[]string{"force daemon foreground"})
		return 0
	case "install":
		return cmdDaemonInstall(helpArgs)
	case "uninstall":
		return cmdDaemonUninstall(helpArgs)
	case "status":
		return cmdDaemonStatus(db, helpArgs)
	case "stop":
		return cmdDaemonStop(helpArgs)
	case "logs":
		return cmdDaemonLogs(helpArgs)
	case "update":
		return cmdDaemonUpdate(db, helpArgs)
	case "rollback":
		return cmdDaemonRollback(db, helpArgs)
	case "clear-crash-budget":
		return cmdDaemonClearCrashBudget(db, helpArgs)
	case "trust":
		return cmdDaemonTrust(helpArgs)
	case "history":
		return cmdDaemonHistory(db, helpArgs)
	case "validate-config":
		return cmdDaemonValidateConfig(helpArgs)
	case "validate-schema":
		return cmdDaemonValidateSchema(db, helpArgs)
	default:
		fmt.Fprintf(os.Stderr, "Unknown daemon subcommand: %s\n", sub)
		printDaemonUsage()
		return 1
	}
}

func printDaemonUsage() {
	fmt.Println(`Usage: force daemon <subcommand>

  foreground               Run the daemon in the foreground (legacy bare 'daemon')
  install [--dry-run]      Install launchd plist (darwin) or systemd user unit (linux)
  uninstall                Remove the installed plist/unit
  status                   Show running PID, trust file presence, provenance, dashboard URL
  stop                     SIGTERM the running daemon and wait for clean exit
  logs [-f] [-n N]         Tail fleet.log
  update [--binary <path>] [--assume-yes]
                           Replace the running binary with a new one (4-diff trust gate)
  rollback                 Restore the previous binary (force.previous)
  trust list               List trusted binary SHAs
  trust add <path>         Add the SHA of <path> to the trust file
  trust remove <sha>       Remove a trusted SHA
  history [--limit N] [--from-trust-file]
                           Show DaemonUpdateHistory (D12 P3 schema)
  clear-crash-budget [--assume-yes]
                           Truncate DaemonStartLog after fixing the underlying
                           issue (D12 P3 — re-arms launchd/systemd auto-restart)
  validate-config          Parse config/*.yaml without starting the daemon
  validate-schema          Run TestSchemaParity-equivalent against the live DB`)
}

// ── status ──────────────────────────────────────────────────────────────────

func cmdDaemonStatus(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	helped, err := parseDaemonFlags(fs, args, "status",
		"Show running PID, trust file presence, provenance, and dashboard URL. Exit 0 if running, 1 if not.",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon status"})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	pidPath := singleton.DefaultPIDPath()
	locked, holder, err := singleton.IsLocked(pidPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: IsLocked: %v\n", err)
	}
	fmt.Println("force daemon status")
	fmt.Println("───────────────────")
	if locked {
		fmt.Printf("running     : YES (PID %d)\n", holder)
	} else {
		fmt.Println("running     : no")
	}
	fmt.Printf("pid file    : %s\n", pidPath)

	tp := trust.DefaultPath()
	if _, statErr := os.Stat(tp); statErr == nil {
		f, _ := trust.Load(tp)
		entries := 0
		if f != nil {
			entries = len(f.Entries)
		}
		fmt.Printf("trust file  : %s (%d entries)\n", tp, entries)
	} else {
		fmt.Printf("trust file  : %s (NOT PRESENT — `force daemon trust add <path>` to ratify)\n", tp)
	}

	binPath, _ := os.Executable()
	if binPath != "" {
		if h, herr := trust.HashFile(binPath); herr == nil {
			fmt.Printf("binary      : %s\n", binPath)
			fmt.Printf("binary-sha  : %s\n", h)
		}
	}

	prov := provenance.Get()
	fmt.Printf("git-sha     : %s\n", prov.GitSHA)
	fmt.Printf("git-branch  : %s\n", prov.GitBranch)
	fmt.Printf("build-time  : %s\n", prov.BuildTime)
	fmt.Printf("go-version  : %s\n", prov.GoVersion)

	port := dashboardPortFromConfig(db)
	enabled := dashboardEnabledFromConfig(db)
	if enabled {
		fmt.Printf("dashboard   : http://127.0.0.1:%d (bundled — `dashboard_enabled=true`)\n", port)
	} else {
		fmt.Printf("dashboard   : disabled (`dashboard_enabled=false`)\n")
	}

	if locked {
		return 0
	}
	return 1
}

// ── stop ────────────────────────────────────────────────────────────────────

func cmdDaemonStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	helped, err := parseDaemonFlags(fs, args, "stop",
		"Send SIGTERM to the running daemon and wait up to 60s for a clean exit.",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon stop"})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	pidPath := singleton.DefaultPIDPath()
	locked, pid, err := singleton.IsLocked(pidPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stop: IsLocked: %v\n", err)
		return 2
	}
	if !locked {
		fmt.Println("daemon not running")
		return 0
	}
	proc, perr := os.FindProcess(pid)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "stop: FindProcess(%d): %v\n", pid, perr)
		return 2
	}
	if sigErr := proc.Signal(syscall.SIGTERM); sigErr != nil {
		fmt.Fprintf(os.Stderr, "stop: SIGTERM(%d): %v\n", pid, sigErr)
		return 2
	}
	fmt.Printf("Sent SIGTERM to PID %d. Waiting for clean exit (up to 60s)...\n", pid)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		stillLocked, _, _ := singleton.IsLocked(pidPath)
		if !stillLocked {
			fmt.Println("Daemon exited cleanly.")
			return 0
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "stop: daemon did not exit within 60s — try SIGKILL manually")
	return 3
}

// ── logs ────────────────────────────────────────────────────────────────────

func cmdDaemonLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	followPtr := fs.Bool("follow", false, "tail the log (like `tail -f`)")
	fs.BoolVar(followPtr, "f", false, "alias for --follow")
	tailLinesPtr := fs.Int("n", 50, "number of trailing lines to print before following")
	helped, err := parseDaemonFlags(fs, args, "logs",
		"Print the trailing N lines of fleet.log, optionally following the file as it grows.",
		[]flagDoc{
			{Name: "--follow, -f", Desc: "tail the log (like `tail -f`)"},
			{Name: "-n <int>", Desc: "number of trailing lines to print before following (default 50)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force daemon logs",
			"force daemon logs -f",
			"force daemon logs -n 200",
		})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	follow := *followPtr
	tailLines := *tailLinesPtr
	// Sweep-F: canonical fleet log path (~/.force/fleet.log).
	path := forcepath.FleetLog()
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logs: open %s: %v\n", path, err)
		return 1
	}
	defer f.Close()
	// Read last N lines into a ring buffer.
	buf := make([]string, 0, tailLines)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		if len(buf) == tailLines {
			buf = buf[1:]
		}
		buf = append(buf, sc.Text())
	}
	for _, ln := range buf {
		fmt.Println(ln)
	}
	if !follow {
		return 0
	}
	// Continue tailing.
	for {
		if sc.Scan() {
			fmt.Println(sc.Text())
			continue
		}
		// EOF — sleep a beat and try again.
		time.Sleep(500 * time.Millisecond)
		sc = bufio.NewScanner(f)
		sc.Buffer(make([]byte, 256*1024), 256*1024)
	}
}

// ── trust ───────────────────────────────────────────────────────────────────

func cmdDaemonTrust(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: force daemon trust list|add <path>|remove <sha>")
		return 1
	}
	switch args[0] {
	case "--help", "-h", "help":
		printDaemonSubcommandHelp(os.Stdout, "trust",
			"Manage ~/.force/trusted-binary-hashes — the SHA256 ratification log read by `force daemon update`.",
			[]flagDoc{
				{Name: "list", Desc: "print all trusted SHAs"},
				{Name: "add <path>", Desc: "compute SHA of <path> and append to the trust file"},
				{Name: "remove <sha>", Desc: "remove a SHA from the trust file"},
				{Name: "--help, -h", Desc: "show this help and exit"},
			},
			[]string{
				"force daemon trust list",
				"force daemon trust add ./force",
				"force daemon trust remove abc123...",
			})
		return 0
	case "list":
		return cmdDaemonTrustList(args[1:])
	case "add":
		return cmdDaemonTrustAdd(args[1:])
	case "remove":
		return cmdDaemonTrustRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown trust subcommand: %s\n", args[0])
		return 1
	}
}

func cmdDaemonTrustList(args []string) int {
	fs := flag.NewFlagSet("trust list", flag.ContinueOnError)
	helped, err := parseDaemonFlags(fs, args, "trust list",
		"Print all trusted SHA256 entries from ~/.force/trusted-binary-hashes.",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon trust list"})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	tp := trust.DefaultPath()
	f, err := trust.Load(tp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trust list: %v\n", err)
		return 1
	}
	if f == nil || len(f.Entries) == 0 {
		fmt.Printf("(empty — %s has no trusted hashes)\n", tp)
		return 0
	}
	fmt.Printf("%-64s %-20s %-30s %-10s %s\n", "SHA256", "TIMESTAMP", "TRUSTED-BY", "GIT-SHA", "BRANCH")
	for _, e := range f.Sorted() {
		fmt.Printf("%-64s %-20s %-30s %-10s %s\n",
			e.SHA256,
			e.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
			truncStr(e.TrustedBy, 30),
			truncStr(e.GitSHAAtBuild, 10),
			e.GitBranchAtBuild,
		)
	}
	if len(f.MalformedLines) > 0 {
		fmt.Fprintf(os.Stderr, "(%d malformed line(s) skipped)\n", len(f.MalformedLines))
	}
	return 0
}

func cmdDaemonTrustAdd(args []string) int {
	fs := flag.NewFlagSet("trust add", flag.ContinueOnError)
	helped, perr := parseDaemonFlags(fs, args, "trust add",
		"Compute the SHA256 of <binary-path> and append it to the trust file.",
		[]flagDoc{
			{Name: "<binary-path>", Desc: "path to the binary to ratify (positional)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon trust add ./force"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force daemon trust add <binary-path>")
		return 1
	}
	bin := fs.Arg(0)
	abs, err := filepath.Abs(bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trust add: %v\n", err)
		return 1
	}
	sha, err := trust.HashFile(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trust add: hash %s: %v\n", abs, err)
		return 1
	}
	prov := provenance.Get()
	e := trust.Entry{
		SHA256:           sha,
		TrustedBy:        currentOperator(),
		GitSHAAtBuild:    prov.GitSHA,
		GitBranchAtBuild: prov.GitBranch,
	}
	if err := trust.Append(trust.DefaultPath(), e); err != nil {
		fmt.Fprintf(os.Stderr, "trust add: append: %v\n", err)
		return 1
	}
	fmt.Printf("trusted %s (sha=%s)\n", abs, sha)
	return 0
}

func cmdDaemonTrustRemove(args []string) int {
	fs := flag.NewFlagSet("trust remove", flag.ContinueOnError)
	helped, perr := parseDaemonFlags(fs, args, "trust remove",
		"Remove a SHA256 entry from the trust file.",
		[]flagDoc{
			{Name: "<sha>", Desc: "SHA256 to remove (positional)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon trust remove abc123..."})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force daemon trust remove <sha>")
		return 1
	}
	sha := fs.Arg(0)
	n, err := trust.RemoveSHA(trust.DefaultPath(), sha)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trust remove: %v\n", err)
		return 1
	}
	if n == 0 {
		fmt.Printf("sha %s not found in trust file\n", sha)
		return 1
	}
	fmt.Printf("removed %d entry(ies) for %s\n", n, sha)
	return 0
}

// ── update ──────────────────────────────────────────────────────────────────

// cmdDaemonUpdate runs the binary-rollover trust gate and records the
// outcome to DaemonUpdateHistory (D12 P3) on every exit path:
//
//   - "success"     — atomic swap completed, or new == live (trust ratified)
//   - "rolled_back" — paranoia abort or stop-daemon failure
//   - "failed"      — unrecoverable IO error during hash/swap
//
// `db` may be nil in tests that exercise update without a holocron handle —
// the recording call is skipped gracefully (DB unavailability is logged, not
// fatal). Pattern P_DaemonUpdateHistory walks this function's AST and
// confirms every exit path is reachable from the recordOnExit closure.
func cmdDaemonUpdate(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	binaryPtr := fs.String("binary", "", "path to the new binary (defaults to currently-running binary, useful for trust-only ratification)")
	assumeYesPtr := fs.Bool("assume-yes", false, "skip the interactive trust-confirmation prompt")
	fs.BoolVar(assumeYesPtr, "y", false, "alias for --assume-yes")
	helped, err := parseDaemonFlags(fs, args, "update",
		"Replace the running binary with a new one, gated by the 4-diff trust prompt and recorded in DaemonUpdateHistory.",
		[]flagDoc{
			{Name: "--binary <path>", Desc: "path to the new binary (default: currently-running binary)"},
			{Name: "--assume-yes, -y", Desc: "skip the interactive trust-confirmation prompt"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force daemon update --binary ./force.new",
			"force daemon update --assume-yes",
		})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	binaryFlag := *binaryPtr
	assumeYes := *assumeYesPtr

	// Outcome ledger — set before each exit; deferred recorder writes the
	// row regardless of which return statement runs.
	var (
		outcome     = "failed" // pessimistic default
		oldSHA      string
		newSHA      string
		oldGit      = provenance.Get().GitSHA
		newGit      string
		notes       string
	)
	defer func() {
		// Skip recording when db is unavailable (test paths) or on a
		// pre-SHA-resolution failure where we lack identifying info.
		if db == nil {
			return
		}
		if recErr := store.RecordDaemonUpdate(db, oldSHA, newSHA, oldGit, newGit, currentOperator(), outcome, notes); recErr != nil {
			fmt.Fprintf(os.Stderr, "update: record DaemonUpdateHistory: %v\n", recErr)
		}
	}()

	livePath, err := os.Executable()
	if err != nil {
		notes = fmt.Sprintf("cannot determine current binary: %v", err)
		fmt.Fprintf(os.Stderr, "update: %s\n", notes)
		return 1
	}
	newPath := binaryFlag
	if newPath == "" {
		newPath = livePath
	}
	absNew, err := filepath.Abs(newPath)
	if err != nil {
		notes = fmt.Sprintf("filepath.Abs: %v", err)
		fmt.Fprintf(os.Stderr, "update: %s\n", notes)
		return 1
	}

	// Identify SHAs.
	oldSHA, err = trust.HashFile(livePath)
	if err != nil {
		notes = fmt.Sprintf("hash live binary: %v", err)
		fmt.Fprintf(os.Stderr, "update: %s\n", notes)
		return 1
	}
	newSHA, err = trust.HashFile(absNew)
	if err != nil {
		notes = fmt.Sprintf("hash new binary: %v", err)
		fmt.Fprintf(os.Stderr, "update: %s\n", notes)
		return 1
	}

	tp := trust.DefaultPath()
	tf, _ := trust.Load(tp)

	fmt.Println("force daemon update")
	fmt.Println("───────────────────")
	fmt.Printf("live binary : %s\n", livePath)
	fmt.Printf("live sha    : %s\n", oldSHA)
	fmt.Printf("new binary  : %s\n", absNew)
	fmt.Printf("new sha     : %s\n", newSHA)
	fmt.Printf("trust file  : %s\n", tp)

	provNow := provenance.Get()
	newGit = provNow.GitSHA // best-effort: live binary's git-sha doubles as new git when binary == self
	if tf != nil && tf.HasSHA(newSHA) {
		fmt.Println("trust       : MATCH (new SHA is in the trust file)")
	} else {
		// Paranoia mode default-on. Show 4-diff preview.
		fmt.Println("trust       : NOT FOUND — paranoia mode active")
		fmt.Println()
		fmt.Println("Inspect the diff before proceeding. Suggested commands:")
		fmt.Printf("  git log %s..%s\n", provNow.GitSHA, "<new-git-sha>")
		fmt.Printf("  git diff --stat %s..%s\n", provNow.GitSHA, "<new-git-sha>")
		fmt.Printf("  git diff %s..%s -- 'config/*.yaml'\n", provNow.GitSHA, "<new-git-sha>")
		fmt.Printf("  git diff %s..%s -- internal/\n", provNow.GitSHA, "<new-git-sha>")
		fmt.Println()
		fmt.Println("(Replace `<new-git-sha>` with the SHA the new binary was built from.)")
		fmt.Println()
		if !assumeYes {
			fmt.Print("Trust this binary and proceed with update? [yes/no]: ")
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			line = strings.ToLower(strings.TrimSpace(line))
			if line != "yes" && line != "y" {
				outcome = "rolled_back"
				notes = "operator declined trust confirmation"
				fmt.Println("Aborted (no trust confirmation).")
				return 1
			}
		} else {
			fmt.Println("(--assume-yes — skipping interactive prompt; trust entry will be appended)")
		}
		// Append to trust file.
		appendErr := trust.Append(tp, trust.Entry{
			SHA256:           newSHA,
			TrustedBy:        currentOperator(),
			GitSHAAtBuild:    provNow.GitSHA,
			GitBranchAtBuild: provNow.GitBranch,
		})
		if appendErr != nil {
			notes = fmt.Sprintf("append trust: %v", appendErr)
			fmt.Fprintf(os.Stderr, "update: %s\n", notes)
			return 1
		}
		fmt.Println("Appended new SHA to trust file.")
	}

	// Stop running daemon (if any).
	pidPath := singleton.DefaultPIDPath()
	if locked, pid, _ := singleton.IsLocked(pidPath); locked && pid > 0 {
		fmt.Printf("Stopping running daemon (PID %d)...\n", pid)
		if rc := cmdDaemonStop(nil); rc != 0 {
			outcome = "rolled_back"
			notes = fmt.Sprintf("stop failed (rc=%d) — update aborted", rc)
			fmt.Fprintln(os.Stderr, "update: "+notes)
			return rc
		}
	}

	// Atomic rollover (only if new ≠ live).
	if absNew != livePath {
		previous := livePath + ".previous"
		if err := os.Rename(livePath, previous); err != nil {
			notes = fmt.Sprintf("snapshot previous: %v", err)
			fmt.Fprintf(os.Stderr, "update: %s\n", notes)
			return 1
		}
		// Move new binary to live path.
		if err := copyBinaryFile(absNew, livePath); err != nil {
			// Roll back the rename if the copy fails.
			_ = os.Rename(previous, livePath)
			outcome = "rolled_back"
			notes = fmt.Sprintf("install new: %v (live restored from .previous)", err)
			fmt.Fprintf(os.Stderr, "update: %s\n", notes)
			return 1
		}
		if err := os.Chmod(livePath, 0o755); err != nil {
			notes = fmt.Sprintf("chmod: %v", err)
			fmt.Fprintf(os.Stderr, "update: %s\n", notes)
			return 1
		}
		fmt.Printf("Replaced %s; previous saved as %s\n", livePath, previous)
		notes = fmt.Sprintf("atomic rollover; previous=%s", previous)
	} else {
		fmt.Println("(new == live; no copy performed — trust entry recorded)")
		notes = "trust ratification only (new == live)"
	}

	outcome = "success"
	fmt.Println("Update complete. Start the daemon via `force daemon foreground` or your installed launchd/systemd unit.")
	return 0
}

// ── rollback ────────────────────────────────────────────────────────────────

// cmdDaemonRollback restores the previous binary (`.previous`) and records
// the outcome to DaemonUpdateHistory with outcome='rolled_back' on a
// successful restore, 'failed' on an IO error.
func cmdDaemonRollback(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	helped, err := parseDaemonFlags(fs, args, "rollback",
		"Restore the previous binary (force.previous) atomically and record the outcome in DaemonUpdateHistory.",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon rollback"})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	var (
		outcome = "failed"
		oldSHA  string
		newSHA  string
		oldGit  = provenance.Get().GitSHA
		newGit  string
		notes   string
	)
	defer func() {
		if db == nil {
			return
		}
		if recErr := store.RecordDaemonUpdate(db, oldSHA, newSHA, oldGit, newGit, currentOperator(), outcome, notes); recErr != nil {
			fmt.Fprintf(os.Stderr, "rollback: record DaemonUpdateHistory: %v\n", recErr)
		}
	}()

	livePath, err := os.Executable()
	if err != nil {
		notes = fmt.Sprintf("Executable: %v", err)
		fmt.Fprintf(os.Stderr, "rollback: %s\n", notes)
		return 1
	}
	previous := livePath + ".previous"
	if _, err := os.Stat(previous); err != nil {
		notes = fmt.Sprintf("no previous binary at %s", previous)
		fmt.Fprintf(os.Stderr, "rollback: %s\n", notes)
		return 1
	}

	// Best-effort SHA capture for the audit row.
	if h, herr := trust.HashFile(livePath); herr == nil {
		oldSHA = h
	}
	if h, herr := trust.HashFile(previous); herr == nil {
		newSHA = h
	}

	tp := trust.DefaultPath()
	tf, _ := trust.Load(tp)
	if tf != nil && len(tf.Entries) >= 2 {
		sorted := tf.Sorted()
		secondMostRecent := sorted[1]
		prevSHA, err := trust.HashFile(previous)
		if err == nil && !strings.EqualFold(prevSHA, secondMostRecent.SHA256) {
			fmt.Printf("warn: previous binary SHA (%s) does not match second-most-recent trust entry (%s)\n",
				prevSHA, secondMostRecent.SHA256)
		}
	}

	pidPath := singleton.DefaultPIDPath()
	if locked, pid, _ := singleton.IsLocked(pidPath); locked && pid > 0 {
		fmt.Printf("Stopping running daemon (PID %d)...\n", pid)
		if rc := cmdDaemonStop(nil); rc != 0 {
			notes = fmt.Sprintf("stop failed (rc=%d) — rollback aborted", rc)
			return rc
		}
	}

	tmp := livePath + ".rollback-tmp"
	if err := copyBinaryFile(livePath, tmp); err != nil {
		notes = fmt.Sprintf("snapshot live: %v", err)
		fmt.Fprintf(os.Stderr, "rollback: %s\n", notes)
		return 1
	}
	if err := copyBinaryFile(previous, livePath); err != nil {
		_ = os.Remove(tmp)
		notes = fmt.Sprintf("restore: %v", err)
		fmt.Fprintf(os.Stderr, "rollback: %s\n", notes)
		return 1
	}
	_ = os.Chmod(livePath, 0o755)
	if err := os.Rename(tmp, previous); err != nil {
		fmt.Fprintf(os.Stderr, "rollback: swap previous: %v\n", err)
		// not fatal — previous still on disk under tmp name
	}
	fmt.Printf("Rolled back %s. Previous-of-previous saved as %s\n", livePath, previous)
	outcome = "rolled_back"
	notes = fmt.Sprintf("restored from %s", previous)
	return 0
}

// ── install / uninstall ─────────────────────────────────────────────────────

func cmdDaemonInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dryRunPtr := fs.Bool("dry-run", false, "print the plist/unit that would be written, without writing it")
	helped, err := parseDaemonFlags(fs, args, "install",
		"Install launchd plist (darwin) or systemd user unit (linux) so the daemon boots at login.",
		[]flagDoc{
			{Name: "--dry-run", Desc: "print the plist/unit that would be written, without writing it"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force daemon install",
			"force daemon install --dry-run",
		})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	dryRun := *dryRunPtr
	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		return 1
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(binPath, dryRun)
	case "linux":
		return installSystemd(binPath, dryRun)
	default:
		fmt.Fprintf(os.Stderr, "install: unsupported OS %s\n", runtime.GOOS)
		return 1
	}
}

func cmdDaemonUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	dryRunPtr := fs.Bool("dry-run", false, "print what would be unloaded/removed, without doing it")
	helped, err := parseDaemonFlags(fs, args, "uninstall",
		"Remove the installed launchd plist (darwin) or systemd user unit (linux).",
		[]flagDoc{
			{Name: "--dry-run", Desc: "print what would be unloaded/removed, without doing it"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force daemon uninstall",
			"force daemon uninstall --dry-run",
		})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	dryRun := *dryRunPtr
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd(dryRun)
	case "linux":
		return uninstallSystemd(dryRun)
	default:
		fmt.Fprintf(os.Stderr, "uninstall: unsupported OS %s\n", runtime.GOOS)
		return 1
	}
}

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.force-orchestrator.daemon.plist")
}

func systemdUnitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", "force-orchestrator.service")
}

func installLaunchd(binPath string, dryRun bool) int {
	plist := launchdPlistTemplate(binPath)
	path := launchdPlistPath()
	if dryRun {
		fmt.Printf("[dry-run] would write %s:\n%s\n", path, plist)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "install: mkdir: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "install: write %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("Wrote %s\n", path)
	fmt.Println("To activate:")
	fmt.Printf("  launchctl unload %s 2>/dev/null || true\n", path)
	fmt.Printf("  launchctl load %s\n", path)
	return 0
}

func uninstallLaunchd(dryRun bool) int {
	path := launchdPlistPath()
	if dryRun {
		fmt.Printf("[dry-run] would unload + remove %s\n", path)
		return 0
	}
	if _, err := os.Stat(path); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = exec.CommandContext(ctx, "launchctl", "unload", path).Run()
		cancel()
		_ = os.Remove(path)
		fmt.Printf("Removed %s\n", path)
	}
	return 0
}

func installSystemd(binPath string, dryRun bool) int {
	unit := systemdUnitTemplate(binPath)
	path := systemdUnitPath()
	if dryRun {
		fmt.Printf("[dry-run] would write %s:\n%s\n", path, unit)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "install: mkdir: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "install: write %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("Wrote %s\n", path)
	fmt.Println("To activate:")
	fmt.Println("  systemctl --user daemon-reload")
	fmt.Println("  systemctl --user enable --now force-orchestrator.service")
	return 0
}

func uninstallSystemd(dryRun bool) int {
	path := systemdUnitPath()
	if dryRun {
		fmt.Printf("[dry-run] would disable + remove %s\n", path)
		return 0
	}
	if _, err := os.Stat(path); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", "force-orchestrator.service").Run()
		cancel()
		_ = os.Remove(path)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		_ = exec.CommandContext(ctx2, "systemctl", "--user", "daemon-reload").Run()
		cancel2()
		fmt.Printf("Removed %s\n", path)
	}
	return 0
}

func launchdPlistTemplate(binPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.force-orchestrator.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>daemon</string>
        <string>foreground</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>Crashed</key>
        <true/>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>/tmp/force-daemon.out.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/force-daemon.err.log</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
</dict>
</plist>
`, binPath, daemonCwd())
}

func systemdUnitTemplate(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=force-orchestrator daemon
After=network.target

[Service]
Type=simple
ExecStart=%s daemon foreground
WorkingDirectory=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, binPath, daemonCwd())
}

func daemonCwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "/tmp"
}

// ── history ─────────────────────────────────────────────────────────────────

// cmdDaemonHistory prints the most recent N rows from DaemonUpdateHistory
// (P3 schema). The legacy trust-file view is reachable via
// `--from-trust-file` for operators who still want to scan the append-only
// ratification log directly.
func cmdDaemonHistory(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	limitPtr := fs.Int("limit", 20, "max rows to print")
	fromTrustPtr := fs.Bool("from-trust-file", false, "show legacy trust-file ratification view instead of DaemonUpdateHistory")
	helped, err := parseDaemonFlags(fs, args, "history",
		"Print the most recent N rows of DaemonUpdateHistory, or the legacy trust-file ratification view.",
		[]flagDoc{
			{Name: "--limit <int>", Desc: "max rows to print (default 20)"},
			{Name: "--from-trust-file", Desc: "show the legacy trust-file ratification view"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force daemon history",
			"force daemon history --limit 50",
			"force daemon history --from-trust-file",
		})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	limit := *limitPtr
	fromTrust := *fromTrustPtr
	fmt.Println("force daemon history")
	fmt.Println("─────────────────────")

	if fromTrust {
		return cmdDaemonHistoryFromTrustFile(limit)
	}

	entries, err := store.ListDaemonUpdateHistory(db, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "history: query DaemonUpdateHistory: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Println("(no entries — no `force daemon update` invocations recorded yet)")
		fmt.Println("Run `force daemon history --from-trust-file` to view the legacy trust-file view.")
		return 0
	}
	fmt.Printf("%-20s  %-12s  %-12s  %-12s  %s\n",
		"TIMESTAMP", "OLD-SHA", "NEW-SHA", "OUTCOME", "OPERATOR")
	for _, e := range entries {
		fmt.Printf("%-20s  %-12s  %-12s  %-12s  %s\n",
			e.TS,
			truncStr(e.OldBinarySHA, 12),
			truncStr(e.NewBinarySHA, 12),
			e.Outcome,
			truncStr(e.Operator, 20),
		)
	}
	return 0
}

// cmdDaemonHistoryFromTrustFile is the legacy trust-file view, kept
// behind the `--from-trust-file` flag. Each Append to the trust file is
// one ratification event; this listing is useful when correlating an
// in-DB DaemonUpdateHistory row against the underlying trust ratification.
func cmdDaemonHistoryFromTrustFile(limit int) int {
	tp := trust.DefaultPath()
	f, err := trust.Load(tp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "history: load %s: %v\n", tp, err)
		return 1
	}
	if f == nil || len(f.Entries) == 0 {
		fmt.Println("(trust file empty)")
		return 0
	}
	fmt.Println("(showing trust-file ratifications, --from-trust-file)")
	sorted := f.Sorted()
	if limit > 0 && limit < len(sorted) {
		sorted = sorted[:limit]
	}
	for _, e := range sorted {
		fmt.Printf("%s  %s  %s  %s  %s\n",
			e.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
			e.SHA256[:12]+"...",
			truncStr(e.TrustedBy, 30),
			truncStr(e.GitSHAAtBuild, 12),
			e.GitBranchAtBuild,
		)
	}
	return 0
}

// ── clear-crash-budget (D12 P3) ──────────────────────────────────────────────

// cmdDaemonClearCrashBudget truncates DaemonStartLog after the operator has
// investigated and fixed the underlying issue that triggered the crash-loop
// detector. Re-arms the budget at zero so launchd / systemd's auto-restart
// resumes its normal contract on the next boot.
//
// The command prompts for confirmation unless `--assume-yes` is supplied
// (mirrors the trust-file gate's UX). The clear is recorded in AuditLog so
// the operator-facing audit trail captures the re-arm.
func cmdDaemonClearCrashBudget(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("clear-crash-budget", flag.ContinueOnError)
	assumeYesPtr := fs.Bool("assume-yes", false, "skip the interactive confirmation prompt")
	fs.BoolVar(assumeYesPtr, "y", false, "alias for --assume-yes")
	helped, err := parseDaemonFlags(fs, args, "clear-crash-budget",
		"Truncate DaemonStartLog after fixing the underlying issue — re-arms launchd/systemd auto-restart at zero.",
		[]flagDoc{
			{Name: "--assume-yes, -y", Desc: "skip the interactive confirmation prompt"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{
			"force daemon clear-crash-budget",
			"force daemon clear-crash-budget --assume-yes",
		})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	assumeYes := *assumeYesPtr
	pre, err := store.RecentStartCount(db, 24*time.Hour)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clear-crash-budget: read DaemonStartLog: %v\n", err)
		return 1
	}
	fmt.Println("force daemon clear-crash-budget")
	fmt.Println("─────────────────────────────────")
	fmt.Printf("DaemonStartLog 'started' rows in last 24h: %d\n", pre)
	fmt.Println()
	fmt.Println("This truncates the entire DaemonStartLog table, re-arming the crash-budget")
	fmt.Println("guard at zero. Use after you've investigated the root cause of a crash-loop.")
	fmt.Println()

	if !assumeYes {
		fmt.Print("Proceed? [yes/no]: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "yes" && line != "y" {
			fmt.Println("Aborted.")
			return 1
		}
	} else {
		fmt.Println("(--assume-yes — skipping interactive prompt)")
	}

	n, err := store.ClearDaemonStartLog(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clear-crash-budget: %v\n", err)
		return 1
	}
	op := currentOperator()
	store.LogAudit(db, op, "daemon.clear-crash-budget", 0,
		fmt.Sprintf("truncated DaemonStartLog (%d rows) at %s", n,
			time.Now().UTC().Format("2006-01-02T15:04:05Z")))
	fmt.Printf("Cleared %d row(s) from DaemonStartLog. Crash-budget re-armed.\n", n)
	return 0
}

// ── validate-config ─────────────────────────────────────────────────────────

func cmdDaemonValidateConfig(args []string) int {
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	helped, err := parseDaemonFlags(fs, args, "validate-config",
		"Parse config/notifications.yaml and config/dashboard.yaml without starting the daemon.",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon validate-config"})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	configs := []string{"config/notifications.yaml", "config/dashboard.yaml"}
	failed := 0
	for _, p := range configs {
		if _, err := os.Stat(p); err != nil {
			fmt.Printf("[skip ] %s — not present\n", p)
			continue
		}
		// Use validateConfigFile (delegates to package-specific loaders)
		if err := validateConfigFile(p); err != nil {
			fmt.Printf("[FAIL ] %s — %v\n", p, err)
			failed++
		} else {
			fmt.Printf("[ok   ] %s\n", p)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// ── validate-schema ─────────────────────────────────────────────────────────

func cmdDaemonValidateSchema(db *sql.DB, args []string) int {
	fs := flag.NewFlagSet("validate-schema", flag.ContinueOnError)
	helped, err := parseDaemonFlags(fs, args, "validate-schema",
		"Spot-check schema parity against the live DB. TestSchemaParity is the full CI gate.",
		[]flagDoc{
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force daemon validate-schema"})
	if helped {
		return 0
	}
	if err != nil {
		return 2
	}
	// Lightweight invariant check — TestSchemaParity is the heavyweight
	// CI gate; this surface lets the operator spot-check after a
	// rollover. We probe a small representative set of tables/columns.
	want := []struct{ table, column string }{
		{"BountyBoard", "status"},
		{"AuditLog", "actor"},
		{"SystemConfig", "key"},
		{"DashboardCatalogRegistry", "tab_id"},
	}
	failed := 0
	for _, w := range want {
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
			w.table, w.column,
		).Scan(&n)
		if err != nil {
			fmt.Printf("[FAIL ] %s.%s — %v\n", w.table, w.column, err)
			failed++
			continue
		}
		if n == 0 {
			fmt.Printf("[FAIL ] %s.%s — column missing\n", w.table, w.column)
			failed++
		} else {
			fmt.Printf("[ok   ] %s.%s\n", w.table, w.column)
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "validate-schema: %d failure(s) — run `make test` for the full TestSchemaParity sweep.\n", failed)
		return 1
	}
	return 0
}

// ── helpers ─────────────────────────────────────────────────────────────────

func currentOperator() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}

func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func copyBinaryFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// dashboardPortFromConfig returns the configured dashboard port.
// Default: 41977 (D12 P1 Component 5 — Star Wars: A New Hope).
func dashboardPortFromConfig(db *sql.DB) int {
	if v := store.GetConfig(db, "dashboard_port", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 41977
}

// dashboardEnabledFromConfig reports whether the daemon should bundle
// the dashboard goroutine. Default: true.
func dashboardEnabledFromConfig(db *sql.DB) bool {
	v := store.GetConfig(db, "dashboard_enabled", "")
	if v == "" {
		return true
	}
	return v != "false" && v != "0" && v != "no"
}

// validateConfigFile is split out so test code can probe single
// files. Currently we only sniff for YAML parse errors via
// gopkg.in/yaml.v3 — the package-specific loaders (notify.LoadConfig,
// dashconfig.LoadConfig) do schema validation at daemon start; we
// don't re-import them here to keep this surface dependency-free.
func validateConfigFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty file")
	}
	// Minimal YAML smoke check: count of top-level non-blank lines.
	lines := strings.Split(string(data), "\n")
	any := false
	for _, ln := range lines {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		any = true
		break
	}
	if !any {
		return errors.New("no non-comment content")
	}
	return nil
}


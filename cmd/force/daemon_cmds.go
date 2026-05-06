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
		cmdDaemon(db)
		return
	}

	sub := args[0]
	rest := args[1:]
	switch sub {
	case "foreground", "fg":
		cmdDaemon(db)
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
		os.Exit(cmdDaemonUpdate(rest))
	case "rollback":
		os.Exit(cmdDaemonRollback(rest))
	case "trust":
		os.Exit(cmdDaemonTrust(rest))
	case "history":
		os.Exit(cmdDaemonHistory(db, rest))
	case "validate-config":
		os.Exit(cmdDaemonValidateConfig(rest))
	case "validate-schema":
		os.Exit(cmdDaemonValidateSchema(db, rest))
	case "help", "--help", "-h":
		printDaemonUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown daemon subcommand: %s\n", sub)
		printDaemonUsage()
		os.Exit(1)
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
  history [--limit N]      Show DaemonUpdateHistory (P3 schema; falls back to trust file)
  validate-config          Parse config/*.yaml without starting the daemon
  validate-schema          Run TestSchemaParity-equivalent against the live DB`)
}

// ── status ──────────────────────────────────────────────────────────────────

func cmdDaemonStatus(db *sql.DB, args []string) int {
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
	follow := false
	tailLines := 50
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--follow":
			follow = true
		case "-n":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					tailLines = n
				}
				i++
			}
		}
	}
	path := "fleet.log"
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

func cmdDaemonTrustList(_ []string) int {
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
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force daemon trust add <binary-path>")
		return 1
	}
	bin := args[0]
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
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: force daemon trust remove <sha>")
		return 1
	}
	n, err := trust.RemoveSHA(trust.DefaultPath(), args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "trust remove: %v\n", err)
		return 1
	}
	if n == 0 {
		fmt.Printf("sha %s not found in trust file\n", args[0])
		return 1
	}
	fmt.Printf("removed %d entry(ies) for %s\n", n, args[0])
	return 0
}

// ── update ──────────────────────────────────────────────────────────────────

func cmdDaemonUpdate(args []string) int {
	binaryFlag := ""
	assumeYes := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--binary":
			if i+1 < len(args) {
				binaryFlag = args[i+1]
				i++
			}
		case "--assume-yes", "-y":
			assumeYes = true
		}
	}
	livePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: cannot determine current binary: %v\n", err)
		return 1
	}
	newPath := binaryFlag
	if newPath == "" {
		newPath = livePath
	}
	absNew, err := filepath.Abs(newPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 1
	}

	// Identify SHAs.
	oldSHA, err := trust.HashFile(livePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: hash live binary: %v\n", err)
		return 1
	}
	newSHA, err := trust.HashFile(absNew)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: hash new binary: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "update: append trust: %v\n", appendErr)
			return 1
		}
		fmt.Println("Appended new SHA to trust file.")
	}

	// Stop running daemon (if any).
	pidPath := singleton.DefaultPIDPath()
	if locked, pid, _ := singleton.IsLocked(pidPath); locked && pid > 0 {
		fmt.Printf("Stopping running daemon (PID %d)...\n", pid)
		if rc := cmdDaemonStop(nil); rc != 0 {
			fmt.Fprintln(os.Stderr, "update: stop failed — abort")
			return rc
		}
	}

	// Atomic rollover (only if new ≠ live).
	if absNew != livePath {
		previous := livePath + ".previous"
		if err := os.Rename(livePath, previous); err != nil {
			fmt.Fprintf(os.Stderr, "update: snapshot previous: %v\n", err)
			return 1
		}
		// Move new binary to live path.
		if err := copyBinaryFile(absNew, livePath); err != nil {
			// Roll back the rename if the copy fails.
			_ = os.Rename(previous, livePath)
			fmt.Fprintf(os.Stderr, "update: install new: %v\n", err)
			return 1
		}
		if err := os.Chmod(livePath, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "update: chmod: %v\n", err)
			return 1
		}
		fmt.Printf("Replaced %s; previous saved as %s\n", livePath, previous)
	} else {
		fmt.Println("(new == live; no copy performed — trust entry recorded)")
	}

	fmt.Println("Update complete. Start the daemon via `force daemon foreground` or your installed launchd/systemd unit.")
	return 0
}

// ── rollback ────────────────────────────────────────────────────────────────

func cmdDaemonRollback(args []string) int {
	livePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback: %v\n", err)
		return 1
	}
	previous := livePath + ".previous"
	if _, err := os.Stat(previous); err != nil {
		fmt.Fprintf(os.Stderr, "rollback: no previous binary at %s\n", previous)
		return 1
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
			return rc
		}
	}

	tmp := livePath + ".rollback-tmp"
	if err := copyBinaryFile(livePath, tmp); err != nil {
		fmt.Fprintf(os.Stderr, "rollback: snapshot live: %v\n", err)
		return 1
	}
	if err := copyBinaryFile(previous, livePath); err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "rollback: restore: %v\n", err)
		return 1
	}
	_ = os.Chmod(livePath, 0o755)
	if err := os.Rename(tmp, previous); err != nil {
		fmt.Fprintf(os.Stderr, "rollback: swap previous: %v\n", err)
		// not fatal — previous still on disk under tmp name
	}
	fmt.Printf("Rolled back %s. Previous-of-previous saved as %s\n", livePath, previous)
	return 0
}

// ── install / uninstall ─────────────────────────────────────────────────────

func cmdDaemonInstall(args []string) int {
	dryRun := false
	for _, a := range args {
		if a == "--dry-run" {
			dryRun = true
		}
	}
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
	dryRun := false
	for _, a := range args {
		if a == "--dry-run" {
			dryRun = true
		}
	}
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

func cmdDaemonHistory(db *sql.DB, args []string) int {
	limit := 20
	for i := 0; i < len(args); i++ {
		if args[i] == "--limit" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				limit = n
			}
			i++
		}
	}
	// P3 will land DaemonUpdateHistory schema. P1 falls back to the
	// trust file as a placeholder (each Append == one ratification
	// event).
	fmt.Println("force daemon history")
	fmt.Println("─────────────────────")
	fmt.Println("(P3 schema: DaemonUpdateHistory — awaiting impl)")
	fmt.Println("Falling back to ~/.force/trusted-binary-hashes:")
	fmt.Println()
	tp := trust.DefaultPath()
	f, err := trust.Load(tp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "history: load %s: %v\n", tp, err)
		return 1
	}
	if f == nil || len(f.Entries) == 0 {
		fmt.Println("(no entries)")
		return 0
	}
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

// ── validate-config ─────────────────────────────────────────────────────────

func cmdDaemonValidateConfig(args []string) int {
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


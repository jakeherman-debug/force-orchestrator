// D3 P6A.13 / exit criterion 13 — `force install-sleep-hook` CLI.
//
// Roadmap reference: docs/roadmap.md line 1229, line 1270.
//
// One-time setup that wires the daemon's heartbeat-based sleep
// detection (internal/agents/cinematic.go DetectSleepStartedAt) into
// the OS sleep/wake transition pipeline. On darwin, the integration
// is via sleepwatcher (brew install sleepwatcher) which invokes
// ~/.sleep and ~/.wakeup scripts on system sleep / wake events.
// The scripts here ping the daemon's heartbeat — that's all the
// daemon needs to detect the wake on its own (the heartbeat goroutine
// in cinematic.go infers sleep when it sees a > 90s gap between
// consecutive ticks, so the wake-script's heartbeat ping closes the
// gap immediately and reconciliation runs).
//
// Idempotent: re-running the command is a no-op if hooks are already
// installed. The hook scripts include a marker line so subsequent
// runs detect their own writes and skip rather than duplicate.
//
// Non-darwin platforms: print a helpful message and exit 0. The
// daemon's heartbeat-based sleep detection works on any platform —
// the OS hook is only the integration point that closes the
// post-wake reconciliation latency. Linux operators using the
// daemon get full reconciliation on the next 30s heartbeat tick.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// SleepHookMarker is the recognisable comment header written into
// every sleep hook script the CLI installs. Re-running the command
// looks for this marker to decide whether the file is already
// owned by force or whether it pre-dates the hook (operator
// authored their own ~/.sleep — we never overwrite without
// --force).
const SleepHookMarker = "# force-orchestrator: install-sleep-hook"

// sleepHookHomeFunc resolves the operator's home directory. Set as
// a function variable so tests can redirect to a temp dir.
var sleepHookHomeFunc = os.UserHomeDir

// sleepHookExecLookPath wraps exec.LookPath; tests substitute a
// stub that reports sleepwatcher as either present or absent.
var sleepHookExecLookPath = exec.LookPath

// sleepHookOS reports the current GOOS; tests substitute to drive
// the linux / unsupported branches without a real cross-compile.
var sleepHookOS = func() string { return runtime.GOOS }

// cmdInstallSleepHook is the entry point for `force install-sleep-hook`.
// Returns the desired process exit code (0 = success, 1 = error).
func cmdInstallSleepHook(_ context.Context, _ *sql.DB, args []string) int {
	fs := flag.NewFlagSet("install-sleep-hook", flag.ContinueOnError)
	forceFlag := fs.Bool("force", false, "overwrite operator-authored ~/.sleep / ~/.wakeup scripts")
	checkFlag := fs.Bool("check", false, "report current install state; do not modify files")
	uninstallFlag := fs.Bool("uninstall", false, "remove force-owned hook scripts")
	helped, perr := parseSubcommandFlags(fs, args, "install-sleep-hook",
		"Install ~/.sleep and ~/.wakeup hook scripts so the daemon's heartbeat-based sleep/wake detection runs immediately after wake.",
		[]flagDoc{
			{Name: "--force", Desc: "overwrite operator-authored scripts"},
			{Name: "--check", Desc: "report install state without modifying"},
			{Name: "--uninstall", Desc: "remove force-owned hook scripts"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force install-sleep-hook", "force install-sleep-hook --check"})
	if helped {
		return 0
	}
	if perr != nil {
		return 2
	}
	force := *forceFlag
	check := *checkFlag
	uninstall := *uninstallFlag

	switch sleepHookOS() {
	case "darwin":
		return installSleepHookDarwin(force, check, uninstall)
	case "linux":
		fmt.Println("install-sleep-hook: linux is not the canonical target for sleepwatcher integration.")
		fmt.Println("  The daemon's heartbeat goroutine still detects sleep/wake transitions via the")
		fmt.Println("  > 90s gap heuristic (internal/agents/cinematic.go DetectSleepStartedAt). On linux")
		fmt.Println("  this catches the wake within one heartbeat tick (~30s). Equivalent OS-level")
		fmt.Println("  hooks would route through systemd's sleep.target — out of scope for D3 (see")
		fmt.Println("  docs/roadmap.md exit criterion 13: macOS sleepwatcher is the named integration).")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "install-sleep-hook: GOOS=%s is not supported. The heartbeat-based detection still works in-daemon; the OS-level hook is only available on darwin.\n", sleepHookOS())
		return 0
	}
}

// installSleepHookDarwin handles the macOS branch: check
// sleepwatcher availability, write ~/.sleep and ~/.wakeup, report
// status. Returns the process exit code.
func installSleepHookDarwin(force, check, uninstall bool) int {
	home, err := sleepHookHomeFunc()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install-sleep-hook: resolve home directory: %v\n", err)
		return 1
	}

	sleepPath := filepath.Join(home, ".sleep")
	wakePath := filepath.Join(home, ".wakeup")

	if uninstall {
		return uninstallSleepHookFiles(sleepPath, wakePath)
	}

	// Check sleepwatcher availability — informational only when --check
	// or --force; required for non-check non-force install.
	swPath, swErr := sleepHookExecLookPath("sleepwatcher")
	swInstalled := swErr == nil

	if check {
		return reportSleepHookState(home, sleepPath, wakePath, swInstalled, swPath)
	}

	if !swInstalled && !force {
		fmt.Fprintln(os.Stderr, "install-sleep-hook: sleepwatcher binary not found in PATH.")
		fmt.Fprintln(os.Stderr, "  Install it first: brew install sleepwatcher")
		fmt.Fprintln(os.Stderr, "  Or re-run with --force to write the hook scripts anyway (they're harmless without sleepwatcher).")
		return 1
	}

	// Write ~/.sleep — idempotent, marker-aware.
	if err := writeSleepHookScript(sleepPath, sleepHookSleepScript(), force); err != nil {
		fmt.Fprintf(os.Stderr, "install-sleep-hook: write %s: %v\n", sleepPath, err)
		return 1
	}
	// Write ~/.wakeup — same shape.
	if err := writeSleepHookScript(wakePath, sleepHookWakeScript(), force); err != nil {
		fmt.Fprintf(os.Stderr, "install-sleep-hook: write %s: %v\n", wakePath, err)
		return 1
	}

	fmt.Printf("install-sleep-hook: OK — wrote %s and %s.\n", sleepPath, wakePath)
	if !swInstalled {
		fmt.Println("  Note: sleepwatcher is NOT in PATH. Install it (`brew install sleepwatcher`) and start it (`brew services start sleepwatcher`) for the hooks to fire on sleep/wake events.")
	} else {
		fmt.Printf("  sleepwatcher binary: %s\n", swPath)
		fmt.Println("  If sleepwatcher isn't already running, start it with: brew services start sleepwatcher")
	}
	return 0
}

// reportSleepHookState prints the current install state without
// modifying anything. Used for `--check`.
func reportSleepHookState(home, sleepPath, wakePath string, swInstalled bool, swPath string) int {
	fmt.Printf("install-sleep-hook --check: state report\n")
	fmt.Printf("  home directory: %s\n", home)

	for _, target := range []struct{ path, role string }{
		{sleepPath, "sleep"},
		{wakePath, "wake"},
	} {
		state := classifySleepHookFile(target.path)
		fmt.Printf("  %s (%s): %s\n", target.path, target.role, state)
	}

	if swInstalled {
		fmt.Printf("  sleepwatcher: present at %s\n", swPath)
	} else {
		fmt.Printf("  sleepwatcher: NOT FOUND — install with `brew install sleepwatcher`\n")
	}
	return 0
}

// classifySleepHookFile returns a one-line string describing the
// current state of a hook file: missing / force-owned / operator-
// authored / unreadable.
func classifySleepHookFile(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "missing (will be created)"
		}
		return fmt.Sprintf("unreadable: %v", err)
	}
	if strings.Contains(string(body), SleepHookMarker) {
		return "force-owned (idempotent re-write OK)"
	}
	return "OPERATOR-AUTHORED (re-run with --force to overwrite)"
}

// writeSleepHookScript writes the script to path. Idempotent: if
// the existing file contains SleepHookMarker the new contents
// replace it (force-owned). If the existing file is operator-
// authored (no marker), the write fails unless --force was passed.
// New files are created with mode 0o755 so the OS can exec them
// directly.
func writeSleepHookScript(path, script string, force bool) error {
	if existing, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(existing), SleepHookMarker) {
			// Force-owned — idempotent overwrite.
			return os.WriteFile(path, []byte(script), 0o755)
		}
		if !force {
			return fmt.Errorf("file exists and is operator-authored (no force marker): %s — re-run with --force to overwrite", path)
		}
	}
	return os.WriteFile(path, []byte(script), 0o755)
}

// uninstallSleepHookFiles removes force-owned hook files. Operator-
// authored files are left in place (the marker check is the same
// shape as writeSleepHookScript). Returns 0 on success.
func uninstallSleepHookFiles(sleepPath, wakePath string) int {
	for _, p := range []string{sleepPath, wakePath} {
		body, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Printf("install-sleep-hook --uninstall: %s already absent\n", p)
				continue
			}
			fmt.Fprintf(os.Stderr, "install-sleep-hook --uninstall: read %s: %v\n", p, err)
			return 1
		}
		if !strings.Contains(string(body), SleepHookMarker) {
			fmt.Printf("install-sleep-hook --uninstall: %s is operator-authored (no force marker) — preserved\n", p)
			continue
		}
		if err := os.Remove(p); err != nil {
			fmt.Fprintf(os.Stderr, "install-sleep-hook --uninstall: remove %s: %v\n", p, err)
			return 1
		}
		fmt.Printf("install-sleep-hook --uninstall: removed %s\n", p)
	}
	return 0
}

// sleepHookSleepScript returns the body of ~/.sleep. Sleepwatcher
// invokes this on system sleep. The script logs the event and
// touches a marker file so the wake-side reconciliation can read
// the sleep timestamp from disk if the daemon's heartbeat table
// was rotated. The actual sleep detection is heartbeat-based
// (DetectSleepStartedAt) — this script's job is to leave a
// sufficient breadcrumb for post-mortem.
func sleepHookSleepScript() string {
	return SleepHookMarker + " (sleep.script)\n" +
		"#!/bin/sh\n" +
		"# Invoked by sleepwatcher on system sleep.\n" +
		"# The daemon's heartbeat goroutine detects the sleep on its own\n" +
		"# via the > 90s gap heuristic; this script just leaves a marker\n" +
		"# file the operator can grep post-wake.\n" +
		"date -u +%FT%TZ > \"$HOME/.force-last-sleep\"\n"
}

// sleepHookWakeScript returns the body of ~/.wakeup. Sleepwatcher
// invokes this on system wake. The script pings the daemon's
// heartbeat endpoint (HTTP GET) so the heartbeat row appears
// immediately, closing the gap and triggering DetectSleepStartedAt
// to recognise the wake on the next tick. If the dashboard isn't
// running, curl fails harmlessly.
func sleepHookWakeScript() string {
	return SleepHookMarker + " (wakeup.script)\n" +
		"#!/bin/sh\n" +
		"# Invoked by sleepwatcher on system wake.\n" +
		"# Ping the daemon dashboard's heartbeat endpoint so the heartbeat\n" +
		"# row appears immediately and DetectSleepStartedAt sees the gap.\n" +
		"# If the dashboard isn't running, curl exits non-zero and we\n" +
		"# fall through quietly.\n" +
		"date -u +%FT%TZ > \"$HOME/.force-last-wake\"\n" +
		"curl --silent --max-time 3 http://127.0.0.1:8080/api/dashboard/health > /dev/null 2>&1 || true\n"
}

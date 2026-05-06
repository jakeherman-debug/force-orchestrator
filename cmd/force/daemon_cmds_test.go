package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/daemon/trust"
	"force-orchestrator/internal/store"
)

// TestDispatchDaemon_NoArgs_FallsToCmdDaemon: bare `force daemon`
// with no subcommand should NOT take the new dispatcher path
// directly — it falls through to cmdDaemon (which we don't run here
// because it'd block). We just verify dispatchDaemon is exported
// and accepts the right shape.
func TestDispatchDaemon_Signature(t *testing.T) {
	// Smoke check: type assertions only.
	_ = dispatchDaemon
}

// TestDashboardPortFromConfig_Default: returns 41977 when SystemConfig
// has no key.
func TestDashboardPortFromConfig_Default(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if got := dashboardPortFromConfig(db); got != 41977 {
		t.Errorf("dashboardPortFromConfig() = %d, want 41977", got)
	}
}

// TestDashboardPortFromConfig_Override: SystemConfig key wins.
func TestDashboardPortFromConfig_Override(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.SetConfig(db, "dashboard_port", "9999")
	if got := dashboardPortFromConfig(db); got != 9999 {
		t.Errorf("dashboardPortFromConfig() = %d, want 9999", got)
	}
}

// TestDashboardEnabledFromConfig_DefaultTrue: missing key → true.
func TestDashboardEnabledFromConfig_DefaultTrue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if !dashboardEnabledFromConfig(db) {
		t.Errorf("default should be enabled=true")
	}
}

// TestDashboardEnabledFromConfig_FalseValues: false / 0 / no all
// disable.
func TestDashboardEnabledFromConfig_FalseValues(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	for _, val := range []string{"false", "0", "no"} {
		store.SetConfig(db, "dashboard_enabled", val)
		if dashboardEnabledFromConfig(db) {
			t.Errorf("dashboard_enabled=%q should disable, got enabled", val)
		}
	}
}

// TestCmdDaemonStatus_NotRunning_ReturnsNonZero: status returns 1
// when no daemon is running. We redirect HOME to a tmp dir so we
// don't disturb the user's real ~/.force/force.pid.
func TestCmdDaemonStatus_NotRunning_ReturnsNonZero(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	rc := cmdDaemonStatus(db, nil)
	if rc != 1 {
		t.Errorf("status with no daemon should return 1, got %d", rc)
	}
}

// TestCmdDaemonStop_NotRunning: returns 0 with informative msg.
func TestCmdDaemonStop_NotRunning(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	rc := cmdDaemonStop(nil)
	if rc != 0 {
		t.Errorf("stop with no daemon should return 0, got %d", rc)
	}
}

// TestCmdDaemonTrust_AddListRemove: round-trip a SHA through the
// trust file via the CLI surface.
func TestCmdDaemonTrust_AddListRemove(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a tiny fake binary.
	binPath := filepath.Join(tmpHome, "fake-binary")
	if err := os.WriteFile(binPath, []byte("hello"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" // SHA256("hello")

	rc := cmdDaemonTrustAdd([]string{binPath})
	if rc != 0 {
		t.Fatalf("trust add: rc=%d", rc)
	}

	tp := trust.DefaultPath()
	f, err := trust.Load(tp)
	if err != nil {
		t.Fatalf("trust.Load: %v", err)
	}
	if !f.HasSHA(want) {
		t.Errorf("trust file missing SHA after add (entries=%d)", len(f.Entries))
	}

	rc = cmdDaemonTrustRemove([]string{want})
	if rc != 0 {
		t.Errorf("trust remove rc=%d", rc)
	}
	f2, _ := trust.Load(tp)
	if f2.HasSHA(want) {
		t.Errorf("trust file still has SHA after remove")
	}
}

// TestCmdDaemonInstall_DryRun: --dry-run must not actually write the
// plist/unit.
func TestCmdDaemonInstall_DryRun(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	rc := cmdDaemonInstall([]string{"--dry-run"})
	if rc != 0 {
		t.Errorf("install --dry-run rc=%d", rc)
	}

	// Neither path should exist.
	for _, p := range []string{launchdPlistPath(), systemdUnitPath()} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("--dry-run wrote %s", p)
		}
	}
}

// TestCmdDaemonValidateConfig_AllSkippable: when no config files exist
// (typical test cwd), every probe is a [skip] — exit 0.
func TestCmdDaemonValidateConfig_AllSkippable(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(dir)
	rc := cmdDaemonValidateConfig(nil)
	if rc != 0 {
		t.Errorf("validate-config with no configs should be 0, got %d", rc)
	}
}

// TestCmdDaemonValidateConfig_DetectsEmptyFile: an empty config file
// is reported as a failure.
func TestCmdDaemonValidateConfig_DetectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(dir)

	if err := os.MkdirAll("config", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile("config/notifications.yaml", []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rc := cmdDaemonValidateConfig(nil)
	if rc == 0 {
		t.Errorf("expected non-zero on empty config")
	}
}

// TestCmdDaemonHistory_FallsBackToTrustFile: with no trust file,
// prints "(no entries)" and returns 0.
func TestCmdDaemonHistory_EmptyTrustFile(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	rc := cmdDaemonHistory(db, nil)
	if rc != 0 {
		t.Errorf("history with empty trust should be 0, got %d", rc)
	}
}

// TestLaunchdPlistTemplate_HasRequiredKeys: the rendered plist
// contains the essential launchd keys so a real `launchctl load`
// would not fail.
func TestLaunchdPlistTemplate_HasRequiredKeys(t *testing.T) {
	plist := launchdPlistTemplate("/usr/local/bin/force")
	for _, want := range []string{
		"<key>Label</key>",
		"<key>ProgramArguments</key>",
		"<key>RunAtLoad</key>",
		"daemon",
		"foreground",
		"/usr/local/bin/force",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

// TestSystemdUnitTemplate_HasRequiredKeys.
func TestSystemdUnitTemplate_HasRequiredKeys(t *testing.T) {
	unit := systemdUnitTemplate("/usr/local/bin/force")
	for _, want := range []string{
		"[Unit]",
		"[Service]",
		"[Install]",
		"ExecStart=/usr/local/bin/force daemon foreground",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

// TestCmdDaemonUpdate_AssumeYes_AppendsTrust: simulate a fresh trust
// file, run update --assume-yes against the same binary (so no
// rollover occurs), and verify a trust entry was appended.
func TestCmdDaemonUpdate_AssumeYes_AppendsTrust(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Use the test binary (os.Args[0]) as both old + new — that path
	// always exists. Update sees same SHA on both sides; a trust entry
	// should still be appended (paranoia mode default-on).
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	rc := cmdDaemonUpdate(nil, []string{"--binary", exe, "--assume-yes"})
	if rc != 0 {
		t.Fatalf("update --assume-yes rc=%d", rc)
	}
	f, err := trust.Load(trust.DefaultPath())
	if err != nil {
		t.Fatalf("Load trust: %v", err)
	}
	if len(f.Entries) != 1 {
		t.Errorf("trust entries = %d, want 1", len(f.Entries))
	}

	// Run again — already trusted, no new entry appended.
	rc = cmdDaemonUpdate(nil, []string{"--binary", exe, "--assume-yes"})
	if rc != 0 {
		t.Fatalf("update second pass rc=%d", rc)
	}
	f2, _ := trust.Load(trust.DefaultPath())
	if len(f2.Entries) != 1 {
		t.Errorf("re-update should not add duplicate entry, got %d", len(f2.Entries))
	}
}

// ── D12 P3 tests ────────────────────────────────────────────────────────────

// TestCmdDaemonUpdate_RecordsToHistory: a successful trust ratification
// against the test binary records an outcome='success' row.
func TestCmdDaemonUpdate_RecordsToHistory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	rc := cmdDaemonUpdate(db, []string{"--binary", exe, "--assume-yes"})
	if rc != 0 {
		t.Fatalf("update --assume-yes rc=%d", rc)
	}
	entries, err := store.ListDaemonUpdateHistory(db, 10)
	if err != nil {
		t.Fatalf("ListDaemonUpdateHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 history row, got %d", len(entries))
	}
	if entries[0].Outcome != "success" {
		t.Errorf("outcome = %q, want success", entries[0].Outcome)
	}
	if entries[0].OldBinarySHA == "" || entries[0].NewBinarySHA == "" {
		t.Errorf("expected old/new SHA populated, got %+v", entries[0])
	}
}

// TestCmdDaemonClearCrashBudget_HappyPath: --assume-yes truncates the
// DaemonStartLog and writes an AuditLog row.
func TestCmdDaemonClearCrashBudget_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 3; i++ {
		_ = store.RecordDaemonStart(db, "bin", "git", i)
	}
	rc := cmdDaemonClearCrashBudget(db, []string{"--assume-yes"})
	if rc != 0 {
		t.Fatalf("clear-crash-budget rc=%d", rc)
	}
	entries, _ := store.ListDaemonStarts(db, 10)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries post-clear, got %d", len(entries))
	}
	audits := store.ListAuditLog(db, 10)
	found := false
	for _, a := range audits {
		if a.Action == "daemon.clear-crash-budget" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected an AuditLog row with action=daemon.clear-crash-budget, got: %+v", audits)
	}
}

// TestCmdDaemonClearCrashBudget_AbortPath: stdin returns "no" → rc=1
// and the table is NOT truncated.
func TestCmdDaemonClearCrashBudget_AbortPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_ = store.RecordDaemonStart(db, "bin", "git", 1)
	pre, _ := store.ListDaemonStarts(db, 10)
	if len(pre) != 1 {
		t.Fatalf("seed wrong: %d", len(pre))
	}

	// Simulate stdin "no\n" without --assume-yes.
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()
	go func() {
		_, _ = w.Write([]byte("no\n"))
		_ = w.Close()
	}()

	rc := cmdDaemonClearCrashBudget(db, nil)
	if rc != 1 {
		t.Errorf("expected rc=1 on abort, got %d", rc)
	}
	post, _ := store.ListDaemonStarts(db, 10)
	if len(post) != 1 {
		t.Errorf("table modified despite abort: %d entries", len(post))
	}
}

// TestLaunchdPlistTemplate_HasCrashedKey: D12 P3 — the plist must
// auto-restart on crash AND on non-zero exit.
func TestLaunchdPlistTemplate_HasCrashedKey(t *testing.T) {
	plist := launchdPlistTemplate("/usr/local/bin/force")
	for _, want := range []string{
		"<key>Crashed</key>",
		"<true/>",
		"<key>SuccessfulExit</key>",
		"<false/>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing required KeepAlive sub-key %q", want)
		}
	}
}

// TestSystemdUnitTemplate_HasRestartOnFailure: D12 P3 — the unit must
// restart on failure with a 5-second delay.
func TestSystemdUnitTemplate_HasRestartOnFailure(t *testing.T) {
	unit := systemdUnitTemplate("/usr/local/bin/force")
	for _, want := range []string{
		"Restart=on-failure",
		"RestartSec=5",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit missing %q", want)
		}
	}
}

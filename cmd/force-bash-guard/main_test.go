package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withCurlHosts injects a temporary host allowlist into the config used
// by tests. Returns a guardConfig with the supplied hosts and a
// per-test log path so concurrent tests don't fight over bash.log.
func withCurlHosts(t *testing.T, hosts ...string) guardConfig {
	t.Helper()
	dir := t.TempDir()
	return guardConfig{
		curlHosts:   hosts,
		logMaxBytes: defaultLogMaxBytes,
		logPath:     filepath.Join(dir, "bash.log"),
	}
}

// ── allow-path tests ──────────────────────────────────────────────────────

func TestBashGuard_AllowsGitStatus(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("git status", cfg)
	if !v.allowed {
		t.Fatalf("git status rejected: %s", v.reason)
	}
}

func TestBashGuard_AllowsGoTest(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("go test ./...", cfg)
	if !v.allowed {
		t.Fatalf("go test rejected: %s", v.reason)
	}
}

func TestBashGuard_AllowsCurlAllowedHost(t *testing.T) {
	cfg := withCurlHosts(t, "api.github.com")
	v := evaluateCompound("curl https://api.github.com/repos/foo/bar", cfg)
	if !v.allowed {
		t.Fatalf("curl with allowed host rejected: %s", v.reason)
	}
}

func TestBashGuard_AllowsCompoundOfAllowedSegments(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("git status && go build ./...", cfg)
	if !v.allowed {
		t.Fatalf("compound of allowed segments rejected: %s", v.reason)
	}
}

// ── deny-path tests ───────────────────────────────────────────────────────

func TestBashGuard_RejectsRmRfHome(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("rm -rf ~/Documents", cfg)
	if v.allowed {
		t.Fatalf("rm -rf ~/Documents was allowed (should be denied)")
	}
}

func TestBashGuard_RejectsRmRfRoot(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("rm -rf /", cfg)
	if v.allowed {
		t.Fatalf("rm -rf / was allowed (should be denied)")
	}
}

func TestBashGuard_RejectsCompoundWithDenied(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("git status && rm -rf /tmp/foo", cfg)
	if v.allowed {
		t.Fatalf("compound with rm rejected segment was allowed")
	}
	if !strings.Contains(v.reason, "rm") {
		t.Errorf("rejection reason should mention rm: %q", v.reason)
	}
}

func TestBashGuard_RejectsPathTraversal(t *testing.T) {
	cfg := withCurlHosts(t)
	// We can't use rm because rm itself is denied; pick a tool that's
	// allowed but whose argument resolves under /etc.
	v := evaluateCompound("cat /../../etc/hosts", cfg)
	if v.allowed {
		t.Fatalf("cat /../../etc/hosts allowed (should resolve and deny)")
	}
}

func TestBashGuard_RejectsSedInPlace(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("sed -i 's/foo/bar/' file.txt", cfg)
	if v.allowed {
		t.Fatalf("sed -i was allowed")
	}
}

func TestBashGuard_RejectsCurlDisallowedHost(t *testing.T) {
	cfg := withCurlHosts(t, "api.github.com") // evil.example.com NOT in list
	v := evaluateCompound("curl https://evil.example.com/loot", cfg)
	if v.allowed {
		t.Fatalf("curl evil.example.com was allowed (host allowlist breach)")
	}
}

func TestBashGuard_RejectsForkBomb(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound(":(){ :|:& };:", cfg)
	if v.allowed {
		t.Fatalf("fork bomb pattern allowed")
	}
}

func TestBashGuard_RejectsSudo(t *testing.T) {
	cfg := withCurlHosts(t)
	for _, c := range []string{"sudo ls", "su -", "doas cat /etc/shadow"} {
		v := evaluateCompound(c, cfg)
		if v.allowed {
			t.Errorf("%q was allowed", c)
		}
	}
}

func TestBashGuard_RejectsEtcWrites(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("cat /etc/passwd", cfg)
	if v.allowed {
		t.Fatalf("read of /etc/passwd allowed")
	}
}

func TestBashGuard_RejectsSSHRead(t *testing.T) {
	cfg := withCurlHosts(t)
	// `~/.ssh/id_rsa` resolves to a credential path.
	v := evaluateCompound("cat ~/.ssh/id_rsa", cfg)
	if v.allowed {
		t.Fatalf("read of ~/.ssh/id_rsa allowed")
	}
}

func TestBashGuard_RejectsCommandSubstitution(t *testing.T) {
	cfg := withCurlHosts(t)
	for _, c := range []string{
		"echo $(whoami)",
		"echo `whoami`",
		"diff <(ls) <(cat foo)",
	} {
		v := evaluateCompound(c, cfg)
		if v.allowed {
			t.Errorf("%q was allowed (substitution should be denied)", c)
		}
	}
}

func TestBashGuard_RejectsUnknownProgram(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("nethack", cfg)
	if v.allowed {
		t.Fatalf("unknown program nethack was allowed")
	}
}

func TestBashGuard_RejectsBashEnvOverride(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("BASH_ENV=/tmp/evil.sh git status", cfg)
	if v.allowed {
		t.Fatalf("BASH_ENV override was allowed")
	}
}

func TestBashGuard_RejectsFindExec(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("find . -name '*.tmp' -exec rm {} ;", cfg)
	if v.allowed {
		t.Fatalf("find -exec was allowed")
	}
}

func TestBashGuard_AllowsFindPrint(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("find . -name '*.go' -type f", cfg)
	if !v.allowed {
		t.Fatalf("plain find rejected: %s", v.reason)
	}
}

func TestBashGuard_RejectsChmodRecursive(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("chmod -R 777 .", cfg)
	if v.allowed {
		t.Fatalf("chmod -R was allowed")
	}
}

func TestBashGuard_AllowsChmodNumeric(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("chmod 644 file.go", cfg)
	if !v.allowed {
		t.Fatalf("chmod 644 file.go rejected: %s", v.reason)
	}
}

func TestBashGuard_RejectsKillInit(t *testing.T) {
	cfg := withCurlHosts(t)
	v := evaluateCompound("kill -9 1", cfg)
	if v.allowed {
		t.Fatalf("kill -9 1 was allowed")
	}
}

// ── logging tests ─────────────────────────────────────────────────────────

func TestBashGuard_LogsAllowed(t *testing.T) {
	cfg := withCurlHosts(t)
	writeLogEntry(cfg, "allowed", "git status", "")
	body := readLog(t, cfg.logPath)
	if !strings.Contains(body, "allowed") {
		t.Errorf("log missing 'allowed' marker; got: %q", body)
	}
	if !strings.Contains(body, "git status") {
		t.Errorf("log missing command line; got: %q", body)
	}
}

func TestBashGuard_LogsRejected(t *testing.T) {
	cfg := withCurlHosts(t)
	writeLogEntry(cfg, "rejected", "rm -rf /", "rm not on allowlist")
	body := readLog(t, cfg.logPath)
	if !strings.Contains(body, "rejected") {
		t.Errorf("log missing 'rejected' marker; got: %q", body)
	}
	if !strings.Contains(body, "not on allowlist") {
		t.Errorf("log missing rejection reason; got: %q", body)
	}
}

func TestBashGuard_LogRotatesAtSizeCap(t *testing.T) {
	dir := t.TempDir()
	cfg := guardConfig{logMaxBytes: 64, logPath: filepath.Join(dir, "bash.log")}
	for i := 0; i < 50; i++ {
		writeLogEntry(cfg, "allowed", "git status", "")
	}
	if _, err := os.Stat(cfg.logPath + ".1"); err != nil {
		t.Errorf("expected rotated log at %s.1: %v", cfg.logPath, err)
	}
}

// ── tokenizer / compound-split unit tests ─────────────────────────────────

func TestSplitCompound_QuoteAware(t *testing.T) {
	segs, err := splitCompound(`echo "a && b" && git status`)
	if err != nil {
		t.Fatalf("splitCompound: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("len(segs) = %d, want 2 (got %v)", len(segs), segs)
	}
}

func TestTokenize_SingleQuotedBlocksEscape(t *testing.T) {
	toks, err := tokenize(`echo 'a b\nc'`)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if len(toks) != 2 || toks[0] != "echo" {
		t.Fatalf("unexpected tokens: %v", toks)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

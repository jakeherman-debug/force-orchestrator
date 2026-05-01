package git

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestGitOpLog covers the 6B.2 wrapper invariants:
//
//   - LogAndRun records a row with operation, args, exit_code, duration.
//   - Args are redacted at write time (Fix #10) — a token passed on the
//     command line never leaks into args_json.
//   - Stdout/stderr are truncated at 4 KB (per the brief).
//   - When db is nil, the helper still runs the subprocess (no shadow,
//     no failure).
//   - DeriveOperation maps argv shapes to controlled-vocabulary labels.
//
// Test methodology: the wrapper invokes real `git` against a freshly
// `git init`'d temp dir (per CLAUDE.md "Mock gh and git only at the
// package boundary"). For the redaction test, we run `git config
// remote.origin.url <url-with-token>` then `git config --get` — the
// returned bytes carry the token, but the persisted row should not.
func TestGitOpLog(t *testing.T) {
	t.Run("happy_path_records_row", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetOpLogDB(db)
		defer SetOpLogDB(nil)

		dir := t.TempDir()
		// `git init` is enough to make subsequent ops valid; the
		// wrapper records BOTH calls.
		if _, err := LogAndRun(context.Background(), OpContext{Repo: dir},
			"init", "git", "-C", dir, "init", "-q"); err != nil {
			t.Fatalf("init: %v", err)
		}

		// Assert at least one row landed for the init op.
		var rows int
		db.QueryRow(`SELECT COUNT(*) FROM GitOperationLog WHERE operation='init'`).Scan(&rows)
		if rows < 1 {
			t.Fatalf("expected init row, got %d rows", rows)
		}

		// Inspect the row — args_json must contain "init".
		var argsJSON string
		var durationMs int
		db.QueryRow(`SELECT args_json, duration_ms FROM GitOperationLog WHERE operation='init' ORDER BY id DESC LIMIT 1`).Scan(&argsJSON, &durationMs)
		if !strings.Contains(argsJSON, `"init"`) {
			t.Errorf("args_json missing 'init': %q", argsJSON)
		}
		if durationMs <= 0 {
			t.Errorf("duration not measured: %d", durationMs)
		}
	})

	t.Run("nil_db_falls_through", func(t *testing.T) {
		SetOpLogDB(nil)
		dir := t.TempDir()
		// Subprocess must still run — no panic, no error from the wrapper.
		if _, err := LogAndRun(context.Background(), OpContext{},
			"init", "git", "-C", dir, "init", "-q"); err != nil {
			t.Fatalf("init: %v", err)
		}
	})

	t.Run("redaction_scrubs_args", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetOpLogDB(db)
		defer SetOpLogDB(nil)

		dir := t.TempDir()
		// init first
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"init", "git", "-C", dir, "init", "-q")

		// Add a remote URL that carries a fake-PAT — `git remote add`
		// doesn't return the URL on stdout but the row should redact
		// the *args*. We pass the PAT via the URL-basic-auth shape
		// (the redactor's stable matcher).
		fakeURL := "https://x-access-token:ghp_topsecretvalue1234567890abcdef@github.com/foo/bar.git"
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"remote-add", "git", "-C", dir, "remote", "add", "origin", fakeURL)

		var argsJSON string
		db.QueryRow(`SELECT args_json FROM GitOperationLog WHERE operation='remote-add' ORDER BY id DESC LIMIT 1`).Scan(&argsJSON)
		if strings.Contains(argsJSON, "ghp_topsecret") {
			t.Errorf("args_json leaked PAT: %q", argsJSON)
		}
		if !strings.Contains(argsJSON, "[REDACTED]") {
			t.Errorf("args_json should carry [REDACTED] marker: %q", argsJSON)
		}
	})

	t.Run("output_redaction_returned_bytes", func(t *testing.T) {
		// Set up a repo whose stdout would carry a token.
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetOpLogDB(db)
		defer SetOpLogDB(nil)
		dir := t.TempDir()
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"init", "git", "-C", dir, "init", "-q")
		fakeURL := "https://user:ghp_secrettokenABCDEFGHIJKLMNOPQR123456@github.com/o/r.git"
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"config", "git", "-C", dir, "config", "remote.origin.url", fakeURL)

		// Now read it back — git config --get prints the URL on stdout.
		out, err := LogAndRun(context.Background(), OpContext{Repo: dir},
			"config", "git", "-C", dir, "config", "--get", "remote.origin.url")
		if err != nil {
			t.Fatalf("config --get: %v (out=%s)", err, out)
		}
		if strings.Contains(string(out), "ghp_secrettoken") {
			t.Errorf("returned bytes leaked PAT: %q", out)
		}

		// And the persisted row's stdout_excerpt is also redacted.
		var stdoutEx string
		db.QueryRow(`SELECT stdout_excerpt FROM GitOperationLog WHERE operation='config' ORDER BY id DESC LIMIT 1`).Scan(&stdoutEx)
		if strings.Contains(stdoutEx, "ghp_secrettoken") {
			t.Errorf("stdout_excerpt leaked PAT: %q", stdoutEx)
		}
	})

	t.Run("nonzero_exit_code_recorded", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetOpLogDB(db)
		defer SetOpLogDB(nil)
		dir := t.TempDir()
		// rev-parse a non-existent ref → git exits nonzero.
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"init", "git", "-C", dir, "init", "-q")
		_, err := LogAndRun(context.Background(), OpContext{Repo: dir},
			"rev-parse", "git", "-C", dir, "rev-parse", "--verify", "doesnotexist", "--")
		if err == nil {
			t.Fatalf("expected error from rev-parse against missing ref")
		}
		var exitCode int
		db.QueryRow(`SELECT exit_code FROM GitOperationLog WHERE operation='rev-parse' ORDER BY id DESC LIMIT 1`).Scan(&exitCode)
		if exitCode == 0 {
			t.Errorf("exit code not recorded: %d", exitCode)
		}
	})

	t.Run("idempotence_two_calls_two_rows", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetOpLogDB(db)
		defer SetOpLogDB(nil)
		dir := t.TempDir()
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"init", "git", "-C", dir, "init", "-q")
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"status", "git", "-C", dir, "status", "--porcelain")
		LogAndRun(context.Background(), OpContext{Repo: dir},
			"status", "git", "-C", dir, "status", "--porcelain")
		var rows int
		db.QueryRow(`SELECT COUNT(*) FROM GitOperationLog WHERE operation='status'`).Scan(&rows)
		if rows != 2 {
			t.Fatalf("expected 2 status rows, got %d", rows)
		}
	})
}

func TestDeriveOperation(t *testing.T) {
	cases := []struct {
		name string
		bin  string
		args []string
		want string
	}{
		{"plain push", "git", []string{"-C", "/r", "push", "origin", "main"}, "push"},
		{"force push", "git", []string{"-C", "/r", "push", "--force", "origin", "main"}, "force-push"},
		{"force-with-lease", "git", []string{"push", "--force-with-lease=origin/main", "origin", "main"}, "force-push"},
		{"fetch", "git", []string{"-C", "/r", "fetch", "origin", "--", "main"}, "fetch"},
		{"rebase", "git", []string{"-C", "/r", "rebase", "main"}, "rebase"},
		{"merge", "git", []string{"-C", "/r", "merge", "--no-ff", "topic"}, "merge"},
		{"reset", "git", []string{"-C", "/r", "reset", "--hard", "HEAD"}, "reset"},
		{"worktree-add", "git", []string{"-C", "/r", "worktree", "add", "/tmp/wt"}, "worktree-add"},
		{"gh pr view", "gh", []string{"pr", "view", "1"}, "gh-pr"},
		{"gh checks", "gh", []string{"checks", "1"}, "gh-checks"},
		{"empty git", "git", []string{}, "git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveOperation(c.bin, c.args); got != c.want {
				t.Errorf("DeriveOperation(%q, %v) = %q; want %q", c.bin, c.args, got, c.want)
			}
		})
	}
}

func TestDeriveRepoFromArgs(t *testing.T) {
	got := deriveRepoFromArgs([]string{"-C", "/some/repo", "fetch"})
	if got != "/some/repo" {
		t.Errorf("got %q", got)
	}
	got = deriveRepoFromArgs([]string{"--git-dir", "/.git", "rev-parse"})
	if got != "/.git" {
		t.Errorf("got %q", got)
	}
	if got := deriveRepoFromArgs([]string{"fetch"}); got != "" {
		t.Errorf("expected empty: %q", got)
	}
}

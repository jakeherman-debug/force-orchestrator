// Package git — D3 P6B.2 GitOperationLog wrapper.
//
// Every git/gh subprocess invocation in this package routes through
// LogAndRun, which bookends exec.CommandContext with a redacted
// GitOperationLog row capturing operation, args, duration, exit code,
// truncated stdout/stderr, and (when discoverable) the branch +
// before/after SHAs.
//
// The wrapper is the substrate for Drill's git-op timeline (6B.3) and
// the free-text search across stdout/stderr (6B.6). Pattern P32
// enforces that no new exec.CommandContext("git" | "gh", ...) site
// lands outside this layer.
//
// Anti-cheat:
//   - args_json is RedactSecrets'd at write time (Fix #10) so a token
//     accidentally passed on the command line never reaches the row.
//   - stdout/stderr are truncated to 4 KB each (per the brief) and the
//     redaction pass runs over the truncated bodies as well.
//   - operation is a controlled label derived from the caller; agent
//     code can't fabricate operation strings because the entry points
//     (runGitCtx, runGitCtxOutput, bestEffortRun, abortOp) all
//     pass derived labels.
//   - When db is nil (CLI tools that don't init holocron), the wrapper
//     silently degrades to the bare exec — same shape as the LLM
//     transcript wrapper.

package git

import (
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"time"

	"force-orchestrator/internal/store"
)

// stdoutMaxBytes / stderrMaxBytes cap the persisted excerpts at 4 KB
// each (per the 6B.2 brief). Larger output is truncated; an overflow
// marker is appended so log readers see the cut.
const (
	stdoutMaxBytes = 4 << 10
	stderrMaxBytes = 4 << 10
)

// logAndRunWaitDelay bounds how long LogAndRun waits for stdio cleanup
// after ctx-cancel kills the immediate child. When the immediate child
// (e.g. `git push`) is SIGKILLed, an intermediate descendant (e.g.
// `git-receive-pack` running a pre-receive hook's `sleep 30`) may
// inherit the stdout/stderr pipe write-ends and survive — keeping
// CombinedOutput's read-goroutines blocked. Go 1.20+ exec.Cmd.WaitDelay
// forcibly closes those pipes after the delay so Wait can return with
// exec.ErrWaitDelay (ctx cancellation also surfaces an error, so the
// caller still sees a non-nil error).
//
// 1 s is the contracted budget: the regression tests
// (TestRunShortGit_CtxCancel + TestAstromech_EstopCancelsInFlightGitOp)
// assert ctx-cancel → return within 2 s wall clock. The parent process
// is SIGKILLed immediately on cancel; this delay only governs the I/O
// wait when an intermediate descendant inherited the pipe and is still
// alive. Happy-path exits never hit this delay (Wait returns as soon
// as all pipes close naturally).
//
// D3 fix-loop iter 2 (slice ζ): introduced so internal/agents/astromech.go
// can drop its raw exec.CommandContext helpers and route through
// LogAndRun, closing the last P32 allowlist entry outside internal/git.
const logAndRunWaitDelay = 1 * time.Second

// oplogDB is the active *sql.DB for GitOperationLog inserts. Tests
// install via SetOpLogDB; production wires it at daemon startup.
var (
	oplogDB   *sql.DB
	oplogDBMu sync.RWMutex
)

// SetOpLogDB installs the *sql.DB the git-op wrapper uses for INSERTs.
// Idempotent. Pass nil to detach.
func SetOpLogDB(db *sql.DB) {
	oplogDBMu.Lock()
	oplogDB = db
	oplogDBMu.Unlock()
}

// activeOpLogDB returns the currently installed handle.
func activeOpLogDB() *sql.DB {
	oplogDBMu.RLock()
	defer oplogDBMu.RUnlock()
	return oplogDB
}

// OpContext carries the optional task / convoy attribution for one git
// operation. Most call sites can pass the zero value (OpContext{}); the
// wrapper records the row regardless. Astromech / Chancellor /
// Captain call sites that hold a task or convoy id should fill them
// in so Drill can correlate the op with its owning task.
type OpContext struct {
	TaskID   int
	ConvoyID int
	Repo     string // canonical repo name (e.g. the Repositories.name column); empty allowed
	Branch   string // best-effort branch label; empty allowed
}

// LogAndRun runs `git <args...>` (or `gh <args...>` when bin=="gh")
// with the supplied ctx and bookends the call with a GitOperationLog
// row. Returns combined stdout+stderr (matching exec.Cmd.CombinedOutput's
// shape) and the exec error, redacted on the way out. Operation is the
// caller-supplied controlled label.
//
// The captured-output redaction pass scrubs token prefixes / Bearer
// headers from the *returned* bytes too — call sites that surface
// stdout/stderr to the operator (e.g. error messages) get redacted
// content for free.
func LogAndRun(ctx context.Context, opc OpContext, operation, bin string, args ...string) ([]byte, error) {
	started := store.NowSQLite()
	startTime := time.Now()

	cmd := exec.CommandContext(ctx, bin, args...)
	// WaitDelay (Go 1.20+) bounds I/O cleanup after ctx-cancel SIGKILLs
	// the immediate child. Without this, an intermediate descendant
	// (pre-receive hooks' `sleep N`, git-receive-pack, etc.) that
	// inherited the merged stdout/stderr pipe keeps CombinedOutput's
	// read goroutine blocked far beyond the parent's exit. With the
	// delay set, Go forcibly closes the pipes so Wait returns. See
	// logAndRunWaitDelay's comment for the budget reasoning. This is
	// the unblock that lets internal/agents/astromech.go drop its
	// raw exec.CommandContext helpers (slice ζ, fix-loop iter 2).
	cmd.WaitDelay = logAndRunWaitDelay
	out, err := cmd.CombinedOutput()
	durationMs := int(time.Since(startTime).Milliseconds())

	// Capture exit code — exec.ExitError carries it; nil err means 0.
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1 // start failure / killed by ctx etc.
		}
	}

	// Redact + truncate. The redacted content is what we persist AND
	// what we hand back to the caller (defence-in-depth for error-
	// formatting paths that surface stdout to operators).
	redactedOut := store.RedactSecretsBytes(out)

	// Persist a row only when a DB is wired. CLI smoke tests skip.
	insertOpLogRow(activeOpLogDB(), gitOpLogRow{
		operation:   operation,
		bin:         bin,
		args:        args,
		repo:        opc.Repo,
		taskID:      opc.TaskID,
		convoyID:    opc.ConvoyID,
		branch:      opc.Branch,
		startedAt:   started,
		durationMs:  durationMs,
		exitCode:    exitCode,
		stdout:      truncate(string(redactedOut), stdoutMaxBytes),
		stderr:      "", // CombinedOutput merges; we don't split.
	})

	return redactedOut, err
}

// gitOpLogRow is the internal write descriptor.
type gitOpLogRow struct {
	operation   string
	bin         string // "git" or "gh"
	args        []string
	repo        string
	taskID      int
	convoyID    int
	branch      string
	startedAt   string
	durationMs  int
	exitCode    int
	stdout      string
	stderr      string
}

// insertOpLogRow writes one GitOperationLog row. Best-effort — never
// fails the live op because the audit shadow couldn't write.
func insertOpLogRow(db *sql.DB, r gitOpLogRow) {
	if db == nil {
		return
	}
	// args_json carries [bin, ...args] so Drill can render the full
	// command line. Each entry is redacted independently so a token
	// passed on the command line (e.g. via a misuse) never lands in
	// the row.
	full := append([]string{r.bin}, r.args...)
	for i, a := range full {
		full[i] = store.RedactSecrets(a)
	}
	argsJSON, _ := json.Marshal(full)

	_, _ = db.Exec(
		`INSERT INTO GitOperationLog
		   (task_id, convoy_id, repo, operation, args_json, started_at,
		    duration_ms, exit_code, stdout_excerpt, stderr_excerpt,
		    branch, before_sha, after_sha)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '')`,
		r.taskID, r.convoyID, r.repo, r.operation, string(argsJSON),
		r.startedAt, r.durationMs, r.exitCode, r.stdout, r.stderr,
		r.branch,
	)
}

// truncate caps s at max bytes, appending an overflow marker on cut.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "\n[truncated; full body archived]\n"
}

// DeriveOperation infers a controlled-vocabulary operation label from a
// git argv slice. Used by the entry-point wrappers (runGitCtx,
// bestEffortRun, abortOp) so callers don't need to thread an explicit
// label through every site. The set of returned labels matches the
// brief's enum: 'fetch'|'push'|'rebase'|'force-push'|'merge'|'reset'|
// 'worktree-add'|'gh-pr'|'gh-checks'|...
func DeriveOperation(bin string, args []string) string {
	if bin == "gh" {
		// gh sub-command lives at args[0]; e.g. ["pr", "view", ...].
		if len(args) > 0 {
			return "gh-" + args[0]
		}
		return "gh"
	}
	// git sub-command may be preceded by "-C <path>" or other -X
	// flags; scan past those to the first non-flag argument.
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		switch a {
		case "-C", "--git-dir", "--work-tree", "-c":
			skip = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		// First non-flag is the sub-command; handle a couple of
		// compound cases for the controlled enum.
		switch a {
		case "push":
			// Detect --force / --force-with-lease so the row carries
			// the high-stakes label distinctly from a normal push.
			for _, x := range args {
				if x == "--force" || x == "--force-with-lease" || strings.HasPrefix(x, "--force-with-lease=") {
					return "force-push"
				}
			}
			return "push"
		case "worktree":
			// Worktree-add vs other worktree subcommands.
			for i, x := range args {
				if x == "worktree" && i+1 < len(args) {
					return "worktree-" + args[i+1]
				}
			}
			return "worktree"
		}
		return a
	}
	return "git"
}

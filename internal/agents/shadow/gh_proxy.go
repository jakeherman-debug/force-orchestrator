package shadow

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"force-orchestrator/internal/gh"
)

// GhRecording is a single recorded gh CLI invocation in a JSONL file.
// One record per gh call: command + args + the response that came back
// (in shadow mode, may be the cached real-arm response; in real mode,
// the live response from the actual gh binary).
type GhRecording struct {
	// Timestamp is RFC 3339 UTC.
	Timestamp string `json:"ts"`

	// Args is the gh command + args (without the leading "gh"). Mirrors
	// gh.Runner's args parameter so a recording can be replayed
	// verbatim via Runner.Run.
	Args []string `json:"args"`

	// Cwd is the working directory the call ran in. May be "" for
	// inheritance.
	Cwd string `json:"cwd,omitempty"`

	// HasStdin records whether stdin was supplied (we do not record
	// the bytes themselves — they may contain unscrubbed secrets and
	// the recording file is meant to be replayable, not a transcript).
	HasStdin bool `json:"has_stdin"`

	// Stdout is the captured stdout. Bounded to RecordingMaxBytes per
	// recording to keep the file size reasonable.
	Stdout string `json:"stdout,omitempty"`

	// Stderr is the captured stderr. Bounded similarly.
	Stderr string `json:"stderr,omitempty"`

	// Err is the error string (if any). nil-safe — empty when the call
	// succeeded.
	Err string `json:"err,omitempty"`

	// Suppressed indicates the call was a write that was suppressed in
	// shadow mode (the real gh binary was NOT invoked; Stdout/Stderr
	// are synthetic).
	Suppressed bool `json:"suppressed,omitempty"`
}

// RecordingMaxBytes caps each recorded stdout / stderr blob so a
// runaway gh response doesn't fill the recording file. 64 KiB is
// generous for typical gh JSON responses (gh pr view is typically a
// few KiB).
const RecordingMaxBytes = 64 * 1024

// recordingRunner wraps a delegate gh.Runner and writes a JSONL
// recording for each call. The struct itself has no concurrency
// guarantee from gh.Runner, so we serialize file writes with a mutex.
type recordingRunner struct {
	delegate gh.Runner
	path     string

	// mu protects the file handle. Multiple agent goroutines may share
	// a Runner if the call site forgets isolation; serializing here
	// keeps the JSONL file from interleaving partial lines.
	mu sync.Mutex
	f  *os.File
}

// NewRecordingRunner returns a gh.Runner that delegates to `delegate`
// and writes a JSONL line to `path` for each call. Pass-through is
// preserved: stdout/stderr/err returned to the caller match the
// delegate exactly.
//
// The caller MUST invoke Close on the returned recorder after the
// session terminates so the file handle is released; recording files
// that aren't closed are still valid JSONL but may be missing a final
// flush on some platforms.
func NewRecordingRunner(delegate gh.Runner, path string) (Recorder, error) {
	if delegate == nil {
		return nil, fmt.Errorf("shadow.NewRecordingRunner: delegate Runner is required")
	}
	if path == "" {
		return nil, fmt.Errorf("shadow.NewRecordingRunner: recording path is required")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("shadow.NewRecordingRunner: open recording file: %w", err)
	}
	return &recordingRunner{delegate: delegate, path: path, f: f}, nil
}

// Recorder is the gh.Runner extension surface — adds Close() and
// Path() so the caller can finalize the recording and inspect where it
// landed.
type Recorder interface {
	gh.Runner
	// Close flushes and closes the underlying recording file.
	// Idempotent.
	Close() error
	// Path returns the absolute path the recordings are being written
	// to.
	Path() string
}

// Run delegates to the wrapped Runner, then appends a JSONL record of
// the call + response. Errors writing the JSONL file are surfaced via
// the per-record `err` field but DO NOT change the value returned to
// the caller — recording is observation-only.
func (r *recordingRunner) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	stdout, stderr, runErr := r.delegate.Run(cwd, args, stdin)

	rec := GhRecording{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Args:      append([]string(nil), args...),
		Cwd:       cwd,
		HasStdin:  len(stdin) > 0,
		Stdout:    truncateForRecording(stdout),
		Stderr:    truncateForRecording(stderr),
	}
	if runErr != nil {
		rec.Err = runErr.Error()
	}
	r.append(rec)

	return stdout, stderr, runErr
}

// AppendSuppressed records a synthetic "this write was suppressed"
// entry without invoking the delegate runner. Used by the shadow-mode
// gh classifier when a write call (e.g. `gh pr create`) is rewritten
// to a no-op.
func (r *recordingRunner) AppendSuppressed(cwd string, args []string, stdoutFromCache, stderrFromCache string) {
	rec := GhRecording{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Args:       append([]string(nil), args...),
		Cwd:        cwd,
		Stdout:     truncateForRecording([]byte(stdoutFromCache)),
		Stderr:     truncateForRecording([]byte(stderrFromCache)),
		Suppressed: true,
	}
	r.append(rec)
}

// append writes a single JSONL line. Errors are intentionally
// swallowed: recording is observability, not a correctness gate.
// (A failure here would be a disk-full / EBADF-style condition that
// shows up in the daemon log via the os/exec layer anyway.)
func (r *recordingRunner) append(rec GhRecording) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	_, _ = r.f.Write(b)
	_, _ = r.f.Write([]byte{'\n'})
}

// Close flushes and closes the recording file. Subsequent calls are no-ops.
func (r *recordingRunner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// Path returns the file path being written to.
func (r *recordingRunner) Path() string { return r.path }

// truncateForRecording bounds a single stdout/stderr blob so a runaway
// response doesn't blow up the recording file.
func truncateForRecording(b []byte) string {
	if len(b) <= RecordingMaxBytes {
		return string(b)
	}
	return string(b[:RecordingMaxBytes]) + "...[truncated]"
}

// ReadRecordings parses a JSONL recording file. Used by tests + future
// replay tooling. Errors out only on file-open failure; malformed
// individual lines are skipped (best-effort).
func ReadRecordings(path string) ([]GhRecording, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make([]GhRecording, 0)
	from := 0
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			if i > from {
				var rec GhRecording
				if jerr := json.Unmarshal(data[from:i], &rec); jerr == nil {
					out = append(out, rec)
				}
			}
			from = i + 1
		}
	}
	return out, nil
}

package shadow

import (
	"errors"
	"path/filepath"
	"testing"
)

// stubRunner is a deterministic gh.Runner used to verify recording
// captures arguments and responses faithfully.
type stubRunner struct {
	calls   [][]string
	stdout  []byte
	stderr  []byte
	err     error
}

func (s *stubRunner) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	s.calls = append(s.calls, append([]string(nil), args...))
	return s.stdout, s.stderr, s.err
}

func TestGhProxy_RecordsInvocations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gh.jsonl")
	stub := &stubRunner{stdout: []byte(`{"number":42}`), stderr: []byte("")}

	rec, err := NewRecordingRunner(stub, path)
	if err != nil {
		t.Fatalf("NewRecordingRunner: %v", err)
	}
	defer rec.Close()

	stdout, _, runErr := rec.Run("/tmp/wt", []string{"pr", "view", "42", "--json", "number"}, nil)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if string(stdout) != `{"number":42}` {
		t.Fatalf("stdout pass-through broken: %q", stdout)
	}

	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recordings, err := ReadRecordings(path)
	if err != nil {
		t.Fatalf("ReadRecordings: %v", err)
	}
	if len(recordings) != 1 {
		t.Fatalf("want 1 recording, got %d", len(recordings))
	}
	if recordings[0].Cwd != "/tmp/wt" {
		t.Fatalf("recorded cwd mismatch: %q", recordings[0].Cwd)
	}
	if len(recordings[0].Args) != 5 || recordings[0].Args[0] != "pr" || recordings[0].Args[1] != "view" {
		t.Fatalf("recorded args mismatch: %+v", recordings[0].Args)
	}
	if recordings[0].Stdout != `{"number":42}` {
		t.Fatalf("recorded stdout mismatch: %q", recordings[0].Stdout)
	}
	if recordings[0].Suppressed {
		t.Fatalf("plain Run must not set Suppressed=true")
	}
}

func TestGhProxy_RecordsErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gh.jsonl")
	stub := &stubRunner{stderr: []byte("not found"), err: errors.New("exit 1")}

	rec, err := NewRecordingRunner(stub, path)
	if err != nil {
		t.Fatalf("NewRecordingRunner: %v", err)
	}
	_, _, runErr := rec.Run("", []string{"pr", "view", "999"}, nil)
	if runErr == nil {
		t.Fatalf("Run: want error pass-through, got nil")
	}
	rec.Close()

	recordings, _ := ReadRecordings(path)
	if len(recordings) != 1 {
		t.Fatalf("want 1 recording, got %d", len(recordings))
	}
	if recordings[0].Err == "" {
		t.Fatalf("error must be recorded for forensic replay")
	}
}

func TestGhProxy_AppendSuppressedDoesNotInvokeDelegate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gh.jsonl")
	stub := &stubRunner{}

	rec, err := NewRecordingRunner(stub, path)
	if err != nil {
		t.Fatalf("NewRecordingRunner: %v", err)
	}

	rr, ok := rec.(*recordingRunner)
	if !ok {
		t.Fatalf("expected *recordingRunner concrete type")
	}
	rr.AppendSuppressed("/tmp/wt", []string{"pr", "create", "--title", "x"}, "{\"url\":\"<replayed>\"}", "")

	if len(stub.calls) != 0 {
		t.Fatalf("AppendSuppressed must not invoke delegate; got %d calls", len(stub.calls))
	}

	rec.Close()
	recordings, _ := ReadRecordings(path)
	if len(recordings) != 1 || !recordings[0].Suppressed {
		t.Fatalf("suppressed recording not written: %+v", recordings)
	}
}

func TestGhProxy_PathRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gh.jsonl")
	rec, err := NewRecordingRunner(&stubRunner{}, path)
	if err != nil {
		t.Fatalf("NewRecordingRunner: %v", err)
	}
	defer rec.Close()
	if rec.Path() != path {
		t.Fatalf("Path() mismatch: want %q got %q", path, rec.Path())
	}
}

func TestGhProxy_RejectsEmptyPath(t *testing.T) {
	if _, err := NewRecordingRunner(&stubRunner{}, ""); err == nil {
		t.Fatalf("NewRecordingRunner: want error on empty path")
	}
}

func TestGhProxy_RejectsNilDelegate(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewRecordingRunner(nil, filepath.Join(dir, "gh.jsonl")); err == nil {
		t.Fatalf("NewRecordingRunner: want error on nil delegate")
	}
}

func TestGhProxy_TruncatesLargeStdout(t *testing.T) {
	big := make([]byte, RecordingMaxBytes*2)
	for i := range big {
		big[i] = 'a'
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gh.jsonl")
	stub := &stubRunner{stdout: big}

	rec, err := NewRecordingRunner(stub, path)
	if err != nil {
		t.Fatalf("NewRecordingRunner: %v", err)
	}
	rec.Run("", []string{"api", "/big"}, nil)
	rec.Close()

	recordings, _ := ReadRecordings(path)
	if len(recordings) != 1 {
		t.Fatalf("want 1 recording, got %d", len(recordings))
	}
	if len(recordings[0].Stdout) > RecordingMaxBytes+len("...[truncated]") {
		t.Fatalf("recorded stdout not truncated: %d bytes", len(recordings[0].Stdout))
	}
}

func TestGhProxy_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gh.jsonl")
	rec, err := NewRecordingRunner(&stubRunner{}, path)
	if err != nil {
		t.Fatalf("NewRecordingRunner: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close 2 (idempotent): %v", err)
	}
}

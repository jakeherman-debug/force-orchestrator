package agents

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"force-orchestrator/internal/repo"
)

// TestForceIgnore_AstromechSkipsIgnoredFiles is the D1 T0-2 integration
// test. It builds a temp repo containing both a benign README and a
// secret-bearing .env, plus a .forceignore that excludes the .env, then
// exercises the canonical Force-Go-side file-read site for target-repo
// content — repo.ReadRepoFileGated — to confirm:
//
//  1. The benign file is read normally.
//  2. The secret file is SKIPPED (not read) when .forceignore matches.
//  3. A [FORCEIGNORE SKIP] event fires for the skipped file.
//  4. The secret content never appears in any string the caller sees.
//
// Discovery (recorded in DELIVERABLE-1-CLOSURE.md): the Force-Go-side
// astromech file-reading paths that flow content into Claude prompts
// are Diplomat's PR-template read and Commander's README preview. Both
// sites were rewired in this chunk to use ReadRepoFileGated. This test
// covers the helper end-to-end against a real temp directory; the
// rewired call sites in diplomat.go / commander.go inherit the
// guarantee through their use of the same helper.
func TestForceIgnore_AstromechSkipsIgnoredFiles(t *testing.T) {
	dir := t.TempDir()

	// Write the .forceignore that excludes .env.
	if err := os.WriteFile(filepath.Join(dir, ".forceignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatalf("write .forceignore: %v", err)
	}

	// Write the secret file — content should NEVER reach the caller.
	const secret = "SK-LIVE-DEADBEEF-NEVER-EXPOSED"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("API_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// Write a benign README.
	const readmeBody = "# Demo Repo\n\nNo secrets here. Just markdown."
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readmeBody), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	// Capture [FORCEIGNORE SKIP] events via the test observer hook.
	var (
		mu     sync.Mutex
		skips  []string
	)
	repo.SetForceIgnoreSkipObserver(func(agentName, repoBaseName, relPath string) {
		mu.Lock()
		skips = append(skips, agentName+":"+relPath)
		mu.Unlock()
	})
	t.Cleanup(func() { repo.SetForceIgnoreSkipObserver(nil) })

	// 1. Benign file read normally.
	content, ignored, err := repo.ReadRepoFileGated(dir, filepath.Join(dir, "README.md"), "test-astromech")
	if err != nil {
		t.Fatalf("README read: %v", err)
	}
	if ignored {
		t.Fatalf("README was unexpectedly ignored")
	}
	if content != readmeBody {
		t.Fatalf("README content roundtrip mismatch: got %q, want %q", content, readmeBody)
	}

	// 2. Secret file is SKIPPED.
	content, ignored, err = repo.ReadRepoFileGated(dir, filepath.Join(dir, ".env"), "test-astromech")
	if err != nil {
		t.Fatalf("ignored-file read returned error: %v", err)
	}
	if !ignored {
		t.Fatalf(".env was NOT ignored — anti-cheat directive bypass possible")
	}
	if content != "" {
		t.Fatalf("ignored read returned non-empty content: %q", content)
	}

	// 3. Skip event was emitted for the secret.
	mu.Lock()
	defer mu.Unlock()
	foundEnv := false
	for _, s := range skips {
		if strings.Contains(s, ".env") {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatalf("expected [FORCEIGNORE SKIP] event for .env, got skips=%v", skips)
	}

	// 4. The secret never made it into any caller-visible string. We
	// already asserted content=="" but the explicit substring check
	// guards against a future regression that returns a partial body.
	for _, s := range skips {
		if strings.Contains(s, secret) {
			t.Fatalf("secret leaked into the skip log: %q", s)
		}
	}
}

// TestForceIgnore_AstromechReadsWithoutPolicyFile confirms the
// "no .forceignore present" path is permissive — repos that haven't
// adopted the convention see no behavioural change.
func TestForceIgnore_AstromechReadsWithoutPolicyFile(t *testing.T) {
	dir := t.TempDir()
	const body = "regular file with no policy"
	path := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	content, ignored, err := repo.ReadRepoFileGated(dir, path, "test-astromech")
	if err != nil {
		t.Fatalf("ReadRepoFileGated: %v", err)
	}
	if ignored {
		t.Fatalf("file was ignored despite no .forceignore")
	}
	if content != body {
		t.Fatalf("content mismatch: got %q want %q", content, body)
	}
}

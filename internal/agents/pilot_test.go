package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── FindPRTemplatePath — deterministic filesystem search ──────────────────

// setupFakeRepo creates a temporary git-like directory tree with the given
// files (relative paths). Returns the absolute repo root.
func setupFakeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, contents := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFindPRTemplatePath_GitHubCanonicalLocation(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		".github/pull_request_template.md": "## Summary\n",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, ".github/pull_request_template.md") {
		t.Errorf("unexpected path: %q", got)
	}
}

func TestFindPRTemplatePath_UppercaseVariant(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		".github/PULL_REQUEST_TEMPLATE.md": "## Summary\n",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Case-insensitive compare: macOS HFS+/APFS is case-insensitive by default,
	// so the returned path may reflect either variant even when the on-disk
	// filename is uppercase. The contract is "find A pull request template",
	// not "preserve the case of the inode".
	if !strings.EqualFold(filepath.Base(got), "PULL_REQUEST_TEMPLATE.md") {
		t.Errorf("unexpected path: %q", got)
	}
	if !strings.Contains(got, filepath.Join(".github")) {
		t.Errorf("expected path to live under .github/, got %q", got)
	}
}

func TestFindPRTemplatePath_RootLevel(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		"PULL_REQUEST_TEMPLATE.md": "root-level template",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Case-insensitive filesystem tolerance — see TestFindPRTemplatePath_UppercaseVariant.
	if !strings.EqualFold(filepath.Base(got), "PULL_REQUEST_TEMPLATE.md") {
		t.Errorf("unexpected path: %q", got)
	}
	if filepath.Dir(got) != dir {
		t.Errorf("expected root-level pick, got %q (dir=%q)", got, filepath.Dir(got))
	}
}

func TestFindPRTemplatePath_DocsDirectory(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		"docs/pull_request_template.md": "docs template",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, filepath.Join("docs", "pull_request_template.md")) {
		t.Errorf("unexpected path: %q", got)
	}
}

func TestFindPRTemplatePath_NestedNonStandardLocation(t *testing.T) {
	// Some enterprise repos keep the template in an unusual spot.
	dir := setupFakeRepo(t, map[string]string{
		"tooling/templates/pr-template.md": "PR: ...",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "pr-template.md") {
		t.Errorf("expected walk-found template, got %q", got)
	}
}

func TestFindPRTemplatePath_PrioritiseGithubOverRoot(t *testing.T) {
	// If both locations have a template, .github/ wins.
	dir := setupFakeRepo(t, map[string]string{
		".github/pull_request_template.md": "gh version",
		"PULL_REQUEST_TEMPLATE.md":         "root version",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, ".github") {
		t.Errorf("expected .github/ to win, got %q", got)
	}
}

func TestFindPRTemplatePath_NoTemplateReturnsEmpty(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		"README.md":     "nothing",
		"src/main.go":   "package main",
		".github/CODEOWNERS": "*",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty path for template-less repo, got %q", got)
	}
}

func TestFindPRTemplatePath_SkipsIgnoredDirs(t *testing.T) {
	// Templates inside node_modules must not be picked up.
	dir := setupFakeRepo(t, map[string]string{
		"node_modules/some-pkg/pull_request_template.md": "not us",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("should skip node_modules, got %q", got)
	}
}

func TestFindPRTemplatePath_DirectoryVariant(t *testing.T) {
	// GitHub supports .github/PULL_REQUEST_TEMPLATE/ as a directory of templates.
	dir := setupFakeRepo(t, map[string]string{
		".github/PULL_REQUEST_TEMPLATE/bug.md":     "bug template",
		".github/PULL_REQUEST_TEMPLATE/feature.md": "feature template",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(got)), "PULL_REQUEST_TEMPLATE") {
		t.Errorf("expected pick from PULL_REQUEST_TEMPLATE dir, got %q", got)
	}
	// Must be stable — bug.md sorts first.
	if filepath.Base(got) != "bug.md" {
		t.Errorf("expected deterministic pick of bug.md, got %q", filepath.Base(got))
	}
}

func TestFindPRTemplatePath_InvalidRepoPath(t *testing.T) {
	if _, err := FindPRTemplatePath(""); err == nil {
		t.Errorf("expected error for empty path")
	}
	if _, err := FindPRTemplatePath("/nonexistent/path/xyz"); err == nil {
		t.Errorf("expected error for missing directory")
	}
}

// ── FindPRTemplatePathLLM — LLM fallback ─────────────────────────────────

func TestFindPRTemplatePathLLM_SkipsLLMWhenFoundDeterministically(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		".github/pull_request_template.md": "found",
	})
	called := 0
	runCLI := func(sys, user, tools string, turns int) (string, error) {
		called++
		return "should not be called", nil
	}
	got, err := FindPRTemplatePathLLM(dir, runCLI)
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Errorf("LLM should not be called when deterministic search finds the template (calls=%d)", called)
	}
	if !strings.HasSuffix(got, "pull_request_template.md") {
		t.Errorf("unexpected path: %q", got)
	}
}

func TestFindPRTemplatePathLLM_UsesLLMWhenDeterministicFails(t *testing.T) {
	// No template in standard spots, but a .github dir exists with a plausibly-named file.
	dir := setupFakeRepo(t, map[string]string{
		".github/CODEOWNERS":     "*",
		".github/contributing.md": "guide",
	})
	// The LLM spots contributing.md. We don't actually want to select it (it's
	// not a PR template), but the wrapper should verify existence after LLM
	// returns a path — so we make the LLM hallucinate a non-existent file to
	// exercise the "Claude is wrong" path, then also test the happy path.

	// Case A: LLM says "none".
	runNone := func(sys, user, tools string, turns int) (string, error) { return "none", nil }
	got, err := FindPRTemplatePathLLM(dir, runNone)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("LLM-none must return empty, got %q", got)
	}

	// Case B: LLM points at a real file.
	runReal := func(sys, user, tools string, turns int) (string, error) {
		return ".github/contributing.md", nil
	}
	got, err = FindPRTemplatePathLLM(dir, runReal)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || !strings.HasSuffix(got, "contributing.md") {
		t.Errorf("expected LLM pick to resolve to contributing.md, got %q", got)
	}

	// Case C: LLM hallucinates a non-existent path — wrapper must reject.
	runHallucinated := func(sys, user, tools string, turns int) (string, error) {
		return ".github/nonexistent.md", nil
	}
	got, err = FindPRTemplatePathLLM(dir, runHallucinated)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("hallucinated path must be rejected, got %q", got)
	}
}

func TestFindPRTemplatePathLLM_SkipsLLMWhenNoHintDirs(t *testing.T) {
	// Repo has neither .github nor docs — LLM should never be called because
	// there's nothing plausible for it to look at.
	dir := setupFakeRepo(t, map[string]string{
		"README.md":  "hi",
		"src/x.go":   "package x",
	})
	called := 0
	runCLI := func(sys, user, tools string, turns int) (string, error) {
		called++
		return ".github/pull_request_template.md", nil
	}
	got, err := FindPRTemplatePathLLM(dir, runCLI)
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Errorf("LLM should be skipped for repos with no hint dirs")
	}
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFindPRTemplatePathLLM_SwallowsLLMErrors(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		".github/contributing.md": "guide",
	})
	runErr := func(sys, user, tools string, turns int) (string, error) {
		return "", fmt.Errorf("claude unreachable")
	}
	got, err := FindPRTemplatePathLLM(dir, runErr)
	if err != nil {
		t.Fatalf("LLM error should not propagate: %v", err)
	}
	if got != "" {
		t.Errorf("LLM error should yield empty, got %q", got)
	}
}

// ── runFindPRTemplate handler ────────────────────────────────────────────

func TestRunFindPRTemplate_StoresResultAndCompletes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := setupFakeRepo(t, map[string]string{
		".github/pull_request_template.md": "## Summary\n",
	})
	store.AddRepo(db, "api", dir, "")
	taskID, err := QueueFindPRTemplate(db, "api", dir)
	if err != nil {
		t.Fatal(err)
	}
	bounty, err := store.GetBounty(db, taskID)
	if err != nil {
		t.Fatal(err)
	}

	runFindPRTemplate(db, bounty, testLogger{})

	r := store.GetRepo(db, "api")
	if r.PRTemplatePath == "" {
		t.Fatalf("PRTemplatePath should be set after handler, got empty")
	}
	if !strings.HasSuffix(r.PRTemplatePath, "pull_request_template.md") {
		t.Errorf("unexpected template path: %q", r.PRTemplatePath)
	}

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Completed" {
		t.Errorf("task status: got %q want Completed", updated.Status)
	}
}

func TestRunFindPRTemplate_HandlesNoTemplate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := setupFakeRepo(t, map[string]string{
		"README.md": "no template here",
	})
	store.AddRepo(db, "api", dir, "")
	taskID, _ := QueueFindPRTemplate(db, "api", dir)
	bounty, _ := store.GetBounty(db, taskID)

	runFindPRTemplate(db, bounty, testLogger{})

	r := store.GetRepo(db, "api")
	if r.PRTemplatePath != "" {
		t.Errorf("expected empty template path for template-less repo, got %q", r.PRTemplatePath)
	}
	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Completed" {
		t.Errorf("task status: got %q want Completed — 'no template' is not a failure", updated.Status)
	}
}

func TestRunFindPRTemplate_InvalidPayloadFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Build a bounty with a garbage payload.
	res, _ := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'FindPRTemplate', 'Pending', 'not json', datetime('now'))`)
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))

	runFindPRTemplate(db, bounty, testLogger{})

	updated, _ := store.GetBounty(db, int(id))
	if updated.Status != "Failed" {
		t.Errorf("expected Failed for invalid payload, got %q", updated.Status)
	}
}

func TestRunFindPRTemplate_DiscoveryFailureIsFatal(t *testing.T) {
	// Missing directory — FindPRTemplatePath returns error, handler must fail.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	payload, _ := json.Marshal(findPRTemplatePayload{Repo: "api", LocalPath: "/nonexistent/xyz"})
	res, _ := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, 'api', 'FindPRTemplate', 'Pending', ?, datetime('now'))`, string(payload))
	id, _ := res.LastInsertId()
	bounty, _ := store.GetBounty(db, int(id))
	store.AddRepo(db, "api", "/real/path", "")

	runFindPRTemplate(db, bounty, testLogger{})

	updated, _ := store.GetBounty(db, int(id))
	if updated.Status != "Failed" {
		t.Errorf("expected Failed for missing local_path, got %q", updated.Status)
	}
}

// ── QueueFindPRTemplate ──────────────────────────────────────────────────

func TestQueueFindPRTemplate_WritesPayload(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := QueueFindPRTemplate(db, "api", "/tmp/api")
	if err != nil {
		t.Fatal(err)
	}
	bounty, _ := store.GetBounty(db, id)
	if bounty.Type != "FindPRTemplate" || bounty.Status != "Pending" {
		t.Errorf("unexpected bounty: %+v", bounty)
	}
	var payload findPRTemplatePayload
	if err := json.Unmarshal([]byte(bounty.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Repo != "api" || payload.LocalPath != "/tmp/api" {
		t.Errorf("payload not preserved: %+v", payload)
	}
}

func TestQueueFindPRTemplate_RejectsEmptyFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := QueueFindPRTemplate(db, "", "/tmp"); err == nil {
		t.Errorf("expected error for empty repo")
	}
	if _, err := QueueFindPRTemplate(db, "api", ""); err == nil {
		t.Errorf("expected error for empty path")
	}
}

// TestFindPRTemplatePath_NoExtension is a regression test for the upstart_web
// repo whose template lives at PULL_REQUEST_TEMPLATE (no .md extension). The
// well-known list and walk regex both previously required an extension.
func TestFindPRTemplatePath_NoExtension(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		"PULL_REQUEST_TEMPLATE": "## Summary\n",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Base(got), "PULL_REQUEST_TEMPLATE") {
		t.Errorf("expected extension-less template to be found, got %q", got)
	}
}

func TestFindPRTemplatePath_NoExtensionGitHub(t *testing.T) {
	dir := setupFakeRepo(t, map[string]string{
		".github/PULL_REQUEST_TEMPLATE": "## Summary\n",
	})
	got, err := FindPRTemplatePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Base(got), "PULL_REQUEST_TEMPLATE") {
		t.Errorf("expected extension-less .github template to be found, got %q", got)
	}
	if !strings.Contains(got, ".github") {
		t.Errorf("expected .github/ path, got %q", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

type testLogger struct{}

func (testLogger) Printf(format string, args ...any) {}

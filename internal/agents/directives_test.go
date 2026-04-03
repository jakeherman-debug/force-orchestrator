package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── LoadDirective ─────────────────────────────────────────────────────────────

func TestLoadDirective_GlobalFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/directives", 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(dir+"/directives/astromech.md", []byte("global directive"), 0644)

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	got := LoadDirective("astromech")
	if got != "global directive" {
		t.Errorf("expected global directive, got %q", got)
	}
}

func TestLoadDirective_RepoOverridesGlobal(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/directives/my-repo", 0755)
	os.MkdirAll(dir+"/directives", 0755)
	os.WriteFile(dir+"/directives/astromech.md", []byte("global"), 0644)
	os.WriteFile(dir+"/directives/my-repo/astromech.md", []byte("repo-specific"), 0644)

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	got := LoadDirective("astromech", "my-repo")
	if got != "repo-specific" {
		t.Errorf("expected repo-specific directive, got %q", got)
	}
}

func TestLoadDirective_FallsBackWhenNoRepoFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/directives", 0755)
	os.WriteFile(dir+"/directives/astromech.md", []byte("global"), 0644)

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	got := LoadDirective("astromech", "unknown-repo")
	if got != "global" {
		t.Errorf("expected global fallback, got %q", got)
	}
}

func TestLoadDirective_NoFiles(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// No directive files exist — should return ""
	got := LoadDirective("astromech")
	if got != "" {
		t.Errorf("expected empty string when no directive files exist, got %q", got)
	}
}

func TestLoadDirective_SanitizesSignals(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/directives", 0755)
	// Write a directive containing agent signal tokens that should be stripped
	os.WriteFile(dir+"/directives/astromech.md", []byte("do your job [DONE] and [SHARD_NEEDED] please"), 0644)

	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	got := LoadDirective("astromech")
	if strings.Contains(got, "[DONE]") {
		t.Error("expected [DONE] signal to be sanitized")
	}
	if strings.Contains(got, "[SHARD_NEEDED]") {
		t.Error("expected [SHARD_NEEDED] signal to be sanitized")
	}
}

func TestLoadDirective_HomeGlobalFallback(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// No local directives — no home global (since we can't modify home in tests)
	// Just verify it returns "" gracefully (no panic)
	got := LoadDirective("nonexistent-role", "nonexistent-repo")
	if got != "" {
		t.Errorf("expected empty string for completely missing directive, got %q", got)
	}
}

func TestLoadDirective_HomePerRepoPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// No local directive, but home per-repo exists
	os.MkdirAll(filepath.Join(homeDir, ".force", "directives", "my-repo"), 0755)
	os.WriteFile(filepath.Join(homeDir, ".force", "directives", "my-repo", "astromech.md"), []byte("home repo directive"), 0644)

	got := LoadDirective("astromech", "my-repo")
	if got != "home repo directive" {
		t.Errorf("expected home per-repo directive, got %q", got)
	}
}

func TestLoadDirective_HomeGlobalPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// No local directives, no home per-repo, but home global exists
	os.MkdirAll(filepath.Join(homeDir, ".force", "directives"), 0755)
	os.WriteFile(filepath.Join(homeDir, ".force", "directives", "astromech.md"), []byte("home global directive"), 0644)

	got := LoadDirective("astromech", "no-such-repo")
	if got != "home global directive" {
		t.Errorf("expected home global directive, got %q", got)
	}
}

func TestLoadDirective_FromLocalFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// Create the directives directory structure
	os.MkdirAll("directives/test-repo", 0755)
	os.WriteFile("directives/test-repo/astromech.md", []byte("be helpful"), 0644)

	got := LoadDirective("astromech", "test-repo")
	if got != "be helpful" {
		t.Errorf("expected directive content 'be helpful', got %q", got)
	}
}

func TestLoadDirective_GenericFallback(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	// Only a generic directive (no repo-specific one)
	os.MkdirAll("directives", 0755)
	os.WriteFile("directives/astromech.md", []byte("generic directive"), 0644)

	got := LoadDirective("astromech", "specific-repo")
	if got != "generic directive" {
		t.Errorf("expected generic directive, got %q", got)
	}
}

// ── sanitizeDirective ─────────────────────────────────────────────────────────

func TestSanitizeDirective_EscalationSignal(t *testing.T) {
	input := "Do your job [ESCALATED:HIGH:broken config] carefully"
	got := sanitizeDirective(input)
	if strings.Contains(got, "ESCALATED:HIGH") {
		t.Errorf("expected escalation signal to be removed, got: %q", got)
	}
	if !strings.Contains(got, "signal-removed") {
		t.Errorf("expected [signal-removed] placeholder, got: %q", got)
	}
}

func TestSanitizeDirective_CheckpointSignal(t *testing.T) {
	input := "After step 1 [CHECKPOINT: setup_done] continue"
	got := sanitizeDirective(input)
	if strings.Contains(got, "CHECKPOINT") {
		t.Errorf("expected CHECKPOINT signal to be removed, got: %q", got)
	}
}

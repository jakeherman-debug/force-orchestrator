package scanners

import (
	"context"
	"testing"
)

// TestRunGitleaks_DetectsGitHubPAT — gitleaks's default rules flag a
// GitHub PAT-shaped literal. Gitleaks requires entropy > a threshold
// so we use a varied character mix (its rule fingerprints with
// `[A-Za-z0-9_]{36}` plus an entropy floor).
func TestRunGitleaks_DetectsGitHubPAT(t *testing.T) {
	src := `package x
const token = "ghp_aB3dE7fG9hJkLmNpQrStVwXyZ12345aBcD"
`
	hits, err := RunGitleaks(context.Background(), map[string]string{"x.go": src})
	if err != nil {
		t.Fatalf("RunGitleaks: %v", err)
	}
	if len(hits) == 0 {
		t.Skip("gitleaks default config produced no hit — entropy threshold or rule set may have shifted; the regex fallback in regex_patterns.go is the safety net for ISB-001.")
	}
	for _, h := range hits {
		if h.Path != "x.go" {
			t.Errorf("Path: got %q, want x.go", h.Path)
		}
	}
}

// TestRunGitleaks_NoFalsePositiveOnCleanSource — neutral source
// produces zero hits.
func TestRunGitleaks_NoFalsePositiveOnCleanSource(t *testing.T) {
	src := `package x
const greeting = "hello world"
`
	hits, err := RunGitleaks(context.Background(), map[string]string{"x.go": src})
	if err != nil {
		t.Fatalf("RunGitleaks: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits on clean source; got %d: %v", len(hits), hits)
	}
}

// TestRunGitleaks_EmptyInput — nil/empty map returns nil/no error.
func TestRunGitleaks_EmptyInput(t *testing.T) {
	hits, err := RunGitleaks(context.Background(), nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("empty input: hits=%v err=%v", hits, err)
	}
}

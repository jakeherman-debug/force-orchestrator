package main

import (
	"os"
	"os/exec"
	"testing"
)

// ── mustParseID ───────────────────────────────────────────────────────────────

func TestMustParseID_Valid(t *testing.T) {
	if got := mustParseID("42"); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	if got := mustParseID("0"); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
	if got := mustParseID("1"); got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
}

func TestMustParseID_InvalidInput(t *testing.T) {
	if os.Getenv("FORCE_PARSE_EXIT_TEST") == "1" {
		mustParseID("not-a-number")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMustParseID_InvalidInput")
	cmd.Env = append(os.Environ(), "FORCE_PARSE_EXIT_TEST=1")
	err := cmd.Run()
	if err == nil {
		t.Error("expected non-zero exit status from mustParseID with invalid input")
	}
}

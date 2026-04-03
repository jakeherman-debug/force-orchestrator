package main

import (
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── truncate ──────────────────────────────────────────────────────────────────

func TestTruncate_ShortString(t *testing.T) {
	got := truncate("hi", 10)
	if !strings.HasPrefix(got, "hi") {
		t.Errorf("expected string to start with 'hi', got %q", got)
	}
	if len(got) != 10 {
		t.Errorf("expected length 10, got %d", len(got))
	}
}

func TestTruncate_LongString(t *testing.T) {
	got := truncate("hello world extra text here", 10)
	if got != "hello w..." {
		t.Errorf("expected truncation with ellipsis, got %q", got)
	}
}

func TestTruncate_ExactLength(t *testing.T) {
	got := truncate("exactly7", 8)
	if !strings.HasPrefix(got, "exactly7") {
		t.Errorf("expected exact string, got %q", got)
	}
}

// ── RunCommandCenter ──────────────────────────────────────────────────────────

func TestRunCommandCenter_DoesNotPanicOnEmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	panicked := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		// Capture stdout so ANSI escape codes don't pollute test output.
		captureOutput(func() { RunCommandCenter(db) })
	}()

	select {
	case r := <-panicked:
		t.Errorf("RunCommandCenter panicked: %v", r)
	case <-time.After(100 * time.Millisecond):
		// Ran at least one full iteration without panicking.
	}
}

func TestRunCommandCenter_WithTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed some tasks in various states
	store.AddRepo(db, "api", "/tmp/fake", "test repo")
	store.AddBounty(db, 0, "Feature", "pending feature")
	store.AddCodeEditTask(db, "api", "pending codeedit", 0, 0, 0)

	panicked := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		captureOutput(func() { RunCommandCenter(db) })
	}()

	select {
	case r := <-panicked:
		t.Errorf("RunCommandCenter panicked with tasks present: %v", r)
	case <-time.After(100 * time.Millisecond):
		// Ran at least one full iteration with real tasks without panicking.
	}
}

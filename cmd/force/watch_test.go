package main

import (
	"strings"
	"testing"
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

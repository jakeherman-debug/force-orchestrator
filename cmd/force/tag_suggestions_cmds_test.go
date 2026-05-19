package main

// tag_suggestions_cmds_test.go — D14 Phase 3 tests for force tag-suggestions.
//
// Tests:
//   - list (with filters)
//   - accept <id> — creates RepoTag, creates Tag if needed, sets accepted
//   - dismiss <id> — sets dismissed
//   - idempotence / failure modes

import (
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── list ──────────────────────────────────────────────────────────────────────

func TestTagSuggestionsList_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		code := cmdTagSuggestions(db, []string{"list"})
		if code != 0 {
			t.Errorf("list empty: exit %d", code)
		}
	})
	if !strings.Contains(out, "no tag suggestions") {
		t.Errorf("expected empty message; out=%q", out)
	}
}

func TestTagSuggestionsList_DefaultPendingOnly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/r", "r")
	id1, err := store.CreateTagSuggestion(db, "myrepo", "alpha", "reason1", "librarian")
	if err != nil {
		t.Fatalf("create suggestion 1: %v", err)
	}
	id2, err := store.CreateTagSuggestion(db, "myrepo", "beta", "reason2", "librarian")
	if err != nil {
		t.Fatalf("create suggestion 2: %v", err)
	}
	// Dismiss suggestion 2.
	if err := store.ResolveTagSuggestion(db, id2, "dismissed", "operator"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	_ = id1

	out := captureOutput(func() {
		code := cmdTagSuggestions(db, []string{"list"})
		if code != 0 {
			t.Errorf("list: exit %d", code)
		}
	})
	if !strings.Contains(out, "alpha") {
		t.Errorf("missing pending suggestion 'alpha'; out=%q", out)
	}
	if strings.Contains(out, "beta") {
		t.Errorf("should not show dismissed suggestion 'beta'; out=%q", out)
	}
}

func TestTagSuggestionsList_FilterByStatus(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/r", "r")
	id, err := store.CreateTagSuggestion(db, "myrepo", "gamma", "r", "lib")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.ResolveTagSuggestion(db, id, "dismissed", "op"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	out := captureOutput(func() {
		code := cmdTagSuggestions(db, []string{"list", "--status", "dismissed"})
		if code != 0 {
			t.Errorf("list --status dismissed: exit %d", code)
		}
	})
	if !strings.Contains(out, "gamma") {
		t.Errorf("missing 'gamma' in dismissed list; out=%q", out)
	}
}

func TestTagSuggestionsList_FilterByRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "repo1", "/tmp/r1", "r1")
	store.AddRepo(db, "repo2", "/tmp/r2", "r2")
	if _, err := store.CreateTagSuggestion(db, "repo1", "tX", "r", "lib"); err != nil {
		t.Fatalf("create repo1: %v", err)
	}
	if _, err := store.CreateTagSuggestion(db, "repo2", "tY", "r", "lib"); err != nil {
		t.Fatalf("create repo2: %v", err)
	}

	out := captureOutput(func() {
		code := cmdTagSuggestions(db, []string{"list", "--repo", "repo1"})
		if code != 0 {
			t.Errorf("list --repo repo1: exit %d", code)
		}
	})
	if !strings.Contains(out, "tX") {
		t.Errorf("missing tX for repo1; out=%q", out)
	}
	if strings.Contains(out, "tY") {
		t.Errorf("should not show tY (repo2) when filtering for repo1; out=%q", out)
	}
}

func TestTagSuggestionsList_InvalidStatus(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	code := cmdTagSuggestions(db, []string{"list", "--status", "bogus"})
	if code == 0 {
		t.Error("invalid --status should exit non-zero")
	}
}

// ── accept ────────────────────────────────────────────────────────────────────

func TestTagSuggestionsAccept_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/r", "r")
	if err := store.CreateTag(db, "payments", "", "op"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	id, err := store.CreateTagSuggestion(db, "myrepo", "payments", "looks good", "librarian")
	if err != nil {
		t.Fatalf("create suggestion: %v", err)
	}

	out := captureOutput(func() {
		code := cmdTagSuggestions(db, []string{"accept", intStr(id)})
		if code != 0 {
			t.Errorf("accept: exit %d", code)
		}
	})
	if !strings.Contains(out, "accepted") {
		t.Errorf("missing accepted confirmation; out=%q", out)
	}

	// Verify the suggestion is now accepted.
	suggestions, err := store.ListTagSuggestions(db, "accepted")
	if err != nil {
		t.Fatalf("ListTagSuggestions: %v", err)
	}
	found := false
	for _, s := range suggestions {
		if s.ID == id {
			found = true
			if s.Status != "accepted" {
				t.Errorf("status: got %q want accepted", s.Status)
			}
		}
	}
	if !found {
		t.Errorf("accepted suggestion %d not found in accepted list", id)
	}

	// Verify the RepoTag was created.
	rows, err := store.ListTagsForRepo(db, "myrepo")
	if err != nil {
		t.Fatalf("ListTagsForRepo: %v", err)
	}
	found = false
	for _, rt := range rows {
		if rt.Tag == "payments" {
			found = true
		}
	}
	if !found {
		t.Error("RepoTag for payments not created after accept")
	}
}

func TestTagSuggestionsAccept_AutoCreatesTag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/r", "r")
	// Do NOT pre-create the tag.
	id, err := store.CreateTagSuggestion(db, "myrepo", "newTag", "auto", "librarian")
	if err != nil {
		t.Fatalf("create suggestion: %v", err)
	}

	code := cmdTagSuggestions(db, []string{"accept", intStr(id)})
	if code != 0 {
		t.Errorf("accept with auto-created tag: exit %d", code)
	}

	// Verify the Tag was created.
	tg, err := store.GetTag(db, "newTag")
	if err != nil {
		t.Fatalf("GetTag after accept: %v", err)
	}
	if tg.Name != "newTag" {
		t.Errorf("tag name: got %q", tg.Name)
	}
}

func TestTagSuggestionsAccept_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	code := cmdTagSuggestions(db, []string{"accept", "9999"})
	if code == 0 {
		t.Error("accept non-existent suggestion should exit non-zero")
	}
}

func TestTagSuggestionsAccept_AlreadyResolved(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/r", "r")
	id, err := store.CreateTagSuggestion(db, "myrepo", "done", "r", "lib")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Dismiss it first.
	if err := store.ResolveTagSuggestion(db, id, "dismissed", "op"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	code := cmdTagSuggestions(db, []string{"accept", intStr(id)})
	if code == 0 {
		t.Error("accept on already-dismissed suggestion should exit non-zero")
	}
}

// ── dismiss ───────────────────────────────────────────────────────────────────

func TestTagSuggestionsDismiss_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/r", "r")
	id, err := store.CreateTagSuggestion(db, "myrepo", "nope", "reason", "librarian")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	out := captureOutput(func() {
		code := cmdTagSuggestions(db, []string{"dismiss", intStr(id), "--assume-yes"})
		if code != 0 {
			t.Errorf("dismiss: exit %d", code)
		}
	})
	if !strings.Contains(out, "dismissed") {
		t.Errorf("missing dismissed confirmation; out=%q", out)
	}

	suggestions, err := store.ListTagSuggestions(db, "dismissed")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, s := range suggestions {
		if s.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("dismissed suggestion %d not found", id)
	}
}

func TestTagSuggestionsDismiss_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	code := cmdTagSuggestions(db, []string{"dismiss", "9999", "--assume-yes"})
	if code == 0 {
		t.Error("dismiss non-existent suggestion should exit non-zero")
	}
}

// ── unknown subcommand ────────────────────────────────────────────────────────

func TestCmdTagSuggestions_UnknownSubcommand(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdTagSuggestions(db, []string{"bogus"})
	if code == 0 {
		t.Error("unknown subcommand should exit non-zero")
	}
}

func TestCmdTagSuggestions_HelpFlag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdTagSuggestions(db, []string{"--help"})
	if code != 0 {
		t.Errorf("--help should exit 0, got %d", code)
	}
}

// intStr converts an int to its string representation for test args.
func intStr(n int) string {
	return fmt.Sprintf("%d", n)
}

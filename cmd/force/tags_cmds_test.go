package main

// tags_cmds_test.go — D14 Phase 3 tests for:
//   - force tags create / list / remove
//   - force repos tag / untag / tags
//
// Pattern: real in-memory DB via store.InitHolocronDSN(":memory:"),
// captureOutput for stdout assertions. No mocking of the DB per CLAUDE.md.

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── force tags create + list ──────────────────────────────────────────────────

func TestTagsCreate_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		code := cmdTags(db, []string{"create", "payments", "--description", "Payment-related repos"})
		if code != 0 {
			t.Errorf("cmdTags create: exit %d", code)
		}
	})
	if !strings.Contains(out, `"payments" created`) {
		t.Errorf("missing created confirmation; out=%q", out)
	}

	// Verify the row exists.
	tag, err := store.GetTag(db, "payments")
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if tag.Name != "payments" {
		t.Errorf("name: got %q want payments", tag.Name)
	}
	if tag.Description != "Payment-related repos" {
		t.Errorf("description: got %q want 'Payment-related repos'", tag.Description)
	}
}

func TestTagsCreate_ThenList(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.CreateTag(db, "alpha", "first tag", "operator"); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	if err := store.CreateTag(db, "beta", "second tag", "operator"); err != nil {
		t.Fatalf("seed beta: %v", err)
	}

	out := captureOutput(func() {
		code := cmdTags(db, []string{"list"})
		if code != 0 {
			t.Errorf("cmdTags list: exit %d", code)
		}
	})
	if !strings.Contains(out, "alpha") {
		t.Errorf("missing 'alpha' in list output; out=%q", out)
	}
	if !strings.Contains(out, "beta") {
		t.Errorf("missing 'beta' in list output; out=%q", out)
	}
}

func TestTagsCreate_DuplicateFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.CreateTag(db, "dup", "", "op"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code := cmdTags(db, []string{"create", "dup"})
	if code == 0 {
		t.Error("cmdTags create with duplicate should exit non-zero")
	}
}

func TestTagsList_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		code := cmdTags(db, []string{"list"})
		if code != 0 {
			t.Errorf("cmdTags list empty: exit %d", code)
		}
	})
	if !strings.Contains(out, "no tags") {
		t.Errorf("expected empty-tags message; out=%q", out)
	}
}

func TestTagsCreate_EmptyNameFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// create with no positional arg should fail.
	code := cmdTags(db, []string{"create"})
	if code == 0 {
		t.Error("cmdTags create with no name should exit non-zero")
	}
}

// ── force tags remove ─────────────────────────────────────────────────────────

func TestTagsRemove_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.CreateTag(db, "gone", "", "op"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := captureOutput(func() {
		code := cmdTags(db, []string{"remove", "gone", "--assume-yes"})
		if code != 0 {
			t.Errorf("cmdTags remove: exit %d", code)
		}
	})
	if !strings.Contains(out, `"gone" removed`) {
		t.Errorf("missing removal confirmation; out=%q", out)
	}

	// Verify the row is gone.
	tags, err := store.ListTags(db)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	for _, tg := range tags {
		if tg.Name == "gone" {
			t.Error("tag 'gone' still present after remove")
		}
	}
}

func TestTagsRemove_WithRepoTagRefFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed repo + tag + association.
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")
	if err := store.CreateTag(db, "inuse", "", "op"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := store.AddRepoTag(db, "myrepo", "inuse", "op", "test"); err != nil {
		t.Fatalf("seed repotag: %v", err)
	}

	code := cmdTags(db, []string{"remove", "inuse", "--assume-yes"})
	if code == 0 {
		t.Error("cmdTags remove with FK reference should exit non-zero")
	}
}

func TestTagsRemove_NotFoundFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	code := cmdTags(db, []string{"remove", "nosuch", "--assume-yes"})
	if code == 0 {
		t.Error("cmdTags remove non-existent tag should exit non-zero")
	}
}

// ── force repos tag / untag / repos tags ─────────────────────────────────────

func TestReposTag_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test repo")
	if err := store.CreateTag(db, "payments", "Payment repos", "op"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	out := captureOutput(func() {
		cmdReposTag(db, []string{"myrepo", "payments"})
	})
	if !strings.Contains(out, `"payments" added to repo "myrepo"`) {
		t.Errorf("missing confirmation; out=%q", out)
	}

	rows, err := store.ListTagsForRepo(db, "myrepo")
	if err != nil {
		t.Fatalf("ListTagsForRepo: %v", err)
	}
	if len(rows) != 1 || rows[0].Tag != "payments" {
		t.Errorf("expected 1 row with tag=payments, got %+v", rows)
	}
}

func TestReposTag_AutoCreatesTag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test repo")
	// Do NOT pre-create the tag — cmdReposTag should create it automatically.
	out := captureOutput(func() {
		cmdReposTag(db, []string{"myrepo", "autocreated"})
	})
	if !strings.Contains(out, `"autocreated" added to repo "myrepo"`) {
		t.Errorf("missing confirmation; out=%q", out)
	}

	// Verify tag was created.
	tg, err := store.GetTag(db, "autocreated")
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if tg.Name != "autocreated" {
		t.Errorf("tag name: got %q", tg.Name)
	}
}

func TestReposUntag_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test repo")
	if err := store.CreateTag(db, "temp", "", "op"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := store.AddRepoTag(db, "myrepo", "temp", "op", "test"); err != nil {
		t.Fatalf("seed repotag: %v", err)
	}

	out := captureOutput(func() {
		cmdReposUntag(db, []string{"myrepo", "temp"})
	})
	if !strings.Contains(out, `"temp" removed from repo "myrepo"`) {
		t.Errorf("missing confirmation; out=%q", out)
	}

	rows, err := store.ListTagsForRepo(db, "myrepo")
	if err != nil {
		t.Fatalf("ListTagsForRepo: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after untag, got %d", len(rows))
	}
}

func TestReposTags_FilterByRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "repoA", "/tmp/a", "A")
	store.AddRepo(db, "repoB", "/tmp/b", "B")
	if err := store.CreateTag(db, "tagX", "", "op"); err != nil {
		t.Fatalf("seed tagX: %v", err)
	}
	if err := store.CreateTag(db, "tagY", "", "op"); err != nil {
		t.Fatalf("seed tagY: %v", err)
	}
	if err := store.AddRepoTag(db, "repoA", "tagX", "op", "test"); err != nil {
		t.Fatalf("seed repoA/tagX: %v", err)
	}
	if err := store.AddRepoTag(db, "repoB", "tagY", "op", "test"); err != nil {
		t.Fatalf("seed repoB/tagY: %v", err)
	}

	// With --repo repoA: should show tagX only.
	out := captureOutput(func() {
		cmdReposTags(db, []string{"--repo", "repoA"})
	})
	if !strings.Contains(out, "tagX") {
		t.Errorf("missing tagX for repoA; out=%q", out)
	}
	if strings.Contains(out, "tagY") {
		t.Errorf("should not show tagY for repoA; out=%q", out)
	}
}

func TestReposTags_FilterByTag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "repoA", "/tmp/a", "A")
	store.AddRepo(db, "repoB", "/tmp/b", "B")
	if err := store.CreateTag(db, "shared", "", "op"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := store.AddRepoTag(db, "repoA", "shared", "op", "test"); err != nil {
		t.Fatalf("seed repoA: %v", err)
	}
	if err := store.AddRepoTag(db, "repoB", "shared", "op", "test"); err != nil {
		t.Fatalf("seed repoB: %v", err)
	}

	out := captureOutput(func() {
		cmdReposTags(db, []string{"--tag", "shared"})
	})
	if !strings.Contains(out, "repoA") {
		t.Errorf("missing repoA; out=%q", out)
	}
	if !strings.Contains(out, "repoB") {
		t.Errorf("missing repoB; out=%q", out)
	}
}

func TestReposTags_BothFilters_Intersection(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "repoA", "/tmp/a", "A")
	if err := store.CreateTag(db, "tA", "", "op"); err != nil {
		t.Fatalf("seed tA: %v", err)
	}
	if err := store.CreateTag(db, "tB", "", "op"); err != nil {
		t.Fatalf("seed tB: %v", err)
	}
	if err := store.AddRepoTag(db, "repoA", "tA", "op", "test"); err != nil {
		t.Fatalf("seed repoA/tA: %v", err)
	}
	if err := store.AddRepoTag(db, "repoA", "tB", "op", "test"); err != nil {
		t.Fatalf("seed repoA/tB: %v", err)
	}

	// --repo repoA --tag tA: shows only repoA/tA row.
	out := captureOutput(func() {
		cmdReposTags(db, []string{"--repo", "repoA", "--tag", "tA"})
	})
	if !strings.Contains(out, "tA") {
		t.Errorf("missing tA in intersection; out=%q", out)
	}
	if strings.Contains(out, "tB") {
		t.Errorf("should not show tB; out=%q", out)
	}
}

func TestReposTags_NoAssociations(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdReposTags(db, []string{})
	})
	if !strings.Contains(out, "no repo") {
		t.Errorf("expected empty message; out=%q", out)
	}
}

// ── unknown subcommand ────────────────────────────────────────────────────────

func TestCmdTags_UnknownSubcommand(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdTags(db, []string{"bogus"})
	if code == 0 {
		t.Error("unknown subcommand should exit non-zero")
	}
}

func TestCmdTags_NoSubcommand(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdTags(db, []string{})
	if code == 0 {
		t.Error("no subcommand should exit non-zero")
	}
}

func TestCmdTags_HelpFlag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	code := cmdTags(db, []string{"--help"})
	if code != 0 {
		t.Errorf("--help should exit 0, got %d", code)
	}
}

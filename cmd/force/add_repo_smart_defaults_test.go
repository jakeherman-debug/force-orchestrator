package main

// add_repo_smart_defaults_test.go — unit tests for the deterministic
// helpers introduced in Sweep D. These exercise the helpers in isolation
// (no database, no subprocess) so a regression in the derivation logic
// fails loud without dragging in the full add-repo write pipeline.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepoWithRemote creates a fresh git repo at dir with the given
// origin URL configured. Returns the dir for chaining.
func initRepoWithRemote(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := exec.Command("git", "init", "-q", "-b", "main", dir).Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if originURL != "" {
		if err := exec.Command("git", "-C", dir, "remote", "add", "origin", originURL).Run(); err != nil {
			t.Fatalf("remote add: %v", err)
		}
	}
}

func TestDeriveRepoName_FromGitRemote_StripsDotGit(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "weird-basename")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, repo, "git@github.com:org/foo.git")
	got := deriveRepoName(repo)
	if got != "foo" {
		t.Errorf("want %q, got %q", "foo", got)
	}
}

func TestDeriveRepoName_FromGitRemote_HTTPS(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "anything")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, repo, "https://github.com/acme/widget.git")
	got := deriveRepoName(repo)
	if got != "widget" {
		t.Errorf("want %q, got %q", "widget", got)
	}
}

func TestDeriveRepoName_FallsBackToBasename(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "fallback-repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, repo, "")
	got := deriveRepoName(repo)
	if got != "fallback-repo" {
		t.Errorf("want %q, got %q", "fallback-repo", got)
	}
}

func TestDeriveRepoName_HandlesTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "foo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, repo, "")
	got := deriveRepoName(repo + "/")
	if got != "foo" {
		t.Errorf("trailing slash: want %q, got %q", "foo", got)
	}
}

func TestDeriveRepoName_EmptyInput(t *testing.T) {
	if got := deriveRepoName(""); got != "" {
		t.Errorf("empty input must return empty, got %q", got)
	}
}

func TestRepoNameFromRemoteURL_Forms(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"git@github.com:org/foo.git", "foo"},
		{"git@github.com:org/foo", "foo"},
		{"https://github.com/org/foo.git", "foo"},
		{"https://github.com/org/foo", "foo"},
		{"ssh://git@github.com/org/foo.git", "foo"},
		{"https://github.com/org/sub-group/foo.git", "foo"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		got := repoNameFromRemoteURL(c.in)
		if got != c.want {
			t.Errorf("repoNameFromRemoteURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDeriveRepoDescription_FirstParagraph(t *testing.T) {
	dir := t.TempDir()
	readme := `---
title: My Repo
---

# My Repo

This is the canonical first paragraph of the README. It should be
returned exactly, with internal newlines collapsed to spaces.

A second paragraph that should be ignored.
`
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
	got := deriveRepoDescription(dir)
	want := "This is the canonical first paragraph of the README. It should be returned exactly, with internal newlines collapsed to spaces."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestDeriveRepoDescription_SkipsBadges(t *testing.T) {
	dir := t.TempDir()
	readme := `# Project

[![CI](https://example.com/ci.svg)](https://example.com/ci)
[![Coverage](https://example.com/cov.svg)](https://example.com/cov)
![Logo](https://example.com/logo.png)

This is the real description that should appear.
`
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
	got := deriveRepoDescription(dir)
	want := "This is the real description that should appear."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestDeriveRepoDescription_NoReadme(t *testing.T) {
	dir := t.TempDir()
	if got := deriveRepoDescription(dir); got != "" {
		t.Errorf("no README must return empty, got %q", got)
	}
}

func TestDeriveRepoDescription_TruncatesLongParagraph(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 250)
	readme := "# Project\n\n" + long + "\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
	got := deriveRepoDescription(dir)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated output must end with ellipsis, got %q", got)
	}
	// Length: 200 runes of payload + 1 ellipsis rune = 201 runes total.
	gotRunes := []rune(got)
	if len(gotRunes) != repoDescriptionMaxLen+1 {
		t.Errorf("truncated output length = %d runes, want %d", len(gotRunes), repoDescriptionMaxLen+1)
	}
}

func TestDeriveRepoDescription_HandlesReadmeVariants(t *testing.T) {
	cases := []string{"README", "README.md", "readme.md", "Readme.md", "readme"}
	for _, name := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("Hello world.\n"), 0644); err != nil {
			t.Fatal(err)
		}
		got := deriveRepoDescription(dir)
		if got != "Hello world." {
			t.Errorf("variant %q: want %q, got %q", name, "Hello world.", got)
		}
	}
}

func TestDeriveRepoDescription_SkipsHTMLComments(t *testing.T) {
	dir := t.TempDir()
	readme := `<!--
This is a copyright header that should be skipped.
Multiple lines of legalese.
-->

# Project

This sentence is the description.
`
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
	got := deriveRepoDescription(dir)
	if got != "This sentence is the description." {
		t.Errorf("got %q", got)
	}
}

func TestDeriveRepoDescription_SkipsHorizontalRules(t *testing.T) {
	dir := t.TempDir()
	readme := `---

***

This is the description after the rules.
`
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
	got := deriveRepoDescription(dir)
	if got != "This is the description after the rules." {
		t.Errorf("got %q", got)
	}
}

func TestDeriveRepoDescription_EmptyInput(t *testing.T) {
	if got := deriveRepoDescription(""); got != "" {
		t.Errorf("empty input must return empty, got %q", got)
	}
}

func TestExtractFirstParagraph_CRLF(t *testing.T) {
	// Windows line endings in a hand-edited README must not break parsing.
	in := "# Title\r\n\r\nThis is the paragraph.\r\n\r\nSecond paragraph.\r\n"
	got := extractFirstParagraph(in)
	if got != "This is the paragraph." {
		t.Errorf("got %q", got)
	}
}

func TestReorderFlagsFirst(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"flags after positional get hoisted",
			[]string{"/path", "--name", "foo"},
			[]string{"--name", "foo", "/path"},
		},
		{
			"value-flag with =",
			[]string{"/path", "--name=foo"},
			[]string{"--name=foo", "/path"},
		},
		{
			"bool flag does not consume next positional",
			[]string{"/dir", "--assume-yes"},
			[]string{"--assume-yes", "/dir"},
		},
		{
			"-y short bool",
			[]string{"/dir", "-y"},
			[]string{"-y", "/dir"},
		},
		{
			"already in flags-first order",
			[]string{"--name", "foo", "/path"},
			[]string{"--name", "foo", "/path"},
		},
		{
			"after -- everything is positional",
			[]string{"/path", "--", "--name", "foo"},
			[]string{"/path", "--name", "foo"},
		},
		{
			"pure positionals untouched",
			[]string{"a", "b", "c"},
			[]string{"a", "b", "c"},
		},
	}
	for _, c := range cases {
		got := reorderFlagsFirst(c.in, addRepoBoolFlags)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: in=%v\n  got=%v\n want=%v", c.name, c.in, got, c.want)
		}
	}
}

func TestIsHorizontalRule(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"---", true},
		{"***", true},
		{"___", true},
		{"-- -", true},
		{"--", false},
		{"-x-", false},
		{"#", false},
		{"hello", false},
	}
	for _, c := range cases {
		if got := isHorizontalRule(c.in); got != c.want {
			t.Errorf("isHorizontalRule(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

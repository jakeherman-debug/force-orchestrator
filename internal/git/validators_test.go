package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── ValidateRef ──────────────────────────────────────────────────────────────

func TestValidateRef_Accepts(t *testing.T) {
	cases := []string{
		"main",
		"master",
		"feature/add-widget",
		"force/ask-1-widget",
		"agent/R2-D2/task-42",
		"release-2025.04.23",
		"hotfix_1",
		"jake.herman/force/ask-9-validators",
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			if err := ValidateRef(c); err != nil {
				t.Fatalf("ValidateRef(%q) = %v, want nil", c, err)
			}
			if !IsValidRef(c) {
				t.Fatalf("IsValidRef(%q) = false, want true", c)
			}
		})
	}
}

func TestValidateRef_Rejects(t *testing.T) {
	cases := []struct {
		name string
		want string // expected substring of error
	}{
		{"", "empty"},
		{"--upload-pack=/tmp/evil", "leading `-`"},
		{"-rm", "leading `-`"},
		{"/leading-slash", "leading `/`"},
		{".hidden", "leading `.`"},
		{"trailing-slash/", "trailing `/`"},
		{"trailing-dot.", "trailing `.`"},
		{"hotfix.lock", ".lock"},
		{"refs/../evil", "contains `..`"},
		{"..", "contains `..`"},
		{"foo//bar", "contains `//`"},
		{"foo@{1}", "contains `@{`"},
		{"@", "reserved `@`"},
		{"foo\x00bar", "control byte"},
		{"foo\nbar", "control byte"},
		{"foo\tbar", "control byte"},
		{"foo\x7f", "control byte"},
		{"branch with spaces", "forbidden character"},
		{"branch~tilde", "forbidden character"},
		{"branch^caret", "forbidden character"},
		{"branch:colon", "forbidden character"},
		{"branch?q", "forbidden character"},
		{"branch*star", "forbidden character"},
		{"branch[bracket", "forbidden character"},
		{"branch\\back", "forbidden character"},
		{strings.Repeat("a", refNameMaxLen+1), "exceeds"},
	}
	for _, c := range cases {
		c := c
		t.Run(safeLabelForTest(c.name), func(t *testing.T) {
			err := ValidateRef(c.name)
			if err == nil {
				t.Fatalf("ValidateRef(%q) = nil, want error containing %q", c.name, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ValidateRef(%q): error = %v, want to contain %q", c.name, err, c.want)
			}
		})
	}
}

// ── ValidateRepoPath ─────────────────────────────────────────────────────────

func TestValidateRepoPath_Accepts(t *testing.T) {
	// The zero-value options still require absolute + no traversal. Use the
	// test tmp dir (guaranteed absolute) and a fabricated non-existent path
	// so the on-disk checks don't trip.
	tmp := t.TempDir()
	cases := []string{
		tmp,
		filepath.Join(tmp, "sub", "dir"),
		"/usr/local/src/some-repo",     // absolute, doesn't exist — skip-check path
		"/home/operator/code/go/force", // absolute, doesn't exist
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			if err := ValidateRepoPath(c, RepoPathOptions{}); err != nil {
				t.Fatalf("ValidateRepoPath(%q) = %v, want nil", c, err)
			}
		})
	}
}

func TestValidateRepoPath_Rejects(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"empty", "", "empty"},
		{"relative", "relative/path", "not absolute"},
		{"leading-dash", "-/home/x", "leading `-`"},
		{"nul", "/foo\x00bar", "NUL"},
		{"newline", "/foo\nbar", "newline"},
		{"traversal", "/home/../etc/passwd", "`..`"},
		{"dotdot-alone", "/..", "`..`"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRepoPath(c.path, RepoPathOptions{})
			if err == nil {
				t.Fatalf("ValidateRepoPath(%q) = nil, want error containing %q", c.path, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ValidateRepoPath(%q): error = %v, want to contain %q", c.path, err, c.want)
			}
		})
	}
}

func TestValidateRepoPath_RejectsSymlinksWhenRequired(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	// Without RejectSymlinks: symlink is fine (points within base).
	if err := ValidateRepoPath(link, RepoPathOptions{Base: base}); err != nil {
		t.Errorf("symlink inside base should pass without RejectSymlinks, got %v", err)
	}
	// With RejectSymlinks: refused.
	if err := ValidateRepoPath(link, RepoPathOptions{Base: base, RejectSymlinks: true}); err == nil {
		t.Errorf("symlink should be rejected when RejectSymlinks=true")
	}

	// Escaping symlink: link -> /tmp/evil (outside base).
	outside := t.TempDir() // separate tempdir
	escape := filepath.Join(base, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := ValidateRepoPath(escape, RepoPathOptions{Base: base}); err == nil {
		t.Errorf("symlink escaping base should be rejected")
	}
}

// ── ValidateRemoteURL ────────────────────────────────────────────────────────

func TestValidateRemoteURL_Accepts(t *testing.T) {
	cases := []string{
		"git@github.com:acme/api.git",
		"git@github.com:acme/api",
		"https://github.com/acme/api.git",
		"https://github.com/acme/api",
		"ssh://git@github.com/acme/api.git",
		"ssh://git@github.com:22/acme/api.git",
		"git://github.com/acme/api.git",
		"http://internal-git/acme/mirror.git", // plain http accepted (CI mirrors)
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			if err := ValidateRemoteURL(c); err != nil {
				t.Fatalf("ValidateRemoteURL(%q) = %v, want nil", c, err)
			}
		})
	}
}

func TestValidateRemoteURL_Rejects(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"empty", "", "empty"},
		{"leading-dash", "-upload-pack=/tmp/evil", "leading `-`"},
		{"file-scheme", "file:///etc/passwd", "disallowed scheme"},
		{"gopher", "gopher://example.com/1/", "disallowed scheme"},
		{"ext", "ext::sh -c pwnd", "not a recognisable remote"},
		{"nul", "https://host.example\x00evil/repo", "control byte"},
		{"newline", "https://host/repo\n--evil", "control byte"},
		{"upload-pack-embedded", "git@github.com:--upload-pack=/tmp/evil/foo.git", "embedded git-flag"},
		{"receive-pack-embedded", "https://github.com/acme/api?--receive-pack=/bin/sh", "embedded git-flag"},
		{"scp-path-leading-dash", "git@github.com:-not/good.git", "not a recognisable remote"},
		{"scp-traversal", "git@github.com:../../etc/passwd", "not a recognisable remote"},
		{"loopback-ip", "https://127.0.0.1/acme/api.git", "loopback/link-local/RFC1918"},
		{"link-local", "https://169.254.169.254/metadata", "loopback/link-local/RFC1918"},
		{"rfc1918", "https://192.168.1.1/acme/api.git", "loopback/link-local/RFC1918"},
		{"scheme-host-dash", "https://-evil.example/repo", "host begins with `-`"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRemoteURL(c.url)
			if err == nil {
				t.Fatalf("ValidateRemoteURL(%q) = nil, want error containing %q", c.url, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ValidateRemoteURL(%q): error = %v, want to contain %q", c.url, err, c.want)
			}
		})
	}
}

// ── ValidateGHRepoSpec ───────────────────────────────────────────────────────

func TestValidateGHRepoSpec_Accepts(t *testing.T) {
	cases := []string{
		"acme/api",
		"jake.herman/force-orchestrator",
		"gh_foo/bar.lib",
		"a/b",
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			if err := ValidateGHRepoSpec(c); err != nil {
				t.Fatalf("ValidateGHRepoSpec(%q) = %v, want nil", c, err)
			}
		})
	}
}

func TestValidateGHRepoSpec_Rejects(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"empty", ""},
		{"leading-dash", "-acme/api"},
		{"missing-slash", "acmeapi"},
		{"extra-slash", "acme/api/extra"},
		{"whitespace", "acme /api"},
		{"flag-in-owner", "--upload-pack=/tmp/x/y"},
		{"traversal", "acme/../api"},
		{"hash", "acme/api#tag"},
		{"query", "acme/api?x=1"},
		{"scheme", "https://acme/api"},
		{"empty-owner", "/api"},
		{"empty-repo", "acme/"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateGHRepoSpec(c.spec); err == nil {
				t.Fatalf("ValidateGHRepoSpec(%q) = nil, want error", c.spec)
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// safeLabelForTest mirrors safeLabel in audit_pattern_p10_test.go so
// adversarial subtest names don't crash Go's test runner.
func safeLabelForTest(s string) string {
	if s == "" {
		return "empty"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "sym"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

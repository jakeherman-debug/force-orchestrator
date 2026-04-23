package git

import (
	"strings"
	"testing"
)

// ── FuzzValidateRef ──────────────────────────────────────────────────────────
//
// Seeds the corpus from the P10 adversarial set and fuzz-drives ValidateRef
// with arbitrary strings. The invariant under test: ANY accepted ref (err ==
// nil) must also satisfy the hard-coded safety properties enumerated in
// refSafetyInvariants. If ValidateRef ever returns nil for a string that
// violates one of those, the fuzzer will flag the case and we have a concrete
// regression.

func FuzzValidateRef(f *testing.F) {
	// Seed corpus drawn from AUDIT-018/049/050/051/098 and the git-check-ref-
	// format grammar. Every entry MUST currently be rejected.
	seeds := []string{
		"",
		"--upload-pack=/tmp/evil",
		"--receive-pack=/bin/sh",
		"-rm",
		"-rm -rf /",
		"..",
		"refs/../evil",
		"foo\x00bar",
		"foo\nbar",
		"foo\r\n",
		"foo\tbar",
		"foo\x7fbar",
		"@{",
		"branch@{1}",
		"hotfix.lock",
		"trailing/",
		"/leading",
		"double//slash",
		".hidden",
		"trailing.",
		"branch with spaces",
		"branch~tilde",
		"branch^caret",
		"branch:colon",
		"branch?q",
		"branch*star",
		"branch[bracket",
		"branch\\back",
		"@",
		"refs/heads/main\nrefs/heads/evil",
		"main;rm -rf /",
		"main`whoami`",
		"main$(whoami)",
		"main|cat /etc/passwd",
		// And a handful of positive cases so the fuzzer can verify the
		// accept path doesn't drift.
		"main",
		"feature/add-widget",
		"force/ask-1-widget",
		"agent/R2-D2/task-42",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		err := ValidateRef(name)
		if err != nil {
			return // rejection path — always safe
		}
		// Accept path — enforce the safety invariants independently so we
		// catch any future loosening of the validator that would re-expose
		// the CVE-2017-1000117 class.
		if err := checkRefSafetyInvariants(name); err != nil {
			t.Fatalf("ValidateRef accepted %q but invariant check says: %v", name, err)
		}
	})
}

// checkRefSafetyInvariants is the independent, audit-anchored property
// checker. It must NOT share code with ValidateRef — the fuzzer's job is to
// catch any drift between the two.
func checkRefSafetyInvariants(name string) error {
	if name == "" {
		return errFuzz("empty")
	}
	if name[0] == '-' {
		return errFuzz("leading -")
	}
	if name[0] == '/' {
		return errFuzz("leading /")
	}
	if name[0] == '.' {
		return errFuzz("leading .")
	}
	if name[len(name)-1] == '/' {
		return errFuzz("trailing /")
	}
	if strings.HasSuffix(name, ".lock") {
		return errFuzz("trailing .lock")
	}
	if strings.Contains(name, "..") {
		return errFuzz("..")
	}
	if strings.Contains(name, "//") {
		return errFuzz("//")
	}
	if strings.Contains(name, "@{") {
		return errFuzz("@{")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7F {
			return errFuzz("control byte")
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return errFuzz("forbidden char")
		}
	}
	return nil
}

// ── FuzzValidateRepoPath ─────────────────────────────────────────────────────

func FuzzValidateRepoPath(f *testing.F) {
	seeds := []string{
		"",
		"relative/path",
		"-/tmp/leading-dash",
		"/home/../etc/passwd",
		"/..",
		"/foo\x00bar",
		"/foo\nbar",
		"/etc/passwd",
		"/usr/local/src/repo",
		"/tmp/valid/absolute",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, path string) {
		err := ValidateRepoPath(path, RepoPathOptions{})
		if err != nil {
			return
		}
		// Accepted paths must satisfy: non-empty, absolute, no NUL/newline,
		// no leading -, no `..` segment.
		if path == "" {
			t.Fatalf("accepted empty path")
		}
		if path[0] == '-' {
			t.Fatalf("accepted leading-dash path %q", path)
		}
		if strings.ContainsRune(path, 0) {
			t.Fatalf("accepted NUL-containing path %q", path)
		}
		if strings.ContainsAny(path, "\n\r") {
			t.Fatalf("accepted newline-containing path %q", path)
		}
		// Absolute: starts with '/' on unix, or drive-letter on windows —
		// covered by filepath.IsAbs, so just smoke-check '/' here.
		if path[0] != '/' {
			t.Fatalf("accepted non-absolute path %q", path)
		}
		for _, seg := range strings.Split(path, "/") {
			if seg == ".." {
				t.Fatalf("accepted path with `..` segment %q", path)
			}
		}
	})
}

// ── FuzzValidateRemoteURL ────────────────────────────────────────────────────

func FuzzValidateRemoteURL(f *testing.F) {
	seeds := []string{
		"",
		"-upload-pack=/tmp/evil",
		"file:///etc/passwd",
		"ext::sh -c pwnd",
		"gopher://example.com/1/",
		"https://127.0.0.1/repo",
		"https://169.254.169.254/meta",
		"https://192.168.1.1/x",
		"https://10.0.0.1/x",
		"https://-evil.example/repo",
		"git@github.com:--upload-pack=/tmp/evil/foo.git",
		"git@github.com:-not/good.git",
		"git@github.com:../../etc/passwd",
		"https://host.example\x00evil/repo",
		"https://host/repo\n--evil",
		"git@github.com:acme/api.git",
		"https://github.com/acme/api.git",
		"ssh://git@github.com/acme/api.git",
		"git://github.com/acme/api.git",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		err := ValidateRemoteURL(raw)
		if err != nil {
			return
		}
		// Accept path — the URL must be safe to hand to git or gh.
		if raw == "" {
			t.Fatalf("accepted empty URL")
		}
		if raw[0] == '-' {
			t.Fatalf("accepted leading-dash URL %q", raw)
		}
		for i := 0; i < len(raw); i++ {
			c := raw[i]
			if c < 0x20 || c == 0x7F {
				t.Fatalf("accepted URL with control byte: %q", raw)
			}
		}
		low := strings.ToLower(raw)
		if strings.Contains(low, "--upload-pack=") ||
			strings.Contains(low, "--receive-pack=") ||
			strings.Contains(low, "--config=") ||
			strings.Contains(low, "--exec=") {
			t.Fatalf("accepted URL with embedded git flag: %q", raw)
		}
		if strings.HasPrefix(low, "file://") {
			t.Fatalf("accepted file:// URL: %q", raw)
		}
	})
}

type fuzzErr string

func (e fuzzErr) Error() string { return string(e) }

func errFuzz(reason string) error { return fuzzErr(reason) }

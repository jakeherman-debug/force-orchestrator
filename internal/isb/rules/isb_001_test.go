package rules

import "testing"

// TestISB001_Red_GitHubPAT — a literal containing a ghp_-prefixed
// token triggers a finding.
func TestISB001_Red_GitHubPAT(t *testing.T) {
	src := `package x
const token = "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
`
	out := runRule(t, isb001{}, "internal/foo/leak.go", src)
	assertHasFinding(t, out, "ISB-001", "")
}

// TestISB001_Red_BasicAuthURL — basic-auth credentials embedded in a
// URL trigger a finding via the regex fallback.
func TestISB001_Red_BasicAuthURL(t *testing.T) {
	src := `package x
const url = "https://user:hunter2@api.example.com/foo"
`
	out := runRule(t, isb001{}, "internal/foo/leak.go", src)
	assertHasFinding(t, out, "ISB-001", "")
}

// TestISB001_Green_NoSecret — neutral source produces no finding.
func TestISB001_Green_NoSecret(t *testing.T) {
	src := `package x
const greeting = "hello world this is a longer non-secret string"
`
	out := runRule(t, isb001{}, "internal/foo/clean.go", src)
	assertNoFindings(t, out)
}

// TestISB001_Green_TestFileExempt — _test.go files don't get the
// "named-like-a-credential" heuristic to keep the fixture noise down.
func TestISB001_Green_TestFileExempt(t *testing.T) {
	src := `package x
var fakeToken = "this-is-a-fake-test-fixture-not-a-real-token"
`
	out := runRule(t, isb001{}, "internal/foo/leak_test.go", src)
	assertNoFindings(t, out)
}

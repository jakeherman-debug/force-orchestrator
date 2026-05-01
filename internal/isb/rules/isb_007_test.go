package rules

import "testing"

// TestISB007_Red_RemoveAllWithoutContainment — os.RemoveAll without a
// containment check triggers a finding.
func TestISB007_Red_RemoveAllWithoutContainment(t *testing.T) {
	src := `package x
import "os"
func F(p string) error { return os.RemoveAll(p) }
`
	out := runRule(t, isb007{}, "internal/foo/d.go", src)
	assertHasFinding(t, out, "ISB-007", "")
}

// TestISB007_Red_GitCleanFdx — exec.Command("git","clean","-fdx",..)
// without a containment check triggers.
func TestISB007_Red_GitCleanFdx(t *testing.T) {
	src := `package x
import "os/exec"
func F(p string) error {
	return exec.Command("git", "-C", p, "clean", "-fdx").Run()
}
`
	out := runRule(t, isb007{}, "internal/foo/c.go", src)
	assertHasFinding(t, out, "ISB-007", "")
}

// TestISB007_Green_WithContainmentCheck — same destructive op but
// preceded by AssertWithinRepo passes.
func TestISB007_Green_WithContainmentCheck(t *testing.T) {
	src := `package x
import "os"
func AssertWithinRepo(p string) error { return nil }
func F(p string) error {
	if err := AssertWithinRepo(p); err != nil { return err }
	return os.RemoveAll(p)
}
`
	out := runRule(t, isb007{}, "internal/foo/d.go", src)
	assertNoFindings(t, out)
}

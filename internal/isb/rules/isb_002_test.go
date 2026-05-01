package rules

import "testing"

// TestISB002_Red_PositionalBeforeDoubleDash — exec.Command with a
// variable arg before any `--` triggers a finding.
func TestISB002_Red_PositionalBeforeDoubleDash(t *testing.T) {
	src := `package x
import "os/exec"
func F(branch string) {
	_ = exec.Command("git", "checkout", branch)
}
`
	out := runRule(t, isb002{}, "internal/foo/x.go", src)
	assertHasFinding(t, out, "ISB-002", "")
}

// TestISB002_Green_DoubleDashSeparator — once a literal "--" is
// present before the variable arg, the rule is satisfied.
func TestISB002_Green_DoubleDashSeparator(t *testing.T) {
	src := `package x
import "os/exec"
func F(branch string) {
	_ = exec.Command("git", "checkout", "--", branch)
}
`
	out := runRule(t, isb002{}, "internal/foo/x.go", src)
	assertNoFindings(t, out)
}

// TestISB002_Green_AllLiterals — exec.Command with only string
// literals doesn't trip the rule.
func TestISB002_Green_AllLiterals(t *testing.T) {
	src := `package x
import "os/exec"
func F() {
	_ = exec.Command("git", "status", "--porcelain")
}
`
	out := runRule(t, isb002{}, "internal/foo/x.go", src)
	assertNoFindings(t, out)
}

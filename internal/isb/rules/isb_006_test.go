package rules

import "testing"

// TestISB006_Red_OpenModeOnSensitivePath — os.WriteFile with 0777 in
// /etc/ triggers a finding.
func TestISB006_Red_OpenModeOnSensitivePath(t *testing.T) {
	src := `package x
import "os"
func F() {
	_ = os.WriteFile("/etc/foo.conf", []byte("x"), 0o777)
}
`
	out := runRule(t, isb006{}, "internal/foo/p.go", src)
	assertHasFinding(t, out, "ISB-006", "")
}

// TestISB006_Green_TightMode — mode 0600 passes.
func TestISB006_Green_TightMode(t *testing.T) {
	src := `package x
import "os"
func F() {
	_ = os.WriteFile("/etc/foo.conf", []byte("x"), 0o600)
}
`
	out := runRule(t, isb006{}, "internal/foo/p.go", src)
	assertNoFindings(t, out)
}

// TestISB006_Green_NonSensitivePathLiteral — open mode + literal
// path that is NOT sensitive doesn't trip the rule.
func TestISB006_Green_NonSensitivePathLiteral(t *testing.T) {
	src := `package x
import "os"
func F() {
	_ = os.WriteFile("/tmp/foo.conf", []byte("x"), 0o777)
}
`
	out := runRule(t, isb006{}, "internal/foo/p.go", src)
	assertNoFindings(t, out)
}

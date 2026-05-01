package rules

import "testing"

// TestISB004_Red_HTTPGetWithoutValidator — http.Get without an
// earlier ValidateOutboundURL call triggers a finding.
func TestISB004_Red_HTTPGetWithoutValidator(t *testing.T) {
	src := `package x
import "net/http"
func F(url string) {
	_, _ = http.Get(url)
}
`
	out := runRule(t, isb004{}, "internal/foo/h.go", src)
	assertHasFinding(t, out, "ISB-004", "")
}

// TestISB004_Green_ValidatorCalledFirst — when ValidateOutboundURL is
// called in the same function before the http.Get, the rule passes.
func TestISB004_Green_ValidatorCalledFirst(t *testing.T) {
	src := `package x
import "net/http"
func ValidateOutboundURL(u string) error { return nil }
func F(url string) {
	_ = ValidateOutboundURL(url)
	_, _ = http.Get(url)
}
`
	out := runRule(t, isb004{}, "internal/foo/h.go", src)
	assertNoFindings(t, out)
}

// TestISB004_Green_NoOutboundCall — function with no http.* call.
func TestISB004_Green_NoOutboundCall(t *testing.T) {
	src := `package x
func F() string { return "hello" }
`
	out := runRule(t, isb004{}, "internal/foo/h.go", src)
	assertNoFindings(t, out)
}

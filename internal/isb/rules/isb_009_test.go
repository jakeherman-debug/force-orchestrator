package rules

import "testing"

// TestISB009_Red_ReadAllRespBody — io.ReadAll(resp.Body) without
// LimitReader triggers a finding.
func TestISB009_Red_ReadAllRespBody(t *testing.T) {
	src := `package x
import (
	"io"
	"net/http"
)
func F(resp *http.Response) ([]byte, error) {
	return io.ReadAll(resp.Body)
}
`
	out := runRule(t, isb009{}, "internal/foo/r.go", src)
	assertHasFinding(t, out, "ISB-009", "")
}

// TestISB009_Green_LimitReaderWrap — same shape but LimitReader is
// called first.
func TestISB009_Green_LimitReaderWrap(t *testing.T) {
	src := `package x
import (
	"io"
	"net/http"
)
func F(resp *http.Response) ([]byte, error) {
	r := io.LimitReader(resp.Body, 1<<20)
	return io.ReadAll(r)
}
`
	out := runRule(t, isb009{}, "internal/foo/r.go", src)
	assertNoFindings(t, out)
}

// TestISB009_Green_NoExternalReader — io.ReadAll on a non-external
// reader name doesn't trip.
func TestISB009_Green_NoExternalReader(t *testing.T) {
	src := `package x
import (
	"io"
	"strings"
)
func F() ([]byte, error) {
	src := strings.NewReader("hello")
	return io.ReadAll(src)
}
`
	out := runRule(t, isb009{}, "internal/foo/r.go", src)
	assertNoFindings(t, out)
}

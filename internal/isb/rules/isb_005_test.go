package rules

import "testing"

// TestISB005_Red_MutatingPathBareHandler — http.HandleFunc on a
// /update path with a bare handler triggers a finding.
func TestISB005_Red_MutatingPathBareHandler(t *testing.T) {
	src := `package x
import "net/http"
func updateHandler(w http.ResponseWriter, r *http.Request) {}
func main() {
	http.HandleFunc("/api/update", updateHandler)
}
`
	out := runRule(t, isb005{}, "internal/foo/srv.go", src)
	assertHasFinding(t, out, "ISB-005", "")
}

// TestISB005_Green_WrappedBySecurityMiddleware — handler wrapped by
// securityMiddleware passes.
func TestISB005_Green_WrappedBySecurityMiddleware(t *testing.T) {
	src := `package x
import "net/http"
func updateHandler(w http.ResponseWriter, r *http.Request) {}
func securityMiddleware(h http.HandlerFunc) http.HandlerFunc { return h }
func main() {
	http.HandleFunc("/api/update", securityMiddleware(updateHandler))
}
`
	out := runRule(t, isb005{}, "internal/foo/srv.go", src)
	assertNoFindings(t, out)
}

// TestISB005_Green_ReadOnlyPath — non-mutating path doesn't trip the
// rule.
func TestISB005_Green_ReadOnlyPath(t *testing.T) {
	src := `package x
import "net/http"
func listHandler(w http.ResponseWriter, r *http.Request) {}
func main() {
	http.HandleFunc("/api/list", listHandler)
}
`
	out := runRule(t, isb005{}, "internal/foo/srv.go", src)
	assertNoFindings(t, out)
}

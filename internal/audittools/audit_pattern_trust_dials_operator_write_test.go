// D3 P6A.6 — Trust dials operator-write discipline.
//
// The brief: "no system code path inserts into OperatorTrustDials with
// set_by='operator' from a non-operator-routed handler". The audit
// walks production Go code outside the dashboard handler files and
// asserts no string literal `"operator"` appears as a SetBy value.
//
// Approved sites for SetBy='operator':
//   - internal/dashboard/handlers_trust_dials.go (the operator API)
//   - cmd/force/trust_cmds.go (the operator CLI)
package audittools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var trustDialsOperatorWriteAllowlist = map[string]string{
	"internal/dashboard/handlers_trust_dials.go": "operator API endpoint — the legitimate operator path",
	"cmd/force/trust_cmds.go":                    "operator CLI — direct operator invocation",
	"internal/store/trust_dials.go":              "the helper itself; constants live here",
	// Test files: tests run with set_by='operator' on synthetic data.
}

func TestPattern_TrustDialsOperatorWriteDiscipline(t *testing.T) {
	root := repoRootForTrust(t)
	dirs := []string{
		filepath.Join(root, "internal/agents"),
		filepath.Join(root, "internal/dashboard"),
		filepath.Join(root, "internal/store"),
		filepath.Join(root, "cmd"),
	}
	var offenders []string
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			relSlash := filepath.ToSlash(rel)
			if _, ok := trustDialsOperatorWriteAllowlist[relSlash]; ok {
				return nil
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			text := string(src)
			// Look for SetTrustDial(...) calls AND "operator" SetBy string.
			if !strings.Contains(text, "SetTrustDial(") {
				return nil
			}
			// Check whether the file uses TrustDialOperator constant or
			// the string literal "operator" as the set_by value.
			if strings.Contains(text, `TrustDialOperator`) || strings.Contains(text, `set_by="operator"`) || strings.Contains(text, `SetBy: "operator"`) {
				offenders = append(offenders, relSlash)
			}
			return nil
		})
		if err != nil {
			t.Logf("walk %s: %v", dir, err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("Pattern P-trust-dials-discipline: SetBy=TrustDialOperator from non-operator-routed file:\n  %s\n"+
			"These writes must route through the operator API or CLI, not synthesised by an agent.",
			strings.Join(offenders, "\n  "))
	}
}

func repoRootForTrust(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("could not locate repo root from %s", wd)
	return ""
}

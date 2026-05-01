// D3 P6B.8 — Pattern P-AnnotationsOperatorOnly: only operator-facing
// code paths may INSERT INTO OperatorEventAnnotations. Walks
// production code under internal/ (excluding internal/store where
// the CRUD itself lives, and excluding the dashboard handler that
// fronts operator HTTP requests, and excluding cmd/force where the
// CLI parity command runs).
//
// Forbidden: agent code paths (Captain, Council, Medic, ...) writing
// annotations on the operator's behalf. Annotations must come from
// the operator UI or the CLI, never from a system path that masks
// agent action as operator note.
package audittools

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pAnnotationsAllowedDirs are the directory roots where INSERTs into
// OperatorEventAnnotations are legitimate.
var pAnnotationsAllowedDirs = []string{
	"internal/store",          // the CRUD layer itself
	"internal/dashboard",      // operator-facing HTTP handlers
	"cmd/force",               // CLI parity (`force annotate`)
	"internal/audittools",     // this test file references the table name
}

func TestPattern_AnnotationsOperatorOnly(t *testing.T) {
	root := repoRootPAnn(t)
	walkRoots := []string{"internal", "cmd"}
	type hit struct {
		path string
		line int
	}
	var hits []hit
	for _, walkRoot := range walkRoots {
		_ = filepath.WalkDir(filepath.Join(root, walkRoot), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rErr := os.ReadFile(path)
			if rErr != nil {
				return nil
			}
			content := string(b)
			if !strings.Contains(content, "OperatorEventAnnotations") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			// Allow if under one of the operator-facing roots.
			allowed := false
			for _, allowedDir := range pAnnotationsAllowedDirs {
				if strings.HasPrefix(rel, allowedDir) {
					allowed = true
					break
				}
			}
			if allowed {
				return nil
			}
			// Detect actual INSERT/UPDATE/DELETE — not just a comment.
			lines := strings.Split(content, "\n")
			for ln, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
					continue
				}
				upper := strings.ToUpper(trimmed)
				if (strings.Contains(upper, "INSERT INTO OPERATOREVENTANNOTATIONS") ||
					strings.Contains(upper, "UPDATE OPERATOREVENTANNOTATIONS") ||
					strings.Contains(upper, "DELETE FROM OPERATOREVENTANNOTATIONS")) ||
					strings.Contains(line, "InsertAnnotation(") {
					hits = append(hits, hit{rel, ln + 1})
				}
			}
			return nil
		})
	}
	if len(hits) > 0 {
		var msg strings.Builder
		msg.WriteString("Pattern P-AnnotationsOperatorOnly: non-operator paths writing OperatorEventAnnotations:\n")
		for _, h := range hits {
			msg.WriteString("  " + h.path + ":")
			// inline itoa to avoid extra import
			n := h.line
			if n == 0 {
				msg.WriteString("0")
			} else {
				buf := []byte{}
				for n > 0 {
					buf = append([]byte{byte('0' + n%10)}, buf...)
					n /= 10
				}
				msg.Write(buf)
			}
			msg.WriteString("\n")
		}
		msg.WriteString("\nFix: route through store.InsertAnnotation from an operator-facing path only (dashboard handler or CLI command).\n")
		t.Error(msg.String())
	}
}

func repoRootPAnn(t *testing.T) string {
	t.Helper()
	wd, _ := filepath.Abs(".")
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found from %s", wd)
	return ""
}

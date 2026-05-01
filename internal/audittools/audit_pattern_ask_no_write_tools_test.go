// D3 P6B.10 — Pattern P-AskNoWriteTools: the Ask handler must not
// reach into any store mutator. Walks internal/agents/ask_handler.go
// for forbidden mutator call references.
//
// Allowed: any *read* helper (SearchDrill, GetConfig, QueryRow, etc.).
// Forbidden: writers like UpdateBountyStatus, FailBounty, SendMail,
// InsertEscalation, UpsertFleetRule, SetOperatorTrustDial,
// InsertConvoyReviewCycle, InsertAnnotation.

package audittools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var pAskForbidden = []string{
	"UpdateBountyStatus", "FailBounty",
	"UpsertFleetRule",
	"InsertEscalation", "EscalateOpen",
	"SendMail",
	"SetOperatorTrustDial",
	"InsertConvoyReviewCycle",
	"InsertAnnotation",
	"UpdateAnnotation",
	"DeleteAnnotation",
}

func TestPattern_AskNoWriteTools(t *testing.T) {
	root := repoRootPAsk(t)
	path := filepath.Join(root, "internal/agents/ask_handler.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ask_handler.go: %v", err)
	}
	src := string(b)

	for _, forbidden := range pAskForbidden {
		if strings.Contains(src, forbidden+"(") {
			t.Errorf("Pattern P-AskNoWriteTools: ask_handler.go must not call %s — Ask is read-only", forbidden)
		}
	}
	// Also no UPDATE/DELETE/INSERT against any non-trivial table.
	if regexp.MustCompile(`(?i)\bUPDATE\s+[A-Za-z_]+\s+SET\b`).MatchString(src) {
		t.Errorf("Pattern P-AskNoWriteTools: ask_handler.go contains UPDATE")
	}
	if regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+[A-Za-z_]+`).MatchString(src) {
		t.Errorf("Pattern P-AskNoWriteTools: ask_handler.go contains DELETE")
	}
	if regexp.MustCompile(`(?i)\bINSERT\s+INTO\s+[A-Za-z_]+`).MatchString(src) {
		t.Errorf("Pattern P-AskNoWriteTools: ask_handler.go contains INSERT")
	}
}

func repoRootPAsk(t *testing.T) string {
	t.Helper()
	wd, _ := filepath.Abs(".")
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found")
	return ""
}

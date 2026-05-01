package rules

import (
	"testing"

	"force-orchestrator/internal/bos"
)

func TestBOS011_Red_CompositeLiteral(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/clients/librarian"
type myAgent struct{}
func newAgent() *myAgent {
	_ = &librarian.InProcessClient{}
	return &myAgent{}
}
`
	out := runRule(t, bos011{}, "internal/agents/example.go", src)
	assertHasFinding(t, out, "BOS-011", "InProcessClient")
}

// Verify severity is block.
func TestBOS011_BlockSeverity(t *testing.T) {
	r := bos011{}
	if r.Severity() != bos.SeverityBlock {
		t.Fatalf("BOS-011 severity: got %v, want block", r.Severity())
	}
}

func TestBOS011_Green_FactoryCall(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/clients/librarian"
type myAgent struct{ lib librarian.Client }
func newAgent() *myAgent {
	return &myAgent{lib: librarian.NewInProcess(nil)}
}
`
	out := runRule(t, bos011{}, "internal/agents/example.go", src)
	assertNoFindings(t, out)
}

// Out-of-scope: tests are exempt, and non-agent dirs are exempt.
func TestBOS011_NotAgentDir(t *testing.T) {
	src := `
package other
import "force-orchestrator/internal/clients/librarian"
func mk() { _ = &librarian.InProcessClient{} }
`
	out := runRule(t, bos011{}, "cmd/force/other.go", src)
	assertNoFindings(t, out)
}

func TestBOS011_TestFileExempt(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/clients/librarian"
func helper() { _ = &librarian.InProcessClient{} }
`
	out := runRule(t, bos011{}, "internal/agents/example_test.go", src)
	assertNoFindings(t, out)
}

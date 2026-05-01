package rules

import "testing"

func TestBOS002_Red_NoMarker(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/store"
func DoWork() {
	_ = store.UpdateBountyStatus(nil, 1, "Pending")
}
`
	out := runRule(t, bos002{}, "internal/agents/something.go", src)
	assertHasFinding(t, out, "BOS-002", "TODO(Fix #8b)")
}

func TestBOS002_Green_WithMarker(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/store"
func DoWork() {
	// TODO(Fix #8b): downstream caller already routed this error.
	_ = store.UpdateBountyStatus(nil, 1, "Pending")
}
`
	out := runRule(t, bos002{}, "internal/agents/something.go", src)
	assertNoFindings(t, out)
}

// Non-store discards are out of scope for BOS-002.
func TestBOS002_NotStoreCall(t *testing.T) {
	src := `
package agents
import "fmt"
func DoWork() {
	_ = fmt.Errorf("ignored")
}
`
	out := runRule(t, bos002{}, "internal/agents/something.go", src)
	assertNoFindings(t, out)
}

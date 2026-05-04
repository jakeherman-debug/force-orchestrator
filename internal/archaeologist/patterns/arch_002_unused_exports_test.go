package patterns

import (
	"os"
	"testing"

	"force-orchestrator/internal/archaeologist"
)

// TestArchaeologistARCH002_StubReturnsZero is the D8-merge-gated
// pin: until D8 lands, the cross-repo lookup degrades to "graph
// unavailable" and Scan returns no hits. After D8, the
// integration test (in the agents package) re-enables; this unit
// test stays as the stub-shape contract.
func TestArchaeologistARCH002_StubReturnsZero(t *testing.T) {
	if os.Getenv("D8_GRAPH_AVAILABLE") != "" {
		t.Skip("D8-MERGE-GATE: ARCH-002 cross-repo wiring lit up — this stub-shape test no longer applies")
	}
	dir := t.TempDir()
	writeF(t, dir+"/exported.go", "package x\n\nfunc Public() {}\n")

	hits := NewARCH002().Scan(&archaeologist.Repo{ID: 1, Name: "stub-repo", LocalPath: dir})
	if len(hits) != 0 {
		t.Fatalf("D8-merge-gate stub: expected 0 ARCH-002 hits, got %d", len(hits))
	}
}

// TestArchaeologistARCH002_LookupSentinel pins the sentinel "graph
// unavailable" return value. If D8 wiring lands and the lookup
// returns a real consumer count, this test stays valuable as the
// pre-wiring shape regression.
func TestArchaeologistARCH002_LookupSentinel(t *testing.T) {
	got := lookupCrossRepoConsumers("force-orchestrator", "force-orchestrator/internal/foo.Bar")
	if got != -1 {
		t.Errorf("D8-merge-gate stub: lookupCrossRepoConsumers = %d, want -1 (sentinel)", got)
	}
}

package agents

import (
	"testing"
)

// TestArchaeologistD9ExitCriterion5_BlastRadius is exit criterion #5
// of D9 — seed 20 sites of a deprecated-API pattern, run the
// Archaeologist sweep, assert it proposes a Feature, and verify the
// Feature's blast_radius_json (populated by D8's cross-repo graph)
// contains all 20 sites.
//
// D8 has not yet merged; the blast-radius enrichment seam does not
// exist on main. The shape we will test post-merge:
//
//	1. Plant 20 ARCH-001 hits across N repos.
//	2. Run dogArchaeologistSweep + drain the queue.
//	3. Read the emitted PromotionProposals row.
//	4. Assert evidence_summary_json.blast_radius_json (added by D8's
//	   blast-radius enricher in the candidate pipeline) lists all 20
//	   sites.
//
// Exit-criterion #5 is gated on D8's merge per the D9-Archaeologist
// scope note in the implementation brief. Re-enable by removing the
// Skip line below once D8 lands in main.
func TestArchaeologistD9ExitCriterion5_BlastRadius(t *testing.T) {
	t.Skip("D8-MERGE-GATE: re-enable after D8 lands in main")
}

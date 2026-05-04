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
// D8 Track 1 (the cross-repo dependency graph schema + dog) has
// merged on main as of commit 635f699, which introduces the
// `internal/clients/graph` Client interface and the symbol/edge
// tables. However, the Track 1 in-process implementation still
// returns graph.ErrNotImplemented from BlastRadius — the actual
// transitive-consumer query and the Chancellor-side enricher that
// pipes its output into PromotionProposals.evidence_summary_json
// land in D8 Track 2 (D8-Chancellor blast-radius integration). The
// shape we will test post-Track-2:
//
//	1. Plant 20 ARCH-001 hits across N repos.
//	2. Run dogArchaeologistSweep + drain the queue.
//	3. Read the emitted PromotionProposals row.
//	4. Assert evidence_summary_json.blast_radius_json (added by
//	   D8 Track 2's blast-radius enricher in the candidate
//	   pipeline) lists all 20 sites.
//
// Exit-criterion #5 is therefore gated on D8 Track 2's merge, not
// merely on Track 1. Re-enable by removing the Skip line below once
// the Chancellor blast-radius integration lands in main.
func TestArchaeologistD9ExitCriterion5_BlastRadius(t *testing.T) {
	t.Skip("D8-T2-MERGE-GATE: re-enable after D8 Track 2 (Chancellor blast-radius integration) lands in main; D8 Track 1 only provides the schema + ErrNotImplemented BlastRadius")
}

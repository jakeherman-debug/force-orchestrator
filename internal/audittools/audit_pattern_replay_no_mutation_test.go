// D3 P6B.7 — Pattern P-Replay: ReplayDecision must not mutate live
// state. Walks internal/agents/replay.go for any reach into a
// non-replay store mutator (UpdateBountyStatus, FailBounty, FleetRules
// upserts, ConvoyReviewCycles INSERT, Escalations INSERT, FleetMail
// send, OperatorTrustDials write).
//
// Allowed writes inside replay.go:
//   - INSERT INTO ReplayResults (the replay's audit row)
//   - INSERT INTO LLMCallTranscripts (the replay's OWN transcript row)
//
// Anything else is a violation.
package audittools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// p_replay_forbidden lists store mutator names + write SQL fragments
// that the replay code path must never touch.
var pReplayForbidden = []string{
	"UpdateBountyStatus",
	"FailBounty",
	"UpsertFleetRule",
	"InsertEscalation", "EscalateOpen",
	"SendMail",
	"SetOperatorTrustDial",
	"InsertConvoyReviewCycle",
}

// pReplayAllowedTables names the tables the replay path is permitted
// to write to. Detected lexically by an INSERT INTO <name> regex.
var pReplayAllowedTables = map[string]bool{
	"ReplayResults":       true,
	"LLMCallTranscripts": true,
}

func TestPattern_ReplayNoMutation(t *testing.T) {
	root := repoRootPReplay(t)
	path := filepath.Join(root, "internal/agents/replay.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replay.go: %v", err)
	}
	src := string(b)

	for _, forbidden := range pReplayForbidden {
		if strings.Contains(src, forbidden+"(") {
			t.Errorf("Pattern P-Replay: replay.go must not call %s — replay is read-only on live state", forbidden)
		}
	}

	// Detect any `INSERT INTO <Table>` and check the table is on
	// the allowed list.
	insertRe := regexp.MustCompile(`(?i)INSERT\s+INTO\s+([A-Za-z_]+)`)
	for _, m := range insertRe.FindAllStringSubmatch(src, -1) {
		if len(m) < 2 {
			continue
		}
		table := m[1]
		if !pReplayAllowedTables[table] {
			t.Errorf("Pattern P-Replay: replay.go writes to forbidden table %q (allowed: ReplayResults, LLMCallTranscripts)", table)
		}
	}

	// And no UPDATE / DELETE statements at all.
	if regexp.MustCompile(`(?i)\bUPDATE\s+[A-Za-z_]+\s+SET\b`).MatchString(src) {
		t.Errorf("Pattern P-Replay: replay.go contains an UPDATE — replay must not mutate existing rows")
	}
	if regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+[A-Za-z_]+`).MatchString(src) {
		t.Errorf("Pattern P-Replay: replay.go contains a DELETE — replay must not remove rows")
	}
}

func repoRootPReplay(t *testing.T) string {
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

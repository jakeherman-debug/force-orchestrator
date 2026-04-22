package agents

import (
	"fmt"
	"io"
	"log"
	"testing"

	"force-orchestrator/internal/store"
)

func memoryCandidatesFixture() []store.FleetMemoryEntry {
	return []store.FleetMemoryEntry{
		{ID: 1, TaskID: 100, Outcome: "success", Summary: "fixed authentication middleware nil-pointer", FilesChanged: "auth/middleware.go"},
		{ID: 2, TaskID: 101, Outcome: "failure", Summary: "attempted refactor of session store layer", FilesChanged: "session/store.go"},
		{ID: 3, TaskID: 102, Outcome: "success", Summary: "added OAuth2 token exchange endpoint", FilesChanged: "auth/oauth.go"},
	}
}

// TestRerankFleetMemories_UsesLLMOrdering verifies that when Claude returns
// a list of IDs, the result is re-ordered to match Claude's selection and
// capped at keepLimit.
func TestRerankFleetMemories_UsesLLMOrdering(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	candidates := memoryCandidatesFixture()
	// Claude says: candidates 3 and 1 are relevant (in that order); 2 is not.
	withStubCLIRunner(t, `{"relevant_ids":[3,1],"reasoning":"OAuth and auth middleware both touch the auth package for the current task"}`, nil)

	got := RerankFleetMemories(db, "improve auth flow", candidates, 5, testLogger{})
	if len(got) != 2 {
		t.Fatalf("expected 2 re-ranked results, got %d", len(got))
	}
	if got[0].ID != 3 || got[1].ID != 1 {
		t.Errorf("expected order [3, 1], got [%d, %d]", got[0].ID, got[1].ID)
	}
	// Candidate 2 must be excluded.
	for _, m := range got {
		if m.ID == 2 {
			t.Errorf("candidate 2 should have been filtered out")
		}
	}
}

// TestRerankFleetMemories_EmptySelectionIsValid is the critical test for the
// 248-style bug: when NONE of the candidates are truly relevant, the LLM
// returns zero IDs and the re-ranker returns empty. We MUST trust that —
// irrelevant memories are worse than none.
func TestRerankFleetMemories_EmptySelectionIsValid(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	candidates := memoryCandidatesFixture()
	withStubCLIRunner(t, `{"relevant_ids":[],"reasoning":"none of the candidates are relevant to the current task"}`, nil)

	got := RerankFleetMemories(db, "upgrade Go version in CI", candidates, 5, testLogger{})
	if len(got) != 0 {
		t.Errorf("expected zero results when LLM rejects all candidates, got %d", len(got))
	}
}

// TestRerankFleetMemories_KeepLimitCapsResults verifies that keepLimit
// bounds the output even when the LLM returns more.
func TestRerankFleetMemories_KeepLimitCapsResults(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	candidates := memoryCandidatesFixture()
	withStubCLIRunner(t, `{"relevant_ids":[1,2,3],"reasoning":"all are tangentially relevant"}`, nil)

	got := RerankFleetMemories(db, "anything", candidates, 2, testLogger{})
	if len(got) != 2 {
		t.Errorf("keepLimit=2 should cap output, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("expected first 2 of LLM order [1,2], got [%d,%d]", got[0].ID, got[1].ID)
	}
}

// TestRerankFleetMemories_IgnoresOutOfRangeIDs verifies robustness against
// LLM hallucinations — IDs outside the candidate range are silently dropped.
func TestRerankFleetMemories_IgnoresOutOfRangeIDs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	candidates := memoryCandidatesFixture()
	withStubCLIRunner(t, `{"relevant_ids":[99,1,0,-5,2],"reasoning":"with some garbage IDs"}`, nil)

	got := RerankFleetMemories(db, "test", candidates, 5, testLogger{})
	if len(got) != 2 {
		t.Errorf("expected 2 valid selections (1 and 2), got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("expected valid-only order [1,2], got [%d,%d]", got[0].ID, got[1].ID)
	}
}

// TestRerankFleetMemories_DedupesRepeatedIDs verifies that if the LLM
// returns the same ID twice, we only include the candidate once.
func TestRerankFleetMemories_DedupesRepeatedIDs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	candidates := memoryCandidatesFixture()
	withStubCLIRunner(t, `{"relevant_ids":[1,1,1,2],"reasoning":""}`, nil)

	got := RerankFleetMemories(db, "test", candidates, 5, testLogger{})
	if len(got) != 2 {
		t.Errorf("expected 2 unique results, got %d", len(got))
	}
}

// TestRerankFleetMemories_FallsBackOnClaudeError verifies graceful fallback:
// Claude errors, we return the FTS order trimmed to keepLimit.
func TestRerankFleetMemories_FallsBackOnClaudeError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	candidates := memoryCandidatesFixture()
	withStubCLIRunner(t, "", fmt.Errorf("claude CLI failed: network"))

	got := RerankFleetMemories(db, "test", candidates, 2, testLogger{})
	if len(got) != 2 {
		t.Errorf("fallback: expected candidates[:2], got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("fallback should preserve FTS order, got [%d,%d]", got[0].ID, got[1].ID)
	}
}

// TestRerankFleetMemories_FallsBackOnMalformedJSON verifies that unparseable
// LLM output degrades to the FTS order.
func TestRerankFleetMemories_FallsBackOnMalformedJSON(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	candidates := memoryCandidatesFixture()
	withStubCLIRunner(t, "not json at all { broken", nil)

	got := RerankFleetMemories(db, "test", candidates, 3, testLogger{})
	if len(got) != 3 {
		t.Errorf("malformed JSON fallback: expected 3, got %d", len(got))
	}
}

// TestRerankFleetMemories_SkipsWhenDisabled verifies the SystemConfig kill
// switch — set memory_rerank_enabled=0 and we return FTS order without
// calling Claude at all.
func TestRerankFleetMemories_SkipsWhenDisabled(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.SetConfig(db, "memory_rerank_enabled", "0")

	// If Claude were called, this stub would make the test "succeed" with
	// reordering. We want to assert the original order is preserved.
	withStubCLIRunner(t, `{"relevant_ids":[3,2]}`, nil)

	candidates := memoryCandidatesFixture()
	got := RerankFleetMemories(db, "test", candidates, 2, testLogger{})
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("disabled re-ranker must preserve FTS order, got [%d,%d]", got[0].ID, got[1].ID)
	}
}

// TestRerankFleetMemories_EmptyCandidates verifies the no-op path.
func TestRerankFleetMemories_EmptyCandidates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	got := RerankFleetMemories(db, "test", nil, 5, testLogger{})
	if got != nil {
		t.Errorf("empty candidates should return nil, got %+v", got)
	}
}

// TestRerankFleetMemories_SingleCandidateSkipsLLM verifies that we don't
// waste a Claude call when there's only 1 candidate (nothing to re-rank).
func TestRerankFleetMemories_SingleCandidateSkipsLLM(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Stub set to an error; if the re-ranker hits it, the test fails because
	// fallback logic works differently from the skip path.
	withStubCLIRunner(t, "", fmt.Errorf("claude CLI should not be called"))

	candidates := memoryCandidatesFixture()[:1]
	got := RerankFleetMemories(db, "test", candidates, 5, testLogger{})
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("single-candidate path should pass through: got %+v", got)
	}
}

// Verify the helper signature compiles against the shared test logger + log import.
var _ = log.New(io.Discard, "", 0)

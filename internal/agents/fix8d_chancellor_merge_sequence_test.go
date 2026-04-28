package agents

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// Fix #8d Track H — Chancellor SEQUENCE/MERGE empty-subfield fail-closed.
//
// Pre-fix behaviour: a Chancellor LLM ruling with action=SEQUENCE and an
// empty sequence_after_convoy_ids array silently fell through to
// approveProposal (auto-approve). Likewise action=MERGE with
// merge_with_feature_id <= 0. Both drops the sequencing / merge intent
// on the floor — the convoy lands as a standalone top-level feature
// regardless of what the LLM was trying to express.
//
// Post-fix contract: both empty-subfield paths fail CLOSED via
// store.FailBounty + [CHANCELLOR FAIL-CLOSED] operator mail. The feature
// ends Failed and the operator sees a clear reason in the mail subject.

// TestChancellor_SEQUENCE_EmptySubfield_FailsClosed verifies that a
// SEQUENCE ruling with an empty sequence_after_convoy_ids array does NOT
// fall through to auto-approve. The Feature must be marked Failed AND
// an operator mail with [CHANCELLOR FAIL-CLOSED] subject must be sent.
func TestChancellor_SEQUENCE_EmptySubfield_FailsClosed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")
	featureID := store.AddBounty(db, 0, "Feature", "a feature needing sequencing")
	feature, _ := store.GetBounty(db, featureID)

	// Stub Claude to return SEQUENCE with empty sequence_after_convoy_ids.
	withStubCLIRunner(t, `{"action":"SEQUENCE","reason":"needs prior convoy","sequence_after_convoy_ids":[]}`, nil)

	tasks := []store.TaskPlan{{TempID: 1, Repo: "myrepo", Task: "do the work"}}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	runChancellorReview(db, feature, tasks, mustLoadCapProfile(t, "chancellor"), logger)

	// Feature should be Failed.
	after, _ := store.GetBounty(db, featureID)
	if after.Status != "Failed" {
		t.Errorf("SEQUENCE empty-subfield: expected Feature status Failed, got %q", after.Status)
	}
	var errLog string
	db.QueryRow(`SELECT IFNULL(error_log, '') FROM BountyBoard WHERE id = ?`, featureID).Scan(&errLog)
	if !strings.Contains(errLog, "SEQUENCE with empty sequence_after_convoy_ids") {
		t.Errorf("SEQUENCE empty-subfield: expected error_log to mention 'SEQUENCE with empty sequence_after_convoy_ids', got %q", errLog)
	}

	// No convoy should have been created.
	var convoyCount int
	db.QueryRow(`SELECT COUNT(*) FROM Convoys`).Scan(&convoyCount)
	if convoyCount != 0 {
		t.Errorf("SEQUENCE empty-subfield: expected zero convoys created, got %d — auto-approve path re-opened", convoyCount)
	}

	// Operator mail must include [CHANCELLOR FAIL-CLOSED].
	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '%[CHANCELLOR FAIL-CLOSED]%'`).Scan(&mailCount)
	if mailCount == 0 {
		t.Errorf("SEQUENCE empty-subfield: expected [CHANCELLOR FAIL-CLOSED] operator mail, got 0")
	}
}

// TestChancellor_MERGE_EmptySubfield_FailsClosed verifies the MERGE-with-
// no-target analogue. Matches the spec's named test
// "TestChancellor_MERGE_EmptySubfield_FailsClosed".
func TestChancellor_MERGE_EmptySubfield_FailsClosed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")
	featureID := store.AddBounty(db, 0, "Feature", "a feature to merge")
	feature, _ := store.GetBounty(db, featureID)

	// MERGE with merge_with_feature_id=0 (the pre-fix fall-through trigger).
	withStubCLIRunner(t, `{"action":"MERGE","reason":"dup of earlier work","merge_with_feature_id":0}`, nil)

	tasks := []store.TaskPlan{{TempID: 1, Repo: "myrepo", Task: "do the work"}}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	runChancellorReview(db, feature, tasks, mustLoadCapProfile(t, "chancellor"), logger)

	after, _ := store.GetBounty(db, featureID)
	if after.Status != "Failed" {
		t.Errorf("MERGE empty-subfield: expected Feature status Failed, got %q", after.Status)
	}
	var errLog string
	db.QueryRow(`SELECT IFNULL(error_log, '') FROM BountyBoard WHERE id = ?`, featureID).Scan(&errLog)
	if !strings.Contains(errLog, "MERGE with empty merge_with_feature_id") {
		t.Errorf("MERGE empty-subfield: expected error_log to mention 'MERGE with empty merge_with_feature_id', got %q", errLog)
	}

	var convoyCount int
	db.QueryRow(`SELECT COUNT(*) FROM Convoys`).Scan(&convoyCount)
	if convoyCount != 0 {
		t.Errorf("MERGE empty-subfield: expected zero convoys created, got %d — auto-approve path re-opened", convoyCount)
	}

	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '%[CHANCELLOR FAIL-CLOSED]%'`).Scan(&mailCount)
	if mailCount == 0 {
		t.Errorf("MERGE empty-subfield: expected [CHANCELLOR FAIL-CLOSED] operator mail, got 0")
	}
}

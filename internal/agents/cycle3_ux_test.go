package agents

import (
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestShipItNagIncludesDraftPRURLs verifies that the nag mail body contains
// the actual draft PR URL(s) so the operator can click through without
// searching GitHub manually. Regression test for the Cycle 3 UX gap.
func TestShipItNagIncludesDraftPRURLs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] aged-with-url")
	_ = store.SetConvoyStatus(db, cid, "DraftPROpen")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-aged-with-url", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api",
		"https://github.com/acme/api/pull/777", 777, "Open")

	// Backdate to trigger the 24h threshold.
	oldTime := time.Now().Add(-25 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	db.Exec(`UPDATE ConvoyAskBranches SET created_at = ? WHERE convoy_id = ?`, oldTime, cid)

	_ = dogShipItNag(db, testLogger{})

	var body string
	err := db.QueryRow(`SELECT body FROM Fleet_Mail WHERE subject LIKE '%SHIP IT%' LIMIT 1`).Scan(&body)
	if err != nil {
		t.Fatalf("no nag mail found: %v", err)
	}
	if !strings.Contains(body, "https://github.com/acme/api/pull/777") {
		t.Errorf("nag mail body must include the draft PR URL, got: %q", body)
	}
	if !strings.Contains(body, "force convoy ship") {
		t.Errorf("nag mail should suggest the `force convoy ship` command, got: %q", body)
	}
	if !strings.Contains(body, "force convoy pr") {
		t.Errorf("nag mail should suggest the `force convoy pr` command, got: %q", body)
	}
}

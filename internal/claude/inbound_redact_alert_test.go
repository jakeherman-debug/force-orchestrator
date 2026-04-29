package claude

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestRecordInboundRedact_BelowThresholdNoMail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Default threshold is 10. Three small redactions should accumulate
	// without crossing.
	for _, n := range []int{2, 3, 4} {
		if err := recordInboundRedact(db, "captain", 0, n); err != nil {
			t.Fatalf("recordInboundRedact: %v", err)
		}
	}
	mails := store.ListMail(db, "operator")
	for _, m := range mails {
		if strings.Contains(m.Subject, "[INBOUND REDACT") {
			t.Fatalf("alert mail fired below threshold: %+v", m)
		}
	}
	if got := store.GetConfig(db, cfgInboundRedactTotal, "0"); got != "9" {
		t.Fatalf("running total = %s, want 9", got)
	}
}

func TestRecordInboundRedact_ThresholdEmitsOneMail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Cross the threshold in a single call.
	if err := recordInboundRedact(db, "astromech", 42, 12); err != nil {
		t.Fatalf("recordInboundRedact: %v", err)
	}
	mails := store.ListMail(db, "operator")
	count := 0
	for _, m := range mails {
		if strings.Contains(m.Subject, "[INBOUND REDACT") {
			count++
			if !strings.Contains(m.Body, "astromech") {
				t.Fatalf("mail body missing agent name: %s", m.Body)
			}
			if !strings.Contains(m.Body, "task=42") {
				t.Fatalf("mail body missing task id: %s", m.Body)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 alert mail, got %d", count)
	}
	if got := store.GetConfig(db, cfgInboundRedactLastAlert, "0"); got != "12" {
		t.Fatalf("last_alert = %s, want 12", got)
	}
}

func TestRecordInboundRedact_NextAlertWaitsForFreshThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// First burst crosses (12 ≥ 10). Second small burst (3) should NOT
	// re-trigger; running total is 15 but last_alert is 12, delta=3 < 10.
	_ = recordInboundRedact(db, "captain", 1, 12)
	_ = recordInboundRedact(db, "captain", 1, 3)
	mails := store.ListMail(db, "operator")
	count := 0
	for _, m := range mails {
		if strings.Contains(m.Subject, "[INBOUND REDACT") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("second small burst re-triggered alert; got %d mails, want 1", count)
	}

	// Now push past the next threshold: 15 + 8 = 23, delta from last_alert
	// (12) is 11 ≥ 10. One more mail should fire.
	_ = recordInboundRedact(db, "captain", 1, 8)
	mails = store.ListMail(db, "operator")
	count = 0
	for _, m := range mails {
		if strings.Contains(m.Subject, "[INBOUND REDACT") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 alert mails after second threshold, got %d", count)
	}
	if got := store.GetConfig(db, cfgInboundRedactLastAlert, "0"); got != "23" {
		t.Fatalf("last_alert = %s, want 23", got)
	}
}

func TestRecordInboundRedact_OperatorOverrideThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Operator lowers the threshold to 3 — alerts fire much sooner.
	store.SetConfig(db, cfgInboundRedactThreshold, "3")
	_ = recordInboundRedact(db, "captain", 1, 4)
	mails := store.ListMail(db, "operator")
	count := 0
	for _, m := range mails {
		if strings.Contains(m.Subject, "[INBOUND REDACT") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("operator override threshold=3 did not fire on count=4; got %d mails", count)
	}
}

func TestRecordInboundRedact_NilDBNoOp(t *testing.T) {
	// SetInboundRedactDB(nil) means the daemon hasn't started or is in a
	// unit-test context. recordInboundRedact must be a no-op rather than
	// panic.
	if err := recordInboundRedact(nil, "captain", 0, 5); err != nil {
		t.Fatalf("nil-DB call returned error: %v", err)
	}
}

func TestRecordInboundRedact_ZeroCountNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if err := recordInboundRedact(db, "captain", 0, 0); err != nil {
		t.Fatalf("zero-count returned error: %v", err)
	}
	if got := store.GetConfig(db, cfgInboundRedactTotal, "0"); got != "0" {
		t.Fatalf("zero-count modified total: got %s want 0", got)
	}
}

// D5.5 P4 — store-level tests for the stage audit trail (LogStageAudit +
// ListStageAuditLog). These cover the contract expected by the dashboard
// handlers (TestAdvanceStageHandler_*, TestAbortStageHandler_*) and the
// convoy-stage-watch dog (TestDogTransition_AppendsAuditLog).

package store

import (
	"strings"
	"testing"
)

func TestLogStageAudit_AppendsRow(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := LogStageAudit(db, "operator-x", AuditActionStageAdvance,
		1, 2, "AwaitingGate", "AwaitingGate", "looks healthy", ""); err != nil {
		t.Fatalf("LogStageAudit: %v", err)
	}
	logs, err := ListStageAuditLog(db, 1, 2)
	if err != nil {
		t.Fatalf("ListStageAuditLog: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(logs))
	}
	if logs[0].Actor != "operator-x" || logs[0].Action != AuditActionStageAdvance {
		t.Errorf("row mismatch: %+v", logs[0])
	}
	if !strings.Contains(logs[0].Detail, `"reason":"looks healthy"`) {
		t.Errorf("detail missing reason: %q", logs[0].Detail)
	}
	if !strings.Contains(logs[0].Detail, `"stage_num":2`) {
		t.Errorf("detail missing stage_num: %q", logs[0].Detail)
	}
}

func TestLogStageAudit_RejectsZeroIDs(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := LogStageAudit(db, "x", AuditActionStageAdvance, 0, 1, "", "", "", ""); err == nil {
		t.Errorf("expected error for convoyID=0")
	}
	if err := LogStageAudit(db, "x", AuditActionStageAdvance, 1, 0, "", "", "", ""); err == nil {
		t.Errorf("expected error for stageNum=0")
	}
	if err := LogStageAudit(db, "x", "", 1, 1, "", "", "", ""); err == nil {
		t.Errorf("expected error for empty action")
	}
}

func TestListStageAuditLog_OrderedDescending(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 3; i++ {
		if err := LogStageAudit(db, "x", AuditActionStageAdvance,
			7, 1, "AwaitingGate", "AwaitingGate", "click "+itoa(i), ""); err != nil {
			t.Fatalf("LogStageAudit: %v", err)
		}
	}
	logs, err := ListStageAuditLog(db, 7, 1)
	if err != nil {
		t.Fatalf("ListStageAuditLog: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(logs))
	}
	// Newest first: id descending.
	if logs[0].ID < logs[1].ID || logs[1].ID < logs[2].ID {
		t.Errorf("IDs not descending: %d, %d, %d", logs[0].ID, logs[1].ID, logs[2].ID)
	}
}

func TestListStageAuditLog_FilteringByStageNum(t *testing.T) {
	// Two stages on the same convoy must NOT collide. The boundary case
	// is stage_num=1 vs stage_num=10 — naive substring matching of
	// `"stage_num":1` would mis-match. ListStageAuditLog must anchor
	// the trailing comma so only exact matches return.
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if err := LogStageAudit(db, "x", AuditActionStageAdvance, 5, 1, "", "", "stage 1 click", ""); err != nil {
		t.Fatalf("seed stage 1: %v", err)
	}
	if err := LogStageAudit(db, "x", AuditActionStageAdvance, 5, 10, "", "", "stage 10 click", ""); err != nil {
		t.Fatalf("seed stage 10: %v", err)
	}
	logs, err := ListStageAuditLog(db, 5, 1)
	if err != nil {
		t.Fatalf("ListStageAuditLog: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 row for stage_num=1 (got %d): %+v", len(logs), logs)
	}
	if !strings.Contains(logs[0].Detail, "stage 1 click") {
		t.Errorf("filter leaked stage 10 row: %q", logs[0].Detail)
	}
}

// itoa avoids strconv import noise in this small test file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	const digits = "0123456789"
	out := []byte{}
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}

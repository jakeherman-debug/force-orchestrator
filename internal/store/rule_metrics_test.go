// D4 fix-loop-1 α2 — RuleMetrics rollup tests.
//
// Coverage:
//   - empty (no firings) → (nil, nil)
//   - single TP (no overrides) → precision = 1.0
//   - all FP (every firing overridden) → precision = 0.0
//   - mixed → expected precision ratio
//   - ramp status: advise → eligible-for-block at 30 TPs; block stays block
//   - bureau filter scopes rule_id correctly
//   - ListAllRuleMetrics enumerates every (bureau, rule_id) pair
//   - Last-30-day firings excludes older rows

package store

import (
	"testing"
	"time"
)

func TestComputeRuleMetrics_NoFirings(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	m, err := ComputeRuleMetrics(db, "BoS", "BOS-001")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil for no firings, got %+v", m)
	}
}

func TestComputeRuleMetrics_SingleTP(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := InsertSecurityFinding(db, SecurityFinding{
		TaskID: 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block",
		FilePath: "x.go", LineNumber: 1, Message: "m",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	m, err := ComputeRuleMetrics(db, "BoS", "BOS-001")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m == nil || m.TotalFirings != 1 || m.TruePositives != 1 || m.FalsePositives != 0 {
		t.Errorf("expected 1/1/0, got %+v", m)
	}
	if m.Precision != 1.0 {
		t.Errorf("expected precision=1.0, got %v", m.Precision)
	}
}

func TestComputeRuleMetrics_AllFP(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 3; i++ {
		_, err := InsertSecurityFinding(db, SecurityFinding{
			TaskID: i + 1, Bureau: "BoS", RuleID: "BOS-002", Severity: "advise",
			FilePath: "x.go", LineNumber: i + 1, Message: "m",
			Disposition: "overridden", BypassAuditID: "AUDIT-001", BypassReason: "ten chars at least",
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	m, err := ComputeRuleMetrics(db, "BoS", "BOS-002")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m == nil || m.TotalFirings != 3 || m.TruePositives != 0 || m.FalsePositives != 3 {
		t.Errorf("expected 3/0/3, got %+v", m)
	}
	if m.Precision != 0.0 {
		t.Errorf("expected precision=0, got %v", m.Precision)
	}
	if m.OverriddenCount != 3 {
		t.Errorf("expected overriddenCount=3, got %d", m.OverriddenCount)
	}
}

func TestComputeRuleMetrics_Mixed(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	// 4 TP (open or resolved), 1 FP (overridden) → precision = 4/5 = 0.8
	rows := []SecurityFinding{
		{TaskID: 1, Bureau: "BoS", RuleID: "BOS-003", Severity: "block", Message: "m"},
		{TaskID: 2, Bureau: "BoS", RuleID: "BOS-003", Severity: "block", Message: "m", Disposition: "resolved"},
		{TaskID: 3, Bureau: "BoS", RuleID: "BOS-003", Severity: "block", Message: "m"},
		{TaskID: 4, Bureau: "BoS", RuleID: "BOS-003", Severity: "block", Message: "m"},
		{TaskID: 5, Bureau: "BoS", RuleID: "BOS-003", Severity: "block", Message: "m", Disposition: "overridden", BypassAuditID: "AUDIT-X", BypassReason: "ten chars."},
	}
	for i, r := range rows {
		if _, err := InsertSecurityFinding(db, r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	m, err := ComputeRuleMetrics(db, "BoS", "BOS-003")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m.TruePositives != 4 || m.FalsePositives != 1 {
		t.Errorf("expected 4/1, got TP=%d FP=%d (%+v)", m.TruePositives, m.FalsePositives, m)
	}
	if m.Precision < 0.799 || m.Precision > 0.801 {
		t.Errorf("expected precision~0.8, got %v", m.Precision)
	}
}

func TestComputeRuleMetrics_RampStatus_AdviseUnderThreshold(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 5; i++ {
		_, err := InsertSecurityFinding(db, SecurityFinding{
			TaskID: i + 1, Bureau: "BoS", RuleID: "BOS-004", Severity: "advise",
			FilePath: "x.go", LineNumber: i + 1, Message: "m",
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	m, _ := ComputeRuleMetrics(db, "BoS", "BOS-004")
	if m.RampStatus != "advise" {
		t.Errorf("expected ramp_status=advise, got %q", m.RampStatus)
	}
	if m.FiringsToBlockReady != 25 {
		t.Errorf("expected 25 to ready, got %d", m.FiringsToBlockReady)
	}
}

func TestComputeRuleMetrics_RampStatus_EligibleAt30TP(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 30; i++ {
		_, err := InsertSecurityFinding(db, SecurityFinding{
			TaskID: i + 1, Bureau: "BoS", RuleID: "BOS-005", Severity: "advise",
			FilePath: "x.go", LineNumber: i + 1, Message: "m",
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	m, _ := ComputeRuleMetrics(db, "BoS", "BOS-005")
	if m.RampStatus != "eligible-for-block" {
		t.Errorf("expected eligible-for-block, got %q", m.RampStatus)
	}
	if m.FiringsToBlockReady != 0 {
		t.Errorf("expected 0, got %d", m.FiringsToBlockReady)
	}
}

func TestComputeRuleMetrics_RampStatus_Block(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := InsertSecurityFinding(db, SecurityFinding{
		TaskID: 1, Bureau: "BoS", RuleID: "BOS-006", Severity: "block",
		FilePath: "x.go", LineNumber: 1, Message: "m",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	m, _ := ComputeRuleMetrics(db, "BoS", "BOS-006")
	if m.RampStatus != "block" {
		t.Errorf("expected block, got %q", m.RampStatus)
	}
}

func TestComputeRuleMetrics_BureauFilter(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	// Same rule_id on two bureaus — bureau filter must scope correctly.
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 1, Bureau: "BoS", RuleID: "X-001", Severity: "block", Message: "m"})
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 2, Bureau: "ISB", RuleID: "X-001", Severity: "block", Message: "m"})
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 3, Bureau: "ISB", RuleID: "X-001", Severity: "block", Message: "m"})
	m1, _ := ComputeRuleMetrics(db, "BoS", "X-001")
	m2, _ := ComputeRuleMetrics(db, "ISB", "X-001")
	if m1 == nil || m1.TotalFirings != 1 {
		t.Errorf("BoS filter: expected 1, got %+v", m1)
	}
	if m2 == nil || m2.TotalFirings != 2 {
		t.Errorf("ISB filter: expected 2, got %+v", m2)
	}
}

func TestComputeRuleMetrics_Last30Days(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	// Insert two recent (default datetime('now')) and one explicit ancient row.
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 1, Bureau: "BoS", RuleID: "BOS-T", Severity: "block", Message: "m"})
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 2, Bureau: "BoS", RuleID: "BOS-T", Severity: "block", Message: "m"})
	old := time.Now().UTC().Add(-90 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	_, err := db.Exec(`INSERT INTO SecurityFindings (task_id, bureau, rule_id, severity, message, created_at) VALUES (3, 'BoS', 'BOS-T', 'block', 'old', ?)`, old)
	if err != nil {
		t.Fatalf("insert old: %v", err)
	}
	m, _ := ComputeRuleMetrics(db, "BoS", "BOS-T")
	if m.TotalFirings != 3 {
		t.Errorf("expected 3 total, got %d", m.TotalFirings)
	}
	if m.Last30DayFirings != 2 {
		t.Errorf("expected 2 last-30-day, got %d", m.Last30DayFirings)
	}
}

func TestListAllRuleMetrics(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 1, Bureau: "BoS", RuleID: "BOS-A", Severity: "block", Message: "m"})
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 2, Bureau: "BoS", RuleID: "BOS-B", Severity: "advise", Message: "m"})
	_, _ = InsertSecurityFinding(db, SecurityFinding{TaskID: 3, Bureau: "ISB", RuleID: "ISB-A", Severity: "block", Message: "m"})
	all, err := ListAllRuleMetrics(db, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 rules, got %d", len(all))
	}
	bos, _ := ListAllRuleMetrics(db, "BoS")
	if len(bos) != 2 {
		t.Errorf("expected 2 BoS rules, got %d", len(bos))
	}
}

func TestListAllRuleMetrics_Empty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	all, err := ListAllRuleMetrics(db, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected empty, got %d", len(all))
	}
}

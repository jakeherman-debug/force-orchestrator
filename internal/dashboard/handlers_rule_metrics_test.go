// D4 fix-loop-1 α2 — Rule-metrics handler tests.

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

func TestRuleMetrics_Empty(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/rule-metrics", nil)
	w := httptest.NewRecorder()
	handleRuleMetrics(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Rules []store.RuleMetrics `json:"rules"`
		Count int                 `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Rules) != 0 || resp.Count != 0 {
		t.Errorf("expected empty, got %+v", resp)
	}
}

func TestRuleMetrics_ListAcrossBureaus(t *testing.T) {
	db := openDashTestDB(t)
	_, _ = store.InsertSecurityFinding(db, store.SecurityFinding{TaskID: 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block", Message: "m"})
	_, _ = store.InsertSecurityFinding(db, store.SecurityFinding{TaskID: 2, Bureau: "ISB", RuleID: "ISB-001", Severity: "block", Message: "m"})

	r := httptest.NewRequest(http.MethodGet, "/api/rule-metrics", nil)
	w := httptest.NewRecorder()
	handleRuleMetrics(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Rules []store.RuleMetrics `json:"rules"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(resp.Rules))
	}
}

func TestRuleMetrics_BureauFilter(t *testing.T) {
	db := openDashTestDB(t)
	_, _ = store.InsertSecurityFinding(db, store.SecurityFinding{TaskID: 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block", Message: "m"})
	_, _ = store.InsertSecurityFinding(db, store.SecurityFinding{TaskID: 2, Bureau: "ISB", RuleID: "ISB-001", Severity: "block", Message: "m"})

	r := httptest.NewRequest(http.MethodGet, "/api/rule-metrics?bureau=BoS", nil)
	w := httptest.NewRecorder()
	handleRuleMetrics(db)(w, r)
	var resp struct {
		Rules []store.RuleMetrics `json:"rules"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Rules) != 1 || resp.Rules[0].Bureau != "BoS" {
		t.Errorf("expected 1 BoS rule, got %+v", resp)
	}
}

func TestRuleMetrics_SingleRule(t *testing.T) {
	db := openDashTestDB(t)
	_, _ = store.InsertSecurityFinding(db, store.SecurityFinding{TaskID: 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block", Message: "m"})
	_, _ = store.InsertSecurityFinding(db, store.SecurityFinding{TaskID: 2, Bureau: "BoS", RuleID: "BOS-001", Severity: "block", Message: "m", Disposition: "overridden", BypassAuditID: "AUDIT-1", BypassReason: "ten chars."})

	r := httptest.NewRequest(http.MethodGet, "/api/rule-metrics?bureau=BoS&rule_id=BOS-001", nil)
	w := httptest.NewRecorder()
	handleRuleMetrics(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		RuleID  string             `json:"rule_id"`
		Bureau  string             `json:"bureau"`
		Metrics *store.RuleMetrics `json:"metrics"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Metrics == nil {
		t.Fatalf("expected metrics, got nil (body %s)", w.Body.String())
	}
	if resp.Metrics.TotalFirings != 2 {
		t.Errorf("expected 2 firings, got %d", resp.Metrics.TotalFirings)
	}
	if resp.Metrics.FalsePositives != 1 {
		t.Errorf("expected 1 FP, got %d", resp.Metrics.FalsePositives)
	}
	// Precision: TP=1 FP=1 → 0.5
	if resp.Metrics.Precision < 0.49 || resp.Metrics.Precision > 0.51 {
		t.Errorf("expected precision~0.5, got %v", resp.Metrics.Precision)
	}
}

func TestRuleMetrics_SingleRule_NotFound(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/rule-metrics?rule_id=DOES-NOT-EXIST", nil)
	w := httptest.NewRecorder()
	handleRuleMetrics(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	// metrics: null is the documented absent-row shape.
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["metrics"] != nil {
		t.Errorf("expected metrics=null, got %v", resp["metrics"])
	}
}

func TestRuleMetrics_BadBureau(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/rule-metrics?bureau=Bogus", nil)
	w := httptest.NewRecorder()
	handleRuleMetrics(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRuleMetrics_MethodNotAllowed(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost, "/api/rule-metrics", nil)
	w := httptest.NewRecorder()
	handleRuleMetrics(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

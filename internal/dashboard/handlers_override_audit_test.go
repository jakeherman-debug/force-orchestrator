// D4 fix-loop-1 α3 — Override-audit handler tests.

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

func TestOverrideAudit_Empty(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/override-audit", nil)
	w := httptest.NewRecorder()
	handleOverrideAudit(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Overrides []store.SecurityFinding `json:"overrides"`
		Count     int                     `json:"count"`
		Total     int                     `json:"total"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Overrides) != 0 || resp.Total != 0 {
		t.Errorf("expected empty, got %+v", resp)
	}
}

func TestOverrideAudit_OnlyOverriddenRowsReturned(t *testing.T) {
	db := openDashTestDB(t)
	// Overridden row.
	_, err := store.InsertSecurityFinding(db, store.SecurityFinding{
		TaskID: 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block",
		FilePath: "a.go", LineNumber: 10, Message: "m",
		CommitSHA: "deadbeef0000",
		Disposition: "overridden", BypassAuditID: "AUDIT-001", BypassReason: "ten chars min",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Non-overridden row (open).
	_, err = store.InsertSecurityFinding(db, store.SecurityFinding{
		TaskID: 2, Bureau: "BoS", RuleID: "BOS-002", Severity: "block",
		FilePath: "b.go", LineNumber: 20, Message: "m",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/override-audit", nil)
	w := httptest.NewRecorder()
	handleOverrideAudit(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Overrides []store.SecurityFinding `json:"overrides"`
		Total     int                     `json:"total"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(resp.Overrides))
	}
	if resp.Overrides[0].BypassAuditID != "AUDIT-001" {
		t.Errorf("expected AUDIT-001, got %q", resp.Overrides[0].BypassAuditID)
	}
}

func TestOverrideAudit_Filters(t *testing.T) {
	db := openDashTestDB(t)
	rows := []store.SecurityFinding{
		{TaskID: 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block", FilePath: "a.go", LineNumber: 10, Message: "m", Disposition: "overridden", BypassAuditID: "AUDIT-001", BypassReason: "ten chars min"},
		{TaskID: 2, Bureau: "BoS", RuleID: "BOS-002", Severity: "block", FilePath: "b.go", LineNumber: 20, Message: "m", Disposition: "overridden", BypassAuditID: "AUDIT-002", BypassReason: "ten chars min"},
		{TaskID: 3, Bureau: "ISB", RuleID: "ISB-001", Severity: "block", FilePath: "c.go", LineNumber: 30, Message: "m", Disposition: "overridden", BypassAuditID: "AUDIT-003", BypassReason: "ten chars min"},
	}
	for _, f := range rows {
		if _, err := store.InsertSecurityFinding(db, f); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	cases := []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?bureau=BoS", 2},
		{"?bureau=ISB", 1},
		{"?bureau=all", 3},
		{"?rule_id=BOS-001", 1},
		{"?audit_id=AUDIT-002", 1},
		{"?bureau=BoS&rule_id=BOS-002", 1},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/override-audit"+tc.query, nil)
		w := httptest.NewRecorder()
		handleOverrideAudit(db)(w, r)
		var resp struct {
			Overrides []store.SecurityFinding `json:"overrides"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Overrides) != tc.want {
			t.Errorf("filter %q: want %d, got %d", tc.query, tc.want, len(resp.Overrides))
		}
	}
}

func TestOverrideAudit_Pagination(t *testing.T) {
	db := openDashTestDB(t)
	for i := 0; i < 4; i++ {
		_, err := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID: i + 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block",
			FilePath: "x.go", LineNumber: i + 1, Message: "m",
			Disposition: "overridden", BypassAuditID: "AUDIT-X", BypassReason: "ten chars min",
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/override-audit?limit=2&offset=2", nil)
	w := httptest.NewRecorder()
	handleOverrideAudit(db)(w, r)
	var resp struct {
		Overrides []store.SecurityFinding `json:"overrides"`
		Total     int                     `json:"total"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Overrides) != 2 {
		t.Errorf("expected 2 rows, got %d", len(resp.Overrides))
	}
	if resp.Total != 4 {
		t.Errorf("expected total=4, got %d", resp.Total)
	}
}

func TestOverrideAudit_BadBureau(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/override-audit?bureau=Bogus", nil)
	w := httptest.NewRecorder()
	handleOverrideAudit(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestOverrideAudit_MethodNotAllowed(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost, "/api/override-audit", nil)
	w := httptest.NewRecorder()
	handleOverrideAudit(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

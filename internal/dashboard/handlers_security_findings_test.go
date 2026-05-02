// D4 fix-loop-1 α1 — Security Findings handler tests.
//
// Coverage:
//   - GET list (empty + populated + filter matrix + pagination)
//   - POST resolve (happy path + missing email + invalid disposition +
//     unknown id → 404 + bypass requires audit_id+reason)
//   - Round-trip: resolve flips disposition, list filter reflects it.

package dashboard

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"force-orchestrator/internal/store"
)

func TestSecurityFindings_List_Empty(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/security-findings", nil)
	w := httptest.NewRecorder()
	handleSecurityFindings(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Findings []store.SecurityFinding `json:"findings"`
		Count    int                     `json:"count"`
		Total    int                     `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Findings) != 0 || resp.Total != 0 {
		t.Errorf("expected empty list, got count=%d total=%d", resp.Count, resp.Total)
	}
}

func seedSecurityFindings(t *testing.T, db *sql.DB) []int {
	t.Helper()
	ids := []int{}
	rows := []store.SecurityFinding{
		{TaskID: 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block", FilePath: "a.go", LineNumber: 10, Message: "no silent fail"},
		{TaskID: 1, Bureau: "BoS", RuleID: "BOS-002", Severity: "advise", FilePath: "b.go", LineNumber: 20, Message: "advise only"},
		{TaskID: 2, Bureau: "ISB", RuleID: "ISB-001", Severity: "block", FilePath: "c.go", LineNumber: 30, Message: "secret leak"},
		{TaskID: 2, Bureau: "ISB", RuleID: "ISB-002", Severity: "block", FilePath: "d.go", LineNumber: 40, Message: "auth bypass", Disposition: "overridden", BypassAuditID: "AUDIT-001", BypassReason: "intentional dev hook"},
	}
	for _, f := range rows {
		id, err := store.InsertSecurityFinding(db, f)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestSecurityFindings_List_Populated_AllFilters(t *testing.T) {
	db := openDashTestDB(t)
	seedSecurityFindings(t, db)

	cases := []struct {
		name  string
		query string
		want  int // expected len(findings)
	}{
		{"all", "", 4},
		{"bureau-bos", "bureau=BoS", 2},
		{"bureau-isb", "bureau=ISB", 2},
		{"bureau-all", "bureau=all", 4},
		{"disposition-open", "disposition=open", 3},
		{"disposition-overridden", "disposition=overridden", 1},
		{"rule-bos-001", "rule_id=BOS-001", 1},
		{"bureau-bos-and-rule", "bureau=BoS&rule_id=BOS-002", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := "/api/security-findings"
			if tc.query != "" {
				url += "?" + tc.query
			}
			r := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()
			handleSecurityFindings(db)(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			var resp struct {
				Findings []store.SecurityFinding `json:"findings"`
				Count    int                     `json:"count"`
				Total    int                     `json:"total"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(resp.Findings) != tc.want {
				t.Errorf("filter %q: want %d findings, got %d (total=%d)", tc.query, tc.want, len(resp.Findings), resp.Total)
			}
		})
	}
}

func TestSecurityFindings_List_Pagination(t *testing.T) {
	db := openDashTestDB(t)
	for i := 0; i < 5; i++ {
		_, err := store.InsertSecurityFinding(db, store.SecurityFinding{
			TaskID: i + 1, Bureau: "BoS", RuleID: "BOS-001", Severity: "block",
			FilePath: "x.go", LineNumber: i + 1, Message: "m",
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// limit=2 offset=0 → 2 rows; limit=2 offset=2 → 2 rows; limit=2 offset=4 → 1 row.
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"limit=2&offset=0", 2},
		{"limit=2&offset=2", 2},
		{"limit=2&offset=4", 1},
		{"limit=2&offset=10", 0},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/security-findings?"+tc.query, nil)
		w := httptest.NewRecorder()
		handleSecurityFindings(db)(w, r)
		var resp struct {
			Findings []store.SecurityFinding `json:"findings"`
			Total    int                     `json:"total"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Findings) != tc.want {
			t.Errorf("query %q: want %d, got %d (total=%d)", tc.query, tc.want, len(resp.Findings), resp.Total)
		}
		if resp.Total != 5 {
			t.Errorf("query %q: total should be 5, got %d", tc.query, resp.Total)
		}
	}
}

func TestSecurityFindings_List_BadBureauRejected(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/security-findings?bureau=Bogus", nil)
	w := httptest.NewRecorder()
	handleSecurityFindings(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSecurityFindings_List_BadDispositionRejected(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/security-findings?disposition=zzz", nil)
	w := httptest.NewRecorder()
	handleSecurityFindings(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSecurityFindings_List_MethodNotAllowed(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost, "/api/security-findings", nil)
	w := httptest.NewRecorder()
	handleSecurityFindings(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestSecurityFindings_Resolve_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	ids := seedSecurityFindings(t, db)
	body := securityFindingResolveRequest{
		Disposition:   "resolved",
		OperatorEmail: "operator@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost,
		"/api/security-findings/"+strconv.Itoa(ids[0])+"/resolve", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleSecurityFindingsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	// Confirm disposition flipped.
	var disp string
	db.QueryRow(`SELECT disposition FROM SecurityFindings WHERE id = ?`, ids[0]).Scan(&disp)
	if disp != "resolved" {
		t.Errorf("expected disposition=resolved, got %q", disp)
	}
}

func TestSecurityFindings_Resolve_OperatorEmailRequired(t *testing.T) {
	db := openDashTestDB(t)
	ids := seedSecurityFindings(t, db)
	body := securityFindingResolveRequest{Disposition: "resolved"}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost,
		"/api/security-findings/"+strconv.Itoa(ids[0])+"/resolve", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleSecurityFindingsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSecurityFindings_Resolve_InvalidDisposition(t *testing.T) {
	db := openDashTestDB(t)
	ids := seedSecurityFindings(t, db)
	body := map[string]string{
		"disposition":    "bogus",
		"operator_email": "operator@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost,
		"/api/security-findings/"+strconv.Itoa(ids[0])+"/resolve", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleSecurityFindingsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSecurityFindings_Resolve_NotFound(t *testing.T) {
	db := openDashTestDB(t)
	body := securityFindingResolveRequest{
		Disposition:   "resolved",
		OperatorEmail: "operator@example.com",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost,
		"/api/security-findings/99999/resolve", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleSecurityFindingsSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d body %s", w.Code, w.Body.String())
	}
}

func TestSecurityFindings_Resolve_OverrideRequiresAuditAndReason(t *testing.T) {
	db := openDashTestDB(t)
	ids := seedSecurityFindings(t, db)
	body := securityFindingResolveRequest{
		Disposition:   "overridden",
		OperatorEmail: "operator@example.com",
		// No bypass fields → store.SetDisposition will reject.
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost,
		"/api/security-findings/"+strconv.Itoa(ids[0])+"/resolve", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleSecurityFindingsSubroutes(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d body %s", w.Code, w.Body.String())
	}
}

func TestSecurityFindings_Resolve_OverrideRoundTrip(t *testing.T) {
	db := openDashTestDB(t)
	ids := seedSecurityFindings(t, db)
	body := securityFindingResolveRequest{
		Disposition:   "overridden",
		OperatorEmail: "operator@example.com",
		BypassAuditID: "AUDIT-555",
		BypassReason:  "operator-acked finding for legacy file",
	}
	bb, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost,
		"/api/security-findings/"+strconv.Itoa(ids[0])+"/resolve", bytes.NewReader(bb))
	w := httptest.NewRecorder()
	handleSecurityFindingsSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", w.Code, w.Body.String())
	}
	// disposition=overridden should now appear in the override-audit
	// filter (disposition=overridden returns 2 rows: seeded #4 + this one).
	r2 := httptest.NewRequest(http.MethodGet, "/api/security-findings?disposition=overridden", nil)
	w2 := httptest.NewRecorder()
	handleSecurityFindings(db)(w2, r2)
	var resp struct {
		Findings []store.SecurityFinding `json:"findings"`
	}
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if len(resp.Findings) != 2 {
		t.Errorf("expected 2 overridden findings, got %d", len(resp.Findings))
	}
}

func TestSecurityFindings_Subroutes_UnknownVerb(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost, "/api/security-findings/1/zonk", nil)
	w := httptest.NewRecorder()
	handleSecurityFindingsSubroutes(db)(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

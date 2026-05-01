package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandleConflictsTickets_ListAndResolve seeds a contradiction
// pair, runs the detector, then exercises the GET-list and POST-
// resolve endpoints.
func TestHandleConflictsTickets_ListAndResolve(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.StoreFleetMemory(db, "repoA", 1, "success",
		"Server always restarts on config change.", "config.go", "")
	store.StoreFleetMemory(db, "repoA", 2, "success",
		"Server never restarts on config change.", "config.go", "")
	if _, err := store.DetectConflicts(context.Background(), db); err != nil {
		t.Fatalf("DetectConflicts: %v", err)
	}

	h := handleConflictsTickets(db)

	// GET — the list endpoint.
	req := httptest.NewRequest(http.MethodGet, "/api/conflicts/tickets", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET status: %d (body=%s)", rec.Code, rec.Body.String())
	}
	var listBody struct {
		Tickets []map[string]any `json:"tickets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if len(listBody.Tickets) != 1 {
		t.Fatalf("expected 1 ticket in list, got %d", len(listBody.Tickets))
	}
	idF, _ := listBody.Tickets[0]["id"].(float64)
	if idF == 0 {
		t.Fatalf("expected non-zero ticket id, got %v", listBody.Tickets[0]["id"])
	}

	// POST — resolve.
	resolveURL := "/api/conflicts/tickets/" + intStr(int(idF)) + "/resolve"
	req2 := httptest.NewRequest(http.MethodPost, resolveURL,
		strings.NewReader(`{"note":"operator picked option A"}`))
	rec2 := httptest.NewRecorder()
	h(rec2, req2)
	if rec2.Code != 200 {
		t.Errorf("resolve status: %d (body=%s)", rec2.Code, rec2.Body.String())
	}

	// Re-list — should be empty.
	rec3 := httptest.NewRecorder()
	h(rec3, httptest.NewRequest(http.MethodGet, "/api/conflicts/tickets", nil))
	var after struct {
		Tickets []map[string]any `json:"tickets"`
	}
	json.Unmarshal(rec3.Body.Bytes(), &after) //nolint:errcheck
	if len(after.Tickets) != 0 {
		t.Errorf("expected 0 open tickets after resolve, got %d", len(after.Tickets))
	}
}

func TestHandleConflictsTickets_ResolveBadID(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	h := handleConflictsTickets(db)

	req := httptest.NewRequest(http.MethodPost, "/api/conflicts/tickets/abc/resolve", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for bad id, got %d", rec.Code)
	}
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestReflectionLearningHandler covers the GET (read latest) and POST
// (refresh) flows for /api/reflection/learning.
func TestReflectionLearningHandler(t *testing.T) {
	t.Run("get_empty_returns_zero_id", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()

		req := httptest.NewRequest(http.MethodGet, "/api/reflection/learning", nil)
		rr := httptest.NewRecorder()
		handleReflectionLearning(db)(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d", rr.Code)
		}
		var resp learningPanelResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ID != 0 {
			t.Errorf("expected 0 id on empty: %d", resp.ID)
		}
	})

	t.Run("post_refresh_inserts_and_returns_row", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/reflection/learning", strings.NewReader(""))
		rr := httptest.NewRecorder()
		handleReflectionLearning(db)(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status: %d, body: %s", rr.Code, rr.Body.String())
		}
		var resp learningPanelResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ID == 0 {
			t.Errorf("expected non-zero id after refresh")
		}
		if resp.Prose == "" {
			t.Errorf("expected non-empty prose")
		}

		// Get round-trip
		req2 := httptest.NewRequest(http.MethodGet, "/api/reflection/learning", nil)
		rr2 := httptest.NewRecorder()
		handleReflectionLearning(db)(rr2, req2)
		var resp2 learningPanelResponse
		json.Unmarshal(rr2.Body.Bytes(), &resp2)
		if resp2.ID != resp.ID {
			t.Errorf("get/post id mismatch: %d vs %d", resp2.ID, resp.ID)
		}
	})

	t.Run("method_not_allowed", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		req := httptest.NewRequest(http.MethodDelete, "/api/reflection/learning", nil)
		rr := httptest.NewRecorder()
		handleReflectionLearning(db)(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", rr.Code)
		}
	})
}

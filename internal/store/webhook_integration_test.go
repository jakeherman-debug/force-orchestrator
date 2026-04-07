package store

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIntegration_UpdateBountyStatus_CompletedFiresWebhook verifies that
// transitioning a task to Completed via UpdateBountyStatus triggers a webhook
// POST with the correct JSON fields — exercising the full wiring path rather
// than calling FireWebhook directly.
func TestIntegration_UpdateBountyStatus_CompletedFiresWebhook(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'my-repo', 'CodeEdit', 'Locked', 'fix the login bug', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	id64, _ := res.LastInsertId()
	taskID := int(id64)

	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Store webhook_url in the live SQLite store — FireWebhook reads it via GetConfig.
	SetConfig(db, "webhook_url", srv.URL)

	UpdateBountyStatus(db, taskID, "Completed")

	select {
	case body := <-received:
		var p webhookPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("unmarshal webhook body: %v", err)
		}
		if p.ID != taskID {
			t.Errorf("id: got %d, want %d", p.ID, taskID)
		}
		if p.Type != "CodeEdit" {
			t.Errorf("type: got %q, want %q", p.Type, "CodeEdit")
		}
		if p.Status != "Completed" {
			t.Errorf("status: got %q, want %q", p.Status, "Completed")
		}
		if p.TargetRepo != "my-repo" {
			t.Errorf("target_repo: got %q, want %q", p.TargetRepo, "my-repo")
		}
		if p.Payload != "fix the login bug" {
			t.Errorf("payload: got %q, want %q", p.Payload, "fix the login bug")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook POST after UpdateBountyStatus(Completed)")
	}
}

// TestIntegration_UpdateBountyStatus_FailedFiresWebhook verifies that
// transitioning a task to Failed via UpdateBountyStatus triggers the webhook.
func TestIntegration_UpdateBountyStatus_FailedFiresWebhook(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'fail-repo', 'Feature', 'Locked', 'ship the feature', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	id64, _ := res.LastInsertId()
	taskID := int(id64)

	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)
	UpdateBountyStatus(db, taskID, "Failed")

	select {
	case body := <-received:
		var p webhookPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.Status != "Failed" {
			t.Errorf("status: got %q, want %q", p.Status, "Failed")
		}
		if p.ID != taskID {
			t.Errorf("id: got %d, want %d", p.ID, taskID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook POST after UpdateBountyStatus(Failed)")
	}
}

// TestIntegration_UpdateBountyStatus_EscalatedFiresWebhook verifies that
// transitioning a task to Escalated via UpdateBountyStatus triggers the webhook.
func TestIntegration_UpdateBountyStatus_EscalatedFiresWebhook(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'escalate-repo', 'Investigate', 'Locked', 'look into the crash', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	id64, _ := res.LastInsertId()
	taskID := int(id64)

	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)
	UpdateBountyStatus(db, taskID, "Escalated")

	select {
	case body := <-received:
		var p webhookPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.Status != "Escalated" {
			t.Errorf("status: got %q, want %q", p.Status, "Escalated")
		}
		if p.ID != taskID {
			t.Errorf("id: got %d, want %d", p.ID, taskID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook POST after UpdateBountyStatus(Escalated)")
	}
}

// TestIntegration_UpdateBountyStatus_NonTerminalNoWebhook verifies that
// non-terminal status transitions (Pending, Locked, etc.) do NOT fire the webhook.
func TestIntegration_UpdateBountyStatus_NonTerminalNoWebhook(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Locked', 'some work', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	id64, _ := res.LastInsertId()

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)

	for _, status := range []string{"Pending", "Locked", "AwaitingCouncilReview", "UnderReview"} {
		UpdateBountyStatus(db, int(id64), status)
	}

	select {
	case <-called:
		t.Error("webhook fired for a non-terminal status transition")
	case <-time.After(300 * time.Millisecond):
		// correct: no webhook for non-terminal statuses
	}
}

// TestIntegration_UpdateBountyStatus_GoroutineFiresWithinWindow verifies that
// the webhook goroutine fires within a reasonable window after UpdateBountyStatus
// returns — confirming the fire-and-forget behaviour when called from the wiring layer.
func TestIntegration_UpdateBountyStatus_GoroutineFiresWithinWindow(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'timing-repo', 'CodeEdit', 'Locked', 'timing test', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	id64, _ := res.LastInsertId()

	fired := make(chan time.Time, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fired <- time.Now()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)

	callTime := time.Now()
	UpdateBountyStatus(db, int(id64), "Completed")

	select {
	case t0 := <-fired:
		// Goroutine must fire within 2 seconds of UpdateBountyStatus returning.
		lag := t0.Sub(callTime)
		if lag > 2*time.Second {
			t.Errorf("webhook goroutine fired too late: %v after UpdateBountyStatus", lag)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook goroutine did not fire within 3 seconds")
	}
}

// TestIntegration_WebhookURL_ReadFromLiveSQLiteStore verifies the end-to-end
// config round-trip: SetConfig writes webhook_url to the live SQLite store, and
// UpdateBountyStatus → FireWebhook reads it back via GetConfig to route the POST.
// This catches any mis-wiring where FireWebhook uses a hardcoded or stale URL.
func TestIntegration_WebhookURL_ReadFromLiveSQLiteStore(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'config-repo', 'Feature', 'Locked', 'config round-trip', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	id64, _ := res.LastInsertId()

	// First server — not configured yet; must receive nothing.
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("decoy server received a request before webhook_url was set")
	}))
	defer decoy.Close()
	_ = decoy // webhook_url is intentionally NOT set to decoy.URL

	// Second server — configured after the fact via SetConfig.
	received := make(chan struct{}, 1)
	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer real.Close()

	// Store the real URL in the live SQLite store.
	SetConfig(db, "webhook_url", real.URL)

	UpdateBountyStatus(db, int(id64), "Completed")

	select {
	case <-received:
		// correct: FireWebhook read webhook_url from the live SQLite store
	case <-time.After(3 * time.Second):
		t.Fatal("webhook POST not received — FireWebhook may not be reading webhook_url from the SQLite store")
	}
}

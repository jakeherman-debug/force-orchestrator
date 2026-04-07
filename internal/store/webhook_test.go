package store

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFireWebhook_NoOpWhenURLUnset verifies that FireWebhook is a no-op when
// webhook_url is not configured — no HTTP request should be sent.
func TestFireWebhook_NoOpWhenURLUnset(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Pending', 'do something', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id64, _ := res.LastInsertId()

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
	}))
	defer srv.Close()

	// webhook_url is NOT set — FireWebhook must return immediately without posting.
	FireWebhook(db, int(id64), "Completed")

	select {
	case <-called:
		t.Error("expected no HTTP call when webhook_url is unset, but one was made")
	case <-time.After(200 * time.Millisecond):
		// correct: no request arrived
	}
}

// TestFireWebhook_PostsCorrectJSON verifies that FireWebhook sends the right
// JSON fields (id, type, status, target_repo, payload) to the configured URL.
func TestFireWebhook_PostsCorrectJSON(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'my-repo', 'Feature', 'Pending', 'short payload', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
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
	FireWebhook(db, taskID, "Completed")

	select {
	case body := <-received:
		var p webhookPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if p.ID != taskID {
			t.Errorf("id: got %d, want %d", p.ID, taskID)
		}
		if p.Type != "Feature" {
			t.Errorf("type: got %q, want %q", p.Type, "Feature")
		}
		if p.Status != "Completed" {
			t.Errorf("status: got %q, want %q", p.Status, "Completed")
		}
		if p.TargetRepo != "my-repo" {
			t.Errorf("target_repo: got %q, want %q", p.TargetRepo, "my-repo")
		}
		if p.Payload != "short payload" {
			t.Errorf("payload: got %q, want %q", p.Payload, "short payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook POST")
	}
}

// TestFireWebhook_PayloadTruncated verifies that a payload longer than 500 chars
// is truncated to 500 bytes with a trailing ellipsis (…).
func TestFireWebhook_PayloadTruncated(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	longPayload := strings.Repeat("x", 600)
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Pending', ?, datetime('now'))`,
		longPayload,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id64, _ := res.LastInsertId()

	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)
	FireWebhook(db, int(id64), "Failed")

	select {
	case body := <-received:
		var p webhookPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !strings.HasSuffix(p.Payload, "…") {
			t.Errorf("expected payload to end with ellipsis (…), got suffix: %q", p.Payload[len(p.Payload)-5:])
		}
		// First 500 bytes must match the original.
		if p.Payload[:500] != longPayload[:500] {
			t.Error("first 500 bytes of truncated payload do not match original")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook POST")
	}
}

// TestFireWebhook_IsAsynchronous verifies that FireWebhook returns immediately
// even when the HTTP server is slow to respond.
func TestFireWebhook_IsAsynchronous(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Pending', 'payload', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id64, _ := res.LastInsertId()

	// Slow server — sleeps 2 seconds before responding.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)

	start := time.Now()
	FireWebhook(db, int(id64), "Escalated")
	elapsed := time.Since(start)

	// FireWebhook must return well before the server's 2-second delay.
	if elapsed > 500*time.Millisecond {
		t.Errorf("FireWebhook blocked for %v — expected fire-and-forget (<500ms)", elapsed)
	}
}

// TestFireWebhook_FailureDoesNotPropagate verifies that a non-2xx response
// does not cause FireWebhook to panic or surface an error to the caller.
func TestFireWebhook_FailureDoesNotPropagate(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		 VALUES (0, 'repo', 'CodeEdit', 'Pending', 'payload', datetime('now'))`,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id64, _ := res.LastInsertId()

	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		done <- struct{}{}
	}))
	defer srv.Close()

	SetConfig(db, "webhook_url", srv.URL)

	// Must not panic. FireWebhook returns nothing, so any HTTP error is silently dropped.
	FireWebhook(db, int(id64), "Failed")

	// Wait for the goroutine to fire — confirms it ran to completion without crashing.
	select {
	case <-done:
		// goroutine received a 500 — no panic, no error propagation
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook goroutine to fire")
	}
}

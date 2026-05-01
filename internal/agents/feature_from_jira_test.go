package agents

// JIRA-from-UI — tests for QueueFeatureFromJira.
//
// LIVE_HAIKU_DISABLED is pinned to "1" by TestMain (testmain_test.go), so
// every test here runs through the deterministic stub branch in
// fetchJiraDescription. That keeps unit tests free of any live MCP call
// and lets us assert the queue-side bookkeeping (BountyBoard payload,
// priority, plan-only sentinel) end-to-end against a real :memory: db.
//
// Coverage:
//   - Happy path: row queued, payload formatted, summary returned.
//   - Priority preserved.
//   - Plan-only prepends [PLAN_ONLY] sentinel.
//   - Empty ticket id rejected with error (no row queued).
//   - Idempotence-shape: two calls with the same ticket id produce two
//     rows (the helper is the right level for "queue more"; dedup is a
//     downstream concern handled by AddBountyClassifying / fingerprints).

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestQueueFeatureFromJira_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })

	res, err := QueueFeatureFromJira(context.Background(), db, "ABC-123", 0, false)
	if err != nil {
		t.Fatalf("QueueFeatureFromJira: %v", err)
	}
	if res.TaskID == 0 {
		t.Fatalf("expected non-zero task id, got 0")
	}
	if res.Summary == "" {
		t.Fatalf("expected non-empty summary, got empty")
	}

	var taskType, status, payload string
	var priority int
	row := db.QueryRow(`SELECT type, status, payload, priority FROM BountyBoard WHERE id = ?`, res.TaskID)
	if err := row.Scan(&taskType, &status, &payload, &priority); err != nil {
		t.Fatalf("query queued row: %v", err)
	}
	if taskType != "Feature" {
		t.Errorf("type: got %q want Feature", taskType)
	}
	if status != "Pending" {
		t.Errorf("status: got %q want Pending", status)
	}
	if !strings.HasPrefix(payload, "[JIRA: ABC-123]\n") {
		t.Errorf("payload prefix: got %q", payload)
	}
	if strings.HasPrefix(payload, "[PLAN_ONLY]") {
		t.Errorf("plan-only sentinel should NOT be present when plan_only=false; got %q", payload)
	}
	if priority != 0 {
		t.Errorf("priority: got %d want 0 (default)", priority)
	}
}

func TestQueueFeatureFromJira_PriorityApplied(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })

	res, err := QueueFeatureFromJira(context.Background(), db, "TEAM-42", 7, false)
	if err != nil {
		t.Fatalf("QueueFeatureFromJira: %v", err)
	}
	var priority int
	if err := db.QueryRow(`SELECT priority FROM BountyBoard WHERE id = ?`, res.TaskID).Scan(&priority); err != nil {
		t.Fatalf("query priority: %v", err)
	}
	if priority != 7 {
		t.Errorf("priority: got %d want 7", priority)
	}
}

func TestQueueFeatureFromJira_PlanOnlySentinel(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })

	res, err := QueueFeatureFromJira(context.Background(), db, "PLAN-1", 0, true)
	if err != nil {
		t.Fatalf("QueueFeatureFromJira: %v", err)
	}
	var payload string
	if err := db.QueryRow(`SELECT payload FROM BountyBoard WHERE id = ?`, res.TaskID).Scan(&payload); err != nil {
		t.Fatalf("query payload: %v", err)
	}
	if !strings.HasPrefix(payload, "[PLAN_ONLY]\n") {
		t.Errorf("plan-only payload: got %q want [PLAN_ONLY]\\n prefix", payload)
	}
	if !strings.Contains(payload, "[JIRA: PLAN-1]") {
		t.Errorf("plan-only payload should still contain ticket marker; got %q", payload)
	}
}

func TestQueueFeatureFromJira_EmptyTicketIDRejected(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })

	for _, ticket := range []string{"", "   ", "\t"} {
		_, err := QueueFeatureFromJira(context.Background(), db, ticket, 0, false)
		if err == nil {
			t.Errorf("expected error for ticket=%q, got nil", ticket)
		}
	}

	// Asserting no row was queued (count must be 0).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Errorf("BountyBoard rows: got %d want 0 — empty-ticket reject must not queue anything", n)
	}
}

func TestQueueFeatureFromJira_NilDBRejected(t *testing.T) {
	_, err := QueueFeatureFromJira(context.Background(), nil, "ABC-123", 0, false)
	if err == nil {
		t.Fatal("expected error for nil db, got nil")
	}
}

func TestQueueFeatureFromJira_RepeatedCallsQueueMultipleRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })

	res1, err := QueueFeatureFromJira(context.Background(), db, "DUP-1", 0, false)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	res2, err := QueueFeatureFromJira(context.Background(), db, "DUP-1", 0, false)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if res1.TaskID == res2.TaskID {
		t.Errorf("repeated calls should produce distinct task ids; got %d twice", res1.TaskID)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows queued, got %d", n)
	}
}

func TestTruncateForSummary(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello…"},
		{"", 5, ""},
		{"abc", 0, ""},
		// Multibyte safety: rune count, not byte count.
		{"héllo", 4, "héll…"},
	}
	for _, tc := range cases {
		got := truncateForSummary(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("truncateForSummary(%q, %d): got %q want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

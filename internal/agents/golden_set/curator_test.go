package golden_set

import (
	"context"
	"testing"

	"force-orchestrator/internal/store"
)

func TestCurate_OnlyCleanShipping(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Three tasks for agent "council":
	// 1. clean: status=Completed, no requeue, has TaskHistory with outcome
	// 2. dirty: medic_requeue_count > 0 (rework signal) — must skip
	// 3. fix-task: spawning_at_id != "" — must skip
	_, err := db.Exec(`INSERT INTO BountyBoard (id, type, status, payload, owner, medic_requeue_count, spawning_at_id) VALUES
		(101, 'CodeEdit', 'Completed', 'Add login feature', 'council-Yoda', 0, ''),
		(102, 'CodeEdit', 'Completed', 'Add tests', 'council-Yoda', 2, ''),
		(103, 'CodeEdit', 'Completed', 'Fix typo', 'council-Yoda', 0, 'AT-005')`)
	if err != nil {
		t.Fatalf("seed bounties: %v", err)
	}
	// TaskHistory entries with distinct outcomes (so isTautological does not fire).
	_, err = db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome) VALUES
		(101, 1, 'council-Yoda', 's1', 'reviewed', '{"approved":true,"feedback":""}'),
		(102, 1, 'council-Yoda', 's2', 'reviewed', '{"approved":true,"feedback":""}'),
		(103, 1, 'council-Yoda', 's3', 'reviewed', '{"approved":true,"feedback":""}')`)
	if err != nil {
		t.Fatalf("seed task_history: %v", err)
	}

	n, err := CurateFromCleanShipping(context.Background(), db, "council")
	if err != nil {
		t.Fatalf("CurateFromCleanShipping: %v", err)
	}
	if n != 1 {
		t.Fatalf("only task 101 is clean-shipping; want 1 fixture inserted, got %d", n)
	}
	// Confirm what got inserted.
	var input string
	if err := db.QueryRow(`SELECT input FROM GoldenSetFixtures WHERE agent='council'`).Scan(&input); err != nil {
		t.Fatalf("query inserted: %v", err)
	}
	if input != "Add login feature" {
		t.Fatalf("wrong fixture inserted: %q", input)
	}
}

func TestCurate_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := db.Exec(`INSERT INTO BountyBoard (id, type, status, payload, owner) VALUES
		(101, 'CodeEdit', 'Completed', 'Add login feature', 'council-Yoda')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome) VALUES
		(101, 1, 'council-Yoda', 's1', 'reviewed', '{"approved":true}')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	n1, err := CurateFromCleanShipping(context.Background(), db, "council")
	if err != nil {
		t.Fatalf("first curate: %v", err)
	}
	n2, err := CurateFromCleanShipping(context.Background(), db, "council")
	if err != nil {
		t.Fatalf("second curate: %v", err)
	}
	if n1 != 1 || n2 != 0 {
		t.Fatalf("idempotence: first=%d second=%d (want 1, 0)", n1, n2)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM GoldenSetFixtures`).Scan(&n)
	if n != 1 {
		t.Fatalf("idempotence: expected 1 fixture row, got %d", n)
	}
}

func TestCurate_SkipsTautologies(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// payload == outcome → tautology → skip.
	_, err := db.Exec(`INSERT INTO BountyBoard (id, type, status, payload, owner) VALUES
		(201, 'CodeEdit', 'Completed', 'identical-text', 'council-Yoda')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome) VALUES
		(201, 1, 'council-Yoda', 's1', 'reviewed', 'identical-text')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := CurateFromCleanShipping(context.Background(), db, "council")
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if n != 0 {
		t.Fatalf("tautology must be skipped; got %d insertions", n)
	}
}

func TestAddManualFixture_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := AddManualFixture(context.Background(), db, "medic",
		`{"task_payload":"failing build","error_log":"npm install ECONNRESET"}`,
		`{"decision":"requeue","reasoning":"transient network failure"}`,
		"jake@example.com")
	if err != nil {
		t.Fatalf("AddManualFixture: %v", err)
	}
	if id <= 0 {
		t.Fatalf("AddManualFixture: bad id %d", id)
	}
	var source, curatedBy string
	db.QueryRow(`SELECT source, curated_by FROM GoldenSetFixtures WHERE id=?`, id).Scan(&source, &curatedBy)
	if source != string(SourceOperatorCurated) {
		t.Fatalf("source mismatch: %q", source)
	}
	if curatedBy != "operator:jake@example.com" {
		t.Fatalf("curated_by must be prefixed with operator:; got %q", curatedBy)
	}
}

func TestAddManualFixture_RejectsEmptyFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := AddManualFixture(context.Background(), db, "", "x", "y", "op@x"); err == nil {
		t.Fatal("empty agent: want error")
	}
	if _, err := AddManualFixture(context.Background(), db, "council", "", "y", "op@x"); err == nil {
		t.Fatal("empty input: want error")
	}
	if _, err := AddManualFixture(context.Background(), db, "council", "x", "", "op@x"); err == nil {
		t.Fatal("empty expected: want error")
	}
}

func TestIsTautological(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"hello", "hello", true},
		{"hello\n", "hello", true},
		{"a", "b", false},
		{"long-input-text", "long-input-text-with-extra", true},  // prefix
		{"long-extra-content-here", "different-thing", false},
	}
	for _, c := range cases {
		if got := isTautological(c.a, c.b); got != c.want {
			t.Errorf("isTautological(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

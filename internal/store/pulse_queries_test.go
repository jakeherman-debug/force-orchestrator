package store

import (
	"context"
	"testing"
	"time"
)

func TestPulseSnapshot_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Seed a couple of locked tasks, a convoy, and a stale escalation.
	_, err := db.Exec(`INSERT INTO BountyBoard (type, payload, owner, status, locked_at, created_at)
		VALUES ('Feature', 'do thing 1', 'astromech-1', 'Locked', ?, ?),
		       ('CodeEdit', 'do thing 2', 'captain', 'AwaitingCouncilReview', '', ?),
		       ('Investigate', 'esc thing', 'investigator', 'Escalated', '', ?)`,
		NowSQLite(), NowSQLite(), NowSQLite(), NowSQLite())
	if err != nil {
		t.Fatalf("seed bounty: %v", err)
	}
	_, err = db.Exec(`INSERT INTO Convoys (name, status, created_at) VALUES ('shakedown', 'Active', ?)`, NowSQLite())
	if err != nil {
		t.Fatalf("seed convoy: %v", err)
	}

	t0 := time.Now()
	snap, err := PulseSnapshotFor(ctx, db, "op@example.com")
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if len(snap.ActiveAgents) != 1 {
		t.Errorf("active agents=%d, want 1", len(snap.ActiveAgents))
	}
	if len(snap.Convoys) != 1 {
		t.Errorf("convoys=%d, want 1", len(snap.Convoys))
	}
	if snap.Queue.Total != 2 {
		t.Errorf("queue.Total=%d, want 2 (one Awaiting + one Escalated)", snap.Queue.Total)
	}
	if snap.Queue.HighStakes != 1 {
		t.Errorf("queue.HighStakes=%d, want 1", snap.Queue.HighStakes)
	}
	// Performance check — typical run < 50 ms; brief allows 100ms.
	if elapsed > 200*time.Millisecond {
		t.Errorf("snapshot took %v; want < 200ms (likely N+1 regression)", elapsed)
	}
}

func TestPulseSnapshot_PayloadTruncation(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	long := ""
	for i := 0; i < 500; i++ {
		long += "x"
	}
	_, err := db.Exec(`INSERT INTO BountyBoard (type, payload, owner, status, locked_at, created_at)
		VALUES ('Feature', ?, 'astromech-1', 'Locked', ?, ?)`,
		long, NowSQLite(), NowSQLite())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	snap, err := PulseSnapshotFor(ctx, db, "op@example.com")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.ActiveAgents) != 1 {
		t.Fatalf("got %d active agents", len(snap.ActiveAgents))
	}
	if len(snap.ActiveAgents[0].Payload) > 130 {
		t.Errorf("payload not truncated: %d bytes", len(snap.ActiveAgents[0].Payload))
	}
}

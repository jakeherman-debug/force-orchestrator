package agents

// D17 P2B — T+30 verdict dog tests.
//
// Coverage:
//   - Happy path: a run completed exactly 30 days ago gets a verdict mail
//     and t30_verdict_sent_at is stamped.
//   - Too-early: a run completed only 5 days ago is NOT mailed.
//   - Idempotence: re-running the dog on an already-stamped row is a no-op.
//   - Shadow mode: a 'paired_shadow' run is never mailed (mode guard).
//   - Empty completed_at: an in-flight run (empty completed_at) is skipped.
//   - dogCooldowns/dogOrder registration: the dog is wired into the dispatch
//     table and registered with a 24h cadence.

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestT30Verdict_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert a run completed 30 days + 1 hour ago (inside the 30–31-day window).
	res, err := db.Exec(`
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
			 mode, agent_name, score, score_source, completed_at, t30_verdict_sent_at)
		VALUES (1, 1, 'convoy', 42, 'holdout', 'diplomat', 0.75, 'downstream_verdict',
		        datetime('now', '-30 days', '-1 hour'), '')`)
	if err != nil {
		t.Fatalf("insert ExperimentRun: %v", err)
	}
	runID, _ := res.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogT30Verdict(context.Background(), db, logger); err != nil {
		t.Fatalf("dogT30Verdict: %v", err)
	}

	// Operator mail should have been sent.
	var subject, body string
	err = db.QueryRow(`SELECT subject, body FROM Fleet_Mail WHERE from_agent = 't30-verdict' ORDER BY id DESC LIMIT 1`).
		Scan(&subject, &body)
	if err != nil {
		t.Fatalf("expected operator mail; query failed: %v", err)
	}
	if !strings.Contains(subject, "T+30 VERDICT") {
		t.Errorf("expected subject to contain [T+30 VERDICT]; got %q", subject)
	}
	if !strings.Contains(body, "keep or deprecate") {
		t.Errorf("expected body to mention 'keep or deprecate'; got %q", body)
	}
	if !strings.Contains(body, "0.7500") {
		t.Errorf("expected body to include score 0.7500; got %q", body)
	}

	// t30_verdict_sent_at must be stamped.
	var sentAt string
	db.QueryRow(`SELECT IFNULL(t30_verdict_sent_at, '') FROM ExperimentRuns WHERE id = ?`, runID).Scan(&sentAt)
	if sentAt == "" {
		t.Errorf("expected t30_verdict_sent_at to be stamped after verdict mail; got empty")
	}
}

func TestT30Verdict_TooEarlySkipped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Run completed only 5 days ago — below the 30-day threshold.
	_, err := db.Exec(`
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
			 mode, completed_at, t30_verdict_sent_at)
		VALUES (2, 1, 'convoy', 43, 'holdout', datetime('now', '-5 days'), '')`)
	if err != nil {
		t.Fatalf("insert ExperimentRun: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogT30Verdict(context.Background(), db, logger); err != nil {
		t.Fatalf("dogT30Verdict: %v", err)
	}

	// No mail should have been sent.
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent = 't30-verdict'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 verdict mails for run at T+5; got %d", count)
	}
}

func TestT30Verdict_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Run completed 30 days ago but already has t30_verdict_sent_at set.
	_, err := db.Exec(`
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
			 mode, completed_at, t30_verdict_sent_at)
		VALUES (3, 1, 'convoy', 44, 'holdout', datetime('now', '-30 days', '-1 hour'),
		        datetime('now', '-1 day'))`)
	if err != nil {
		t.Fatalf("insert ExperimentRun: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogT30Verdict(context.Background(), db, logger); err != nil {
		t.Fatalf("dogT30Verdict: %v", err)
	}

	// Must not re-send.
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent = 't30-verdict'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 verdict mails for already-stamped run; got %d", count)
	}
}

func TestT30Verdict_ShadowModeSkipped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Shadow run at T+30 — mode guard should exclude it.
	_, err := db.Exec(`
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
			 mode, completed_at, t30_verdict_sent_at)
		VALUES (4, 1, 'convoy', 45, 'paired_shadow', datetime('now', '-30 days', '-1 hour'), '')`)
	if err != nil {
		t.Fatalf("insert ExperimentRun: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogT30Verdict(context.Background(), db, logger); err != nil {
		t.Fatalf("dogT30Verdict: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent = 't30-verdict'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 verdict mails for shadow-mode run; got %d", count)
	}
}

func TestT30Verdict_InFlightSkipped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// In-flight run: completed_at is empty.
	_, err := db.Exec(`
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
			 mode, completed_at, t30_verdict_sent_at)
		VALUES (5, 1, 'convoy', 46, 'holdout', '', '')`)
	if err != nil {
		t.Fatalf("insert ExperimentRun: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogT30Verdict(context.Background(), db, logger); err != nil {
		t.Fatalf("dogT30Verdict: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent = 't30-verdict'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 verdict mails for in-flight run; got %d", count)
	}
}

func TestT30Verdict_DogRegistered(t *testing.T) {
	// Verify the dog is wired into dogCooldowns and dogOrder.
	if _, ok := dogCooldowns["t30-verdict"]; !ok {
		t.Errorf("t30-verdict missing from dogCooldowns")
	}
	found := false
	for _, name := range dogOrder {
		if name == "t30-verdict" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("t30-verdict missing from dogOrder")
	}
}

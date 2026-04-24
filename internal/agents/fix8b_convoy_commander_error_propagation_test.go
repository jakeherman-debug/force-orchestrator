package agents

import (
	"context"
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// Fix #8b (convoy package) — error-propagation coverage for the two files
// swept in this campaign: internal/agents/convoy_review.go and
// internal/agents/commander.go. Each file's sweep converted a cohort of
// `_ = store.FailBounty(...)` / `_ = store.UpdateBountyStatus(...)` /
// `_, _ = CreateEscalation(...)` statements into `if err := ...; err != nil
// { logger.Printf(..."stale-lock detector will recover"|"convoy-review-watch
// will retry") }` blocks. These tests induce a DB-layer fault that guarantees
// the underlying mutator returns error, then assert the logger output matches
// the documented recovery-hint vocabulary.

// bufferLogger returns a *log.Logger that also satisfies the
// `interface{ Printf(string, ...any) }` shape that runConvoyReview's
// logger parameter requires.
func bufferLogger() (*bytes.Buffer, *log.Logger) {
	var buf bytes.Buffer
	return &buf, log.New(&buf, "", 0)
}

// TestFix8B_ConvoyReview_FailBountyErrorSurfacesToLogger simulates the
// "payload missing convoy_id" branch in runConvoyReview while BountyBoard is
// gone so FailBounty cannot succeed. The handler must log the FailBounty
// error with the "stale-lock detector will recover" recovery hint instead of
// silently swallowing it.
func TestFix8B_ConvoyReview_FailBountyErrorSurfacesToLogger(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Build a bounty row first so GetBounty would work if the table existed.
	id := store.AddBounty(db, 0, "ConvoyReview", `{"convoy_id":0}`)
	bounty, _ := store.GetBounty(db, id)
	if bounty == nil {
		t.Fatalf("seed: GetBounty returned nil")
	}

	// Drop BountyBoard so store.FailBounty's UPDATE errors out.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("setup: drop BountyBoard: %v", err)
	}

	buf, logger := bufferLogger()

	// payload.ConvoyID == 0 → hits the "payload missing convoy_id" FailBounty
	// branch (line ~230 in convoy_review.go post-fix).
	runConvoyReview(context.Background(), db, "Diplomat-test", bounty, logger)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty(missing convoy_id) failed") {
		t.Errorf("expected logger output to name the FailBounty path; got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected logger output to reference recovery path; got:\n%s", logs)
	}
}

// TestFix8B_ConvoyReview_UpdateBountyStatusErrorSurfacesToLogger exercises
// the "no ask-branches — complete as clean" branch. With the convoy present
// but no ask-branches seeded, runConvoyReview calls
// store.UpdateBountyStatus(...,"Completed"). After the table drop, that
// returns an error which the handler must route to the logger with the
// "convoy-review-watch will retry" recovery hint.
func TestFix8B_ConvoyReview_UpdateBountyStatusErrorSurfacesToLogger(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Minimal convoy setup — no ask-branches, which takes the
	// "complete as clean" path inside runConvoyReview.
	cid, _ := store.CreateConvoy(db, "[1] test convoy")

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: cid})
	id := store.AddBounty(db, 0, "ConvoyReview", string(payload))
	bounty, _ := store.GetBounty(db, id)
	if bounty == nil {
		t.Fatalf("seed: GetBounty returned nil")
	}

	// Drop BountyBoard so UpdateBountyStatus's UPDATE errors out.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("setup: drop BountyBoard: %v", err)
	}

	buf, logger := bufferLogger()

	runConvoyReview(context.Background(), db, "Diplomat-test", bounty, logger)

	logs := buf.String()
	if !strings.Contains(logs, "UpdateBountyStatus(Completed, no ask-branches) failed") {
		t.Errorf("expected logger output to name the UpdateBountyStatus call-site; got:\n%s", logs)
	}
	if !strings.Contains(logs, "convoy-review-watch will retry") {
		t.Errorf("expected logger output to reference convoy-review-watch recovery path; got:\n%s", logs)
	}
}

// TestFix8B_Commander_FailBountyErrorSurfacesToLogger exercises the
// "no repos registered" branch of runCommanderTask. With no repos seeded,
// loadRepoContext returns an error and the handler calls
// store.FailBounty(...). After the table drop, FailBounty itself returns
// error which the handler must log with the "stale-lock detector will
// recover" recovery hint rather than silently swallow.
func TestFix8B_Commander_FailBountyErrorSurfacesToLogger(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Deliberately seed NO repos. loadRepoContext will return the
	// "no repositories registered" error.
	id := store.AddBounty(db, 0, "Feature", "do the thing")
	b, _ := store.GetBounty(db, id)
	if b == nil {
		t.Fatalf("seed: GetBounty returned nil")
	}

	// Drop BountyBoard so store.FailBounty's UPDATE fails.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("setup: drop BountyBoard: %v", err)
	}

	buf, logger := bufferLogger()

	runCommanderTask(db, "Commander-test", b, logger)

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty(repo-context load) failed") {
		t.Errorf("expected logger output to name the FailBounty call-site; got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected logger output to reference recovery path; got:\n%s", logs)
	}
}

// TestFix8B_ConvoyReview_NoSilentFailuresGrep is a static regression guard:
// assert there are zero `TODO(Fix #8b)` markers remaining in the two files
// this sub-agent owns. If someone re-introduces the placeholder form, the
// test fails loudly rather than silently reopening the hot-path.
func TestFix8B_ConvoyReview_NoSilentFailuresGrep(t *testing.T) {
	for _, path := range []string{
		"convoy_review.go",
		"commander.go",
	} {
		body := readFile(t, path)
		if strings.Contains(body, "TODO(Fix #8b)") {
			t.Errorf("%s: TODO(Fix #8b) markers still present — the sweep missed a call site", path)
		}
	}
}

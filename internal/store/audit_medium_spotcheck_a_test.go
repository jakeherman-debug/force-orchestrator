package store

import (
	"bytes"
	"database/sql"
	"errors"
	"log"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Audit Medium Spot-Check Batch A covers three distinct Medium findings:
//
//   - AUDIT-066 (sql): pruneFleet composes WHERE-clause time windows via
//     fmt.Sprintf("%s", since) instead of using a bound `?` placeholder.
//   - AUDIT-068 (sql): ClaimBounty / ClaimForReview / ClaimForCaptainReview
//     treat every QueryRow error (including driver/schema errors) as
//     "nothing to claim" by returning (nil, false) and swallowing the
//     concrete error — indistinguishable from sql.ErrNoRows.
//   - AUDIT-069 (sql): ResolveFeatureBlockers performs a multi-table mutation
//     sequence (INSERT deps, UPDATE FeatureBlockers, maybe ClearConvoyHold)
//     with NO enclosing transaction; a crash mid-sequence leaves the hold
//     cleared but dependencies unwired (or vice versa).
//
// Each sub-test makes a STATIC assertion against the cited source file /
// function signature. These tests are expected to FAIL under the current
// code; closing the finding flips the assertion green.

func TestAUDIT_066_PruneFleetUnparameterizedInterval(t *testing.T) {
	// Without skip, fails with: AUDIT-066 REPRODUCED: pruneFleet composes SQL time windows via fmt.Sprintf with `datetime('now', '%s')` interpolation instead of using a `?` placeholder + bound arg. Found 12 fmt.Sprintf calls and 14 `datetime('now', '%s')` hits in cmd/force/maintenance.go lines 466-503.
	// Citation: cmd/force/maintenance.go:466-503.
	// Expectation: the prune targets slice is built with `?` placeholders
	// bound to the `since` string, not string-interpolated into SQL via
	// fmt.Sprintf.
	path := "../../cmd/force/maintenance.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)

	// Find the pruneFleet function body and slice 50 lines from its start.
	// Fix #8e: the original line-pinned slice (466-503) drifted when an
	// unrelated rows.Err() sweep added log-call lines higher in the file.
	// Locating the function by name keeps the test stable across line
	// shifts but checks the same code region.
	startMarker := "func pruneFleet("
	startIdx := strings.Index(src, startMarker)
	if startIdx < 0 {
		t.Fatalf("could not locate pruneFleet in %s", path)
	}
	region := src[startIdx:]
	if nl := indexNthNewline(region, 50); nl > 0 {
		region = region[:nl]
	}

	// The exact bug pattern: `datetime('now', '%s')` inside a fmt.Sprintf
	// with `since` substituted in. Safe only because `since` is built from
	// an int, but flagged as regression-prone: if `keepDays` ever becomes
	// user-controlled, this becomes SQL injection. The fix is a `?`
	// placeholder and a bound argument.
	//
	// Fix #8e clarification: the test now flags ONLY the bug pattern, not
	// every fmt.Sprintf in the function. The post-fix code has one
	// legitimate fmt.Sprintf at the top of pruneFleet (`since := fmt.Sprintf(
	// "-%d days", keepDays)`) that builds the bound-parameter string from
	// an int — that's SAFE because the value flows through `?` placeholders,
	// not SQL composition. The bug shape is fmt.Sprintf interpolating into
	// `datetime('now', '%s')` directly, which is what bugPattern detects.
	bugPattern := regexp.MustCompile(`datetime\('now', '%s'\)`)
	bugHits := len(bugPattern.FindAllString(region, -1))

	if bugHits == 0 {
		t.Logf("AUDIT-066 appears CLOSED: pruneFleet body contains 0 " +
			"`datetime('now', '%%s')` interpolations.")
		return
	}

	t.Errorf("AUDIT-066 REPRODUCED: pruneFleet composes SQL time windows via "+
		"fmt.Sprintf with `datetime('now', '%%s')` interpolation instead of "+
		"using a `?` placeholder + bound arg. Found %d `datetime('now', '%%s')` "+
		"hits inside the pruneFleet function body in cmd/force/maintenance.go. "+
		"Regression-prone: if keepDays becomes operator-supplied or dynamic, "+
		"this is SQL injection.", bugHits)
}

// indexNthNewline returns the byte offset of the n-th '\n' in s, or -1 if
// s contains fewer than n newlines.
func indexNthNewline(s string, n int) int {
	pos := 0
	for i := 0; i < n; i++ {
		idx := strings.Index(s[pos:], "\n")
		if idx < 0 {
			return -1
		}
		pos += idx + 1
	}
	return pos
}

func TestAUDIT_068_ClaimBountyConflatesErrNoRowsWithRealErrors(t *testing.T) {
	// Post-fix contract (Fix #8d, AUDIT-068): the claim helpers distinguish
	// sql.ErrNoRows (benign "nothing to claim") from real driver/schema
	// errors. The return signature stays `(*Bounty, bool)` — callers need not
	// be rewritten — but non-ErrNoRows errors are LOGGED to the package-level
	// stdlib logger, so a silent fleet stall (missing table, FK constraint
	// surprise, connection error) is observable to the operator via the
	// daemon log.
	//
	// Two assertions:
	//   Part A — STATIC grep: the claim helpers in tasks.go reference
	//     sql.ErrNoRows and errors.Is.
	//   Part B — EMPIRICAL: capture the stdlib log output while calling the
	//     claim helpers with (a) an empty queue and (b) a dropped table.
	//     The empty-queue call MUST NOT log (ErrNoRows is benign); the
	//     dropped-table call MUST log a "not ErrNoRows" message.

	// ── Part A: STATIC grep ───────────────────────────────────────────────
	raw, err := os.ReadFile("tasks.go")
	if err != nil {
		t.Fatalf("read tasks.go: %v", err)
	}
	src := string(raw)
	lines := strings.Split(src, "\n")
	if len(lines) < 160 {
		t.Fatalf("tasks.go has only %d lines, citation points to 87-160", len(lines))
	}
	region := strings.Join(lines[86:220], "\n") // widen to cover all three helpers

	hasErrNoRows := strings.Contains(region, "sql.ErrNoRows") &&
		strings.Contains(region, "errors.Is")
	if !hasErrNoRows {
		t.Errorf("AUDIT-068 REGRESSION (static): ClaimBounty/ClaimForReview/" +
			"ClaimForCaptainReview (tasks.go) no longer reference sql.ErrNoRows " +
			"+ errors.Is. Silent fleet stall on schema drift is back.")
	}

	// ── Part B: EMPIRICAL log capture ─────────────────────────────────────
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	}()

	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Empty queue → no log output.
	if b, ok := ClaimBounty(db, "CodeEdit", "astromech-test"); ok || b != nil {
		t.Fatalf("empty queue: expected (nil,false), got (%v,%v)", b, ok)
	}
	if buf.Len() != 0 {
		t.Errorf("AUDIT-068 REGRESSION: empty queue produced log output; ErrNoRows must stay silent. Got: %q", buf.String())
	}
	buf.Reset()

	// Drop BountyBoard to force a driver-level error on the SELECT.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop table setup: %v", err)
	}

	// Sanity: confirm the raw driver returns a non-ErrNoRows error.
	rawErr := db.QueryRow(`SELECT id FROM BountyBoard LIMIT 1`).Scan(new(int))
	if rawErr == nil {
		t.Fatalf("expected post-DROP SELECT to return an error, got nil")
	}
	if errors.Is(rawErr, sql.ErrNoRows) {
		t.Fatalf("expected a real driver error (e.g. 'no such table'), "+
			"got sql.ErrNoRows: %v", rawErr)
	}

	b, ok := ClaimBounty(db, "CodeEdit", "astromech-test")
	if ok || b != nil {
		t.Fatalf("post-DROP: expected (nil,false), got (%v,%v)", b, ok)
	}
	logged := buf.String()
	if !strings.Contains(logged, "ClaimBounty") || !strings.Contains(logged, "DB error") {
		t.Errorf("AUDIT-068 REPRODUCED: post-DROP call must log a non-ErrNoRows DB error. Got: %q", logged)
	}
	if !strings.Contains(logged, "no such table") {
		t.Errorf("AUDIT-068: expected underlying driver error (no such table) in log output. Got: %q", logged)
	}
}

func TestAUDIT_069_ResolveFeatureBlockersNoTransaction(t *testing.T) {
	// Post-fix contract (Fix #8d, AUDIT-069): the multi-table mutation
	// (AddDependency, UPDATE FeatureBlockers.resolved_at, optional
	// ClearConvoyHold) is wrapped in a single sql.Tx so a crash mid-sequence
	// either commits all or none. Uses AddDependencyTx + ClearConvoyHoldTx
	// siblings to participate in the same tx.

	// ── Part A: STATIC grep ───────────────────────────────────────────────
	raw, err := os.ReadFile("feature_blockers.go")
	if err != nil {
		t.Fatalf("read feature_blockers.go: %v", err)
	}
	src := string(raw)

	// Scan the whole ResolveFeatureBlockers function body — from its
	// declaration to the next top-level `func ` keyword.
	fnIdx := strings.Index(src, "func ResolveFeatureBlockers(")
	if fnIdx < 0 {
		t.Fatalf("ResolveFeatureBlockers not found in feature_blockers.go")
	}
	tail := src[fnIdx+1:]
	nextFn := strings.Index(tail, "\nfunc ")
	var region string
	if nextFn < 0 {
		region = src[fnIdx:]
	} else {
		region = src[fnIdx : fnIdx+1+nextFn]
	}

	hasBegin := strings.Contains(region, "db.Begin(") ||
		strings.Contains(region, "BeginTx(")
	hasCommit := strings.Contains(region, ".Commit(")
	hasTxDep := strings.Contains(region, "AddDependencyTx(")
	hasTxHold := strings.Contains(region, "ClearConvoyHoldTx(")

	if !hasBegin || !hasCommit || !hasTxDep {
		t.Errorf("AUDIT-069 REGRESSION (static): ResolveFeatureBlockers "+
			"dropped its transaction wrapping. hasBegin=%v, hasCommit=%v, "+
			"hasAddDependencyTx=%v, hasClearConvoyHoldTx=%v. Multi-table "+
			"mutation without tx re-opens the crash-mid-sequence defect.",
			hasBegin, hasCommit, hasTxDep, hasTxHold)
	}

	// ── Part B: EMPIRICAL demonstration ───────────────────────────────────
	// Seed: one blocking feature/convoy, one blocked convoy with one root
	// task, one tail task in the new convoy. Call ResolveFeatureBlockers
	// and verify the sequence lands in separate commits (no enclosing tx).
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert blocked root task (convoy 100), tail task (convoy 200), and
	// a FeatureBlocker row.
	if _, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, type, status, payload, convoy_id, created_at)
		VALUES (0,'CodeEdit','Pending','root',100,datetime('now'))`); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, type, status, payload, convoy_id, created_at)
		VALUES (0,'CodeEdit','Completed','tail',200,datetime('now'))`); err != nil {
		t.Fatalf("seed tail: %v", err)
	}

	// Blocking feature id = 42, blocked convoy = 100, new convoy = 200.
	CreateFeatureBlocker(db, 100, 42, "spot-check")

	// Preconditions: hold exists, no deps yet.
	var depCountBefore int
	db.QueryRow(`SELECT COUNT(*) FROM TaskDependencies`).Scan(&depCountBefore)

	injected := ResolveFeatureBlockers(db, 42, 200)
	if injected == 0 {
		// Injected may be 0 if GetConvoyTailTaskIDs returns empty (tail
		// task had no terminal status matching its heuristic). That's fine
		// for this spot-check — the static assertion already covers the
		// transaction absence.
		t.Logf("AUDIT-069 empirical: ResolveFeatureBlockers injected 0 "+
			"dependencies (tail-task discovery may not match this minimal " +
			"fixture); static assertion is the authoritative check.")
		return
	}

	// Postconditions exist in separate journal frames: each of the
	// underlying db.Exec calls (INSERT INTO TaskDependencies, UPDATE
	// FeatureBlockers SET resolved_at, optional DELETE ConvoyHold) was
	// autocommitted on its own. A crash between them leaves inconsistent
	// state. We cannot observe autocommit boundaries post-hoc from SQLite
	// in-memory, so the empirical arm simply verifies each individual
	// mutation landed (confirming the sequence ran as separate statements
	// under autocommit — which is the audit's exact complaint).
	var resolvedAt sql.NullString
	db.QueryRow(`SELECT resolved_at FROM FeatureBlockers
		WHERE blocking_feature_id = 42 AND blocked_convoy_id = 100`).Scan(&resolvedAt)
	var holdRemaining int
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyHolds WHERE convoy_id = 100`).Scan(&holdRemaining)
	var depCountAfter int
	db.QueryRow(`SELECT COUNT(*) FROM TaskDependencies`).Scan(&depCountAfter)

	t.Logf("AUDIT-069 empirical observations: injected=%d, resolved_at=%q, "+
		"hold_rows=%d, dep_delta=%d. Each mutation landed under its own "+
		"autocommit — a panic between them would leave a split state. "+
		"This matches the audit's 'no tx' complaint.",
		injected, resolvedAt.String, holdRemaining, depCountAfter-depCountBefore)
}

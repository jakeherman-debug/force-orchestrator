package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPattern_P6_UndocumentedStatusValues verifies the P6 audit pattern:
// the fleet's state machine has trap states and undocumented status values.
//
// Three trap-state behaviors are demonstrated here in one test, each as
// a sub-test. All three are EXPECTED TO FAIL under the current codebase;
// the assertions describe the invariant the fix must restore.
//
//   Sub-test A (AUDIT-012): runStaleConvoysReport's "non-terminal tasks"
//     predicate excludes only 'Completed' and 'Cancelled'. A convoy whose
//     tasks are all 'Failed' or 'Escalated' is treated as terminal and
//     the convoy is silently UPDATEd to Completed — masking failures.
//     Because runStaleConvoysReport is an unexported function in the
//     sibling internal/agents package and cannot be called from
//     internal/store without an import cycle, this sub-test is a static
//     source check against dogs.go: the WHERE clause must include
//     'Failed' and 'Escalated' in the terminal set.
//
//   Sub-test B (AUDIT-025): three code paths write
//     Escalations.status='Resolved', yet nothing in the system lists,
//     sweeps, filters, or cleans up that status. The dashboard list
//     handler passes its ?status= query parameter verbatim to
//     ListEscalations with no allowlist; the docstring enumerates only
//     "Open", "Acknowledged", "Closed", "". 'Resolved' rows accumulate
//     forever. Static check: confirm the writes exist AND confirm the
//     documented allowlist omits 'Resolved'.
//
//   Sub-test C (AUDIT-085): the dashboard ActiveCount query omits
//     'Classifying', 'AwaitingChancellorReview', 'ConflictPending', and
//     'Planned'. Active tasks in those states are invisible on the
//     dashboard. Static check of the literal SQL in handlers.go.
func TestPattern_P6_UndocumentedStatusValues(t *testing.T) {
	// Resolve repo-root-relative paths from this test file's location.
	// This test file lives at internal/store/audit_pattern_p6_test.go
	// so the repo root is two levels up.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot resolve cwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))

	dogsPath := filepath.Join(repoRoot, "internal", "agents", "dogs.go")
	handlersPath := filepath.Join(repoRoot, "internal", "dashboard", "handlers.go")
	escalationPath := filepath.Join(repoRoot, "internal", "agents", "escalation.go")
	sweeperPath := filepath.Join(repoRoot, "internal", "agents", "escalation_sweeper.go")
	medicPath := filepath.Join(repoRoot, "internal", "agents", "medic.go")
	worktreeResetPath := filepath.Join(repoRoot, "internal", "agents", "pilot_worktree_reset.go")

	t.Run("A_staleConvoysReport_treats_Failed_and_Escalated_as_terminal", func(t *testing.T) {
		// STATIC check: runStaleConvoysReport is unexported in
		// package agents, so this sub-test (which is in package store)
		// cannot invoke it directly without creating an import cycle.
		// We instead read the source of dogs.go and assert on the SQL
		// text of the "non-terminal" count predicate.
		//
		// Today's source literally reads:
		//   AND status NOT IN ('Completed', 'Cancelled')
		// which is wrong — a convoy of all-Failed tasks flips to
		// Completed and the failure signal is lost.
		//
		// The fix is to widen the set to include every terminal
		// status, at minimum 'Failed' and 'Escalated', so that Active
		// convoys containing only failures are NOT auto-completed.
		srcBytes, err := os.ReadFile(dogsPath)
		if err != nil {
			t.Fatalf("cannot read %s: %v", dogsPath, err)
		}
		src := string(srcBytes)

		// Locate the function body so we don't accidentally match some
		// unrelated SQL elsewhere in the file.
		const fnSig = "func runStaleConvoysReport("
		fnIdx := strings.Index(src, fnSig)
		if fnIdx < 0 {
			t.Fatalf("AUDIT-012: could not find %q in %s (function was "+
				"renamed or moved — this test must be updated)",
				fnSig, dogsPath)
		}
		// Cap the window at the next top-level "\nfunc " declaration.
		tail := src[fnIdx:]
		nextFn := strings.Index(tail[1:], "\nfunc ")
		var body string
		if nextFn < 0 {
			body = tail
		} else {
			body = tail[:1+nextFn]
		}

		// Today's buggy literal.
		const buggy = "AND status NOT IN ('Completed', 'Cancelled')"

		if !strings.Contains(body, buggy) {
			// If this line is gone the audit may have been fixed; skip
			// the fail-expecting branch and require the fixed form.
			t.Logf("AUDIT-012: buggy literal %q no longer present in "+
				"runStaleConvoysReport — assuming code was fixed; "+
				"requiring Failed+Escalated in terminal set below.", buggy)
		}

		hasFailed := strings.Contains(body, "'Failed'")
		hasEscalated := strings.Contains(body, "'Escalated'")
		if !hasFailed || !hasEscalated {
			t.Errorf("AUDIT-012: runStaleConvoysReport's terminal-status "+
				"predicate does not include both 'Failed' and 'Escalated' "+
				"(hasFailed=%v hasEscalated=%v). A convoy whose tasks are "+
				"all Failed or all Escalated is silently UPDATEd to "+
				"status='Completed', masking the failure from the "+
				"operator and the daily digest. The predicate must "+
				"exclude every terminal status, not just 'Completed' "+
				"and 'Cancelled'.", hasFailed, hasEscalated)
		}
	})

	t.Run("B_Resolved_escalation_status_written_but_unrecognized", func(t *testing.T) {
		// Part 1: confirm at least one sink writes
		// Escalations.status='Resolved'. The audit identifies three:
		// escalation_sweeper.go, medic.go, pilot_worktree_reset.go.
		writers := map[string]string{
			sweeperPath:       "escalation_sweeper.go",
			medicPath:         "medic.go",
			worktreeResetPath: "pilot_worktree_reset.go",
		}
		needle := "status = 'Resolved'"
		foundWriters := 0
		for path, name := range writers {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Logf("cannot read %s (%s): %v", name, path, err)
				continue
			}
			if strings.Contains(string(b), needle) {
				foundWriters++
			}
		}
		if foundWriters == 0 {
			t.Errorf("AUDIT-025: expected at least one sink to write "+
				"Escalations %q; found none. If this fires the audit "+
				"fix is in progress; update the test to track the new "+
				"canonical status.", needle)
		}

		// Part 2: confirm the read side's allowlist omits 'Resolved'.
		// The dashboard's handleEscalationList passes the ?status=
		// parameter straight through to ListEscalations with no
		// filtering. ListEscalations's own docstring enumerates the
		// allowed values: "Open", "Acknowledged", "Closed", or "".
		// 'Resolved' is deliberately NOT documented, yet nothing
		// rejects it — it simply returns zero rows through the
		// WHERE status = ? path. So rows written with 'Resolved'
		// are invisible to every consumer that filters on the
		// documented statuses.
		escBytes, err := os.ReadFile(escalationPath)
		if err != nil {
			t.Fatalf("cannot read %s: %v", escalationPath, err)
		}
		escSrc := string(escBytes)
		docIdx := strings.Index(escSrc, "// ListEscalations returns escalations filtered by status")
		if docIdx < 0 {
			t.Fatalf("AUDIT-025: could not locate ListEscalations docstring in %s", escalationPath)
		}
		// Read the docstring line.
		docEnd := strings.Index(escSrc[docIdx:], "\n")
		if docEnd < 0 {
			t.Fatalf("AUDIT-025: malformed docstring for ListEscalations")
		}
		doc := escSrc[docIdx : docIdx+docEnd]
		if strings.Contains(doc, "Resolved") {
			t.Logf("AUDIT-025: docstring now mentions 'Resolved' — "+
				"audit partially addressed. Docstring: %q", doc)
		} else {
			t.Errorf("AUDIT-025: %d sink(s) write "+
				"Escalations.status='Resolved', but the read-side "+
				"contract (ListEscalations docstring in %s) enumerates "+
				"only 'Open','Acknowledged','Closed',''. 'Resolved' "+
				"rows accumulate and are invisible to every consumer "+
				"that filters on the documented statuses. "+
				"Docstring: %q", foundWriters, escalationPath, doc)
		}

		// Part 3: confirm the maintenance cleanup (documented in the
		// audit at cmd/force/maintenance.go) only recognizes 'Closed'.
		// The dashboard stats query in handlers.go is similarly narrow
		// — it counts only Open. Grep for any use of 'Resolved' in
		// the dashboard handlers: there should be none.
		handlersBytes, err := os.ReadFile(handlersPath)
		if err != nil {
			t.Fatalf("cannot read %s: %v", handlersPath, err)
		}
		if strings.Contains(string(handlersBytes), "'Resolved'") ||
			strings.Contains(string(handlersBytes), `"Resolved"`) {
			t.Logf("AUDIT-025: dashboard handlers.go now references "+
				"'Resolved' — audit partially addressed.")
		} else {
			t.Errorf("AUDIT-025: dashboard handlers.go (%s) contains "+
				"no reference to 'Resolved'. Combined with %d "+
				"writer(s), this confirms the write/read status "+
				"vocabulary mismatch: 'Resolved' rows are created "+
				"but never surfaced, filtered, or cleaned up.",
				handlersPath, foundWriters)
		}
	})

	t.Run("C_dashboard_ActiveCount_omits_real_active_states", func(t *testing.T) {
		// STATIC check against handlers.go:110. The audit calls out
		// four missing statuses: 'Classifying',
		// 'AwaitingChancellorReview', 'ConflictPending', 'Planned'.
		handlersBytes, err := os.ReadFile(handlersPath)
		if err != nil {
			t.Fatalf("cannot read %s: %v", handlersPath, err)
		}
		handlersSrc := string(handlersBytes)

		const activeCountMarker = "Scan(&s.ActiveCount)"
		idx := strings.Index(handlersSrc, activeCountMarker)
		if idx < 0 {
			t.Fatalf("AUDIT-085: could not locate %q in %s — field "+
				"may have been renamed; update this test.",
				activeCountMarker, handlersPath)
		}
		// Walk backward ~400 bytes to capture the SQL literal for
		// the ActiveCount query.
		start := idx - 400
		if start < 0 {
			start = 0
		}
		window := handlersSrc[start : idx+len(activeCountMarker)]

		missing := []string{
			"Classifying",
			"AwaitingChancellorReview",
			"ConflictPending",
			"Planned",
		}
		var omitted []string
		for _, s := range missing {
			// Match the quoted SQL literal form used in the query.
			if !strings.Contains(window, "'"+s+"'") {
				omitted = append(omitted, s)
			}
		}
		if len(omitted) > 0 {
			t.Errorf("AUDIT-085: dashboard ActiveCount SQL at %s:~110 "+
				"omits active statuses %v. Tasks in those states are "+
				"invisible on the dashboard — ActiveCount can read 0 "+
				"while tens of tasks are actively being classified "+
				"(LLM spend is occurring) or awaiting Chancellor "+
				"review. SQL window: %q",
				handlersPath, omitted, strings.TrimSpace(window))
		}
	})
}

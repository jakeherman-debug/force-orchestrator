package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// fetchTaskDetail invokes handleTasksSubroutes for GET /api/tasks/{id} and
// unmarshals the response body. Shared helper for the link-decoration tests.
func fetchTaskDetail(t *testing.T, db *sql.DB, id int) DashboardTaskDetail {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tasks/%d", id), nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	var detail DashboardTaskDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("response is not JSON: %v — body=%s", err, w.Body.String())
	}
	return detail
}

// TestTaskDetail_BranchURL_BuiltFromRemote verifies that a task with a
// branch_name on a repo with a GitHub remote_url gets a clickable web URL.
func TestTaskDetail_BranchURL_BuiltFromRemote(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.SetRepoRemoteInfo(db, "api", "git@github.com:acme/api.git", "main")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "do a thing", 1, 5, "Pending")
	store.SetBranchName(db, taskID, "agent/R2-D2/task-77")

	d := fetchTaskDetail(t, db, taskID)
	if d.BranchName != "agent/R2-D2/task-77" {
		t.Errorf("branch_name wrong: %q", d.BranchName)
	}
	want := "https://github.com/acme/api/tree/agent/R2-D2/task-77"
	if d.BranchURL != want {
		t.Errorf("branch_url = %q, want %q", d.BranchURL, want)
	}
}

// TestTaskDetail_BranchURL_NoRemoteYieldsEmpty confirms the graceful
// degradation path: a repo with no remote_url (legacy or test setup) leaves
// branch_url empty so the frontend renders the branch name as plain text.
func TestTaskDetail_BranchURL_NoRemoteYieldsEmpty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	// Intentionally do NOT call SetRepoRemoteInfo — remote_url stays empty.
	taskID, _ := store.AddConvoyTask(db, 0, "api", "do a thing", 1, 5, "Pending")
	store.SetBranchName(db, taskID, "agent/R2-D2/task-77")

	d := fetchTaskDetail(t, db, taskID)
	if d.BranchURL != "" {
		t.Errorf("branch_url should be empty when remote is unset; got %q", d.BranchURL)
	}
	if d.BranchName == "" {
		t.Error("branch_name must still be populated even without a URL")
	}
}

// TestTaskDetail_NoBranchYieldsNoURL handles the pre-branch-assignment case:
// a fresh CodeEdit task has no branch_name, so no branch link is emitted.
func TestTaskDetail_NoBranchYieldsNoURL(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.SetRepoRemoteInfo(db, "api", "git@github.com:acme/api.git", "main")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "fresh task", 1, 5, "Pending")

	d := fetchTaskDetail(t, db, taskID)
	if d.BranchName != "" {
		t.Errorf("branch_name should be empty pre-claim; got %q", d.BranchName)
	}
	if d.BranchURL != "" {
		t.Errorf("branch_url must be empty when branch_name is empty; got %q", d.BranchURL)
	}
}

// TestTaskDetail_PRLink_PopulatedFromAskBranchPR verifies the sub-PR link
// shows up once an AskBranchPR row exists for the task (Jedi Council has
// opened the sub-PR). Covers pr_number, pr_url, and pr_state.
func TestTaskDetail_PRLink_PopulatedFromAskBranchPR(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.SetRepoRemoteInfo(db, "api", "git@github.com:acme/api.git", "main")
	convoyID, _ := store.CreateConvoy(db, "[1] t")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "do a thing", convoyID, 5, "Pending")
	store.SetBranchName(db, taskID, "agent/R2-D2/task-99")

	prURL := "https://github.com/acme/api/pull/4242"
	if _, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", prURL, 4242); err != nil {
		t.Fatal(err)
	}

	d := fetchTaskDetail(t, db, taskID)
	if d.PRNumber != 4242 {
		t.Errorf("pr_number = %d, want 4242", d.PRNumber)
	}
	if d.PRURL != prURL {
		t.Errorf("pr_url = %q, want %q", d.PRURL, prURL)
	}
	if !strings.EqualFold(d.PRState, "Open") {
		t.Errorf("pr_state should default to Open, got %q", d.PRState)
	}
}

// TestTaskDetail_PRLink_OmittedWhenNoPR ensures tasks without an open sub-PR
// don't emit bogus pr_* fields (matters for frontends that conditionally
// render the "PR" row on truthy pr_number).
func TestTaskDetail_PRLink_OmittedWhenNoPR(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.SetRepoRemoteInfo(db, "api", "git@github.com:acme/api.git", "main")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "pre-PR task", 1, 5, "Pending")
	store.SetBranchName(db, taskID, "agent/R2-D2/task-1")

	d := fetchTaskDetail(t, db, taskID)
	if d.PRNumber != 0 {
		t.Errorf("pr_number must be 0 without a sub-PR, got %d", d.PRNumber)
	}
	if d.PRURL != "" {
		t.Errorf("pr_url must be empty without a sub-PR, got %q", d.PRURL)
	}
	if d.PRState != "" {
		t.Errorf("pr_state must be empty without a sub-PR, got %q", d.PRState)
	}
}

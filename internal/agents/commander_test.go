package agents

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── findPlanCycle ─────────────────────────────────────────────────────────────

func TestFindPlanCycle_NoCycle(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, BlockedBy: nil},
		{TempID: 2, BlockedBy: []int{1}},
		{TempID: 3, BlockedBy: []int{2}},
	}
	if cid := findPlanCycle(tasks); cid != 0 {
		t.Errorf("expected no cycle, got cycle at %d", cid)
	}
}

func TestFindPlanCycle_SimpleCycle(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, BlockedBy: []int{2}},
		{TempID: 2, BlockedBy: []int{1}},
	}
	if cid := findPlanCycle(tasks); cid == 0 {
		t.Error("expected cycle to be detected")
	}
}

func TestFindPlanCycle_SelfDep(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, BlockedBy: []int{1}},
	}
	if cid := findPlanCycle(tasks); cid == 0 {
		t.Error("expected self-dep cycle to be detected")
	}
}

func TestFindPlanCycle_Empty(t *testing.T) {
	if cid := findPlanCycle(nil); cid != 0 {
		t.Errorf("expected 0 for empty tasks, got %d", cid)
	}
}

func TestFindPlanCycle_LongerCycle(t *testing.T) {
	// 1→2→3→1
	tasks := []store.TaskPlan{
		{TempID: 1, BlockedBy: []int{3}},
		{TempID: 2, BlockedBy: []int{1}},
		{TempID: 3, BlockedBy: []int{2}},
	}
	if cid := findPlanCycle(tasks); cid == 0 {
		t.Error("expected 3-node cycle to be detected")
	}
}

func TestFindPlanCycle_ParallelDiamond(t *testing.T) {
	// A and B both depend on C — no cycle
	tasks := []store.TaskPlan{
		{TempID: 1, BlockedBy: []int{}},    // C: no deps
		{TempID: 2, BlockedBy: []int{1}},   // A depends on C
		{TempID: 3, BlockedBy: []int{1}},   // B depends on C
	}
	if cid := findPlanCycle(tasks); cid != 0 {
		t.Errorf("expected no cycle in parallel diamond, got cycle at %d", cid)
	}
}

// ── loadKnownRepos ────────────────────────────────────────────────────────────

func TestLoadKnownRepos_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	repos := loadKnownRepos(db)
	if len(repos) != 0 {
		t.Errorf("expected empty repos, got %v", repos)
	}
}

func TestLoadKnownRepos_WithRepos(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "API service")
	store.AddRepo(db, "frontend", "/tmp/frontend", "Frontend app")
	repos := loadKnownRepos(db)
	if !repos["api"] {
		t.Error("expected 'api' to be in known repos")
	}
	if !repos["frontend"] {
		t.Error("expected 'frontend' to be in known repos")
	}
	if repos["other"] {
		t.Error("expected 'other' to NOT be in known repos")
	}
}

func TestLoadKnownRepos_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	db.Close()
	repos := loadKnownRepos(db)
	if len(repos) != 0 {
		t.Errorf("expected empty repos on DB error, got %d entries", len(repos))
	}
}

// ── readFilePreview ──────────────────────────────────────────────────────────

func TestReadFilePreview_Missing(t *testing.T) {
	got := readFilePreview("/nonexistent/path/file.txt", 10)
	if got != "" {
		t.Errorf("expected empty string for missing file, got %q", got)
	}
}

func TestReadFilePreview_TruncatesLines(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.txt"
	content := "line1\nline2\nline3\nline4\nline5\n"
	os.WriteFile(path, []byte(content), 0644)

	got := readFilePreview(path, 3)
	lines := strings.Split(got, "\n")
	// Should have at most 3 lines
	if len(lines) > 3 {
		t.Errorf("expected at most 3 lines, got %d", len(lines))
	}
	if !strings.Contains(got, "line1") {
		t.Error("expected line1 to be present")
	}
}

func TestReadFilePreview_ShortFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.txt"
	os.WriteFile(path, []byte("hello\nworld"), 0644)

	got := readFilePreview(path, 100)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("expected full short file, got %q", got)
	}
}

// ── runCommanderTask ──────────────────────────────────────────────────────────

func TestRunCommanderTask_NoRepos(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Feature", "add login page")
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", nil)
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed with no repos, got %q", b.Status)
	}
}

func TestRunCommanderTask_CLIError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")

	id := store.AddBounty(db, 0, "Feature", "add login page")
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", fmt.Errorf("claude CLI failed: exit 1"))
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after first CLI error, got %q", b.Status)
	}
}

func TestRunCommanderTask_CLIError_MaxRetries(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")

	id := store.AddBounty(db, 0, "Feature", "add login page")
	// Simulate already at max infra failures
	for i := 0; i < MaxInfraFailures-1; i++ {
		store.IncrementInfraFailures(db, id)
	}
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", fmt.Errorf("claude CLI failed: exit 1"))
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed at max retries, got %q", b.Status)
	}
}

func TestRunCommanderTask_JSONError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")

	id := store.AddBounty(db, 0, "Feature", "add login page")
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "not valid json at all", nil)
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after JSON error (infra failure), got %q", b.Status)
	}
}

func TestRunCommanderTask_EmptyTaskList(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")

	id := store.AddBounty(db, 0, "Feature", "add login page")
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "[]", nil)
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed for empty task list, got %q", b.Status)
	}
}

func TestRunCommanderTask_UnknownRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "known-repo", "/tmp/known", "test")

	id := store.AddBounty(db, 0, "Feature", "do something")
	b, _ := store.GetBounty(db, id)

	// Claude returns a task targeting an unregistered repo
	tasks := `[{"id":1,"repo":"ghost-repo","task":"do it","blocked_by":[]}]`
	withStubCLIRunner(t, tasks, nil)
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed for unknown repo in plan, got %q", b.Status)
	}
}

func TestRunCommanderTask_CyclicDeps(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", "/tmp/myrepo", "test")

	id := store.AddBounty(db, 0, "Feature", "do something")
	b, _ := store.GetBounty(db, id)

	// Tasks 1 and 2 block each other — cycle
	tasks := `[{"id":1,"repo":"myrepo","task":"t1","blocked_by":[2]},{"id":2,"repo":"myrepo","task":"t2","blocked_by":[1]}]`
	withStubCLIRunner(t, tasks, nil)
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed for cyclic dependencies, got %q", b.Status)
	}
}

func TestRunCommanderTask_Success(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", t.TempDir(), "test")

	id := store.AddBounty(db, 0, "Feature", "add login")
	b, _ := store.GetBounty(db, id)

	tasks := `[{"id":1,"repo":"myrepo","task":"Create login handler","blocked_by":[]},` +
		`{"id":2,"repo":"myrepo","task":"Add login form","blocked_by":[1]}]`
	withStubCLIRunner(t, tasks, nil)
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Completed" {
		t.Errorf("expected Completed after successful decomposition, got %q", b.Status)
	}
	// Verify subtasks were created
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit'`, id).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 subtasks, got %d", count)
	}
}

// ── validateTaskPlan ──────────────────────────────────────────────────────────

func TestValidateTaskPlan_Valid(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "myrepo", BlockedBy: []int{}},
		{TempID: 2, Repo: "myrepo", BlockedBy: []int{1}},
	}
	known := map[string]bool{"myrepo": true}
	if err := validateTaskPlan(tasks, known); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidateTaskPlan_EmptyRepo(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "", BlockedBy: []int{}},
	}
	if err := validateTaskPlan(tasks, map[string]bool{}); err == nil {
		t.Error("expected error for empty repo")
	}
}

func TestValidateTaskPlan_UnknownRepo(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "ghost", BlockedBy: []int{}},
	}
	if err := validateTaskPlan(tasks, map[string]bool{"known": true}); err == nil {
		t.Error("expected error for unknown repo")
	}
}

func TestValidateTaskPlan_InvalidBlockedBy(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "myrepo", BlockedBy: []int{99}},
	}
	if err := validateTaskPlan(tasks, map[string]bool{"myrepo": true}); err == nil {
		t.Error("expected error for invalid blocked_by reference")
	}
}

func TestValidateTaskPlan_Cycle(t *testing.T) {
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "myrepo", BlockedBy: []int{2}},
		{TempID: 2, Repo: "myrepo", BlockedBy: []int{1}},
	}
	if err := validateTaskPlan(tasks, map[string]bool{"myrepo": true}); err == nil {
		t.Error("expected error for cyclic dependency")
	}
}

// ── insertConvoyAndTasks ──────────────────────────────────────────────────────

func TestInsertConvoyAndTasks_Success(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", t.TempDir(), "test")

	parentID := store.AddBounty(db, 0, "Feature", "add login")
	b, _ := store.GetBounty(db, parentID)

	convoyID, _ := store.CreateConvoy(db, "test convoy")
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "myrepo", Task: "task one", BlockedBy: []int{}},
		{TempID: 2, Repo: "myrepo", Task: "task two", BlockedBy: []int{1}},
	}

	idMapping, err := insertConvoyAndTasks(db, tasks, b, convoyID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(idMapping) != 2 {
		t.Errorf("expected 2 mapped IDs, got %d", len(idMapping))
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit'`, parentID).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 subtasks inserted, got %d", count)
	}
}

func TestInsertConvoyAndTasks_PlanOnly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", t.TempDir(), "test")

	parentID := store.AddBounty(db, 0, "Feature", "[PLAN_ONLY]\ndo the thing")
	b, _ := store.GetBounty(db, parentID)

	convoyID, _ := store.CreateConvoy(db, "plan convoy")
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "myrepo", Task: "plan task", BlockedBy: []int{}},
	}

	if _, err := insertConvoyAndTasks(db, tasks, b, convoyID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit'`, parentID).Scan(&status)
	if status != "Planned" {
		t.Errorf("expected Planned status for [PLAN_ONLY], got %q", status)
	}
}

func TestInsertConvoyAndTasks_DeletesStaleSubtasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", t.TempDir(), "test")

	parentID := store.AddBounty(db, 0, "Feature", "redo work")
	b, _ := store.GetBounty(db, parentID)

	// Insert a stale subtask from a prior attempt
	convoyID1, _ := store.CreateConvoy(db, "old convoy")
	store.AddConvoyTask(db, parentID, "myrepo", "stale task", convoyID1, 0, "Pending")

	var before int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ?`, parentID).Scan(&before)
	if before != 1 {
		t.Fatalf("expected 1 stale subtask, got %d", before)
	}

	convoyID2, _ := store.CreateConvoy(db, "new convoy")
	tasks := []store.TaskPlan{
		{TempID: 1, Repo: "myrepo", Task: "fresh task", BlockedBy: []int{}},
	}
	if _, err := insertConvoyAndTasks(db, tasks, b, convoyID2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var after int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit'`, parentID).Scan(&after)
	if after != 1 {
		t.Errorf("expected 1 subtask after stale deletion + re-insert, got %d", after)
	}
}

func TestRunCommanderTask_PlanOnly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", t.TempDir(), "test")

	id := store.AddBounty(db, 0, "Feature", "[PLAN_ONLY]\nadd login")
	b, _ := store.GetBounty(db, id)

	tasks := `[{"id":1,"repo":"myrepo","task":"plan task","blocked_by":[]}]`
	withStubCLIRunner(t, tasks, nil)
	logger := log.New(io.Discard, "", 0)
	runCommanderTask(db, "Commander-Cody", b, logger)

	// Subtask should be Planned, not Pending
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit'`, id).Scan(&status)
	if status != "Planned" {
		t.Errorf("expected subtask Planned for [PLAN_ONLY], got %q", status)
	}
}

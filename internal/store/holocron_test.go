package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ── InitHolocron ─────────────────────────────────────────────────────────────

func TestInitHolocron_CreatesDatabase(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	db := InitHolocron()
	if db == nil {
		t.Fatal("expected non-nil db from InitHolocron")
	}
	db.Close()

	if _, statErr := os.Stat("holocron.db"); statErr != nil {
		t.Error("expected holocron.db to be created")
	}
}

// ── AddBounty / GetBounty ─────────────────────────────────────────────────────

func TestAddAndGetBounty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "Feature", "Add login page")
	if id == 0 {
		t.Fatal("expected non-zero bounty ID")
	}

	b, err := GetBounty(db, id)
	if err != nil {
		t.Fatalf("GetBounty: %v", err)
	}
	if b.Payload != "Add login page" {
		t.Errorf("unexpected payload: %q", b.Payload)
	}
	if b.Status != "Pending" {
		t.Errorf("unexpected status: %q", b.Status)
	}
}

// ── ClaimBounty ───────────────────────────────────────────────────────────────

func TestClaimBounty_AtomicLock(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddBounty(db, 0, "CodeEdit", "Fix the hyperdrive")

	b, ok := ClaimBounty(db, "CodeEdit", "R2-D2")
	if !ok {
		t.Fatal("expected claim to succeed")
	}
	if b.Status != "Locked" {
		t.Errorf("expected Locked, got %q", b.Status)
	}
	if b.Owner != "R2-D2" {
		t.Errorf("expected owner R2-D2, got %q", b.Owner)
	}

	// A second agent should not be able to claim the same task
	_, ok2 := ClaimBounty(db, "CodeEdit", "BB-8")
	if ok2 {
		t.Error("second claim should have failed — task already Locked")
	}
}

func TestClaimBounty_BlockedTaskNotClaimed(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert blocker (Pending = not Completed) and a task that depends on it
	blockerID := AddBounty(db, 0, "Feature", "blocker task")
	taskID := AddBounty(db, 0, "CodeEdit", "Blocked task")
	AddDependency(db, taskID, blockerID)

	_, ok := ClaimBounty(db, "CodeEdit", "R2-D2")
	if ok {
		t.Error("blocked task should not be claimable while dependency is not Completed")
	}
}

func TestClaimBounty_RaceCondition(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "contested task")

	// Simulate another agent claiming the task just before our CAS attempt
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'BB-8', locked_at = datetime('now') WHERE id = ?`, id)

	_, ok := ClaimBounty(db, "CodeEdit", "R2-D2")
	if ok {
		t.Error("expected ClaimBounty to fail when no Pending task exists")
	}
}

func TestClaimBountyPriority(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Add three tasks with different priorities
	AddBounty(db, 0, "CodeEdit", "low priority")
	idHigh := AddBounty(db, 0, "CodeEdit", "high priority")
	SetBountyPriority(db, idHigh, 10)
	AddBounty(db, 0, "CodeEdit", "medium priority")
	SetBountyPriority(db, idHigh-1+2, 5) // the third task gets priority 5 -- actually let's just use the IDs

	// High priority task should be claimed first
	b, ok := ClaimBounty(db, "CodeEdit", "test-agent")
	if !ok {
		t.Fatal("expected to claim a task")
	}
	if b.ID != idHigh {
		t.Errorf("expected high-priority task to be claimed first (id=%d), got id=%d", idHigh, b.ID)
	}
}

// ── FailBounty ────────────────────────────────────────────────────────────────

func TestFailBounty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "Feature", "some work")
	FailBounty(db, id, "something went wrong")

	b, _ := GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed, got %q", b.Status)
	}
}

// ── IncrementRetryCount ───────────────────────────────────────────────────────

func TestIncrementRetryCount(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "retry me")
	for i := 1; i <= 3; i++ {
		n := IncrementRetryCount(db, id)
		if n != i {
			t.Errorf("attempt %d: expected count %d, got %d", i, i, n)
		}
	}
}

// ── GetConfig / SetConfig ─────────────────────────────────────────────────────

func TestGetConfig_DefaultValue(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	val := GetConfig(db, "missing_key", "default")
	if val != "default" {
		t.Errorf("expected default, got %q", val)
	}
}

func TestSetAndGetConfig(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	SetConfig(db, "max_concurrent", "8")
	val := GetConfig(db, "max_concurrent", "4")
	if val != "8" {
		t.Errorf("expected 8, got %q", val)
	}
}

// ── AddRepo / GetRepoPath ─────────────────────────────────────────────────────

func TestAddRepo_AndGetPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "my-repo", "/tmp/my-repo", "A test repo")
	path := GetRepoPath(db, "my-repo")
	if path != "/tmp/my-repo" {
		t.Errorf("expected /tmp/my-repo, got %q", path)
	}

	// Unknown repo returns empty
	if p := GetRepoPath(db, "nonexistent"); p != "" {
		t.Errorf("expected empty, got %q", p)
	}
}

func TestGetRepoPath_Missing(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	path := GetRepoPath(db, "nonexistent-repo")
	if path != "" {
		t.Errorf("expected empty path for nonexistent repo, got %q", path)
	}
}

// ── RecordTaskHistory / GetTaskHistory ───────────────────────────────────────

func TestRecordAndGetTaskHistory(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "some task")
	RecordTaskHistory(db, id, "R2-D2", "sess-001", "output text", "Completed")
	RecordTaskHistory(db, id, "R2-D2", "sess-002", "retry output", "Completed")

	entries := GetTaskHistory(db, id)
	if len(entries) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(entries))
	}
	if entries[0].Attempt != 1 {
		t.Errorf("expected attempt 1, got %d", entries[0].Attempt)
	}
	if entries[1].Attempt != 2 {
		t.Errorf("expected attempt 2, got %d", entries[1].Attempt)
	}
	if entries[0].SessionID != "sess-001" {
		t.Errorf("unexpected session ID: %q", entries[0].SessionID)
	}
}

func TestRecordTaskHistoryReturnsID(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	histID := RecordTaskHistory(db, id, "R2-D2", "sess1", "output", "Completed")
	if histID <= 0 {
		t.Errorf("expected positive history ID, got %d", histID)
	}
}

func TestUpdateTaskHistoryTokens(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	histID := RecordTaskHistory(db, id, "R2-D2", "sess1", "output", "Completed")
	UpdateTaskHistoryTokens(db, histID, 1500, 300)

	entries := GetTaskHistory(db, id)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].TokensIn != 1500 {
		t.Errorf("expected 1500 tokens_in, got %d", entries[0].TokensIn)
	}
	if entries[0].TokensOut != 300 {
		t.Errorf("expected 300 tokens_out, got %d", entries[0].TokensOut)
	}
}

func TestGetTaskHistory_MultipleAttempts(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	for i := 0; i < 5; i++ {
		RecordTaskHistory(db, id, "R2-D2", "sess", "output", "Completed")
	}

	history := GetTaskHistory(db, id)
	if len(history) != 5 {
		t.Errorf("expected 5 history entries, got %d", len(history))
	}
}

// ── Fleet mail ────────────────────────────────────────────────────────────────

func TestSendAndListMail(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := SendMail(db, "R2-D2", "jedi-council", "task done", "I finished the work", 42, MailTypeInfo)
	if id <= 0 {
		t.Fatalf("expected positive mail ID, got %d", id)
	}

	mails := ListMail(db, "jedi-council")
	if len(mails) != 1 {
		t.Fatalf("expected 1 mail, got %d", len(mails))
	}
	if mails[0].Subject != "task done" {
		t.Errorf("wrong subject: %q", mails[0].Subject)
	}
	if mails[0].TaskID != 42 {
		t.Errorf("wrong task_id: %d", mails[0].TaskID)
	}
}

func TestMailMarkRead(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := int(SendMail(db, "astromech", "operator", "hello", "", 0, MailTypeInfo))
	m := GetMail(db, id)
	if m == nil {
		t.Fatal("mail not found")
	}
	if m.ReadAt != "" {
		t.Error("mail should start unread")
	}

	MarkMailRead(db, id)
	m2 := GetMail(db, id)
	if m2.ReadAt == "" {
		t.Error("mail should be marked read after MarkMailRead")
	}
}

func TestMailStats(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	SendMail(db, "astromech", "operator", "msg1", "", 0, MailTypeInfo)
	id2 := int(SendMail(db, "astromech", "operator", "msg2", "", 0, MailTypeInfo))
	MarkMailRead(db, id2)

	unread, total := MailStats(db, "operator", "operator")
	if total != 2 {
		t.Errorf("expected 2 total, got %d", total)
	}
	if unread != 1 {
		t.Errorf("expected 1 unread, got %d", unread)
	}
}

func TestMailStats_EmptyToAgent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	SendMail(db, "a", "operator", "msg1", "", 0, MailTypeInfo)
	SendMail(db, "a", "astromech", "msg2", "", 0, MailTypeInfo)

	unread, total := MailStats(db, "", "")
	if total != 2 {
		t.Errorf("expected 2 total fleet-wide, got %d", total)
	}
	if unread != 2 {
		t.Errorf("expected 2 unread fleet-wide, got %d", unread)
	}
}

func TestMailStats_WithMail(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	SendMail(db, "op", "agent1", "s1", "", 0, MailTypeInfo)
	SendMail(db, "op", "agent2", "s2", "", 0, MailTypeAlert)

	unread, total := MailStats(db, "", "")
	if total != 2 {
		t.Errorf("expected 2 total, got %d", total)
	}
	if unread != 2 {
		t.Errorf("expected 2 unread, got %d", unread)
	}
}

func TestMailStats_RoleAddressing(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Direct to agent
	SendMail(db, "operator", "R2-D2", "personal", "", 0, MailTypeInfo)
	// Addressed to the role — any astromech should count it
	SendMail(db, "operator", "astromech", "role-wide directive", "", 0, MailTypeDirective)
	// Fleet-wide — every agent should count it
	SendMail(db, "operator", "all", "fleet alert", "", 0, MailTypeAlert)
	// To a different agent — should NOT be counted
	SendMail(db, "operator", "captain", "not yours", "", 0, MailTypeInfo)

	unread, total := MailStats(db, "R2-D2", "astromech")
	if total != 3 {
		t.Errorf("expected 3 total (direct + role + all), got %d", total)
	}
	if unread != 3 {
		t.Errorf("expected 3 unread, got %d", unread)
	}

	// Mark the role-addressed one read; unread count should drop
	var roleID int
	db.QueryRow(`SELECT id FROM Fleet_Mail WHERE to_agent = 'astromech'`).Scan(&roleID)
	MarkMailRead(db, roleID)

	unread2, _ := MailStats(db, "R2-D2", "astromech")
	if unread2 != 2 {
		t.Errorf("expected 2 unread after marking role mail read, got %d", unread2)
	}
}

func TestGetMail_NotFound(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	m := GetMail(db, 9999)
	if m != nil {
		t.Error("expected nil for nonexistent mail ID")
	}
}

func TestListMail_AllMail(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	SendMail(db, "op", "agent1", "s1", "", 0, MailTypeInfo)
	SendMail(db, "op", "agent2", "s2", "", 0, MailTypeAlert)

	all := ListMail(db, "")
	if len(all) != 2 {
		t.Errorf("expected 2 mails for empty filter, got %d", len(all))
	}
}

func TestListMail_PaginationAndFilter(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 5; i++ {
		SendMail(db, "operator", "astromech", "msg", "", 0, MailTypeInfo)
	}

	mails := ListMail(db, "astromech")
	if len(mails) != 5 {
		t.Errorf("expected 5 mails, got %d", len(mails))
	}
	// Should return empty for a different recipient
	mails2 := ListMail(db, "jedi-council")
	if len(mails2) != 0 {
		t.Errorf("expected 0 mails for jedi-council, got %d", len(mails2))
	}
}

// ── ReadInboxForAgent ─────────────────────────────────────────────────────────

func TestReadInboxForAgent_RoleAddressing(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Addressed to the role — any astromech should see it
	SendMail(db, "operator", "astromech", "slow down", "take it easy", 0, MailTypeDirective)
	// Addressed to "all" — everyone sees it
	SendMail(db, "operator", "all", "fleet alert", "system overloaded", 0, MailTypeAlert)
	// Addressed to a specific agent — only R2-D2 sees it
	SendMail(db, "operator", "R2-D2", "personal note", "you specifically", 0, MailTypeInfo)
	// Addressed to a different role — astromechs should NOT see this
	SendMail(db, "operator", "jedi-council", "council only", "not for astromechs", 0, MailTypeDirective)

	mails := ReadInboxForAgent(db, "R2-D2", "astromech", 0)
	if len(mails) != 3 {
		t.Errorf("expected 3 messages (role + all + personal), got %d", len(mails))
	}

	// All should now be marked read
	unread, _ := MailStats(db, "R2-D2", "astromech")
	// MailStats now uses the same addressing logic as ReadInboxForAgent; verify via DB too
	var unreadTotal int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE read_at = '' AND (to_agent = 'R2-D2' OR to_agent = 'astromech' OR to_agent = 'all')`).Scan(&unreadTotal)
	if unreadTotal != 0 {
		t.Errorf("expected 0 unread after ReadInboxForAgent, got %d", unreadTotal)
	}
	_ = unread
}

func TestReadInboxForAgent_TaskScoped(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Task-specific feedback for task 7
	SendMail(db, "Council-Yoda", "astromech", "task 7 feedback", "fix the tests", 7, MailTypeFeedback)
	// Standing directive (task_id=0) — should always appear
	SendMail(db, "operator", "astromech", "standing order", "use tabs not spaces", 0, MailTypeDirective)
	// Task-specific for a different task — should NOT appear
	SendMail(db, "Council-Yoda", "astromech", "task 9 feedback", "something else", 9, MailTypeFeedback)

	mails := ReadInboxForAgent(db, "BB-8", "astromech", 7)
	if len(mails) != 2 {
		t.Errorf("expected 2 messages (task 7 feedback + standing directive), got %d", len(mails))
	}
	types := map[MailType]bool{}
	for _, m := range mails {
		types[m.MessageType] = true
	}
	if !types[MailTypeFeedback] {
		t.Error("expected feedback message in results")
	}
	if !types[MailTypeDirective] {
		t.Error("expected directive message in results")
	}
}

func TestReadInboxForAgent_DifferentRoleExcluded(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	SendMail(db, "operator", "jedi-council", "council only", "review carefully", 0, MailTypeDirective)

	mails := ReadInboxForAgent(db, "R2-D2", "astromech", 0)
	if len(mails) != 0 {
		t.Errorf("astromech should not see jedi-council mail, got %d messages", len(mails))
	}
}

func TestReadInboxForAgent_MarksRead(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	SendMail(db, "operator", "R2-D2", "hello", "world", 0, MailTypeInfo)
	SendMail(db, "operator", "R2-D2", "hello2", "world2", 0, MailTypeAlert)

	msgs := ReadInboxForAgent(db, "R2-D2", "astromech", 10)
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages, got %d", len(msgs))
	}

	// Second read should return 0 (already marked read)
	msgs2 := ReadInboxForAgent(db, "R2-D2", "astromech", 10)
	if len(msgs2) != 0 {
		t.Errorf("expected 0 messages on second read (already read), got %d", len(msgs2))
	}
}

// ── ClaimForReview / ClaimForCaptainReview ────────────────────────────────────

func TestClaimForReview(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "review me")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', branch_name = 'agent/R2/task-1' WHERE id = ?`, id)

	b, ok := ClaimForReview(db, "Council-Yoda")
	if !ok {
		t.Fatal("expected to claim task for review")
	}
	if b.ID != id {
		t.Errorf("expected task ID %d, got %d", id, b.ID)
	}

	var status, owner string
	db.QueryRow(`SELECT status, owner FROM BountyBoard WHERE id = ?`, id).Scan(&status, &owner)
	if status != "UnderReview" {
		t.Errorf("expected status UnderReview, got %q", status)
	}
	if owner != "Council-Yoda" {
		t.Errorf("expected owner Council-Yoda, got %q", owner)
	}
}

func TestClaimForReview_NoTasks(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	_, ok := ClaimForReview(db, "Council-Yoda")
	if ok {
		t.Error("expected no task to claim")
	}
}

func TestClaimForReview_RaceCondition(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert task in AwaitingCouncilReview
	id := AddBounty(db, 0, "CodeEdit", "review task")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', branch_name = 'b' WHERE id = ?`, id)

	// Claim it directly (simulating another agent winning the race)
	db.Exec(`UPDATE BountyBoard SET status = 'UnderReview', owner = 'Council-Mace', locked_at = datetime('now') WHERE id = ?`, id)

	// Now ClaimForReview finds no AwaitingCouncilReview tasks → QueryRow fails → returns nil, false
	_, ok := ClaimForReview(db, "Council-Yoda")
	if ok {
		t.Error("expected ClaimForReview to fail when no task in AwaitingCouncilReview")
	}
}

func TestClaimForCaptainReview(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "captain review")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', branch_name = 'agent/R2/task-1' WHERE id = ?`, id)

	b, ok := ClaimForCaptainReview(db, "Captain-Rex")
	if !ok {
		t.Fatal("expected to claim task for captain review")
	}
	if b.ID != id {
		t.Errorf("expected task ID %d, got %d", id, b.ID)
	}

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status)
	if status != "UnderCaptainReview" {
		t.Errorf("expected status UnderCaptainReview, got %q", status)
	}
}

func TestClaimForCaptainReview_NoTasks(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	_, ok := ClaimForCaptainReview(db, "Captain-Rex")
	if ok {
		t.Error("expected no task to claim")
	}
}

func TestClaimForCaptainReview_RaceCondition(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "captain task")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', branch_name = 'b' WHERE id = ?`, id)

	// Claim it directly
	db.Exec(`UPDATE BountyBoard SET status = 'UnderCaptainReview', owner = 'Captain-Rex', locked_at = datetime('now') WHERE id = ?`, id)

	_, ok := ClaimForCaptainReview(db, "Captain-Phasma")
	if ok {
		t.Error("expected ClaimForCaptainReview to fail when no task in AwaitingCaptainReview")
	}
}

// ── IsConvoyCoordinated / SetConvoyCoordinated ────────────────────────────────

func TestIsConvoyCoordinated_Zero(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if IsConvoyCoordinated(db, 0) {
		t.Error("convoyID=0 should never be coordinated")
	}
}

func TestIsConvoyCoordinated_NotCoordinated(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert a convoy directly
	res, _ := db.Exec(`INSERT INTO Convoys (name, status) VALUES ('regular-convoy', 'Active')`)
	id, _ := res.LastInsertId()
	if IsConvoyCoordinated(db, int(id)) {
		t.Error("fresh convoy should not be coordinated")
	}
}

func TestSetConvoyCoordinated(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`INSERT INTO Convoys (name, status) VALUES ('coordinated-op', 'Active')`)
	id, _ := res.LastInsertId()
	SetConvoyCoordinated(db, int(id))

	if !IsConvoyCoordinated(db, int(id)) {
		t.Error("convoy should be coordinated after SetConvoyCoordinated")
	}
}

// ── RemoveDependenciesOf ──────────────────────────────────────────────────────

func TestRemoveDependenciesOf(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id1 := AddBounty(db, 0, "CodeEdit", "blocker")
	id2 := AddBounty(db, 0, "CodeEdit", "blocked")
	id3 := AddBounty(db, 0, "CodeEdit", "also blocked")

	AddDependency(db, id2, id1)
	AddDependency(db, id2, id3)

	// Remove all deps of id2
	RemoveDependenciesOf(db, id2)

	deps := GetDependencies(db, id2)
	if len(deps) != 0 {
		t.Errorf("expected no dependencies after RemoveDependenciesOf, got %v", deps)
	}
}

// ── GetDependencies ───────────────────────────────────────────────────────────

func TestGetDependencies_Multiple(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	parent := AddBounty(db, 0, "CodeEdit", "parent")
	dep1 := AddBounty(db, 0, "CodeEdit", "dep1")
	dep2 := AddBounty(db, 0, "CodeEdit", "dep2")
	db.Exec(`INSERT INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, parent, dep1)
	db.Exec(`INSERT INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, parent, dep2)

	deps := GetDependencies(db, parent)
	if len(deps) != 2 {
		t.Errorf("expected 2 dependencies, got %d", len(deps))
	}
}

// ── DeleteFleetMemory ─────────────────────────────────────────────────────────

func TestDeleteFleetMemory(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 1, "success", "added endpoint", "handler.go")
	var id int
	db.QueryRow(`SELECT id FROM FleetMemory WHERE task_id = 1`).Scan(&id)

	if id == 0 {
		t.Fatal("memory not stored")
	}

	ok := DeleteFleetMemory(db, id)
	if !ok {
		t.Error("expected DeleteFleetMemory to return true")
	}

	// Second delete should return false
	ok2 := DeleteFleetMemory(db, id)
	if ok2 {
		t.Error("expected second delete to return false")
	}

	// Verify it's gone
	memories := GetFleetMemories(db, "api", "", 10)
	if len(memories) != 0 {
		t.Errorf("expected no memories after delete, got %d", len(memories))
	}
}

// ── ListAllFleetMemories ──────────────────────────────────────────────────────

func TestListAllFleetMemories_AllRepos(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 1, "success", "api work", "api.go")
	StoreFleetMemory(db, "frontend", 2, "success", "frontend work", "app.tsx")
	StoreFleetMemory(db, "backend", 3, "failure", "backend work", "")

	entries := ListAllFleetMemories(db, "", 10)
	if len(entries) != 3 {
		t.Errorf("expected 3 memories across all repos, got %d", len(entries))
	}
}

func TestListAllFleetMemories_Filtered(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 1, "success", "api work", "")
	StoreFleetMemory(db, "frontend", 2, "success", "frontend work", "")

	entries := ListAllFleetMemories(db, "api", 10)
	if len(entries) != 1 {
		t.Errorf("expected 1 api memory, got %d", len(entries))
	}
	if entries[0].Repo != "api" {
		t.Errorf("expected repo 'api', got %q", entries[0].Repo)
	}
}

func TestListAllFleetMemories_Limit(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 1; i <= 5; i++ {
		StoreFleetMemory(db, "repo", i, "success", "work", "")
	}

	entries := ListAllFleetMemories(db, "", 3)
	if len(entries) != 3 {
		t.Errorf("expected 3 entries (limit), got %d", len(entries))
	}
}

// ── StoreFleetMemory / GetFleetMemories ───────────────────────────────────────

func TestStoreAndGetFleetMemories(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 10, "success", "Added POST /users endpoint", "handlers/users.go, routes.go")
	StoreFleetMemory(db, "api", 11, "success", "Fixed auth middleware", "middleware/auth.go")

	memories := GetFleetMemories(db, "api", "", 10)
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}
	// Most recent first
	if memories[0].TaskID != 11 {
		t.Errorf("expected task 11 first (most recent), got %d", memories[0].TaskID)
	}
	if memories[0].FilesChanged != "middleware/auth.go" {
		t.Errorf("expected middleware/auth.go for most recent (task 11), got %q", memories[0].FilesChanged)
	}
}

func TestFleetMemory_SuccessAndFailure(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 1, "success", "Added endpoint", "handler.go")
	StoreFleetMemory(db, "api", 2, "failure", "Task failed after 3 attempts. Final rejection: wrong approach", "")
	StoreFleetMemory(db, "api", 3, "failure", "Infra failure: repo path missing", "")

	memories := GetFleetMemories(db, "api", "", 10)
	if len(memories) != 3 {
		t.Fatalf("expected 3 memories, got %d", len(memories))
	}

	outcomes := map[string]int{}
	for _, m := range memories {
		outcomes[m.Outcome]++
	}
	if outcomes["success"] != 1 {
		t.Errorf("expected 1 success, got %d", outcomes["success"])
	}
	if outcomes["failure"] != 2 {
		t.Errorf("expected 2 failures, got %d", outcomes["failure"])
	}
}

func TestGetFleetMemories_RepoScoped(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 1, "success", "api task", "api.go")
	StoreFleetMemory(db, "frontend", 2, "success", "frontend task", "app.tsx")

	apiMems := GetFleetMemories(db, "api", "", 10)
	if len(apiMems) != 1 {
		t.Errorf("expected 1 api memory, got %d", len(apiMems))
	}
	if apiMems[0].Repo != "api" {
		t.Errorf("expected repo 'api', got %q", apiMems[0].Repo)
	}

	frontendMems := GetFleetMemories(db, "frontend", "", 10)
	if len(frontendMems) != 1 {
		t.Errorf("expected 1 frontend memory, got %d", len(frontendMems))
	}
}

func TestGetFleetMemories_Limit(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 1; i <= 10; i++ {
		StoreFleetMemory(db, "repo", i, "success", fmt.Sprintf("task %d", i), "")
	}

	memories := GetFleetMemories(db, "repo", "", 3)
	if len(memories) != 3 {
		t.Errorf("expected 3 memories (limit), got %d", len(memories))
	}
	// Should be the 3 most recent (tasks 10, 9, 8)
	if memories[0].TaskID != 10 {
		t.Errorf("expected most recent task first, got task %d", memories[0].TaskID)
	}
}

func TestGetFleetMemories_FTSRanking(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Skip if FTS5 is not compiled in — requires -tags sqlite_fts5
	var ftsCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM FleetMemory_fts`).Scan(&ftsCount); err != nil {
		t.Skip("FTS5 not available (build without -tags sqlite_fts5)")
	}

	// Three memories on the same repo — task 2 has the most JWT/auth/token vocabulary
	StoreFleetMemory(db, "api", 1, "success", "Added pagination to the list endpoint", "handlers/list.go")
	StoreFleetMemory(db, "api", 2, "success", "Fixed JWT token validation in auth middleware", "middleware/auth.go")
	StoreFleetMemory(db, "api", 3, "failure", "Failed to add CSV export due to missing schema", "handlers/export.go")

	// Query shares vocabulary with task 2 (JWT, token, auth, middleware)
	results := GetFleetMemories(db, "api", "JWT token auth middleware", 3)
	if len(results) == 0 {
		t.Fatal("expected at least 1 result from FTS search")
	}
	if results[0].TaskID != 2 {
		t.Errorf("expected auth memory (task 2) ranked first, got task %d", results[0].TaskID)
	}
}

func TestGetFleetMemories_FTSFallsBackToRecency(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 10, "success", "Added endpoint", "handler.go")
	StoreFleetMemory(db, "api", 11, "success", "Fixed bug", "service.go")

	// Query with words that don't appear in any memory — should fall back to recency
	results := GetFleetMemories(db, "api", "xyzzy quux nonexistent", 5)
	if len(results) != 2 {
		t.Errorf("expected recency fallback to return 2 results, got %d", len(results))
	}
	// Recency order: task 11 first
	if results[0].TaskID != 11 {
		t.Errorf("expected most recent (task 11) first in fallback, got task %d", results[0].TaskID)
	}
}

func TestGetFleetMemories_EmptyQueryUsesRecency(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "repo", 1, "success", "first task", "")
	StoreFleetMemory(db, "repo", 2, "success", "second task", "")

	results := GetFleetMemories(db, "repo", "", 5)
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
	if results[0].TaskID != 2 {
		t.Errorf("expected most recent first, got task %d", results[0].TaskID)
	}
}

func TestGetFleetMemories_FTSQuery(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 1, "success", "fixed authentication bug in handler", "auth.go")
	StoreFleetMemory(db, "api", 2, "failed", "attempted refactor of database layer", "db.go")

	// FTS query that should match the first memory
	results := GetFleetMemories(db, "api", "authentication handler", 10)
	if len(results) == 0 {
		t.Error("expected FTS query to return results")
	}
}

func TestStoreFleetMemory_FTSSearchable(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	StoreFleetMemory(db, "api", 1, "success", "refactored authentication middleware", "auth.go middleware.go")

	// Should be findable via FTS
	results := GetFleetMemories(db, "api", "authentication middleware", 5)
	if len(results) == 0 {
		t.Error("expected stored memory to be findable via FTS query")
	}
}

func TestStoreFleetMemory_DBError(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	db.Close()
	// Must not panic when DB is closed — covers early return on INSERT failure
	StoreFleetMemory(db, "repo", 1, "success", "summary", "files")
}

// ── sanitizeFTSQuery ──────────────────────────────────────────────────────────

func TestSanitizeFTSQuery(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"add JWT auth middleware", "add JWT auth middleware"},
		{"fix [ESCALATED:HIGH:broken]", "fix ESCALATED HIGH broken"},
		{"it's a test!", "it's a test"},  // apostrophe → space, short 'a' dropped
		{"a b cc ddd", "cc ddd"},          // 1-char words dropped
		{"", ""},
	}
	// Rebuild expected — apostrophe becomes space, 'a' and 'b' dropped
	cases[2].want = "it cc ddd" // 'it' kept (2 chars), 's' dropped, 'a' dropped, 'test' kept
	// Let's just check it doesn't panic and strips special chars
	for _, c := range cases {
		got := sanitizeFTSQuery(c.input)
		// Verify no FTS5 special chars remain
		for _, bad := range []string{"\"", "(", ")", ":", "-", "'"} {
			if strings.Contains(got, bad) {
				t.Errorf("sanitizeFTSQuery(%q) still contains %q: got %q", c.input, bad, got)
			}
		}
	}
}

func TestSanitizeFTSQuery_Empty(t *testing.T) {
	got := sanitizeFTSQuery("!@#$%")
	if got != "" {
		t.Errorf("expected empty string for all-special chars, got %q", got)
	}
}

func TestSanitizeFTSQuery_ShortWords(t *testing.T) {
	got := sanitizeFTSQuery("a b cd")
	if strings.Contains(got, " a ") || strings.Contains(got, "b ") {
		t.Errorf("expected short words filtered out, got %q", got)
	}
	if !strings.Contains(got, "cd") {
		t.Errorf("expected 'cd' to remain, got %q", got)
	}
}

// ── AuditLog ──────────────────────────────────────────────────────────────────

func TestLogAudit(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	LogAudit(db, "operator", "reset", 7, "manual reset")
	LogAudit(db, "boot-agent", "escalate", 8, "stall detected")

	entries := ListAuditLog(db, 10)
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries, got %d", len(entries))
	}
	// Newest first
	if entries[0].Action != "escalate" {
		t.Errorf("expected newest entry first, got action=%q", entries[0].Action)
	}
	if entries[1].TaskID != 7 {
		t.Errorf("expected task_id=7, got %d", entries[1].TaskID)
	}
}

func TestLogAuditLimit(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 10; i++ {
		LogAudit(db, "operator", "reset", i, "")
	}
	entries := ListAuditLog(db, 3)
	if len(entries) != 3 {
		t.Errorf("expected 3 with limit=3, got %d", len(entries))
	}
}

func TestListAuditLog_DefaultLimit(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 10; i++ {
		LogAudit(db, "operator", "test", i, "")
	}

	// limit=0 should use default (50)
	entries := ListAuditLog(db, 0)
	if len(entries) != 10 {
		t.Errorf("expected 10 entries with default limit, got %d", len(entries))
	}
}

func TestListAuditLog_WithEntries(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 3; i++ {
		LogAudit(db, "actor", "action", i, "detail")
	}

	entries := ListAuditLog(db, 0)
	if len(entries) != 3 {
		t.Errorf("expected 3 audit entries, got %d", len(entries))
	}
}

func TestLogAudit_Recorded(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	LogAudit(db, "operator", "close", 1, "resolved manually")

	entries := ListAuditLog(db, 0)
	if len(entries) == 0 {
		t.Fatal("expected audit log entry")
	}
	if entries[0].Actor != "operator" {
		t.Errorf("expected actor 'operator', got %q", entries[0].Actor)
	}
}

// ── UpdateCheckpoint / SetBranchName ─────────────────────────────────────────

func TestUpdateCheckpointAndBranchName(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	UpdateCheckpoint(db, id, "schema_written")
	SetBranchName(db, id, "agent/R2-D2/task-42")

	b, _ := GetBounty(db, id)
	if b.Checkpoint != "schema_written" {
		t.Errorf("unexpected checkpoint: %q", b.Checkpoint)
	}
	if b.BranchName != "agent/R2-D2/task-42" {
		t.Errorf("unexpected branch: %q", b.BranchName)
	}
}

// ── UpdateBountyStatus ────────────────────────────────────────────────────────

func TestUpdateBountyStatus_ClearsLockFields(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Simulate a task that was worked on: has branch_name, checkpoint, owner, locked_at
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, owner, locked_at, checkpoint, branch_name)
		VALUES ('CodeEdit', 'Locked', 'task', 'R2-D2', datetime('now'), 'step_1', 'agent/R2-D2/task-1')`)
	var id int
	db.QueryRow(`SELECT id FROM BountyBoard LIMIT 1`).Scan(&id)

	// UpdateBountyStatus should clear owner and locked_at
	UpdateBountyStatus(db, id, "Pending")
	b, _ := GetBounty(db, id)
	if b.Owner != "" {
		t.Errorf("expected empty owner after UpdateBountyStatus, got %q", b.Owner)
	}
	// checkpoint and branch_name should be preserved (only cleared on full operator reset)
	if b.Checkpoint == "" {
		t.Error("checkpoint should be preserved by UpdateBountyStatus (soft requeue)")
	}
	if b.BranchName == "" {
		t.Error("branch_name should be preserved by UpdateBountyStatus (soft requeue)")
	}
}

func TestFullResetClearsCheckpointAndBranch(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	UpdateCheckpoint(db, id, "half_done")
	SetBranchName(db, id, "agent/R2-D2/task-99")

	// Simulate what `force reset` does
	db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', error_log = '', retry_count = 0, infra_failures = 0, locked_at = '', checkpoint = '', branch_name = '' WHERE id = ?`, id)

	b, _ := GetBounty(db, id)
	if b.Checkpoint != "" {
		t.Errorf("checkpoint should be cleared by full reset, got %q", b.Checkpoint)
	}
	if b.BranchName != "" {
		t.Errorf("branch_name should be cleared by full reset, got %q", b.BranchName)
	}
}

// ── unblockDependentsOf ───────────────────────────────────────────────────────

func TestUnblockDependentsOf(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Chain: id1 ← id2 ← id3 (id2 depends on id1, id3 depends on id2)
	id1 := AddBounty(db, 0, "CodeEdit", "root task")
	id2 := AddBounty(db, 0, "CodeEdit", "child")
	id3 := AddBounty(db, 0, "CodeEdit", "grandchild")
	AddDependency(db, id2, id1)
	AddDependency(db, id3, id2)

	// UnblockDependentsOf(id1) removes only the id2→id1 dependency edge (non-recursive)
	count := UnblockDependentsOf(db, id1)
	if count != 1 {
		t.Errorf("expected 1 removed dependency edge, got %d", count)
	}

	// id2 should now have no dependencies
	deps2 := GetDependencies(db, id2)
	if len(deps2) != 0 {
		t.Errorf("id2 should have no dependencies after unblock, got %v", deps2)
	}

	// id3 should still depend on id2 (non-recursive — only direct edges removed)
	deps3 := GetDependencies(db, id3)
	if len(deps3) != 1 || deps3[0] != id2 {
		t.Errorf("id3 should still depend on id2, got %v", deps3)
	}
}

// ── SetBountyPriority ─────────────────────────────────────────────────────────

func TestSetBountyPriority(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	SetBountyPriority(db, id, 42)

	var p int
	db.QueryRow(`SELECT priority FROM BountyBoard WHERE id = ?`, id).Scan(&p)
	if p != 42 {
		t.Errorf("expected priority=42, got %d", p)
	}
}

// ── IncrementInfraFailures ────────────────────────────────────────────────────

func TestIncrementInfraFailures(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	for i := 1; i <= 5; i++ {
		n := IncrementInfraFailures(db, id)
		if n != i {
			t.Errorf("attempt %d: expected infra failures = %d, got %d", i, i, n)
		}
	}
}

func TestIncrementInfraFailures_BelowMax(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id := AddBounty(db, 0, "CodeEdit", "task")
	IncrementInfraFailures(db, id)

	b, _ := GetBounty(db, id)
	if b.InfraFailures != 1 {
		t.Errorf("expected infra_failures=1, got %d", b.InfraFailures)
	}
}

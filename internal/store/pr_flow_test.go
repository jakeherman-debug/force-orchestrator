package store

import (
	"database/sql"
	"strings"
	"testing"
)

// ── Repository PR-flow fields ────────────────────────────────────────────────

func TestAddRepo_DefaultsPRFlowEnabled(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "api", "/tmp/api", "The API service")
	r := GetRepo(db, "api")
	if r == nil {
		t.Fatal("GetRepo returned nil")
	}
	if !r.PRFlowEnabled {
		t.Errorf("new repos must default to pr_flow_enabled=true (opt-out model)")
	}
	if r.RemoteURL != "" || r.DefaultBranch != "" || r.PRTemplatePath != "" {
		t.Errorf("new repo should have empty backfill fields: %+v", r)
	}
}

func TestAddRepo_PreservesPRFlowFieldsOnReAdd(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "api", "/old/path", "old desc")
	if err := SetRepoRemoteInfo(db, "api", "git@github.com:acme/api.git", "main"); err != nil {
		t.Fatal(err)
	}
	if err := SetRepoPRTemplatePath(db, "api", "/old/path/.github/pull_request_template.md"); err != nil {
		t.Fatal(err)
	}
	if err := SetRepoPRFlowEnabled(db, "api", false); err != nil {
		t.Fatal(err)
	}

	// Operator re-adds to update local_path — PR-flow fields must not be clobbered.
	AddRepo(db, "api", "/new/path", "new desc")
	r := GetRepo(db, "api")
	if r.LocalPath != "/new/path" {
		t.Errorf("LocalPath not updated: %q", r.LocalPath)
	}
	if r.Description != "new desc" {
		t.Errorf("Description not updated: %q", r.Description)
	}
	if r.RemoteURL != "git@github.com:acme/api.git" {
		t.Errorf("RemoteURL lost: %q", r.RemoteURL)
	}
	if r.DefaultBranch != "main" {
		t.Errorf("DefaultBranch lost: %q", r.DefaultBranch)
	}
	if r.PRTemplatePath != "/old/path/.github/pull_request_template.md" {
		t.Errorf("PRTemplatePath lost: %q", r.PRTemplatePath)
	}
	if r.PRFlowEnabled {
		t.Errorf("PRFlowEnabled should have stayed disabled after re-add")
	}
}

func TestListRepos_IncludesAllFields(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "a", "/a", "desc-a")
	AddRepo(db, "b", "/b", "desc-b")
	_ = SetRepoRemoteInfo(db, "a", "git@github.com:acme/a.git", "main")
	_ = SetRepoPRFlowEnabled(db, "b", false)

	repos := ListRepos(db)
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
	var a, b Repository
	for _, r := range repos {
		switch r.Name {
		case "a":
			a = r
		case "b":
			b = r
		}
	}
	if a.RemoteURL != "git@github.com:acme/a.git" || !a.PRFlowEnabled {
		t.Errorf("repo a not loaded correctly: %+v", a)
	}
	if b.PRFlowEnabled {
		t.Errorf("repo b should be pr_flow_enabled=false: %+v", b)
	}
}

func TestQuarantine_DisablesPRFlow(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "api", "/tmp/api", "")
	if err := QuarantineRepo(db, "api", "remote unreachable for 3 days"); err != nil {
		t.Fatal(err)
	}
	r := GetRepo(db, "api")
	if r.PRFlowEnabled {
		t.Errorf("quarantine must disable PR flow")
	}
	if r.QuarantinedAt == "" {
		t.Errorf("quarantined_at should be set")
	}
	if !strings.Contains(r.QuarantineReason, "unreachable") {
		t.Errorf("quarantine reason not stored: %q", r.QuarantineReason)
	}

	if err := UnquarantineRepo(db, "api"); err != nil {
		t.Fatal(err)
	}
	r = GetRepo(db, "api")
	if r.QuarantinedAt != "" || r.QuarantineReason != "" {
		t.Errorf("unquarantine should clear fields: %+v", r)
	}
	// Unquarantine does NOT automatically re-enable pr_flow — operator must
	// explicitly re-enable after validation.
	if r.PRFlowEnabled {
		t.Errorf("unquarantine must leave pr_flow_enabled as-is (still false)")
	}
}

// ── Convoy PR-flow fields ────────────────────────────────────────────────────

func TestConvoy_AskBranchAndDraftPRFields(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := CreateConvoy(db, "[1] test")
	if err != nil {
		t.Fatal(err)
	}

	// A freshly-created convoy has no ask-branch.
	c := GetConvoy(db, id)
	if c == nil {
		t.Fatal("GetConvoy returned nil")
	}
	if c.AskBranch != "" || c.AskBranchBaseSHA != "" {
		t.Errorf("new convoy should have empty ask-branch fields: %+v", c)
	}
	if c.Status != "Active" {
		t.Errorf("new convoy status should be Active, got %q", c.Status)
	}

	// Set ask-branch.
	if err := SetConvoyAskBranch(db, id, "force/ask-1-test", "abc123"); err != nil {
		t.Fatalf("SetConvoyAskBranch: %v", err)
	}
	c = GetConvoy(db, id)
	if c.AskBranch != "force/ask-1-test" || c.AskBranchBaseSHA != "abc123" {
		t.Errorf("ask-branch fields not persisted: %+v", c)
	}

	// Empty branch/SHA must error — drift detection depends on both being set.
	if err := SetConvoyAskBranch(db, id, "", "abc"); err == nil {
		t.Errorf("empty branch must be rejected")
	}
	if err := SetConvoyAskBranch(db, id, "branch", ""); err == nil {
		t.Errorf("empty baseSHA must be rejected")
	}

	// Drift detection path: update base SHA after rebase.
	if err := UpdateConvoyAskBranchBaseSHA(db, id, "def456"); err != nil {
		t.Fatal(err)
	}
	c = GetConvoy(db, id)
	if c.AskBranchBaseSHA != "def456" {
		t.Errorf("base SHA not updated: %q", c.AskBranchBaseSHA)
	}

	// Draft PR lifecycle: set → update state → shipped_at stamped on Merged.
	if err := SetConvoyDraftPR(db, id, "https://gh/pull/5", 5, "Open"); err != nil {
		t.Fatal(err)
	}
	c = GetConvoy(db, id)
	if c.DraftPRURL != "https://gh/pull/5" || c.DraftPRNumber != 5 || c.DraftPRState != "Open" {
		t.Errorf("draft PR fields not persisted: %+v", c)
	}
	if c.ShippedAt != "" {
		t.Errorf("shipped_at should not be set while Open")
	}
	if err := UpdateConvoyDraftPRState(db, id, "Merged"); err != nil {
		t.Fatal(err)
	}
	c = GetConvoy(db, id)
	if c.DraftPRState != "Merged" {
		t.Errorf("state not updated: %q", c.DraftPRState)
	}
	if c.ShippedAt == "" {
		t.Errorf("shipped_at must be stamped when state transitions to Merged")
	}
}

func TestActiveConvoysMissingAskBranch(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Active with a CodeEdit task on "api" but no ConvoyAskBranch row — returned.
	a, _ := CreateConvoy(db, "[1] a")
	_, _ = AddConvoyTask(db, 0, "api", "t", a, 0, "Pending")

	// Active WITH a ConvoyAskBranch row for every touched repo — NOT returned.
	b, _ := CreateConvoy(db, "[2] b")
	_, _ = AddConvoyTask(db, 0, "api", "t", b, 0, "Pending")
	_ = UpsertConvoyAskBranch(db, b, "api", "force/ask-2-b", "sha-b")

	// Completed convoy — NOT returned (legacy convoy).
	c, _ := CreateConvoy(db, "[3] c")
	_, _ = AddConvoyTask(db, 0, "api", "t", c, 0, "Completed")
	_ = SetConvoyStatus(db, c, "Completed")

	// Multi-repo Active convoy with ONLY ONE repo branched — must be returned.
	// This is the bug the previous scalar-field-only query missed.
	d, _ := CreateConvoy(db, "[4] d")
	_, _ = AddConvoyTask(db, 0, "api", "t", d, 0, "Pending")
	_, _ = AddConvoyTask(db, 0, "monolith", "t", d, 0, "Pending")
	_ = UpsertConvoyAskBranch(db, d, "api", "force/ask-4-d", "sha-d")
	// monolith has no ConvoyAskBranch row

	ids := ActiveConvoysMissingAskBranch(db)
	gotA, gotD := false, false
	for _, id := range ids {
		if id == a {
			gotA = true
		}
		if id == d {
			gotD = true
		}
		if id == b || id == c {
			t.Errorf("convoy %d should not be returned: %v", id, ids)
		}
	}
	if !gotA {
		t.Errorf("convoy %d (missing ask-branch) must be returned: %v", a, ids)
	}
	if !gotD {
		t.Errorf("convoy %d (multi-repo, partial ask-branches) must be returned: %v", d, ids)
	}
}

// ── Layer A migration idempotence ────────────────────────────────────────────

func TestLayerAMigration_IdempotentOnRerun(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert some data — this must survive re-running createSchema.
	AddRepo(db, "api", "/tmp/api", "")
	_ = SetRepoRemoteInfo(db, "api", "git@github.com:acme/api.git", "main")
	id, _ := CreateConvoy(db, "[1] test")
	_ = SetConvoyAskBranch(db, id, "force/ask-1", "sha")

	// Manually re-run schema creation + migrations — simulates a daemon restart.
	createSchema(db)
	runMigrations(db)

	// All data must still be there.
	r := GetRepo(db, "api")
	if r.RemoteURL != "git@github.com:acme/api.git" {
		t.Errorf("remote URL lost on re-migration: %+v", r)
	}
	c := GetConvoy(db, id)
	if c.AskBranch != "force/ask-1" || c.AskBranchBaseSHA != "sha" {
		t.Errorf("convoy ask-branch lost on re-migration: %+v", c)
	}
}

// TestLayerAMigration_PreMigrationDB simulates upgrading an existing DB that
// predates the PR-flow columns. We manually create old-shape tables, populate
// them, then call createSchema (as InitHolocronDSN does) and verify the ALTER
// TABLE statements add the columns without losing existing data.
func TestLayerAMigration_PreMigrationDB(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Drop the tables and recreate in the old shape (pre-PR-flow).
	db.Exec(`DROP TABLE Repositories`)
	db.Exec(`CREATE TABLE Repositories (name TEXT PRIMARY KEY, local_path TEXT, description TEXT)`)
	db.Exec(`DROP TABLE Convoys`)
	db.Exec(`CREATE TABLE Convoys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		status TEXT DEFAULT 'Active',
		coordinated INTEGER DEFAULT 0,
		created_at TEXT DEFAULT (datetime('now'))
	)`)
	db.Exec(`INSERT INTO Repositories (name, local_path, description) VALUES ('legacy', '/legacy', 'predates migration')`)
	db.Exec(`INSERT INTO Convoys (name, status, coordinated) VALUES ('[1] legacy convoy', 'Active', 1)`)

	// Now run createSchema + runMigrations — simulates daemon startup on new code
	// against old DB (matches what InitHolocronDSN does internally).
	createSchema(db)
	runMigrations(db)

	// Verify columns exist and defaults applied.
	r := GetRepo(db, "legacy")
	if r == nil {
		t.Fatal("legacy repo lost during migration")
	}
	if !r.PRFlowEnabled {
		t.Errorf("pre-existing repo must default to pr_flow_enabled=true after migration")
	}
	if r.RemoteURL != "" {
		t.Errorf("expected empty remote_url for pre-migration repo (Layer B fills it), got %q", r.RemoteURL)
	}

	var convoyID int
	db.QueryRow(`SELECT id FROM Convoys WHERE name = '[1] legacy convoy'`).Scan(&convoyID)
	c := GetConvoy(db, convoyID)
	if c == nil {
		t.Fatal("legacy convoy lost during migration")
	}
	if !c.Coordinated {
		t.Errorf("coordinated flag lost: %+v", c)
	}
	if c.AskBranch != "" {
		t.Errorf("expected empty ask_branch for legacy convoy, got %q", c.AskBranch)
	}
}

// ── AskBranchPRs CRUD ────────────────────────────────────────────────────────

func setupConvoyAndRepo(t *testing.T, db *sql.DB) (taskID, convoyID int, repo string) {
	t.Helper()
	AddRepo(db, "api", "/tmp/api", "")
	cID, err := CreateConvoy(db, "[1] test")
	if err != nil {
		t.Fatal(err)
	}
	tID, err := AddConvoyTask(db, 0, "api", "fix foo", cID, 0, "Pending")
	if err != nil {
		t.Fatal(err)
	}
	return tID, cID, "api"
}

func TestCreateAskBranchPR_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	id, err := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/42", 42)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Errorf("expected non-zero PR row ID")
	}
	p := GetAskBranchPR(db, id)
	if p == nil {
		t.Fatal("GetAskBranchPR returned nil")
	}
	if p.PRNumber != 42 || p.State != "Open" || p.ChecksState != "Pending" || p.FailureCount != 0 {
		t.Errorf("unexpected initial state: %+v", p)
	}
}

func TestCreateAskBranchPR_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	id1, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/42", 42)
	id2, err := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/42", 42)
	if err != nil {
		t.Fatalf("duplicate create should not error: %v", err)
	}
	if id1 != id2 {
		t.Errorf("duplicate create must return existing row ID (got %d vs %d)", id1, id2)
	}
}

func TestCreateAskBranchPR_RejectsInvalidArgs(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := CreateAskBranchPR(db, 0, 1, "r", "", 1); err == nil {
		t.Errorf("expected error for taskID=0")
	}
	if _, err := CreateAskBranchPR(db, 1, 0, "r", "", 1); err == nil {
		t.Errorf("expected error for convoyID=0")
	}
	if _, err := CreateAskBranchPR(db, 1, 1, "", "", 1); err == nil {
		t.Errorf("expected error for empty repo")
	}
	if _, err := CreateAskBranchPR(db, 1, 1, "r", "", 0); err == nil {
		t.Errorf("expected error for prNumber=0")
	}
}

func TestAskBranchPR_ChecksAndFailureCount(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	id, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/1", 1)

	if err := UpdateAskBranchPRChecks(db, id, "Success"); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAskBranchPRChecks(db, id, "Failure"); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAskBranchPRChecks(db, id, "Bogus"); err == nil {
		t.Errorf("expected error for invalid checks_state")
	}

	count, err := IncrementAskBranchPRFailureCount(db, id)
	if err != nil || count != 1 {
		t.Errorf("first increment: got (%d, %v)", count, err)
	}
	count, _ = IncrementAskBranchPRFailureCount(db, id)
	count, _ = IncrementAskBranchPRFailureCount(db, id)
	if count != 3 {
		t.Errorf("third increment should yield 3, got %d", count)
	}
}

func TestAskBranchPR_StateTransitions(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	id, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/1", 1)

	if err := MarkAskBranchPRMerged(db, id); err != nil {
		t.Fatal(err)
	}
	p := GetAskBranchPR(db, id)
	if p.State != "Merged" || p.MergedAt == "" || p.ChecksState != "Success" {
		t.Errorf("merged state not persisted: %+v", p)
	}

	// Create another and close it (externally).
	id2, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/2", 2)
	if err := MarkAskBranchPRClosed(db, id2); err != nil {
		t.Fatal(err)
	}
	p2 := GetAskBranchPR(db, id2)
	if p2.State != "Closed" {
		t.Errorf("closed state not set: %+v", p2)
	}
}

func TestListOpenAskBranchPRs_FiltersByState(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	idOpen, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/1", 1)
	idMerged, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/2", 2)
	idClosed, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/3", 3)
	_ = MarkAskBranchPRMerged(db, idMerged)
	_ = MarkAskBranchPRClosed(db, idClosed)

	open := ListOpenAskBranchPRs(db)
	if len(open) != 1 {
		t.Fatalf("expected 1 open PR, got %d", len(open))
	}
	if open[0].ID != idOpen {
		t.Errorf("wrong PR returned: got %d want %d", open[0].ID, idOpen)
	}
}

func TestRollupAskBranchPRs(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID, convoyID, repo := setupConvoyAndRepo(t, db)
	id1, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/1", 1)
	id2, _ := CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/2", 2)
	_, _ = CreateAskBranchPR(db, taskID, convoyID, repo, "https://gh/pull/3", 3) // id3 stays Pending
	_ = UpdateAskBranchPRChecks(db, id1, "Success")
	_ = UpdateAskBranchPRChecks(db, id2, "Failure")
	_ = MarkAskBranchPRMerged(db, id1)

	r := RollupAskBranchPRs(db, convoyID)
	if r.Total != 3 {
		t.Errorf("total: got %d want 3", r.Total)
	}
	if r.Open != 2 || r.Merged != 1 || r.Closed != 0 {
		t.Errorf("state counts: got %+v", r)
	}
	if r.ChecksSuccess != 1 || r.ChecksFailure != 1 || r.ChecksPending != 1 {
		t.Errorf("checks counts: got %+v", r)
	}
}

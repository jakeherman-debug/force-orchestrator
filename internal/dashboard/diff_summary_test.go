package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

// ── parseDiffStats ────────────────────────────────────────────────────────────

func TestParseDiffStats_Empty(t *testing.T) {
	files := parseDiffStats("")
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestParseDiffStats_SingleFile(t *testing.T) {
	diff := `diff --git a/foo/bar.go b/foo/bar.go
index abc..def 100644
--- a/foo/bar.go
+++ b/foo/bar.go
@@ -1,4 +1,6 @@
 package foo
+
+// NewFunc does something
+func NewFunc() {}
-// OldFunc was removed
-func OldFunc() {}
 var x = 1
`
	files := parseDiffStats(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Path != "foo/bar.go" {
		t.Errorf("path: got %q, want %q", f.Path, "foo/bar.go")
	}
	if f.Additions != 3 {
		t.Errorf("additions: got %d, want 3", f.Additions)
	}
	if f.Deletions != 2 {
		t.Errorf("deletions: got %d, want 2", f.Deletions)
	}
}

func TestParseDiffStats_MultipleFiles(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
+added
 context
-removed
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1,1 +1,1 @@
+new line
-old line
`
	files := parseDiffStats(diff)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0].Path != "a.go" || files[0].Additions != 1 || files[0].Deletions != 1 {
		t.Errorf("a.go: %+v", files[0])
	}
	if files[1].Path != "b.go" || files[1].Additions != 1 || files[1].Deletions != 1 {
		t.Errorf("b.go: %+v", files[1])
	}
}

func TestParseDiffStats_IgnoresPlusPlusPlus(t *testing.T) {
	// The "+++" and "---" header lines must not be counted as additions/deletions.
	diff := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -1 +1 @@
+real addition
`
	files := parseDiffStats(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Additions != 1 {
		t.Errorf("expected 1 addition, got %d", files[0].Additions)
	}
	if files[0].Deletions != 0 {
		t.Errorf("expected 0 deletions, got %d", files[0].Deletions)
	}
}

// ── handleConvoyDiffSummary HTTP handler ──────────────────────────────────────

func TestHandleConvoyDiffSummary_NoAskBranches(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] empty-convoy")

	handler := handleConvoysSubroutes(db)
	req := httptest.NewRequest(http.MethodGet, "/api/convoys/1/diff-summary", nil)
	_ = cid
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ConvoyDiffSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v (body=%s)", err, rec.Body.String())
	}
	if len(resp.AskBranches) != 0 {
		t.Errorf("expected 0 ask-branches, got %d", len(resp.AskBranches))
	}
}

func TestHandleConvoyDiffSummary_WithAskBranch_NoRepoPath(t *testing.T) {
	// Ask-branch exists but the repo has no local path registered — entry is skipped.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] no-repo")
	_ = store.UpsertConvoyAskBranch(db, cid, "unknown-repo", "force/ask-1", "deadbeef")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "unknown-repo", "https://gh/pull/42", 42, "Open")

	handler := handleConvoysSubroutes(db)
	req := httptest.NewRequest(http.MethodGet, "/api/convoys/1/diff-summary", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ConvoyDiffSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// unknown-repo has no local path → skipped
	if len(resp.AskBranches) != 0 {
		t.Errorf("expected 0 ask-branches (unknown repo skipped), got %d", len(resp.AskBranches))
	}
}

func TestHandleConvoyDiffSummary_WithAskBranch_KnownRepo(t *testing.T) {
	// Repo is registered; git diff will return empty for the non-existent branch
	// but the entry should still appear in the response.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/nonexistent/path/myrepo", "https://gh/org/myrepo")
	cid, _ := store.CreateConvoy(db, "[1] known-repo")
	_ = store.UpsertConvoyAskBranch(db, cid, "myrepo", "force/ask-1-known-repo", "deadbeef")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "myrepo", "https://gh/pull/7", 7, "Open")

	handler := handleConvoysSubroutes(db)
	req := httptest.NewRequest(http.MethodGet, "/api/convoys/1/diff-summary", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ConvoyDiffSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v (body=%s)", err, rec.Body.String())
	}
	if len(resp.AskBranches) != 1 {
		t.Fatalf("expected 1 ask-branch, got %d", len(resp.AskBranches))
	}
	ab := resp.AskBranches[0]
	if ab.AskBranch != "force/ask-1-known-repo" {
		t.Errorf("ask_branch: got %q", ab.AskBranch)
	}
	if ab.DraftPRNumber != 7 {
		t.Errorf("draft_pr_number: got %d, want 7", ab.DraftPRNumber)
	}
	if ab.DraftPRURL != "https://gh/pull/7" {
		t.Errorf("draft_pr_url: got %q", ab.DraftPRURL)
	}
	// git diff against /nonexistent path returns empty → no files
	if ab.Files == nil {
		t.Error("files should be non-nil (empty slice), got nil")
	}
	if ab.TotalAdditions != 0 || ab.TotalDeletions != 0 {
		t.Errorf("totals should be 0 for empty diff, got +%d/-%d", ab.TotalAdditions, ab.TotalDeletions)
	}
}

func TestHandleConvoyDiffSummary_MethodNotAllowed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = store.CreateConvoy(db, "[1] x")

	handler := handleConvoysSubroutes(db)
	req := httptest.NewRequest(http.MethodPost, "/api/convoys/1/diff-summary", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

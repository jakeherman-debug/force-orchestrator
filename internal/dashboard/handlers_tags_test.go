// D14 Phase 4 — handler tests for Tags / TagSuggestions / Rules API.
package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── GET /api/tags ─────────────────────────────────────────────────────────────

func TestHandleTags_ListEmpty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	w := httptest.NewRecorder()
	handleTags(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []store.Tag
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty list, got %d items", len(out))
	}
}

func TestHandleTags_ListReturnsAll(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.CreateTag(db, "payments", "payment service", "test"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := store.CreateTag(db, "ml", "machine learning", "test"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	w := httptest.NewRecorder()
	handleTags(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []store.Tag
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 tags, got %d", len(out))
	}
}

// ── POST /api/tags ────────────────────────────────────────────────────────────

func TestHandleTags_CreateTag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	body := `{"name":"payments","description":"payment processing"}`
	r := httptest.NewRequest(http.MethodPost, "/api/tags", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleTags(db)(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the tag exists in the DB.
	tag, err := store.GetTag(db, "payments")
	if err != nil {
		t.Fatalf("GetTag after create: %v", err)
	}
	if tag.Description != "payment processing" {
		t.Errorf("expected description %q, got %q", "payment processing", tag.Description)
	}
}

func TestHandleTags_CreateTag_MissingName(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	body := `{"name":"","description":"no name"}`
	r := httptest.NewRequest(http.MethodPost, "/api/tags", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleTags(db)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ── DELETE /api/tags/{name} ───────────────────────────────────────────────────

func TestHandleTagsSubroutes_Delete(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.CreateTag(db, "old-tag", "", "test"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/tags/old-tag", nil)
	w := httptest.NewRecorder()
	handleTagsSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTagsSubroutes_Delete_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodDelete, "/api/tags/nonexistent", nil)
	w := httptest.NewRecorder()
	handleTagsSubroutes(db)(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ── GET /api/repos/{name}/tags ────────────────────────────────────────────────

func TestHandleReposSubroutes_RepoTags_Get(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a repo and tag.
	db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES ('web', '/tmp/web', 'read_only')`)
	if err := store.CreateTag(db, "payments", "", "test"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := store.AddRepoTag(db, "web", "payments", "test", "operator"); err != nil {
		t.Fatalf("seed repo-tag: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/repos/web/tags", nil)
	w := httptest.NewRecorder()
	handleReposSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []store.RepoTag
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 repo-tag, got %d", len(out))
	}
	if out[0].Tag != "payments" {
		t.Errorf("expected tag=payments, got %q", out[0].Tag)
	}
}

// ── POST /api/repos/{name}/tags ───────────────────────────────────────────────

func TestHandleReposSubroutes_RepoTags_Add(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES ('web', '/tmp/web', 'read_only')`)
	if err := store.CreateTag(db, "ml", "", "test"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	body := `{"tag":"ml","source":"operator"}`
	r := httptest.NewRequest(http.MethodPost, "/api/repos/web/tags", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleReposSubroutes(db)(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	tags, err := store.ListTagsForRepo(db, "web")
	if err != nil {
		t.Fatalf("ListTagsForRepo: %v", err)
	}
	if len(tags) != 1 || tags[0].Tag != "ml" {
		t.Errorf("expected repo-tag ml, got %v", tags)
	}
}

// ── DELETE /api/repos/{name}/tags/{tag} ───────────────────────────────────────

func TestHandleReposSubroutes_RepoTags_Remove(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES ('web', '/tmp/web', 'read_only')`)
	if err := store.CreateTag(db, "ml", "", "test"); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := store.AddRepoTag(db, "web", "ml", "test", "operator"); err != nil {
		t.Fatalf("seed repo-tag: %v", err)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/repos/web/tags/ml", nil)
	w := httptest.NewRecorder()
	handleReposSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	tags, err := store.ListTagsForRepo(db, "web")
	if err != nil {
		t.Fatalf("ListTagsForRepo: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("expected 0 repo-tags after removal, got %d", len(tags))
	}
}

// ── GET /api/tag-suggestions ──────────────────────────────────────────────────

func TestHandleTagSuggestions_ListPending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, err := store.CreateTagSuggestion(db, "web", "payments", "matches pattern", "llm")
	if err != nil {
		t.Fatalf("CreateTagSuggestion: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive suggestion id, got %d", id)
	}
	// Create a second suggestion that we'll resolve so it doesn't appear in pending.
	id2, _ := store.CreateTagSuggestion(db, "web", "ml", "matches pattern", "llm")
	_ = store.ResolveTagSuggestion(db, id2, "dismissed", "test")

	r := httptest.NewRequest(http.MethodGet, "/api/tag-suggestions?status=pending", nil)
	w := httptest.NewRecorder()
	handleTagSuggestions(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []store.TagSuggestion
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 pending suggestion, got %d", len(out))
	}
	if out[0].Status != "pending" {
		t.Errorf("expected status=pending, got %q", out[0].Status)
	}
}

// ── POST /api/tag-suggestions/{id}/accept ─────────────────────────────────────

func TestHandleTagSuggestions_Accept_CreatesRepoTag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES ('web', '/tmp/web', 'read_only')`)
	// Note: tag does NOT need to exist before accept — accept auto-creates it.
	sid, err := store.CreateTagSuggestion(db, "web", "payments", "fits", "llm")
	if err != nil {
		t.Fatalf("CreateTagSuggestion: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tag-suggestions/%d/accept", sid), nil)
	w := httptest.NewRecorder()
	handleTagSuggestions(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Suggestion status should be 'accepted'.
	sug, err := store.ListTagSuggestions(db, "accepted")
	if err != nil {
		t.Fatalf("ListTagSuggestions: %v", err)
	}
	if len(sug) != 1 || sug[0].ID != sid {
		t.Errorf("expected 1 accepted suggestion id=%d, got %v", sid, sug)
	}

	// A RepoTag should have been created.
	tags, err := store.ListTagsForRepo(db, "web")
	if err != nil {
		t.Fatalf("ListTagsForRepo: %v", err)
	}
	if len(tags) != 1 || tags[0].Tag != "payments" {
		t.Errorf("expected repo-tag payments, got %v", tags)
	}
}

// ── POST /api/tag-suggestions/{id}/dismiss ────────────────────────────────────

func TestHandleTagSuggestions_Dismiss(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	sid, err := store.CreateTagSuggestion(db, "web", "ml", "maybe", "llm")
	if err != nil {
		t.Fatalf("CreateTagSuggestion: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tag-suggestions/%d/dismiss", sid), nil)
	w := httptest.NewRecorder()
	handleTagSuggestions(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	dismissed, err := store.ListTagSuggestions(db, "dismissed")
	if err != nil {
		t.Fatalf("ListTagSuggestions: %v", err)
	}
	if len(dismissed) != 1 || dismissed[0].ID != sid {
		t.Errorf("expected 1 dismissed suggestion id=%d, got %v", sid, dismissed)
	}
}

func TestHandleTagSuggestions_Dismiss_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/tag-suggestions/9999/dismiss", nil)
	w := httptest.NewRecorder()
	handleTagSuggestions(db)(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ── GET /api/rules?repo=web ───────────────────────────────────────────────────

func TestHandleRules_ResolvedForRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO Repositories (name, local_path, mode) VALUES ('web', '/tmp/web', 'read_only')`)

	// Insert a global rule (senate:*) and a repo-specific rule.
	db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES ('global-rule', 'security', 'senate:*', 'captain', 'captain', 'content', 'hash1', 1, '', '', 'test')`)
	db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES ('web-only-rule', 'security', 'senate:web', 'captain', 'captain', 'content', 'hash2', 1, '', '', 'test')`)
	db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES ('other-repo-rule', 'security', 'senate:other', 'captain', 'captain', 'content', 'hash3', 1, '', '', 'test')`)

	r := httptest.NewRequest(http.MethodGet, "/api/rules?repo=web", nil)
	w := httptest.NewRecorder()
	handleRules(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []store.FleetRulesRow
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Should get global-rule + web-only-rule, NOT other-repo-rule.
	if len(out) != 2 {
		t.Errorf("expected 2 resolved rules for repo=web, got %d: %v",
			len(out), func() []string {
				names := make([]string, len(out))
				for i, r := range out {
					names[i] = r.RuleKey
				}
				return names
			}())
	}
	for _, rule := range out {
		if rule.RuleKey == "other-repo-rule" {
			t.Errorf("other-repo-rule should NOT appear in repo=web resolution")
		}
	}
}

func TestHandleRules_ListAll(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES ('rule-a', 'arch', 'senate:*', 'captain', 'captain', 'c', 'h1', 1, '', '', 'test')`)
	db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES ('rule-b', 'arch', 'senate:*', 'captain', 'captain', 'c', 'h2', 1, '', 'inactive', 'test')`)

	r := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	w := httptest.NewRecorder()
	handleRules(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []store.FleetRulesRow
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Only rule-a is active (active_until = '').
	if len(out) != 1 || out[0].RuleKey != "rule-a" {
		t.Errorf("expected only rule-a (active), got %v",
			func() []string {
				names := make([]string, len(out))
				for i, r := range out {
					names[i] = r.RuleKey
				}
				return names
			}())
	}
}

// ── POST /api/rules/{key}/upgrade-scope ───────────────────────────────────────

func TestHandleRules_UpgradeScope(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES ('my-rule', 'security', 'senate:web', 'captain', 'captain', 'c', 'h1', 1, '', '', 'test')`)

	body := `{"to_scope":"senate:*"}`
	r := httptest.NewRequest(http.MethodPost, "/api/rules/my-rule/upgrade-scope", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleRules(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the scope was updated.
	var scope string
	db.QueryRow(`SELECT agent_scope FROM FleetRules WHERE rule_key = 'my-rule'`).Scan(&scope)
	if scope != "senate:*" {
		t.Errorf("expected agent_scope=senate:*, got %q", scope)
	}
}

func TestHandleRules_UpgradeScope_InvalidScope(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO FleetRules
		(rule_key, category, agent_scope, render_to, enforced_by, content, content_hash, version, active_from, active_until, created_by)
		VALUES ('my-rule', 'security', 'senate:web', 'captain', 'captain', 'c', 'h1', 1, '', '', 'test')`)

	body := `{"to_scope":"captain:*"}`
	r := httptest.NewRequest(http.MethodPost, "/api/rules/my-rule/upgrade-scope", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleRules(db)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid scope, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRules_UpgradeScope_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	body := `{"to_scope":"senate:*"}`
	r := httptest.NewRequest(http.MethodPost, "/api/rules/nonexistent/upgrade-scope", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleRules(db)(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ── No wildcard CORS on new endpoints ─────────────────────────────────────────

func TestHandleTags_NoWildcardCORS(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	w := httptest.NewRecorder()
	handleTags(db)(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("AUDIT-001/053: handleTags must not set Access-Control-Allow-Origin; got %q", got)
	}
}

func TestHandleTagSuggestions_NoWildcardCORS(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/tag-suggestions", nil)
	w := httptest.NewRecorder()
	handleTagSuggestions(db)(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("AUDIT-001/053: handleTagSuggestions must not set Access-Control-Allow-Origin; got %q", got)
	}
}

func TestHandleRules_NoWildcardCORS(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	w := httptest.NewRecorder()
	handleRules(db)(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("AUDIT-001/053: handleRules must not set Access-Control-Allow-Origin; got %q", got)
	}
}

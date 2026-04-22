package gh

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// stubRunner is an in-memory Runner that returns preconfigured responses.
// Tests should populate the response maps keyed by the joined args string.
type stubRunner struct {
	responses   map[string]stubResponse // key = strings.Join(args, " ")
	calls       []stubCall
	defaultResp stubResponse // returned when no exact match
}

type stubResponse struct {
	stdout string
	stderr string
	err    error
}

type stubCall struct {
	cwd   string
	args  []string
	stdin string
}

func (s *stubRunner) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	s.calls = append(s.calls, stubCall{cwd: cwd, args: append([]string(nil), args...), stdin: string(stdin)})
	if r, ok := s.responses[strings.Join(args, " ")]; ok {
		return []byte(r.stdout), []byte(r.stderr), r.err
	}
	return []byte(s.defaultResp.stdout), []byte(s.defaultResp.stderr), s.defaultResp.err
}

func newStub() *stubRunner {
	return &stubRunner{responses: map[string]stubResponse{}}
}

// ── AuthStatus ───────────────────────────────────────────────────────────────

func TestAuthStatus_Success(t *testing.T) {
	stub := newStub()
	stub.responses["auth status"] = stubResponse{
		stderr: "github.com\n  ✓ Logged in to github.com as testuser\n",
	}
	c := NewClientWithRunner(stub)
	ok, detail, err := c.AuthStatus()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected authenticated, got false")
	}
	if !strings.Contains(detail, "Logged in to github.com") {
		t.Fatalf("detail missing login info: %q", detail)
	}
}

func TestAuthStatus_Failure(t *testing.T) {
	stub := newStub()
	stub.responses["auth status"] = stubResponse{
		stderr: "You are not logged into any GitHub hosts. Run `gh auth login` to authenticate.\n",
		err:    fmt.Errorf("exit status 1"),
	}
	c := NewClientWithRunner(stub)
	ok, detail, err := c.AuthStatus()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected not authenticated, got ok=true")
	}
	if !strings.Contains(detail, "not logged into") {
		t.Fatalf("detail missing hint: %q", detail)
	}
}

// ── PRCreate ────────────────────────────────────────────────────────────────

func TestPRCreate_HappyPath(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: "https://github.com/acme/widgets/pull/42\n"}
	c := NewClientWithRunner(stub)
	res, err := c.PRCreate(PRCreateRequest{
		CWD: "/tmp/widgets", Title: "fix bug", Base: "main", Head: "feature/fix", Body: "closes #1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Number != 42 {
		t.Errorf("expected PR number 42, got %d", res.Number)
	}
	if res.URL != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("unexpected URL: %q", res.URL)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(stub.calls))
	}
	call := stub.calls[0]
	if call.cwd != "/tmp/widgets" {
		t.Errorf("cwd not propagated: %q", call.cwd)
	}
	if call.stdin != "closes #1" {
		t.Errorf("stdin not propagated: %q", call.stdin)
	}
	// --base main, --head feature/fix must be present.
	joined := strings.Join(call.args, " ")
	for _, expected := range []string{"pr create", "--base main", "--head feature/fix", "--body-file -", "--title fix bug"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("args missing %q: %q", expected, joined)
		}
	}
}

func TestPRCreate_DraftFlag(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: "https://github.com/acme/widgets/pull/99"}
	c := NewClientWithRunner(stub)
	_, _ = c.PRCreate(PRCreateRequest{Title: "x", Base: "main", Head: "h", Draft: true})
	if !strings.Contains(strings.Join(stub.calls[0].args, " "), "--draft") {
		t.Errorf("missing --draft flag: %v", stub.calls[0].args)
	}
}

func TestPRCreate_MissingRequiredFields(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if _, err := c.PRCreate(PRCreateRequest{Title: "", Base: "main", Head: "h"}); err == nil {
		t.Errorf("expected error for missing title")
	}
	if _, err := c.PRCreate(PRCreateRequest{Title: "x", Base: "", Head: "h"}); err == nil {
		t.Errorf("expected error for missing base")
	}
	if _, err := c.PRCreate(PRCreateRequest{Title: "x", Base: "main", Head: ""}); err == nil {
		t.Errorf("expected error for missing head")
	}
}

func TestPRCreate_MalformedURL(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: "not-a-url\n"}
	c := NewClientWithRunner(stub)
	if _, err := c.PRCreate(PRCreateRequest{Title: "x", Base: "main", Head: "h"}); err == nil {
		t.Errorf("expected error on malformed URL output")
	}
}

func TestPRCreate_UnderlyingError(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stderr: "Error: PR already exists", err: fmt.Errorf("exit 1")}
	c := NewClientWithRunner(stub)
	_, err := c.PRCreate(PRCreateRequest{Title: "x", Base: "main", Head: "h"})
	if err == nil || !strings.Contains(err.Error(), "PR already exists") {
		t.Errorf("expected stderr in wrapped error, got: %v", err)
	}
}

// ── PRView ─────────────────────────────────────────────────────────────────

func TestPRView_ParsesJSON(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: `{
		"number": 42,
		"url": "https://github.com/acme/widgets/pull/42",
		"state": "OPEN",
		"isDraft": true,
		"merged": false,
		"mergedAt": "",
		"closedAt": "",
		"reviews": [{"author":{"login":"alice"},"state":"APPROVED","submittedAt":"2024-01-01T00:00:00Z","body":"lgtm"}]
	}`}
	c := NewClientWithRunner(stub)
	v, err := c.PRView("", "", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Number != 42 || v.State != "OPEN" || !v.IsDraft {
		t.Errorf("unexpected view: %+v", v)
	}
	if len(v.Reviews) != 1 || v.Reviews[0].Author.Login != "alice" || v.Reviews[0].State != "APPROVED" {
		t.Errorf("review not parsed correctly: %+v", v.Reviews)
	}
}

func TestPRView_MalformedJSON(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: "{not json"}
	c := NewClientWithRunner(stub)
	if _, err := c.PRView("", "", 1); err == nil {
		t.Errorf("expected parse error")
	}
}

// ── PRChecks & rollup ───────────────────────────────────────────────────────

func TestPRChecks_AllSuccess(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: `[
		{"name":"build","state":"SUCCESS","bucket":"pass"},
		{"name":"test","state":"SUCCESS","bucket":"pass"}
	]`}
	c := NewClientWithRunner(stub)
	_, rollup, err := c.PRChecks("", "", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rollup != ChecksSuccess {
		t.Errorf("expected Success, got %s", rollup)
	}
}

func TestPRChecks_OneFailureWinsOverSuccess(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: `[
		{"name":"build","state":"SUCCESS","bucket":"pass"},
		{"name":"lint","state":"FAILURE","bucket":"fail"}
	]`, err: fmt.Errorf("exit 1")}
	c := NewClientWithRunner(stub)
	_, rollup, err := c.PRChecks("", "", 1)
	if err != nil {
		t.Fatalf("gh pr checks returns non-zero on failure; wrapper should still parse: %v", err)
	}
	if rollup != ChecksFailure {
		t.Errorf("expected Failure, got %s", rollup)
	}
}

func TestPRChecks_PendingOverridesSuccess(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: `[
		{"name":"build","state":"SUCCESS","bucket":"pass"},
		{"name":"deploy","state":"IN_PROGRESS","bucket":"pending"}
	]`}
	c := NewClientWithRunner(stub)
	_, rollup, _ := c.PRChecks("", "", 1)
	if rollup != ChecksPending {
		t.Errorf("expected Pending, got %s", rollup)
	}
}

func TestPRChecks_EmptyListReturnsPending(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{stdout: `[]`}
	c := NewClientWithRunner(stub)
	checks, rollup, _ := c.PRChecks("", "", 1)
	if len(checks) != 0 {
		t.Errorf("expected empty checks, got %v", checks)
	}
	if rollup != ChecksPending {
		t.Errorf("empty check list must be Pending (so sub-pr-ci-watch can enforce the missing-CI timeout), got %s", rollup)
	}
}

// ── PRMergeAuto & PRReady & PRMerge ─────────────────────────────────────────

func TestPRMergeAuto_PassesStrategy(t *testing.T) {
	stub := newStub()
	stub.defaultResp = stubResponse{}
	c := NewClientWithRunner(stub)
	if err := c.PRMergeAuto("", "", 42, "squash"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(stub.calls[0].args, " ")
	for _, s := range []string{"pr merge", "42", "--auto", "--squash", "--delete-branch"} {
		if !strings.Contains(joined, s) {
			t.Errorf("args missing %q: %q", s, joined)
		}
	}
}

func TestPRMergeAuto_InvalidStrategy(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if err := c.PRMergeAuto("", "", 1, "bogus"); err == nil {
		t.Errorf("expected error on bogus strategy")
	}
}

func TestPRReady_IssuesCorrectCommand(t *testing.T) {
	stub := newStub()
	c := NewClientWithRunner(stub)
	if err := c.PRReady("", "acme/widgets", 99); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(stub.calls[0].args, " ")
	if !strings.Contains(joined, "pr ready 99") || !strings.Contains(joined, "--repo acme/widgets") {
		t.Errorf("unexpected pr ready args: %q", joined)
	}
}

// ── ClassifyError ───────────────────────────────────────────────────────────

func TestClassifyError_EachBucket(t *testing.T) {
	cases := []struct {
		msg  string
		want ErrorClass
	}{
		{"", ErrClassPermanent},
		{"Bad credentials", ErrClassAuthExpired},
		{"API rate limit exceeded for user", ErrClassRateLimited},
		{"You have exceeded a secondary rate limit", ErrClassRateLimited},
		{"protected branch hook declined", ErrClassBranchProtection},
		{"Required status check \"ci/jenkins\" is expected", ErrClassBranchProtection},
		{"At least 1 approving review is required by reviewers", ErrClassBranchProtection},
		{"permission denied (publickey)", ErrClassPermissionDenied},
		{"403 Forbidden", ErrClassPermissionDenied},
		{"Not Found (HTTP 404)", ErrClassNotFound},
		{"could not resolve host: github.com", ErrClassTransient},
		{"connection refused", ErrClassTransient},
		{"gh timed out after 30s", ErrClassTransient},
		{"server closed idle connection", ErrClassTransient},
		{"something totally unexpected exploded", ErrClassPermanent},
	}
	for _, c := range cases {
		if got := ClassifyError(c.msg); got != c.want {
			t.Errorf("ClassifyError(%q) = %s, want %s", c.msg, got, c.want)
		}
	}
}

func TestErrorClass_ShouldRetry(t *testing.T) {
	if !ErrClassTransient.ShouldRetry() {
		t.Errorf("Transient should retry")
	}
	if !ErrClassRateLimited.ShouldRetry() {
		t.Errorf("RateLimited should retry")
	}
	for _, c := range []ErrorClass{ErrClassAuthExpired, ErrClassPermissionDenied, ErrClassBranchProtection, ErrClassNotFound, ErrClassPermanent} {
		if c.ShouldRetry() {
			t.Errorf("%s should not retry", c)
		}
	}
}

func TestErrorClass_BackoffFor(t *testing.T) {
	if ErrClassTransient.BackoffFor() < time.Second {
		t.Errorf("Transient backoff too small")
	}
	if ErrClassRateLimited.BackoffFor() < 30*time.Second {
		t.Errorf("RateLimited backoff should be generous")
	}
	if ErrClassAuthExpired.BackoffFor() != 0 {
		t.Errorf("AuthExpired should have zero backoff (no retry)")
	}
}

// ── PR comment methods ───────────────────────────────────────────────────────

func TestPRIssueComments_ParsesResponse(t *testing.T) {
	stub := newStub()
	stub.responses["api --paginate repos/acme/api/issues/7/comments"] = stubResponse{
		stdout: `[{"id":1,"body":"hi","user":{"login":"alice","type":"User"},"created_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/acme/api/pull/7#c1"},
			{"id":2,"body":"lgtm","user":{"login":"claude[bot]","type":"Bot"},"created_at":"2026-01-01T00:01:00Z","html_url":"https://github.com/acme/api/pull/7#c2"}]`,
	}
	c := NewClientWithRunner(stub)
	out, err := c.PRIssueComments("", "acme/api", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(out))
	}
	if out[0].User.Login != "alice" || out[1].User.Type != "Bot" {
		t.Errorf("parse mismatch: %+v", out)
	}
}

func TestPRIssueComments_RequiresRepo(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if _, err := c.PRIssueComments("", "", 7); err == nil {
		t.Fatal("expected error when repo is empty")
	}
}

func TestPRReviewComments_ParsesResponse(t *testing.T) {
	stub := newStub()
	stub.responses["api --paginate repos/acme/api/pulls/7/comments"] = stubResponse{
		stdout: `[{"id":100,"node_id":"RC_a","body":"nit: rename this var","path":"main.go","line":42,"diff_hunk":"-x\n+y","user":{"login":"coderabbitai[bot]","type":"Bot"},"pull_request_review_id":55,"created_at":"2026-01-01T00:00:00Z","in_reply_to_id":0},
			{"id":101,"node_id":"RC_b","body":"done","path":"main.go","line":42,"user":{"login":"force-bot","type":"User"},"pull_request_review_id":56,"created_at":"2026-01-01T00:01:00Z","in_reply_to_id":100}]`,
	}
	c := NewClientWithRunner(stub)
	out, err := c.PRReviewComments("", "acme/api", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(out))
	}
	if out[0].NodeID != "RC_a" || out[0].Line != 42 || out[0].Path != "main.go" {
		t.Errorf("first comment parse mismatch: %+v", out[0])
	}
	if out[1].InReplyToID != 100 {
		t.Errorf("second comment should be a reply to 100, got InReplyToID=%d", out[1].InReplyToID)
	}
}

func TestPRReviewComments_RequiresRepo(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if _, err := c.PRReviewComments("", "", 7); err == nil {
		t.Fatal("expected error when repo is empty")
	}
}

func TestPostIssueComment_UsesBodyFile(t *testing.T) {
	stub := newStub()
	c := NewClientWithRunner(stub)
	if err := c.PostIssueComment("/tmp/repo", "acme/api", 7, "hello world"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(stub.calls))
	}
	call := stub.calls[0]
	if call.cwd != "/tmp/repo" {
		t.Errorf("cwd should pass through, got %q", call.cwd)
	}
	joined := strings.Join(call.args, " ")
	if !strings.Contains(joined, "pr comment 7") || !strings.Contains(joined, "--body-file -") || !strings.Contains(joined, "--repo acme/api") {
		t.Errorf("unexpected args: %q", joined)
	}
	if call.stdin != "hello world" {
		t.Errorf("body should be on stdin, got %q", call.stdin)
	}
}

func TestPostIssueComment_EmptyBodyErrors(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if err := c.PostIssueComment("", "acme/api", 7, ""); err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestPostReviewThreadReply_UsesAPIPath(t *testing.T) {
	stub := newStub()
	c := NewClientWithRunner(stub)
	if err := c.PostReviewThreadReply("/tmp/repo", "acme/api", 7, 100, "thanks, addressed in task #42"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(stub.calls))
	}
	joined := strings.Join(stub.calls[0].args, " ")
	if !strings.Contains(joined, "api -X POST") {
		t.Errorf("should use POST: %q", joined)
	}
	if !strings.Contains(joined, "repos/acme/api/pulls/7/comments/100/replies") {
		t.Errorf("should target replies path: %q", joined)
	}
	if !strings.Contains(joined, "body=thanks, addressed in task #42") {
		t.Errorf("body should be passed via -f flag: %q", joined)
	}
}

func TestPostReviewThreadReply_Validation(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if err := c.PostReviewThreadReply("", "", 7, 100, "body"); err == nil {
		t.Error("empty repo should error")
	}
	if err := c.PostReviewThreadReply("", "acme/api", 7, 0, "body"); err == nil {
		t.Error("zero comment ID should error")
	}
	if err := c.PostReviewThreadReply("", "acme/api", 7, 100, ""); err == nil {
		t.Error("empty body should error")
	}
}

func TestFindReviewThreadNodeID_MatchesCommentID(t *testing.T) {
	stub := newStub()
	graphqlArgs := "api graphql -f query=" // partial match — the exact arg includes the full query
	_ = graphqlArgs
	// The stub matches by exact joined args, so we stash the response under a default.
	stub.defaultResp = stubResponse{
		stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
			{"id":"PRRT_aaa","isResolved":false,"comments":{"nodes":[{"databaseId":100},{"databaseId":101}]}},
			{"id":"PRRT_bbb","isResolved":false,"comments":{"nodes":[{"databaseId":200}]}}
		]}}}}}`,
	}
	c := NewClientWithRunner(stub)

	// Comment 101 is in thread PRRT_aaa
	nodeID, err := c.FindReviewThreadNodeID("", "acme/api", 7, 101)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nodeID != "PRRT_aaa" {
		t.Errorf("expected PRRT_aaa, got %q", nodeID)
	}

	// Comment 200 is in PRRT_bbb
	nodeID, _ = c.FindReviewThreadNodeID("", "acme/api", 7, 200)
	if nodeID != "PRRT_bbb" {
		t.Errorf("expected PRRT_bbb, got %q", nodeID)
	}

	// Unknown comment returns "" with no error
	nodeID, err = c.FindReviewThreadNodeID("", "acme/api", 7, 999)
	if err != nil {
		t.Fatalf("unexpected error for unknown comment: %v", err)
	}
	if nodeID != "" {
		t.Errorf("expected empty for unknown comment, got %q", nodeID)
	}
}

func TestFindReviewThreadNodeID_RepoValidation(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if _, err := c.FindReviewThreadNodeID("", "", 7, 100); err == nil {
		t.Error("empty repo should error")
	}
	if _, err := c.FindReviewThreadNodeID("", "not-a-slash-form", 7, 100); err == nil {
		t.Error("non-owner/name repo should error")
	}
}

func TestResolveReviewThread_CallsMutation(t *testing.T) {
	stub := newStub()
	c := NewClientWithRunner(stub)
	if err := c.ResolveReviewThread("/tmp/repo", "PRRT_aaa"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(stub.calls))
	}
	joined := strings.Join(stub.calls[0].args, " ")
	if !strings.Contains(joined, "api graphql") {
		t.Errorf("should use graphql: %q", joined)
	}
	if !strings.Contains(joined, "resolveReviewThread") {
		t.Errorf("should call resolveReviewThread mutation: %q", joined)
	}
	if !strings.Contains(joined, "id=PRRT_aaa") {
		t.Errorf("should pass thread ID: %q", joined)
	}
}

func TestResolveReviewThread_Validation(t *testing.T) {
	c := NewClientWithRunner(newStub())
	if err := c.ResolveReviewThread("", ""); err == nil {
		t.Error("empty threadNodeID should error")
	}
}

// ── IsBotAuthor ──────────────────────────────────────────────────────────────

func TestIsBotAuthor_ByUserType(t *testing.T) {
	if !IsBotAuthor("unknown-login", "Bot", nil) {
		t.Error("userType=Bot should classify as bot regardless of allowlist")
	}
	if IsBotAuthor("alice", "User", nil) {
		t.Error("userType=User with no allowlist match should be human")
	}
}

func TestIsBotAuthor_ByAllowlist(t *testing.T) {
	allowlist := []string{"claude[bot]", "coderabbit[bot]", "CustomBot"}
	if !IsBotAuthor("claude[bot]", "User", allowlist) {
		t.Error("login in allowlist should be bot even when userType=User")
	}
	if !IsBotAuthor("CUSTOMBOT", "", allowlist) {
		t.Error("allowlist match should be case-insensitive")
	}
	if IsBotAuthor("alice", "User", allowlist) {
		t.Error("login not in allowlist should be human")
	}
}

func TestIsBotAuthor_EmptyLogin(t *testing.T) {
	if IsBotAuthor("", "", DefaultBotLogins()) {
		t.Error("empty login with no type should not be bot")
	}
}

func TestDefaultBotLogins_Populated(t *testing.T) {
	logins := DefaultBotLogins()
	if len(logins) == 0 {
		t.Fatal("default allowlist should not be empty")
	}
	// Sanity: make sure the common ones are present.
	want := map[string]bool{"claude[bot]": false, "gemini-code-assist[bot]": false, "coderabbitai[bot]": false}
	for _, l := range logins {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("default allowlist missing %q", k)
		}
	}
}

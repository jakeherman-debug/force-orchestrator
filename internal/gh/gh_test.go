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

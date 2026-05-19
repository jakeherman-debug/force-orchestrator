// Package gh is a typed wrapper around the `gh` CLI. The rest of the codebase
// should use this package instead of shelling out to `gh` directly so tests can
// inject a stub Runner and the error-classification logic stays in one place.
//
// The package is deliberately thin — each Client method runs exactly one `gh`
// invocation and returns typed results. Higher-level policy (retry, backoff,
// quarantine) lives in the agents that call us.
package gh

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// maxGHStdoutBytes caps the size of captured `gh` stdout. A large
// `gh api --paginate repos/.../comments` response (or an adversarial GHE
// response) would otherwise be read entirely into a bytes.Buffer and OOM
// the daemon. 64 MB is large enough for any legitimate comment paginate
// and small enough that we never approach daemon RSS caps.
//
// AUDIT-057 fix. Overflow returns ErrOverflow, which higher layers map
// to store.ErrClassPermanent so the task escalates rather than loops.
const maxGHStdoutBytes = 64 * 1024 * 1024

// ErrOverflow is returned when captured gh stdout exceeds maxGHStdoutBytes.
// Classified as Permanent by ClassifyError (see "gh output exceeded").
var ErrOverflow = errors.New("gh output exceeded 64MiB cap")

// capWriter wraps a *bytes.Buffer and refuses further writes once the
// buffer has accepted maxGHStdoutBytes. Subsequent writes return
// ErrOverflow, which os/exec propagates up as the command's error so
// ExecRunner.Run can distinguish "gh exited non-zero" from "we hit the
// cap and killed it." The bytes written BEFORE the cap are preserved so
// partial-response diagnostics still work.
type capWriter struct {
	buf *bytes.Buffer
	cap int
}

func (cw *capWriter) Write(p []byte) (int, error) {
	remaining := cw.cap - cw.buf.Len()
	if remaining <= 0 {
		return 0, ErrOverflow
	}
	if len(p) > remaining {
		// Write what we can, then return the cap error. os/exec stops
		// copying on the first Write error.
		n, _ := cw.buf.Write(p[:remaining])
		return n, ErrOverflow
	}
	return cw.buf.Write(p)
}

// redactGHError wraps a gh stderr into an error with every secret pattern
// scrubbed. This is the single exfil boundary for gh → returned-error →
// BountyBoard.error_log → dashboard. Every `fmt.Errorf("gh ...: %w: %s",
// err, stderr)` site in this file funnels through here (AUDIT-055).
func redactGHError(prefix string, err error, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	return fmt.Errorf("%s: %w: %s", prefix, err, store.RedactSecrets(msg))
}


// Runner executes a gh CLI command. Production uses ExecRunner; tests install
// a stub via NewTestClient to avoid hitting real GitHub.
//
// args does NOT include the leading "gh" — Runner implementations prepend it.
// The cwd is the working directory for the command; "" means inherit.
// stdin is empty for read-only calls, populated for `gh pr create --body-file -`
// style invocations.
type Runner interface {
	Run(cwd string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// ExecRunner is the production Runner — shells out to the real `gh` binary with
// a bounded timeout. The timeout covers the whole command; individual gh calls
// (e.g. `gh pr view`) should never take more than a few seconds in practice,
// so 30s is conservative.
type ExecRunner struct {
	Timeout time.Duration
}

// Run implements Runner.
//
// stdout is captured into a bounded buffer: once the buffer crosses
// maxGHStdoutBytes (64 MiB) further writes return ErrOverflow, which
// surfaces to the caller as the command's error. The partial stdout up
// to the cap is returned alongside so higher layers can still parse
// whatever prefix landed. Unbounded pagination on `gh api --paginate
// repos/.../comments` would otherwise OOM the daemon — AUDIT-057.
func (e ExecRunner) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cmd := exec.Command("gh", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	// Wrap stdout in a capWriter so we never allocate unbounded memory
	// for a hostile or giant gh response. stderr is small by construction
	// (gh prints one error line); no cap needed there.
	cmd.Stdout = &capWriter{buf: &stdoutBuf, cap: maxGHStdoutBytes}
	cmd.Stderr = &stderrBuf

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start gh: %w", err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// If the cap fired mid-command, prefer that error over the
		// generic exit status so the caller can route to
		// ErrClassPermanent (via ClassifyError's "output exceeded" hit).
		if stdoutBuf.Len() >= maxGHStdoutBytes {
			// Kill the process if it's somehow still alive — defensive;
			// os/exec's copier should have already propagated ErrOverflow.
			return stdoutBuf.Bytes(), stderrBuf.Bytes(), ErrOverflow
		}
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		// AUDIT-092 (Fix #8d): the Wait-after-Kill drain can hang forever
		// if the process is in an uninterruptible syscall (e.g., a kernel
		// NFS operation). A 5s backstop via time.After keeps the caller
		// responsive; a genuinely stuck kernel thread will get reaped
		// later by init, but the daemon goroutine is no longer wedged.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), fmt.Errorf("gh timed out after %v", timeout)
	}
}

// Client is the typed entry point for gh operations. Construct with NewClient
// (production) or NewTestClient (tests).
type Client struct {
	runner Runner
}

// NewClient returns a production Client backed by the real gh binary.
func NewClient() *Client {
	return &Client{runner: ExecRunner{}}
}

// NewClientWithRunner wraps a custom Runner — used by tests to inject stubs
// and by code paths that want a custom timeout.
func NewClientWithRunner(r Runner) *Client {
	return &Client{runner: r}
}

// ── Authentication & environment ─────────────────────────────────────────────

// AuthStatus reports whether `gh auth status` succeeds. The returned detail
// string is the combined output of `gh auth status`, whether or not auth
// succeeded, so callers can surface it to operators in error paths.
func (c *Client) AuthStatus() (authenticated bool, detail string, err error) {
	stdout, stderr, runErr := c.runner.Run("", []string{"auth", "status"}, nil)
	detail = strings.TrimSpace(string(stdout) + "\n" + string(stderr))
	if runErr != nil {
		return false, detail, nil // not authenticated — not a runtime error
	}
	return true, detail, nil
}

// ── Pull request CRUD ────────────────────────────────────────────────────────

// PRCreateRequest bundles the arguments to `gh pr create`.
type PRCreateRequest struct {
	Repo  string // "" = infer from cwd
	CWD   string // working directory (the repo's local path)
	Title string
	Body  string
	Base  string
	Head  string
	Draft bool
}

// PRCreateResult is what we pull out of `gh pr create --json`.
type PRCreateResult struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

// PRCreate opens a pull request and returns the number and URL. Passes the body
// via stdin (--body-file -) so arbitrary Markdown is safe.
func (c *Client) PRCreate(req PRCreateRequest) (*PRCreateResult, error) {
	if req.Title == "" || req.Base == "" || req.Head == "" {
		return nil, fmt.Errorf("PRCreate: title, base, and head are required")
	}
	args := []string{"pr", "create",
		"--title", req.Title,
		"--base", req.Base,
		"--head", req.Head,
		"--body-file", "-",
	}
	if req.Draft {
		args = append(args, "--draft")
	}
	if req.Repo != "" {
		args = append(args, "--repo", req.Repo)
	}
	stdout, stderr, err := c.runner.Run(req.CWD, args, []byte(req.Body))
	if err != nil {
		return nil, redactGHError("gh pr create", err, stderr)
	}
	// `gh pr create` prints the PR URL on stdout. Parse it for the number.
	url := strings.TrimSpace(string(stdout))
	// URL format: https://github.com/owner/repo/pull/123
	idx := strings.LastIndex(url, "/pull/")
	if idx < 0 {
		return nil, fmt.Errorf("gh pr create: unexpected output %q", url)
	}
	var number int
	if _, scanErr := fmt.Sscanf(url[idx+len("/pull/"):], "%d", &number); scanErr != nil {
		return nil, fmt.Errorf("gh pr create: could not parse PR number from %q: %v", url, scanErr)
	}
	return &PRCreateResult{Number: number, URL: url, State: "OPEN"}, nil
}

// PRView is the JSON subset of `gh pr view --json`. Only fields we actually use
// are declared; `gh` will return extras we ignore without error.
//
// Note: `gh pr view --json` does NOT expose a boolean "merged" field (as of
// gh 2.x). We derive Merged from State=="MERGED" after unmarshal so callers
// get a stable bool without needing to know the gh quirk.
type PRView struct {
	Number           int        `json:"number"`
	URL              string     `json:"url"`
	State            string     `json:"state"`            // OPEN | CLOSED | MERGED
	IsDraft          bool       `json:"isDraft"`
	Merged           bool       `json:"-"`                // derived from State — NOT requested from gh
	MergedAt         string     `json:"mergedAt"`
	ClosedAt         string     `json:"closedAt"`
	Reviews          []PRReview `json:"reviews"`
	MergeStateStatus string     `json:"mergeStateStatus"` // CLEAN | BLOCKED | BEHIND | DIRTY | DRAFT | UNKNOWN
	Mergeable        string     `json:"mergeable"`        // MERGEABLE | CONFLICTING | UNKNOWN
}

// PRReview is a single review comment or approval pulled from `gh pr view --json reviews`.
type PRReview struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	State       string `json:"state"` // APPROVED | COMMENTED | CHANGES_REQUESTED
	SubmittedAt string `json:"submittedAt"`
	Body        string `json:"body"`
}

// PRView runs `gh pr view <number> --json ...` and unmarshals the result.
func (c *Client) PRView(cwd, repo string, number int) (*PRView, error) {
	args := []string{"pr", "view", fmt.Sprintf("%d", number),
		"--json", "number,url,state,isDraft,mergedAt,closedAt,reviews,mergeStateStatus,mergeable",
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	stdout, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return nil, redactGHError("gh pr view", err, stderr)
	}
	var v PRView
	if unmarshalErr := json.Unmarshal(stdout, &v); unmarshalErr != nil {
		return nil, fmt.Errorf("gh pr view: parse json: %v (payload=%s)", unmarshalErr,
			strings.TrimSpace(string(stdout)))
	}
	// gh does not expose a `merged` field — derive it from state.
	v.Merged = strings.EqualFold(v.State, "MERGED")
	return &v, nil
}

// PRLabels runs `gh pr view <number> --json labels` and returns just the
// label-name list. Used by the D5.5 release_label_present stage gate,
// which polls for a per-repo release-label regex match across the merged
// PRs in a stage. Kept as a focused method (rather than expanding the
// PRView JSON list) so callers that only need labels don't pay for the
// heavier reviews/mergeStateStatus payload, and so the stub-by-arg-string
// test pattern stays pure (the args slice differs, so the runner key is
// disambiguated from PRView).
func (c *Client) PRLabels(cwd, repo string, number int) ([]string, error) {
	args := []string{"pr", "view", fmt.Sprintf("%d", number), "--json", "labels"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	stdout, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return nil, redactGHError("gh pr view --json labels", err, stderr)
	}
	var payload struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if uErr := json.Unmarshal(stdout, &payload); uErr != nil {
		return nil, fmt.Errorf("gh pr view --json labels: parse json: %v (payload=%s)",
			uErr, strings.TrimSpace(string(stdout)))
	}
	out := make([]string, 0, len(payload.Labels))
	for _, l := range payload.Labels {
		out = append(out, l.Name)
	}
	return out, nil
}

// PRCheck is a single row in `gh pr checks --json`.
type PRCheck struct {
	Name       string `json:"name"`
	State      string `json:"state"`      // SUCCESS | FAILURE | IN_PROGRESS | QUEUED | PENDING | SKIPPED | ...
	Conclusion string `json:"conclusion"` // deprecated alias; sometimes empty
	Bucket     string `json:"bucket"`     // pass | fail | pending | skipping | cancel
	Link       string `json:"link"`
}

// ChecksState summarises a list of check runs into the three-valued state
// checks_state column on AskBranchPRs.
type ChecksState string

const (
	ChecksPending ChecksState = "Pending" // at least one check is still running or none are reported yet
	ChecksSuccess ChecksState = "Success" // every check passed (or was skipped)
	ChecksFailure ChecksState = "Failure" // at least one check failed
)

// PRChecks runs `gh pr checks <n> --json` and returns the raw list plus a rollup.
// The rollup follows the conservative rule: any failure → Failure, any pending →
// Pending, only when every check is Success (or skipped/neutral) do we return
// Success. An empty list returns Pending — "no checks configured yet" is
// indistinguishable from "checks not yet reported" at this layer, and the caller
// (sub-pr-ci-watch) is responsible for the empty-list timeout escalation.
func (c *Client) PRChecks(cwd, repo string, number int) ([]PRCheck, ChecksState, error) {
	args := []string{"pr", "checks", fmt.Sprintf("%d", number),
		"--json", "name,state,bucket,link",
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	stdout, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		// `gh pr checks` exits non-zero in two distinct situations:
		//   (a) one or more checks failed — stderr has check output, stdout is JSON
		//   (b) no checks are configured — stderr is "no checks reported on the '…' branch", stdout is empty
		// For (a), parse the JSON and proceed. For (b), return an empty list so
		// callers can detect the no-CI case via len(checks)==0 without an error.
		stderrStr := strings.TrimSpace(string(stderr))
		if strings.Contains(stderrStr, "no checks reported") {
			return nil, ChecksPending, nil
		}
		var checks []PRCheck
		if parseErr := json.Unmarshal(stdout, &checks); parseErr == nil {
			return checks, rollupChecks(checks), nil
		}
		return nil, ChecksPending, fmt.Errorf("gh pr checks: %w: %s", err, store.RedactSecrets(stderrStr))
	}
	var checks []PRCheck
	if unmarshalErr := json.Unmarshal(stdout, &checks); unmarshalErr != nil {
		return nil, ChecksPending, fmt.Errorf("gh pr checks: parse json: %v (payload=%s)", unmarshalErr,
			strings.TrimSpace(string(stdout)))
	}
	return checks, rollupChecks(checks), nil
}

// rollupChecks implements the ChecksState semantics described on PRChecks.
func rollupChecks(checks []PRCheck) ChecksState {
	if len(checks) == 0 {
		return ChecksPending
	}
	sawPending := false
	for _, c := range checks {
		bucket := strings.ToLower(c.Bucket)
		state := strings.ToUpper(c.State)
		// Failure is decisive.
		if bucket == "fail" || state == "FAILURE" || state == "ERROR" || state == "TIMED_OUT" || state == "ACTION_REQUIRED" {
			return ChecksFailure
		}
		// Pending / in-progress keeps us out of Success but does not override a later failure.
		if bucket == "pending" || bucket == "" && (state == "IN_PROGRESS" || state == "QUEUED" || state == "PENDING" || state == "WAITING") {
			sawPending = true
		}
	}
	if sawPending {
		return ChecksPending
	}
	return ChecksSuccess
}

// WebRepoURL converts a git remote URL into the web URL root for the hosting
// service (e.g. "git@github.com:acme/api.git" → "https://github.com/acme/api").
// Supports GitHub SSH and HTTPS forms; returns "" for file:// and anything
// else we can't confidently convert. Used by the dashboard to surface
// clickable links to branches/PRs without any gh CLI roundtrip.
func WebRepoURL(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}
	// SSH form: git@<host>:owner/repo(.git)
	if strings.HasPrefix(remoteURL, "git@") {
		hostEnd := strings.Index(remoteURL, ":")
		if hostEnd <= 4 {
			return ""
		}
		host := remoteURL[4:hostEnd]
		path := strings.TrimSuffix(remoteURL[hostEnd+1:], ".git")
		if host == "" || path == "" {
			return ""
		}
		return "https://" + host + "/" + path
	}
	// HTTPS/HTTP form: already a web URL, just strip .git suffix.
	if strings.HasPrefix(remoteURL, "https://") || strings.HasPrefix(remoteURL, "http://") {
		return strings.TrimSuffix(remoteURL, ".git")
	}
	return ""
}

// WebBranchURL returns the web URL for a branch on the given remote, e.g.
// "https://github.com/acme/api/tree/force/ask-1-feature". Returns "" when
// the remote URL isn't convertible (file://, unknown host) — callers should
// fall back to plain text in that case.
func WebBranchURL(remoteURL, branch string) string {
	root := WebRepoURL(remoteURL)
	if root == "" || branch == "" {
		return ""
	}
	return root + "/tree/" + branch
}

// PRMergeAuto marks a PR for auto-merge once all required checks pass. Passing
// --auto is what we actually want for sub-PRs: the fleet doesn't need to block
// waiting for CI; GitHub merges for us when it's ready.
func (c *Client) PRMergeAuto(cwd, repo string, number int, strategy string) error {
	if strategy == "" {
		strategy = "squash"
	}
	var strategyFlag string
	switch strategy {
	case "merge":
		strategyFlag = "--merge"
	case "rebase":
		strategyFlag = "--rebase"
	case "squash":
		strategyFlag = "--squash"
	default:
		return fmt.Errorf("PRMergeAuto: invalid strategy %q", strategy)
	}
	args := []string{"pr", "merge", fmt.Sprintf("%d", number), "--auto", strategyFlag, "--delete-branch"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	_, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return redactGHError("gh pr merge --auto", err, stderr)
	}
	return nil
}

// PRReady transitions a draft PR to ready-for-review. Used when the operator
// clicks "Ship it" on a Diplomat-opened draft PR.
func (c *Client) PRReady(cwd, repo string, number int) error {
	args := []string{"pr", "ready", fmt.Sprintf("%d", number)}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	_, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return redactGHError("gh pr ready", err, stderr)
	}
	return nil
}

// PRMerge performs a direct merge (no --auto). Used by the ship-it flow when
// the operator wants to merge immediately rather than wait for required checks.
func (c *Client) PRMerge(cwd, repo string, number int, strategy string) error {
	if strategy == "" {
		strategy = "squash"
	}
	var strategyFlag string
	switch strategy {
	case "merge":
		strategyFlag = "--merge"
	case "rebase":
		strategyFlag = "--rebase"
	case "squash":
		strategyFlag = "--squash"
	default:
		return fmt.Errorf("PRMerge: invalid strategy %q", strategy)
	}
	args := []string{"pr", "merge", fmt.Sprintf("%d", number), strategyFlag, "--delete-branch"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	_, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return redactGHError("gh pr merge", err, stderr)
	}
	return nil
}

// PRClose closes a PR on GitHub without merging.
func (c *Client) PRClose(cwd, repo string, number int) error {
	args := []string{"pr", "close", fmt.Sprintf("%d", number)}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	_, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return redactGHError("gh pr close", err, stderr)
	}
	return nil
}

// ── PR review comments ───────────────────────────────────────────────────────
//
// GitHub has three distinct comment concepts on a PR:
//   1. Issue comments — top-level discussion on the PR (not attached to code).
//      Fetched via /repos/{o}/{r}/issues/{n}/comments and posted with
//      `gh pr comment`.
//   2. Review comments — inline comments on specific code lines, each part of
//      a review thread. Fetched via /repos/{o}/{r}/pulls/{n}/comments. Replies
//      are posted with in_reply_to=<parent_id>.
//   3. Review threads — the GraphQL grouping of related review comments.
//      Required only for resolution (marking a thread Resolved).
//
// The fleet's pr-review-poll dog fetches (1) and (2) on every tick and feeds
// new comments into the PRReviewComments table for triage.

// PRIssueComment is a top-level PR discussion comment (not attached to code).
type PRIssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "User" | "Bot"
	} `json:"user"`
}

// PRReviewComment is an inline comment on a specific line/hunk of a PR diff.
// InReplyToID is populated when the comment is a reply within a review thread.
type PRReviewComment struct {
	ID                  int64  `json:"id"`
	NodeID              string `json:"node_id"`
	InReplyToID         int64  `json:"in_reply_to_id"`
	Body                string `json:"body"`
	Path                string `json:"path"`
	Line                int    `json:"line"`
	Position            int    `json:"position"`
	DiffHunk            string `json:"diff_hunk"`
	PullRequestReviewID int64  `json:"pull_request_review_id"`
	HTMLURL             string `json:"html_url"`
	CreatedAt           string `json:"created_at"`
	User                struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "User" | "Bot"
	} `json:"user"`
}

// PRIssueComments lists PR-level (issue) comments for the given PR number.
// Repo format is "owner/name"; may be empty to let gh infer from cwd.
func (c *Client) PRIssueComments(cwd, repo string, number int) ([]PRIssueComment, error) {
	path := fmt.Sprintf("repos/%s/issues/%d/comments", strings.TrimSpace(repo), number)
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("PRIssueComments: repo required (gh api paths must include owner/name)")
	}
	args := []string{"api", "--paginate", path}
	stdout, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return nil, redactGHError("gh api issue comments", err, stderr)
	}
	var out []PRIssueComment
	if parseErr := json.Unmarshal(stdout, &out); parseErr != nil {
		return nil, fmt.Errorf("gh api issue comments: parse json: %v", parseErr)
	}
	return out, nil
}

// PRReviewComments lists inline review comments for the given PR number.
func (c *Client) PRReviewComments(cwd, repo string, number int) ([]PRReviewComment, error) {
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("PRReviewComments: repo required")
	}
	path := fmt.Sprintf("repos/%s/pulls/%d/comments", strings.TrimSpace(repo), number)
	args := []string{"api", "--paginate", path}
	stdout, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return nil, redactGHError("gh api review comments", err, stderr)
	}
	var out []PRReviewComment
	if parseErr := json.Unmarshal(stdout, &out); parseErr != nil {
		return nil, fmt.Errorf("gh api review comments: parse json: %v", parseErr)
	}
	return out, nil
}

// PostIssueComment posts a top-level comment on a PR (not attached to code).
// Uses `gh pr comment` which is simpler than the REST API for this case.
func (c *Client) PostIssueComment(cwd, repo string, number int, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("PostIssueComment: body required")
	}
	args := []string{"pr", "comment", fmt.Sprintf("%d", number), "--body-file", "-"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	_, stderr, err := c.runner.Run(cwd, args, []byte(body))
	if err != nil {
		return redactGHError("gh pr comment", err, stderr)
	}
	return nil
}

// PostIssueCommentGetID posts a top-level comment on a PR via the GitHub REST
// API and returns the newly created comment's REST ID. Unlike PostIssueComment
// (which uses `gh pr comment` and discards the response), this method switches
// to `gh api -X POST repos/{repo}/issues/{number}/comments` so the returned
// JSON `id` field can be captured and stored in PRHandoffSyntheses.comment_id.
//
// D17 P2B: replaces the PostIssueComment call site in runPRHandoffSynthesis so
// the audit row carries the real comment ID instead of 0.
//
// repo must be in "owner/name" format (e.g. "acme/api"); the call fails if
// repo is empty because gh api paths must include owner/name.
func (c *Client) PostIssueCommentGetID(cwd, repo string, number int, body string) (int64, error) {
	if strings.TrimSpace(repo) == "" {
		return 0, fmt.Errorf("PostIssueCommentGetID: repo required (gh api paths must include owner/name)")
	}
	if strings.TrimSpace(body) == "" {
		return 0, fmt.Errorf("PostIssueCommentGetID: body required")
	}
	path := fmt.Sprintf("repos/%s/issues/%d/comments", strings.TrimSpace(repo), number)
	// Use -f (string field) so special characters in body are not re-interpreted
	// as JSON. This mirrors the PostReviewThreadReply convention.
	args := []string{"api", "-X", "POST", path, "-f", "body=" + body}
	stdout, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return 0, redactGHError("gh api post issue comment", err, stderr)
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if parseErr := json.Unmarshal(stdout, &resp); parseErr != nil {
		return 0, fmt.Errorf("gh api post issue comment: parse json: %v", parseErr)
	}
	if resp.ID <= 0 {
		return 0, fmt.Errorf("gh api post issue comment: returned id=%d (unexpected)", resp.ID)
	}
	return resp.ID, nil
}

// PostReviewThreadReply posts a reply inside an existing review-comment thread.
// inReplyToCommentID is the REST ID of the comment being replied to (GitHub
// places the new comment in the same thread automatically).
func (c *Client) PostReviewThreadReply(cwd, repo string, number int, inReplyToCommentID int64, body string) error {
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("PostReviewThreadReply: repo required")
	}
	if inReplyToCommentID <= 0 {
		return fmt.Errorf("PostReviewThreadReply: inReplyToCommentID required")
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("PostReviewThreadReply: body required")
	}
	path := fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies",
		strings.TrimSpace(repo), number, inReplyToCommentID)
	// `gh api -F body=... -X POST <path>` is the canonical invocation. Use -f
	// (string field) so special characters in body don't get re-interpreted.
	args := []string{"api", "-X", "POST", path, "-f", "body=" + body}
	_, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return redactGHError("gh api reply", err, stderr)
	}
	return nil
}

// ── Review thread resolution (GraphQL) ───────────────────────────────────────
//
// GitHub's REST API doesn't expose review-thread IDs directly; those are a
// GraphQL construct. To resolve a thread by REST comment ID we first look up
// the thread containing that comment, then call the resolveReviewThread
// mutation. Two API calls per resolution — acceptable for the post-Council
// sweep which processes a handful of threads at a time.

// FindReviewThreadNodeID queries the PR's review threads and returns the
// GraphQL node ID of the thread containing the given REST comment databaseId.
// Returns "" (no error) if no thread contains that comment — callers treat
// that as "nothing to resolve".
func (c *Client) FindReviewThreadNodeID(cwd, repo string, number int, commentDatabaseID int64) (string, error) {
	if strings.TrimSpace(repo) == "" {
		return "", fmt.Errorf("FindReviewThreadNodeID: repo required")
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("FindReviewThreadNodeID: repo must be owner/name, got %q", repo)
	}
	owner, name := parts[0], parts[1]

	query := `query($owner:String!,$name:String!,$n:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$n){
      reviewThreads(first:100){
        nodes{ id isResolved comments(first:100){ nodes{ databaseId } } }
      }
    }
  }
}`
	args := []string{
		"api", "graphql",
		"-f", "query=" + query,
		"-F", "owner=" + owner,
		"-F", "name=" + name,
		"-F", fmt.Sprintf("n=%d", number),
	}
	stdout, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return "", redactGHError("gh api graphql (reviewThreads)", err, stderr)
	}
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if parseErr := json.Unmarshal(stdout, &resp); parseErr != nil {
		return "", fmt.Errorf("gh api graphql (reviewThreads): parse: %v", parseErr)
	}
	for _, thread := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		for _, com := range thread.Comments.Nodes {
			if com.DatabaseID == commentDatabaseID {
				return thread.ID, nil
			}
		}
	}
	return "", nil // not found — thread may be on a different PR or deleted
}

// ResolveReviewThread marks a review thread Resolved via the GraphQL mutation.
// threadNodeID is the GraphQL node ID (see FindReviewThreadNodeID). Idempotent:
// resolving an already-resolved thread succeeds.
func (c *Client) ResolveReviewThread(cwd string, threadNodeID string) error {
	if strings.TrimSpace(threadNodeID) == "" {
		return fmt.Errorf("ResolveReviewThread: threadNodeID required")
	}
	mutation := `mutation($id:ID!){ resolveReviewThread(input:{threadId:$id}){ thread{ id isResolved } } }`
	args := []string{
		"api", "graphql",
		"-f", "query=" + mutation,
		"-F", "id=" + threadNodeID,
	}
	_, stderr, err := c.runner.Run(cwd, args, nil)
	if err != nil {
		return redactGHError("gh api graphql (resolveReviewThread)", err, stderr)
	}
	return nil
}

// ── Bot author detection ─────────────────────────────────────────────────────
//
// Two signals:
//   1. GitHub API's user.type == "Bot" (most reliable; set when the author is
//      a GitHub App)
//   2. A configurable login allowlist (catches bots that register as type=User,
//      e.g., CodeRabbit in some configurations)
// IsBotAuthor returns true if either signal indicates bot authorship.
//
// Callers pass the configured allowlist (typically from SystemConfig); tests
// and callers that don't need overrides can pass DefaultBotLogins().

// DefaultBotLogins is the compile-time allowlist of known review-bot logins.
// Extendable at runtime via SystemConfig "pr_review_bot_logins" (CSV).
func DefaultBotLogins() []string {
	return []string{
		"claude[bot]",
		"gemini-code-assist[bot]",
		"coderabbitai[bot]",
		"coderabbit[bot]",
		"github-actions[bot]",
	}
}

// IsBotAuthor classifies a review author as bot or human. `userType` is the
// GitHub API's user.type field ("User"|"Bot"|""); pass "" if unknown.
func IsBotAuthor(login, userType string, allowlist []string) bool {
	if strings.EqualFold(strings.TrimSpace(userType), "Bot") {
		return true
	}
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return false
	}
	for _, allowed := range allowlist {
		if strings.EqualFold(strings.TrimSpace(allowed), login) {
			return true
		}
	}
	return false
}

// ── Error classification ─────────────────────────────────────────────────────

// ErrorClass categorises a gh/git error into one of five buckets, letting the
// Pilot retry wrapper pick a recovery strategy without hardcoding error-string
// checks at every call site.
type ErrorClass string

const (
	ErrClassTransient        ErrorClass = "Transient"        // retry with exponential backoff
	ErrClassRateLimited      ErrorClass = "RateLimited"      // back off longer, still retry
	ErrClassAuthExpired      ErrorClass = "AuthExpired"      // can't self-heal; immediate operator mail
	ErrClassPermissionDenied ErrorClass = "PermissionDenied" // escalate with guidance
	ErrClassBranchProtection ErrorClass = "BranchProtection" // Medic BranchProtection class
	ErrClassNotFound         ErrorClass = "NotFound"         // resource missing — often indicates state drift
	ErrClassPermanent        ErrorClass = "Permanent"        // escalate immediately
)

// ClassifyError buckets a gh/git error message into an ErrorClass. Conservative:
// when in doubt, returns Permanent so the caller escalates rather than looping.
// The classifier looks only at the message string — no network calls, no state
// lookups — so it is safe to call from tests.
func ClassifyError(msg string) ErrorClass {
	m := strings.ToLower(msg)
	switch {
	case m == "":
		return ErrClassPermanent
	// AUDIT-057: gh output exceeded the 64MiB stdout cap. No value in
	// retrying — a larger response will just hit the cap again. Escalate.
	case strings.Contains(m, "gh output exceeded"):
		return ErrClassPermanent
	case strings.Contains(m, "authentication token") && strings.Contains(m, "expired"),
		strings.Contains(m, "bad credentials"),
		strings.Contains(m, "please run: gh auth"),
		strings.Contains(m, "could not resolve to a user"):
		return ErrClassAuthExpired
	case strings.Contains(m, "api rate limit"), strings.Contains(m, "rate limit exceeded"),
		strings.Contains(m, "secondary rate limit"):
		return ErrClassRateLimited
	case strings.Contains(m, "protected branch"), strings.Contains(m, "required status check"),
		strings.Contains(m, "required reviews"), strings.Contains(m, "branch protection"),
		strings.Contains(m, "at least 1 approving review is required"):
		return ErrClassBranchProtection
	case strings.Contains(m, "permission") && (strings.Contains(m, "denied") || strings.Contains(m, "forbidden")),
		strings.Contains(m, "must have push access"),
		strings.Contains(m, "403 forbidden"):
		return ErrClassPermissionDenied
	case strings.Contains(m, "not found"), strings.Contains(m, "404"),
		strings.Contains(m, "could not resolve") && strings.Contains(m, "host"):
		// Note: "could not resolve to a user" is handled above as AuthExpired.
		// This branch catches 404-like responses and DNS-level host lookup failures.
		if strings.Contains(m, "host") {
			return ErrClassTransient
		}
		return ErrClassNotFound
	case strings.Contains(m, "timed out"), strings.Contains(m, "connection refused"),
		strings.Contains(m, "temporary failure"), strings.Contains(m, "eof"),
		strings.Contains(m, "broken pipe"), strings.Contains(m, "server closed"),
		strings.Contains(m, "connection reset"):
		return ErrClassTransient
	}
	return ErrClassPermanent
}

// ShouldRetry reports whether a Pilot-level retry is warranted for this class.
func (c ErrorClass) ShouldRetry() bool {
	switch c {
	case ErrClassTransient, ErrClassRateLimited:
		return true
	}
	return false
}

// BackoffFor returns the initial wait before the next retry for this class.
// Callers should multiply by 2^attempt for exponential backoff.
func (c ErrorClass) BackoffFor() time.Duration {
	switch c {
	case ErrClassTransient:
		return 5 * time.Second
	case ErrClassRateLimited:
		return 60 * time.Second
	}
	return 0
}

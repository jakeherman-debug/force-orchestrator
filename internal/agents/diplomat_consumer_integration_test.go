// D8 Track 3 — ConsumerIntegrationCheck tests.
//
// Coverage matrix (per roadmap §D8-IntegTest "Tests" section + Track 3
// task spec):
//   - TestConsumerIntegCheck_GoReplaceDirective — happy path. Two real
//     Go fixture repos; consumer imports producer's symbol; producer's
//     ask-branch leaves consumer green → status=green; producer's
//     ask-branch breaks the signature → status=red.
//   - TestConsumerIntegCheck_PreExistingRed_DoesNotBlock — consumer is
//     already red on its main; status=pre_existing_red; ship not blocked.
//   - TestConsumerIntegCheck_ReadOnlyConsumer_Skips — consumer mode=
//     read_only; status=skipped_read_only; ship not blocked.
//   - TestConsumerIntegCheck_UnsupportedLang_Skips — consumer has
//     package.json (no go.mod); status=skipped_unsupported_lang; ship
//     not blocked; first encounter mails operator.
//   - TestConsumerIntegCheck_TimeoutBudgetHonored — synthetic slow test
//     command + 1s timeout; status=timeout; ship not blocked.
//   - TestDiplomat_QueuesConsumerIntegCheck_OnDraftPROpen — convoy
//     transitions to DraftPROpen with non-empty blast-radius; assert
//     ConsumerIntegrationCheck tasks queued per consumer.
//   - TestQueueConsumerIntegrationCheck_Idempotent — re-queueing the
//     same (feature, consumer) pair is a no-op.

package agents

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// goFixtureRepo lays out a minimal Go module repo at root, initializes git,
// and returns the absolute path. modulePath becomes the `module ...` line.
// files maps relpath → content. Caller commits the initial state on `main`.
func goFixtureRepo(t *testing.T, root, modulePath string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	// Write go.mod first (so `go ...` commands recognise the module).
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = fmt.Sprintf("module %s\n\ngo 1.21\n", modulePath)
	}
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if dErr := os.MkdirAll(filepath.Dir(full), 0o755); dErr != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), dErr)
		}
		if wErr := os.WriteFile(full, []byte(body), 0o644); wErr != nil {
			t.Fatalf("write %s: %v", rel, wErr)
		}
	}
	runGitInRepo(t, root, "init", "-b", "main")
	runGitInRepo(t, root, "config", "user.email", "test@example.com")
	runGitInRepo(t, root, "config", "user.name", "Test")
	runGitInRepo(t, root, "add", ".")
	runGitInRepo(t, root, "commit", "-m", "initial")
	return root
}

func runGitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
}

// commitChange appends/replaces files in repo and creates a commit on the
// current branch.
func commitChange(t *testing.T, repo string, files map[string]string, msg string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(repo, rel)
		if dErr := os.MkdirAll(filepath.Dir(full), 0o755); dErr != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), dErr)
		}
		if wErr := os.WriteFile(full, []byte(body), 0o644); wErr != nil {
			t.Fatalf("write %s: %v", rel, wErr)
		}
	}
	runGitInRepo(t, repo, "add", ".")
	runGitInRepo(t, repo, "commit", "-m", msg)
}

// createBranch checks out a new branch from current HEAD.
func createBranch(t *testing.T, repo, branch string) {
	t.Helper()
	runGitInRepo(t, repo, "checkout", "-b", branch)
}

// switchBranch checks out an existing branch.
func switchBranch(t *testing.T, repo, branch string) {
	t.Helper()
	runGitInRepo(t, repo, "checkout", branch)
}

// quietLogger discards log output during tests; tests assert on persisted
// state, not log lines.
type quietLogger struct{}

func (quietLogger) Printf(format string, args ...any) {
	_ = log.New(io.Discard, "", 0)
}

// requireGo skips the test if `go` isn't on PATH (CI sandboxes occasionally
// strip toolchains; the test is meaningful only when go is available).
func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH — skipping consumer-integ test")
	}
}

// promoteRepoToWrite flips the repo's mode column to 'write' so the
// integration handler doesn't short-circuit on the read-only gate.
func promoteRepoToWrite(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if err := store.SetRepoMode(db, name, store.ModeWrite, "test@example.com"); err != nil {
		t.Fatalf("SetRepoMode(%s, write): %v", name, err)
	}
}

// seedFeatureRow inserts a Feature row in BountyBoard and returns its ID.
func seedFeatureRow(t *testing.T, db *sql.DB, payload string) int {
	t.Helper()
	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'Feature', 'Completed', ?, 0, datetime('now'))`, payload)
	if err != nil {
		t.Fatalf("seed feature: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

// ─── Test 1: Go replace directive happy path (green + red). ─────────────

func TestConsumerIntegCheck_GoReplaceDirective(t *testing.T) {
	requireGo(t)
	ctx := context.Background()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	tmp := t.TempDir()
	producerPath := filepath.Join(tmp, "producer-repo")
	consumerPath := filepath.Join(tmp, "consumer-repo")

	// Producer exports Greet(name string) string.
	goFixtureRepo(t, producerPath, "example.com/producer", map[string]string{
		"greet/greet.go": `package greet

func Greet(name string) string {
	return "hello " + name
}
`,
	})
	// Producer's main branch tip; create an ask-branch (initially identical).
	createBranch(t, producerPath, "feature/ask-branch")
	switchBranch(t, producerPath, "main")

	// Consumer imports producer's Greet and tests it.
	goFixtureRepo(t, consumerPath, "example.com/consumer", map[string]string{
		"go.mod": `module example.com/consumer

go 1.21

require example.com/producer v0.0.0-00010101000000-000000000000
`,
		"main.go": `package main

import (
	"fmt"

	"example.com/producer/greet"
)

func main() {
	fmt.Println(greet.Greet("world"))
}
`,
		"main_test.go": `package main

import (
	"testing"

	"example.com/producer/greet"
)

func TestGreet(t *testing.T) {
	if got := greet.Greet("x"); got != "hello x" {
		t.Fatalf("got %q want %q", got, "hello x")
	}
}
`,
	})

	store.AddRepo(db, "producer-repo", producerPath, "producer")
	store.AddRepo(db, "consumer-repo", consumerPath, "consumer")
	promoteRepoToWrite(t, db, "producer-repo")
	promoteRepoToWrite(t, db, "consumer-repo")
	if err := store.SetRepoRemoteInfo(db, "producer-repo", "git@github.com:test/producer-repo.git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo producer: %v", err)
	}
	if err := store.SetRepoRemoteInfo(db, "consumer-repo", "git@github.com:test/consumer-repo.git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo consumer: %v", err)
	}

	featureID := seedFeatureRow(t, db, "test feature: bump greet signature")

	// ─── Sub-test A: ask-branch == main → green. ────────────────────────
	t.Run("green_when_askbranch_compatible", func(t *testing.T) {
		// Reset any prior CI rows for this feature.
		_, _ = db.Exec(`DELETE FROM ConsumerIntegrationResults WHERE feature_id = ?`, featureID)

		bountyID := queueConsumerIntegCheckTask(t, db, featureID, 1, "consumer-repo", "producer-repo", "feature/ask-branch")
		bounty, _ := store.GetBounty(db, bountyID)
		runConsumerIntegrationCheck(ctx, db, "diplomat-test", bounty, nil, quietLogger{})

		row := lookupCIResult(t, db, featureID, "consumer-repo")
		if row.Status != store.CIStatusGreen {
			t.Fatalf("status: got %s want %s; stdout=%s stderr=%s",
				row.Status, store.CIStatusGreen, row.StdoutTail, row.StderrTail)
		}
		if row.ExitCode != 0 {
			t.Errorf("exit_code: got %d want 0", row.ExitCode)
		}
		// Aggregation: not blocking.
		blocking, repos, _ := store.FeatureHasBlockingConsumerBreakage(db, featureID)
		if blocking || len(repos) != 0 {
			t.Errorf("expected non-blocking aggregation; got blocking=%v repos=%v", blocking, repos)
		}
	})

	// ─── Sub-test B: ask-branch breaks the signature → red. ─────────────
	t.Run("red_when_askbranch_breaks_signature", func(t *testing.T) {
		_, _ = db.Exec(`DELETE FROM ConsumerIntegrationResults WHERE feature_id = ?`, featureID)

		// Mutate ask-branch: rename Greet → Greeting (consumer's
		// `greet.Greet` no longer compiles).
		switchBranch(t, producerPath, "feature/ask-branch")
		commitChange(t, producerPath, map[string]string{
			"greet/greet.go": `package greet

func Greeting(name string) string {
	return "hello " + name
}
`,
		}, "rename Greet → Greeting (breaking)")
		switchBranch(t, producerPath, "main")

		bountyID := queueConsumerIntegCheckTask(t, db, featureID, 1, "consumer-repo", "producer-repo", "feature/ask-branch")
		bounty, _ := store.GetBounty(db, bountyID)
		runConsumerIntegrationCheck(ctx, db, "diplomat-test", bounty, nil, quietLogger{})

		row := lookupCIResult(t, db, featureID, "consumer-repo")
		if row.Status != store.CIStatusRed {
			t.Fatalf("status: got %s want %s; stdout=%s stderr=%s",
				row.Status, store.CIStatusRed, row.StdoutTail, row.StderrTail)
		}
		if row.ExitCode == 0 {
			t.Errorf("exit_code: got 0 want non-zero")
		}
		// Aggregation: blocking.
		blocking, repos, _ := store.FeatureHasBlockingConsumerBreakage(db, featureID)
		if !blocking {
			t.Errorf("expected blocking aggregation; got blocking=false")
		}
		if len(repos) != 1 || repos[0] != "consumer-repo" {
			t.Errorf("blocking_repos: got %v want [consumer-repo]", repos)
		}
		// [CONSUMER BREAKAGE] mail should have landed.
		var mailCount int
		_ = db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '[CONSUMER BREAKAGE]%' AND to_agent='operator'`).Scan(&mailCount)
		if mailCount == 0 {
			t.Errorf("expected [CONSUMER BREAKAGE] operator mail to fire on red status")
		}
	})
}

// ─── Test 2: pre-existing-red baseline → not blocking. ──────────────────

func TestConsumerIntegCheck_PreExistingRed_DoesNotBlock(t *testing.T) {
	requireGo(t)
	ctx := context.Background()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	tmp := t.TempDir()
	producerPath := filepath.Join(tmp, "producer-repo")
	consumerPath := filepath.Join(tmp, "consumer-repo")

	goFixtureRepo(t, producerPath, "example.com/producer", map[string]string{
		"greet/greet.go": `package greet

func Greet(name string) string {
	return "hello " + name
}
`,
	})
	createBranch(t, producerPath, "feature/ask-branch")
	switchBranch(t, producerPath, "main")

	// Consumer's main is ALREADY broken: test asserts a value the impl
	// doesn't produce.
	goFixtureRepo(t, consumerPath, "example.com/consumer", map[string]string{
		"go.mod": `module example.com/consumer

go 1.21

require example.com/producer v0.0.0-00010101000000-000000000000
`,
		"main.go": `package main

import (
	"fmt"

	"example.com/producer/greet"
)

func main() {
	fmt.Println(greet.Greet("world"))
}
`,
		"main_test.go": `package main

import (
	"testing"

	"example.com/producer/greet"
)

func TestGreetBroken(t *testing.T) {
	if got := greet.Greet("x"); got != "this is wrong" {
		t.Fatalf("got %q want %q", got, "this is wrong")
	}
}
`,
	})

	store.AddRepo(db, "producer-repo", producerPath, "producer")
	store.AddRepo(db, "consumer-repo", consumerPath, "consumer")
	promoteRepoToWrite(t, db, "producer-repo")
	promoteRepoToWrite(t, db, "consumer-repo")
	if err := store.SetRepoRemoteInfo(db, "consumer-repo", "git@github.com:test/consumer-repo.git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo: %v", err)
	}
	if err := store.SetRepoRemoteInfo(db, "producer-repo", "git@github.com:test/producer-repo.git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo: %v", err)
	}

	featureID := seedFeatureRow(t, db, "feature whose change is fine but consumer is already broken")
	bountyID := queueConsumerIntegCheckTask(t, db, featureID, 1, "consumer-repo", "producer-repo", "feature/ask-branch")
	bounty, _ := store.GetBounty(db, bountyID)
	runConsumerIntegrationCheck(ctx, db, "diplomat-test", bounty, nil, quietLogger{})

	row := lookupCIResult(t, db, featureID, "consumer-repo")
	if row.Status != store.CIStatusPreExistingRed {
		t.Fatalf("status: got %s want %s; stdout=%s stderr=%s",
			row.Status, store.CIStatusPreExistingRed, row.StdoutTail, row.StderrTail)
	}
	blocking, _, _ := store.FeatureHasBlockingConsumerBreakage(db, featureID)
	if blocking {
		t.Errorf("expected non-blocking aggregation on pre_existing_red; got blocking=true")
	}
}

// ─── Test 3: read-only consumer → skipped. ──────────────────────────────

func TestConsumerIntegCheck_ReadOnlyConsumer_Skips(t *testing.T) {
	ctx := context.Background()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	tmp := t.TempDir()
	consumerPath := filepath.Join(tmp, "consumer-repo")
	if err := os.MkdirAll(consumerPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store.AddRepo(db, "consumer-repo", consumerPath, "consumer")
	store.AddRepo(db, "producer-repo", t.TempDir(), "producer")
	// Consumer stays in default mode='read_only'; producer must be writable
	// to satisfy the producer-side worktree-add… but we should never get there.
	promoteRepoToWrite(t, db, "producer-repo")

	featureID := seedFeatureRow(t, db, "feature whose consumer is read_only")
	bountyID := queueConsumerIntegCheckTask(t, db, featureID, 1, "consumer-repo", "producer-repo", "feature/ask-branch")
	bounty, _ := store.GetBounty(db, bountyID)
	runConsumerIntegrationCheck(ctx, db, "diplomat-test", bounty, nil, quietLogger{})

	row := lookupCIResult(t, db, featureID, "consumer-repo")
	if row.Status != store.CIStatusSkippedReadOnly {
		t.Fatalf("status: got %s want %s", row.Status, store.CIStatusSkippedReadOnly)
	}
	blocking, _, _ := store.FeatureHasBlockingConsumerBreakage(db, featureID)
	if blocking {
		t.Errorf("expected non-blocking on skipped_read_only; got blocking=true")
	}
}

// ─── Test 4: unsupported language → skipped + first-encounter mail. ─────

func TestConsumerIntegCheck_UnsupportedLang_Skips(t *testing.T) {
	ctx := context.Background()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	tmp := t.TempDir()
	// Node consumer: package.json present, no go.mod.
	consumerPath := filepath.Join(tmp, "node-consumer")
	if err := os.MkdirAll(consumerPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(consumerPath, "package.json"),
		[]byte(`{"name":"node-consumer","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	store.AddRepo(db, "node-consumer", consumerPath, "node consumer")
	store.AddRepo(db, "producer-repo", t.TempDir(), "producer")
	promoteRepoToWrite(t, db, "node-consumer")
	promoteRepoToWrite(t, db, "producer-repo")

	featureID := seedFeatureRow(t, db, "feature with node consumer")
	bountyID := queueConsumerIntegCheckTask(t, db, featureID, 1, "node-consumer", "producer-repo", "feature/ask-branch")
	bounty, _ := store.GetBounty(db, bountyID)
	runConsumerIntegrationCheck(ctx, db, "diplomat-test", bounty, nil, quietLogger{})

	row := lookupCIResult(t, db, featureID, "node-consumer")
	if row.Status != store.CIStatusSkippedUnsupportedLang {
		t.Fatalf("status: got %s want %s", row.Status, store.CIStatusSkippedUnsupportedLang)
	}
	// Aggregation: not blocking.
	blocking, _, _ := store.FeatureHasBlockingConsumerBreakage(db, featureID)
	if blocking {
		t.Errorf("expected non-blocking on skipped_unsupported_lang")
	}
	// First-encounter operator mail fired.
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail
		WHERE to_agent='operator' AND subject LIKE '[CONSUMER CHECK SKIPPED: unsupported lang%'`).Scan(&n)
	if n == 0 {
		t.Errorf("expected first-encounter operator mail for unsupported lang")
	}
	// Dedup flag set.
	if v := store.GetConfig(db, "consumer_integ_lang_alerted_node", ""); v != "1" {
		t.Errorf("expected consumer_integ_lang_alerted_node=1; got %q", v)
	}
	// Second encounter (different consumer, same lang): NO new mail.
	prevMailCount := n
	consumerPath2 := filepath.Join(tmp, "node-consumer-2")
	if err := os.MkdirAll(consumerPath2, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(consumerPath2, "package.json"),
		[]byte(`{"name":"node-consumer-2","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	store.AddRepo(db, "node-consumer-2", consumerPath2, "second node consumer")
	promoteRepoToWrite(t, db, "node-consumer-2")
	bountyID2 := queueConsumerIntegCheckTask(t, db, featureID, 1, "node-consumer-2", "producer-repo", "feature/ask-branch")
	bounty2, _ := store.GetBounty(db, bountyID2)
	runConsumerIntegrationCheck(ctx, db, "diplomat-test", bounty2, nil, quietLogger{})
	var n2 int
	_ = db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail
		WHERE to_agent='operator' AND subject LIKE '[CONSUMER CHECK SKIPPED: unsupported lang%'`).Scan(&n2)
	if n2 != prevMailCount {
		t.Errorf("expected dedup on second node-consumer encounter; got %d new mail(s)", n2-prevMailCount)
	}
}

// ─── Test 5: timeout budget. ───────────────────────────────────────────

func TestConsumerIntegCheck_TimeoutBudgetHonored(t *testing.T) {
	requireGo(t)
	ctx := context.Background()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	tmp := t.TempDir()
	producerPath := filepath.Join(tmp, "producer-repo")
	consumerPath := filepath.Join(tmp, "consumer-repo")

	goFixtureRepo(t, producerPath, "example.com/producer", map[string]string{
		"greet/greet.go": `package greet

func Greet(name string) string { return "hello " + name }
`,
	})
	createBranch(t, producerPath, "feature/ask-branch")
	switchBranch(t, producerPath, "main")

	// Consumer's test sleeps for 30 seconds; we configure timeout to 1
	// minute total but use a slow `sleep 30` baseline test command so we
	// hit the bound. Actually we use a much longer sleep + shorter
	// timeout for predictability.
	goFixtureRepo(t, consumerPath, "example.com/consumer", map[string]string{
		"go.mod": `module example.com/consumer

go 1.21

require example.com/producer v0.0.0-00010101000000-000000000000
`,
		// main.go ensures `go build ./...` finds a package to build (an
		// empty repo with only test files exits non-zero on build, which
		// would land as pre_existing_red and hide the timeout signal).
		"main.go": `package main

import (
	"fmt"

	"example.com/producer/greet"
)

func main() {
	fmt.Println(greet.Greet("world"))
}
`,
		"main_test.go": `package main

import (
	"testing"
)

func TestNothing(t *testing.T) {
	// placeholder; the override test command is what we care about
	_ = t
}
`,
	})

	store.AddRepo(db, "producer-repo", producerPath, "producer")
	store.AddRepo(db, "consumer-repo", consumerPath, "consumer")
	promoteRepoToWrite(t, db, "producer-repo")
	promoteRepoToWrite(t, db, "consumer-repo")
	if err := store.SetRepoRemoteInfo(db, "consumer-repo", "git@github.com:test/c.git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo: %v", err)
	}
	if err := store.SetRepoRemoteInfo(db, "producer-repo", "git@github.com:test/p.git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo: %v", err)
	}

	// Fractional minutes are accepted so the test exercises the timeout
	// path without burning a full minute of wall time. 0.05 min = 3s.
	store.SetConfig(db, "consumer_integ_timeout_minutes", "0.05")
	// Override the test command for the consumer with a literal sleep
	// that always exceeds the 3-second budget.
	store.SetConfig(db, "consumer_integ_test_command:consumer-repo", "sleep 30")

	featureID := seedFeatureRow(t, db, "feature whose consumer test is too slow")
	bountyID := queueConsumerIntegCheckTask(t, db, featureID, 1, "consumer-repo", "producer-repo", "feature/ask-branch")
	bounty, _ := store.GetBounty(db, bountyID)

	// Run the handler with a hard outer deadline so the test wall-time
	// is bounded even if the timeout logic ever regresses.
	outerCtx, outerCancel := context.WithTimeout(ctx, 90*time.Second)
	defer outerCancel()
	runConsumerIntegrationCheck(outerCtx, db, "diplomat-test", bounty, nil, quietLogger{})

	row := lookupCIResult(t, db, featureID, "consumer-repo")
	if row.Status != store.CIStatusTimeout {
		t.Fatalf("status: got %s want %s; stdout=%s stderr=%s",
			row.Status, store.CIStatusTimeout, row.StdoutTail, row.StderrTail)
	}
	blocking, _, _ := store.FeatureHasBlockingConsumerBreakage(db, featureID)
	if blocking {
		t.Errorf("expected non-blocking on timeout per roadmap line 2216; got blocking=true")
	}
}

// ─── Test 6: dispatcher queues per consumer on DraftPROpen. ─────────────

func TestDiplomat_QueuesConsumerIntegCheck_OnDraftPROpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a Feature, a convoy, an ask-branch on producer-repo, and a
	// blast-radius listing two consumer repos.
	featureID := seedFeatureRow(t, db, "feature with two consumers")
	convoyID, err := store.CreateConvoy(db, "convoy-d8t3-dispatch")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	// Seed a CodeEdit task linking the convoy to the Feature (so
	// lookupConvoyFeatureID can find it).
	if _, err := store.AddConvoyTask(db, featureID, "producer-repo",
		"do thing", convoyID, 0, "Pending"); err != nil {
		t.Fatalf("AddConvoyTask: %v", err)
	}
	store.AddRepo(db, "producer-repo", t.TempDir(), "producer")
	store.AddRepo(db, "consumer-a", t.TempDir(), "consumer a")
	store.AddRepo(db, "consumer-b", t.TempDir(), "consumer b")
	if err := store.SetRepoRemoteInfo(db, "producer-repo", "git@github.com:t/p.git", "main"); err != nil {
		t.Fatalf("SetRepoRemoteInfo: %v", err)
	}
	if err := store.SetConvoyAskBranch(db, convoyID, "feature/ask-branch", "deadbeef"); err != nil {
		t.Fatalf("SetConvoyAskBranch: %v", err)
	}
	// Per-(convoy, repo) ask-branch row.
	if err := store.UpsertConvoyAskBranch(db, convoyID, "producer-repo",
		"feature/ask-branch", "deadbeef"); err != nil {
		t.Fatalf("UpsertConvoyAskBranch: %v", err)
	}

	rec := store.BlastRadiusRecord{
		ModifiedSymbols:       []store.BlastRadiusSymbol{{SymbolPath: "greet.Greet", Kind: "function", FilePath: "greet/greet.go", LineNumber: 1}},
		AffectedConsumerRepos: []string{"consumer-a", "consumer-b"},
		AutoIncludedTasks:     []int{},
	}
	if err := store.SetFeatureBlastRadius(db, featureID, rec); err != nil {
		t.Fatalf("SetFeatureBlastRadius: %v", err)
	}

	branches := store.ListConvoyAskBranches(db, convoyID)
	if len(branches) == 0 {
		t.Fatalf("expected at least one ConvoyAskBranch row")
	}
	queued, dErr := DispatchConsumerIntegrationChecks(db, convoyID, branches, quietLogger{})
	if dErr != nil {
		t.Fatalf("DispatchConsumerIntegrationChecks: %v", dErr)
	}
	if queued != 2 {
		t.Errorf("queued: got %d want 2", queued)
	}

	// Both ConsumerIntegrationCheck tasks landed in BountyBoard with
	// convoy_id stamped (per CLAUDE.md "convoy-scoped queries use convoy_id").
	rows, _ := db.Query(`SELECT id, target_repo, payload FROM BountyBoard
		WHERE type = 'ConsumerIntegrationCheck' AND convoy_id = ?
		ORDER BY id ASC`, convoyID)
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var id int
		var repo, payload string
		if err := rows.Scan(&id, &repo, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen = append(seen, repo)
		if !strings.Contains(payload, fmt.Sprintf(`"feature_id":%d`, featureID)) {
			t.Errorf("payload missing feature_id=%d: %s", featureID, payload)
		}
		if !strings.Contains(payload, `"producer_ask_branch":"feature/ask-branch"`) {
			t.Errorf("payload missing producer_ask_branch: %s", payload)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 ConsumerIntegrationCheck tasks; got %d (%v)", len(seen), seen)
	}

	// Idempotence: re-dispatching does not double-queue.
	queued2, _ := DispatchConsumerIntegrationChecks(db, convoyID, branches, quietLogger{})
	if queued2 != 0 {
		t.Errorf("re-dispatch queued: got %d want 0 (idempotency)", queued2)
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────

func queueConsumerIntegCheckTask(t *testing.T, db *sql.DB, featureID, convoyID int, consumer, producer, askBranch string) int {
	t.Helper()
	id, err := QueueConsumerIntegrationCheck(db, featureID, convoyID, consumer, producer, askBranch)
	if err != nil {
		t.Fatalf("QueueConsumerIntegrationCheck: %v", err)
	}
	if id == 0 {
		t.Fatalf("QueueConsumerIntegrationCheck returned 0 (already-existed?) — was a previous result row set?")
	}
	return id
}

func lookupCIResult(t *testing.T, db *sql.DB, featureID int, consumer string) store.ConsumerIntegrationResult {
	t.Helper()
	rows, err := store.ListConsumerIntegrationResultsByFeature(db, featureID)
	if err != nil {
		t.Fatalf("ListConsumerIntegrationResultsByFeature: %v", err)
	}
	for _, r := range rows {
		if r.ConsumerRepoName == consumer {
			return r
		}
	}
	t.Fatalf("no ConsumerIntegrationResults row for feature=%d consumer=%s", featureID, consumer)
	return store.ConsumerIntegrationResult{}
}

// TestQueueConsumerIntegrationCheck_Idempotent asserts the
// HasConsumerIntegrationResult gate at the queue layer prevents a re-queue
// from ever creating a second task once a result exists.
func TestQueueConsumerIntegrationCheck_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	featureID := seedFeatureRow(t, db, "x")
	store.AddRepo(db, "producer", t.TempDir(), "")
	store.AddRepo(db, "consumer", t.TempDir(), "")

	// First insert lands a task.
	id1, err := QueueConsumerIntegrationCheck(db, featureID, 1, "consumer", "producer", "feat/ask")
	if err != nil {
		t.Fatalf("first queue: %v", err)
	}
	if id1 == 0 {
		t.Fatalf("first queue returned 0 — expected a fresh task")
	}

	// Second insert while the first is still Pending → idempotency_key
	// dedup → returns 0.
	id2, err := QueueConsumerIntegrationCheck(db, featureID, 1, "consumer", "producer", "feat/ask")
	if err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if id2 != 0 {
		t.Errorf("second queue: got id=%d want 0 (already-pending dedup)", id2)
	}

	// Now write a result row and re-queue: the HasConsumerIntegrationResult
	// gate should also short-circuit even with no live task.
	if _, err := store.UpsertConsumerIntegrationResult(db, store.ConsumerIntegrationResult{
		FeatureID:        featureID,
		ConsumerRepoName: "consumer",
		Status:           store.CIStatusGreen,
		RanAt:            store.NowSQLite(),
	}); err != nil {
		t.Fatalf("upsert result: %v", err)
	}
	// Mark the first task Completed so it stops blocking the idempotency_key
	// dedup; then prove the result-gate alone short-circuits.
	if err := store.UpdateBountyStatus(db, id1, "Completed"); err != nil {
		t.Fatalf("complete first task: %v", err)
	}
	id3, err := QueueConsumerIntegrationCheck(db, featureID, 1, "consumer", "producer", "feat/ask")
	if err != nil {
		t.Fatalf("third queue: %v", err)
	}
	if id3 != 0 {
		t.Errorf("post-result queue: got id=%d want 0 (HasConsumerIntegrationResult gate)", id3)
	}
}

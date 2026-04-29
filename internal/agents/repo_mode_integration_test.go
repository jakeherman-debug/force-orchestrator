package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// TestAstromechClaimSkipsReadOnlyRepo seeds two repos — one read_only and one
// write — with a Pending CodeEdit task each, then calls the astromech's
// claim helper directly. The claim must surface only the write-mode repo's
// task; the read-only task stays Pending.
func TestAstromechClaimSkipsReadOnlyRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "writable", "/tmp/writable", "")
	if err := store.SetRepoMode(db, "writable", store.ModeWrite, "test"); err != nil {
		t.Fatalf("SetRepoMode write: %v", err)
	}

	store.AddRepo(db, "frozen", "/tmp/frozen", "")
	// frozen stays at default mode=read_only.

	// One Pending CodeEdit task per repo.
	rwID, _ := store.AddConvoyTask(db, 0, "writable", "do work", 0, 0, "Pending")
	roID, _ := store.AddConvoyTask(db, 0, "frozen", "do work", 0, 0, "Pending")

	claimed, ok := store.ClaimBountyForWrite(db, "CodeEdit", "test-astromech")
	if !ok {
		t.Fatalf("ClaimBountyForWrite returned no claim — expected the writable repo's task")
	}
	if claimed.ID != rwID {
		t.Fatalf("claim picked wrong task: got id=%d, want id=%d (writable repo)", claimed.ID, rwID)
	}

	// Second claim must miss — the only remaining Pending task is on a
	// read_only repo.
	if _, ok := store.ClaimBountyForWrite(db, "CodeEdit", "test-astromech-2"); ok {
		t.Fatalf("ClaimBountyForWrite returned a second claim — read_only repo should be filtered out")
	}

	// Sanity: the read_only task is still Pending.
	roBounty, _ := store.GetBounty(db, roID)
	if roBounty.Status != "Pending" {
		t.Errorf("read_only repo's task moved to %q; expected Pending", roBounty.Status)
	}

	// Promoting the read_only repo to write should now make the task claimable.
	if err := store.SetRepoMode(db, "frozen", store.ModeWrite, "test"); err != nil {
		t.Fatalf("SetRepoMode promote: %v", err)
	}
	claimed2, ok := store.ClaimBountyForWrite(db, "CodeEdit", "test-astromech-3")
	if !ok {
		t.Fatalf("after promotion, ClaimBountyForWrite returned no claim")
	}
	if claimed2.ID != roID {
		t.Fatalf("after promotion, claim picked wrong task: got id=%d, want id=%d", claimed2.ID, roID)
	}
}

// TestDestructiveGitOpRejectedOnReadOnlyRepo verifies that every named
// destructive git op refuses with ErrRepoNotWritable when the target repo
// is in mode='read_only'. The repoPath/branch are bogus on purpose — the
// guard MUST fire before any git invocation.
func TestDestructiveGitOpRejectedOnReadOnlyRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "frozen", "/tmp/frozen-no-such-path", "")
	// frozen stays at default mode=read_only.

	ctx := context.Background()

	t.Run("ForcePushBranch", func(t *testing.T) {
		err := igit.ForcePushBranch(ctx, db, "frozen", "/tmp/frozen-no-such-path", "force/ask-1-x")
		if err == nil {
			t.Fatalf("ForcePushBranch should refuse against read_only repo, got nil")
		}
		if !errors.Is(err, store.ErrRepoNotWritable) {
			t.Fatalf("expected ErrRepoNotWritable, got: %v", err)
		}
	})

	t.Run("TriggerCIRerun", func(t *testing.T) {
		err := igit.TriggerCIRerun(ctx, db, "frozen", "/tmp/frozen-no-such-path", "force/ask-1-x", "msg")
		if err == nil {
			t.Fatalf("TriggerCIRerun should refuse against read_only repo, got nil")
		}
		if !errors.Is(err, store.ErrRepoNotWritable) {
			t.Fatalf("expected ErrRepoNotWritable, got: %v", err)
		}
	})

	t.Run("DeleteAskBranch", func(t *testing.T) {
		err := igit.DeleteAskBranch(ctx, db, "frozen", "/tmp/frozen-no-such-path", "force/ask-1-x")
		if err == nil {
			t.Fatalf("DeleteAskBranch should refuse against read_only repo, got nil")
		}
		if !errors.Is(err, store.ErrRepoNotWritable) {
			t.Fatalf("expected ErrRepoNotWritable, got: %v", err)
		}
	})

	t.Run("MergeAndCleanup", func(t *testing.T) {
		err := igit.MergeAndCleanup(ctx, db, "frozen", "/tmp/frozen-no-such-path", "feature-branch", "/tmp/wt")
		if err == nil {
			t.Fatalf("MergeAndCleanup should refuse against read_only repo, got nil")
		}
		if !errors.Is(err, store.ErrRepoNotWritable) {
			t.Fatalf("expected ErrRepoNotWritable, got: %v", err)
		}
	})

	t.Run("AssertRepoWritable_direct", func(t *testing.T) {
		err := igit.AssertRepoWritable(db, "frozen")
		if err == nil {
			t.Fatalf("AssertRepoWritable on read_only repo returned nil")
		}
		if !errors.Is(err, store.ErrRepoNotWritable) {
			t.Fatalf("expected ErrRepoNotWritable, got: %v", err)
		}
	})

	// And confirm: once promoted, the guard returns nil (function-level
	// check; the underlying git ops still fail on the bogus path, but
	// the protected-branch guard is the first thing to catch them).
	t.Run("AssertRepoWritable_afterPromotion", func(t *testing.T) {
		if err := store.SetRepoMode(db, "frozen", store.ModeWrite, "test"); err != nil {
			t.Fatalf("SetRepoMode promote: %v", err)
		}
		if err := igit.AssertRepoWritable(db, "frozen"); err != nil {
			t.Fatalf("after promotion AssertRepoWritable should pass, got: %v", err)
		}
	})
}

// TestDestructiveGitOpRejectedOnQuarantinedRepo is the equivalent for
// mode='quarantined' — quarantined behaves like read_only for the
// destructive-op guard.
func TestDestructiveGitOpRejectedOnQuarantinedRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "broken", "/tmp/broken-no-such-path", "")
	if err := store.SetRepoMode(db, "broken", store.ModeQuarantined, "test"); err != nil {
		t.Fatalf("SetRepoMode quarantined: %v", err)
	}

	err := igit.ForcePushBranch(context.Background(), db, "broken", "/tmp/broken-no-such-path", "force/ask-1-x")
	if err == nil {
		t.Fatalf("ForcePushBranch should refuse against quarantined repo, got nil")
	}
	if !errors.Is(err, store.ErrRepoNotWritable) {
		t.Fatalf("expected ErrRepoNotWritable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "quarantined") {
		t.Fatalf("error should mention quarantined mode, got: %v", err)
	}
}

// TestQuarantineEmitsMailAndBlocksClaims wires the full path: a
// quarantined repo (a) does not produce astromech claims, and (b)
// triggers a [QUARANTINED REPO] operator-mail when the
// quarantined-repo-watch dog runs. The dog dedupes per repo per day.
func TestQuarantineEmitsMailAndBlocksClaims(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Two repos — one writable for the control case, one quarantined.
	store.AddRepo(db, "active", "/tmp/active", "")
	if err := store.SetRepoMode(db, "active", store.ModeWrite, "test"); err != nil {
		t.Fatalf("SetRepoMode active write: %v", err)
	}
	store.AddRepo(db, "frozen", "/tmp/frozen", "")
	if err := store.SetRepoMode(db, "frozen", store.ModeQuarantined, "test"); err != nil {
		t.Fatalf("SetRepoMode frozen quarantined: %v", err)
	}
	// Stamp a quarantine_reason directly so the mail body has something
	// meaningful to render.
	_ = store.QuarantineRepo(db, "frozen", "remote unreachable for >48h")

	// Two Pending CodeEdit tasks, one per repo.
	frozenTaskID, _ := store.AddConvoyTask(db, 0, "frozen", "do thing", 0, 0, "Pending")
	activeTaskID, _ := store.AddConvoyTask(db, 0, "active", "do thing", 0, 0, "Pending")

	// (a) Astromech claim must skip the quarantined task entirely.
	claimed, ok := store.ClaimBountyForWrite(db, "CodeEdit", "test-astromech")
	if !ok {
		t.Fatalf("expected a claim from the active repo, got none")
	}
	if claimed.ID != activeTaskID {
		t.Fatalf("claim picked wrong task: got id=%d, want id=%d (active repo)", claimed.ID, activeTaskID)
	}
	if _, ok := store.ClaimBountyForWrite(db, "CodeEdit", "test-astromech-2"); ok {
		t.Fatalf("second claim leaked through despite frozen task being on a quarantined repo")
	}
	frozen, _ := store.GetBounty(db, frozenTaskID)
	if frozen.Status != "Pending" {
		t.Errorf("frozen task status moved to %q; expected Pending", frozen.Status)
	}

	// (b) The dog emits operator mail.
	if err := dogQuarantinedRepoWatch(db, testLogger{}); err != nil {
		t.Fatalf("dogQuarantinedRepoWatch: %v", err)
	}

	rows, err := db.Query(`SELECT subject, body FROM Fleet_Mail
		WHERE to_agent = 'operator' AND subject LIKE '[QUARANTINED REPO]%'
		ORDER BY id`)
	if err != nil {
		t.Fatalf("query mail: %v", err)
	}
	defer rows.Close()
	type mailRow struct {
		subject string
		body    string
	}
	var mails []mailRow
	for rows.Next() {
		var m mailRow
		if err := rows.Scan(&m.subject, &m.body); err != nil {
			t.Fatalf("scan: %v", err)
		}
		mails = append(mails, m)
	}
	if len(mails) != 1 {
		t.Fatalf("expected exactly 1 [QUARANTINED REPO] mail, got %d: %+v", len(mails), mails)
	}
	if !strings.Contains(mails[0].subject, "frozen") {
		t.Errorf("mail subject should name the frozen repo: %q", mails[0].subject)
	}
	if !strings.Contains(mails[0].body, "remote unreachable") {
		t.Errorf("mail body should include the quarantine reason: %q", mails[0].body)
	}

	// Re-run: same-day dedupe should suppress.
	if err := dogQuarantinedRepoWatch(db, testLogger{}); err != nil {
		t.Fatalf("dogQuarantinedRepoWatch second tick: %v", err)
	}
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE to_agent = 'operator' AND subject LIKE '[QUARANTINED REPO]%'`).Scan(&count)
	if count != 1 {
		t.Errorf("dedupe failed: second tick should not re-mail, got count=%d", count)
	}
}

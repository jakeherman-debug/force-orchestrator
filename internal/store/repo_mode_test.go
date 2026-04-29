package store

import (
	"errors"
	"strings"
	"testing"
)

// TestNewRepoDefaultsToReadOnly verifies the D2 T1-4 invariant that AddRepo
// stamps mode='read_only' on every newly-added repo. Operators must
// explicitly promote to write via SetRepoMode (audit-logged).
func TestNewRepoDefaultsToReadOnly(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "freshly-added", "/tmp/freshly-added", "test repo")

	mode, err := GetRepoMode(db, "freshly-added")
	if err != nil {
		t.Fatalf("GetRepoMode after AddRepo: %v", err)
	}
	if mode != ModeReadOnly {
		t.Fatalf("AddRepo should default new repo to read_only, got %q", mode)
	}
}

// TestSetRepoMode_AuditLogged verifies every mode change writes an
// AuditLog row with the operator's email as actor and a detail string
// recording prior + new mode. No silent mode change.
func TestSetRepoMode_AuditLogged(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "audited", "/tmp/audited", "test repo")

	if err := SetRepoMode(db, "audited", ModeWrite, "ops@example.com"); err != nil {
		t.Fatalf("SetRepoMode → write: %v", err)
	}

	mode, err := GetRepoMode(db, "audited")
	if err != nil {
		t.Fatalf("GetRepoMode after SetRepoMode: %v", err)
	}
	if mode != ModeWrite {
		t.Fatalf("expected mode=write after promotion, got %q", mode)
	}

	entries := ListAuditLog(db, 50)
	if len(entries) == 0 {
		t.Fatalf("AuditLog has no entries after SetRepoMode")
	}
	var found bool
	for _, e := range entries {
		if e.Action == "repo.set_mode" && e.Actor == "ops@example.com" {
			if !strings.Contains(e.Detail, "repo=audited") {
				t.Errorf("audit detail missing repo name: %q", e.Detail)
			}
			if !strings.Contains(e.Detail, "prior_mode=read_only") {
				t.Errorf("audit detail missing prior_mode=read_only: %q", e.Detail)
			}
			if !strings.Contains(e.Detail, "new_mode=write") {
				t.Errorf("audit detail missing new_mode=write: %q", e.Detail)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("AuditLog has no repo.set_mode entry from ops@example.com — entries=%+v", entries)
	}
}

// TestSetRepoMode_RejectsInvalidMode confirms the validator rejects modes
// outside the three-value enum. The CHECK constraint catches it at the DB
// layer too, but we want the typed helper to fail first with a clear
// message rather than dropping into an opaque "constraint failed" sql
// error.
func TestSetRepoMode_RejectsInvalidMode(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "any", "/tmp/any", "")

	err := SetRepoMode(db, "any", RepoMode("bogus"), "ops@example.com")
	if err == nil {
		t.Fatalf("SetRepoMode with invalid mode must error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid mode") {
		t.Fatalf("expected invalid-mode error, got: %v", err)
	}
}

// TestSetRepoMode_RejectsUnknownRepo confirms ErrRepoNotFound surfaces
// rather than silently no-op'ing.
func TestSetRepoMode_RejectsUnknownRepo(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	err := SetRepoMode(db, "ghost", ModeWrite, "ops@example.com")
	if err == nil {
		t.Fatalf("SetRepoMode against unknown repo must error, got nil")
	}
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("expected ErrRepoNotFound, got: %v", err)
	}
}

// TestRepoMode_QuarantineRoundtrip confirms the third value works end-to-end.
func TestRepoMode_QuarantineRoundtrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "broken", "/tmp/broken", "")

	if err := SetRepoMode(db, "broken", ModeQuarantined, "ops@example.com"); err != nil {
		t.Fatalf("SetRepoMode → quarantined: %v", err)
	}
	mode, err := GetRepoMode(db, "broken")
	if err != nil {
		t.Fatalf("GetRepoMode after quarantine: %v", err)
	}
	if mode != ModeQuarantined {
		t.Fatalf("expected quarantined, got %q", mode)
	}

	// Restore.
	if err := SetRepoMode(db, "broken", ModeWrite, "ops@example.com"); err != nil {
		t.Fatalf("SetRepoMode → write (restore): %v", err)
	}
	mode, _ = GetRepoMode(db, "broken")
	if mode != ModeWrite {
		t.Fatalf("expected write after restore, got %q", mode)
	}
}

// TestGetRepoMode_NotFound covers the ErrRepoNotFound path for the reader.
func TestGetRepoMode_NotFound(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := GetRepoMode(db, "ghost")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("expected ErrRepoNotFound, got: %v", err)
	}
}

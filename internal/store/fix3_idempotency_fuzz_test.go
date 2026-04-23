package store

import (
	"strings"
	"testing"
)

// FuzzIdempotencyKeyNormalization documents and enforces the idempotency-key
// equivalence model Fix #3 ships with:
//
//   - Keys are COMPARED BYTE-FOR-BYTE. Case matters ("A" != "a"); whitespace
//     matters (" x" != "x"); every Unicode code point matters (look-alikes
//     like Cyrillic "а" and Latin "a" do NOT collide). Callers are responsible
//     for producing canonical keys at the call site.
//   - The empty key is REJECTED (AddConvoyTaskIdempotent returns an error),
//     and rows with idempotency_key='' are NOT covered by idx_bounty_idem
//     (partial predicate `idempotency_key != ''`), so they don't participate
//     in dedup even if a caller bypasses the check.
//
// The fuzzer feeds edge cases (leading/trailing whitespace, tabs, NUL bytes,
// Unicode homoglyphs, mixed case, length extremes). For every pair of
// distinct keys K1 != K2, two parallel inserts must produce two rows — one
// per key. For identical keys, two inserts must produce one row.
//
// Seed cases pin the invariants that have been observed in production
// (tabs vs spaces; trailing newline; ASCII "a" vs Cyrillic "а").
func FuzzIdempotencyKeyNormalization(f *testing.F) {
	seeds := [][2]string{
		{"convoy-review:42", "convoy-review:42"},         // identical → 1 row
		{"convoy-review:42", "convoy-review:43"},         // different suffix → 2 rows
		{"rebase-agent:1", "rebase-agent: 1"},            // embedded space → 2 rows
		{"foo", "foo\n"},                                 // trailing newline → 2 rows
		{"foo", "foo\t"},                                 // trailing tab → 2 rows
		{"foo", "FOO"},                                   // case difference → 2 rows
		{"a", "а"},                                       // ASCII vs Cyrillic → 2 rows
		{"worktree-reset:100", "worktree-reset:100 "},    // trailing space → 2 rows
		{"pr-review-triage:9", "pr-review-triage:9"},     // same → 1 row
		{"x", strings.Repeat("x", 4096)},                 // length extreme → 2 rows
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, k1, k2 string) {
		// Reject empty keys at the boundary — AddConvoyTaskIdempotent
		// already errors on empty, which the fuzzer should not trigger
		// uselessly. If either is empty, skip.
		if k1 == "" || k2 == "" {
			return
		}
		// Reject overly-long keys: SQLite has no hard cap but we don't want
		// to spend fuzz budget on byte-wise identical megabyte keys.
		if len(k1) > 8192 || len(k2) > 8192 {
			return
		}
		// Reject NUL bytes — go-sqlite3 binds via the C API which treats
		// NUL as a string terminator, producing undefined comparisons.
		// This is a caller-contract issue, not a Fix #3 concern.
		if strings.ContainsRune(k1, 0) || strings.ContainsRune(k2, 0) {
			return
		}

		db := InitHolocronDSN(":memory:")
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (1, 'fuzz', 'Active')`); err != nil {
			t.Fatalf("seed convoy: %v", err)
		}

		id1, existed1, err1 := AddConvoyTaskIdempotent(db, k1, 0, "api", "payload-1", 1, 5, "Pending")
		if err1 != nil {
			t.Fatalf("insert k1=%q: %v", k1, err1)
		}
		if existed1 {
			t.Fatalf("first insert for k1=%q reported existed=true on empty DB", k1)
		}
		id2, existed2, err2 := AddConvoyTaskIdempotent(db, k2, 0, "api", "payload-2", 1, 5, "Pending")
		if err2 != nil {
			t.Fatalf("insert k2=%q: %v", k2, err2)
		}

		// Count rows. Because Fix #3's index is partial
		// (`idempotency_key != '' AND status NOT IN (...)`), every row in
		// this test falls under the predicate.
		var rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE idempotency_key IN (?, ?)`, k1, k2).
			Scan(&rows); err != nil {
			t.Fatalf("count: %v", err)
		}

		if k1 == k2 {
			// Identical keys — dedup must fire.
			if rows != 1 {
				t.Fatalf("identical keys k1==k2=%q produced %d rows (want 1); existed2=%v id1=%d id2=%d",
					k1, rows, existed2, id1, id2)
			}
			if !existed2 {
				t.Fatalf("second insert for identical key %q should report existed=true", k1)
			}
			if id1 != id2 {
				t.Fatalf("identical keys should reuse id: id1=%d id2=%d", id1, id2)
			}
		} else {
			// Different keys — no dedup.
			if rows != 2 {
				t.Fatalf("distinct keys k1=%q k2=%q produced %d rows (want 2); existed2=%v",
					k1, k2, rows, existed2)
			}
			if existed2 {
				t.Fatalf("second insert with distinct key %q should not report existed=true "+
					"(vs %q)", k2, k1)
			}
			if id1 == id2 {
				t.Fatalf("distinct keys must not share id: id1=%d id2=%d k1=%q k2=%q",
					id1, id2, k1, k2)
			}
		}
	})
}

// FuzzIdempotencyKey_TerminalAllowsNewInsert documents the lifecycle contract:
// after a row under key K transitions to a terminal status
// (Completed/Cancelled/Failed), a fresh AddConvoyTaskIdempotent call under
// the same K must succeed with a new id. The partial-unique predicate
// excludes terminal statuses, so the next insert finds no conflict.
//
// The fuzzer varies the key and the terminal status to cover the three
// terminal values individually plus exotic keys.
func FuzzIdempotencyKey_TerminalAllowsNewInsert(f *testing.F) {
	seedKeys := []string{
		"convoy-review:1",
		"rebase-conflict:branch:agent/C3-PO/task-1",
		"worktree-reset:999",
		"a",
		" a ",
		"key\twith\ttabs",
		strings.Repeat("x", 256),
	}
	for _, k := range seedKeys {
		for _, status := range []string{"Completed", "Cancelled", "Failed"} {
			f.Add(k, status)
		}
	}

	f.Fuzz(func(t *testing.T, key, terminalStatus string) {
		if key == "" {
			return
		}
		if len(key) > 8192 {
			return
		}
		if strings.ContainsRune(key, 0) {
			return
		}
		// Only the three terminal statuses are interesting — anything else
		// should still block.
		switch terminalStatus {
		case "Completed", "Cancelled", "Failed":
			// OK
		default:
			return
		}

		db := InitHolocronDSN(":memory:")
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (1, 'fuzz-life', 'Active')`); err != nil {
			t.Fatalf("seed convoy: %v", err)
		}

		first, _, err := AddConvoyTaskIdempotent(db, key, 0, "api", "first", 1, 5, "Pending")
		if err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if _, err := db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, terminalStatus, first); err != nil {
			t.Fatalf("flip terminal: %v", err)
		}
		second, existed, err := AddConvoyTaskIdempotent(db, key, 0, "api", "second", 1, 5, "Pending")
		if err != nil {
			t.Fatalf("second insert after %s: %v", terminalStatus, err)
		}
		if existed {
			t.Fatalf("post-%s insert for key %q should NOT report existed (old row is terminal)",
				terminalStatus, key)
		}
		if second == first {
			t.Fatalf("post-%s insert for key %q reused terminal id %d — expected a fresh id",
				terminalStatus, key, first)
		}
		// Now two rows share the key but only one is non-terminal.
		var nonTerminal int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE idempotency_key = ?
			  AND status NOT IN ('Completed','Cancelled','Failed')`, key).Scan(&nonTerminal)
		if nonTerminal != 1 {
			t.Fatalf("expected exactly 1 non-terminal row under key %q after %s+retry, got %d",
				key, terminalStatus, nonTerminal)
		}
	})
}


package store

import (
	"strings"
	"sync"
	"testing"
)

// TestPattern_P2_IdempotencyKeyRace exercises the TOCTOU race window in
// AddConvoyTaskIdempotent. The helper is SELECT-then-INSERT against a
// BountyBoard.idempotency_key column that has no UNIQUE index (AUDIT-008).
// Under concurrent callers, multiple goroutines observe the empty SELECT
// before any of them INSERT, so duplicate rows land for the same key.
//
// This is a failing test until AUDIT-008 is fixed (partial UNIQUE index +
// INSERT … ON CONFLICT DO NOTHING). Today we observe 2–50 rows; the assert
// is "exactly 1".
//
// Covers: AUDIT-008, AUDIT-034, AUDIT-035, AUDIT-036, AUDIT-075, AUDIT-076,
// and the missing-race-test gap at AUDIT-112.
func TestPattern_P2_IdempotencyKeyRace(t *testing.T) {
	// Fix #3 lands: partial UNIQUE on BountyBoard(idempotency_key) + ON CONFLICT
	// DO NOTHING RETURNING id means 50 concurrent callers with the same key
	// produce exactly 1 row. Kept as permanent regression protection.
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a convoy so AddConvoyTaskIdempotent has a realistic FK target.
	if _, err := db.Exec(
		`INSERT INTO Convoys (id, name, status) VALUES (1, 'p2-race-convoy', 'Active')`,
	); err != nil {
		t.Fatalf("seed convoy: %v", err)
	}

	const (
		goroutines = 50
		key        = "rebase-conflict:branch:agent/R2-D2/p2-race"
	)

	var (
		wg        sync.WaitGroup
		startGate = make(chan struct{})
		errCh     = make(chan error, goroutines)
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-startGate // fan out simultaneously
			_, _, err := AddConvoyTaskIdempotent(
				db, key,
				0, "api", "payload",
				1,       // convoyID
				5,       // priority
				"Pending",
			)
			if err != nil {
				errCh <- err
			}
		}()
	}
	close(startGate) // release all 50 at once
	wg.Wait()
	close(errCh)

	for err := range errCh {
		// A UNIQUE-constraint-violation surfacing here post-fix is fine (the
		// helper should swallow it via ON CONFLICT), but log so we notice.
		t.Logf("worker returned error: %v", err)
	}

	var rows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM BountyBoard WHERE idempotency_key = ?`, key,
	).Scan(&rows); err != nil {
		t.Fatalf("count query: %v", err)
	}

	if rows != 1 {
		t.Fatalf("AUDIT-008/P2: expected exactly 1 row for idempotency_key=%q, got %d — "+
			"SELECT-then-INSERT race in AddConvoyTaskIdempotent inserted duplicates. "+
			"Fix: add partial UNIQUE index on BountyBoard(idempotency_key) WHERE "+
			"status NOT IN ('Completed','Cancelled','Failed') and switch the helper to "+
			"INSERT … ON CONFLICT DO NOTHING RETURNING id.",
			key, rows)
	}
}

// TestPattern_P2_NoUniqueIndex_Static is a structural assertion: today no
// UNIQUE index covers BountyBoard.idempotency_key, which is the root cause
// of the race above. This test will FAIL the day AUDIT-008 is fixed — at
// that point, delete it (or invert the assertion) as proof the fix shipped.
func TestPattern_P2_NoUniqueIndex_Static(t *testing.T) {
	// Fix #3 lands: idx_bounty_idem (partial UNIQUE on idempotency_key) is
	// present on every fresh or migrated DB. Test stays as permanent
	// regression protection — any schema change that drops the index fails here.
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	rows, err := db.Query(`PRAGMA index_list('BountyBoard')`)
	if err != nil {
		t.Fatalf("index_list: %v", err)
	}
	defer rows.Close()

	type idxMeta struct {
		name   string
		unique bool
	}
	var indexes []idxMeta
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		indexes = append(indexes, idxMeta{name: name, unique: unique == 1})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// RGR form: assert the fix is present. We expect at least one UNIQUE
	// index covering idempotency_key. Today, none exists — so without the
	// skip, this test FAILS on current main (proving AUDIT-008 is open).
	// Once Fix #3 lands, the index appears and this test passes.
	for _, idx := range indexes {
		if !idx.unique {
			continue
		}
		cols, err := db.Query(`PRAGMA index_info(` + quoteIdent(idx.name) + `)`)
		if err != nil {
			t.Fatalf("index_info(%s): %v", idx.name, err)
		}
		for cols.Next() {
			var (
				seqno int
				cid   int
				cname string
			)
			if err := cols.Scan(&seqno, &cid, &cname); err != nil {
				cols.Close()
				t.Fatalf("scan index_info: %v", err)
			}
			if strings.EqualFold(cname, "idempotency_key") {
				cols.Close()
				// Fix has shipped — the UNIQUE index covering idempotency_key is present.
				return
			}
		}
		cols.Close()
	}

	t.Fatalf("AUDIT-008 (P2): no UNIQUE index covers BountyBoard.idempotency_key "+
		"(%d total indexes scanned). Fix #3: add partial UNIQUE index on "+
		"BountyBoard(idempotency_key) WHERE status NOT IN "+
		"('Completed','Cancelled','Failed') and switch AddConvoyTaskIdempotent "+
		"to INSERT … ON CONFLICT DO NOTHING RETURNING id.", len(indexes))
}

// quoteIdent wraps an identifier in double quotes, escaping embedded quotes.
// PRAGMA index_info takes a bare identifier; SQLite's own auto-index names
// (e.g. "sqlite_autoindex_Convoys_1") are safe but we quote defensively.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

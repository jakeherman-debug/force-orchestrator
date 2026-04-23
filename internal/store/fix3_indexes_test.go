package store

import (
	"database/sql"
	"strings"
	"testing"
)

// Fix #3 structural tests — verify the partial UNIQUE indexes installed by
// createSchema / runMigrations are present on a fresh InitHolocronDSN.
// These are cheap PRAGMA introspection tests; their job is to catch a future
// schema-edit PR that accidentally drops the index.

// ── Helpers ───────────────────────────────────────────────────────────────

type indexMeta struct {
	name    string
	unique  bool
	partial bool
}

// listIndexesForTable returns every index on `table` as reported by
// PRAGMA index_list, keyed by name.
func listIndexesForTable(t *testing.T, db *sql.DB, table string) []indexMeta {
	t.Helper()
	rows, err := db.Query(`PRAGMA index_list(` + quoteIdent(table) + `)`)
	if err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	defer rows.Close()
	var out []indexMeta
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
		out = append(out, indexMeta{name: name, unique: unique == 1, partial: partial == 1})
	}
	return out
}

// indexColumnsFor returns the column names backing a specific index, in order.
func indexColumnsFor(t *testing.T, db *sql.DB, indexName string) []string {
	t.Helper()
	rows, err := db.Query(`PRAGMA index_info(` + quoteIdent(indexName) + `)`)
	if err != nil {
		t.Fatalf("index_info(%s): %v", indexName, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var (
			seqno, cid int
			cname      string
		)
		if err := rows.Scan(&seqno, &cid, &cname); err != nil {
			t.Fatalf("scan index_info: %v", err)
		}
		cols = append(cols, cname)
	}
	return cols
}

// findIndexMetaByColumn returns the first index on `table` that covers
// `column` as its (single) backing column.
func findIndexMetaByColumn(t *testing.T, db *sql.DB, table, column string) *indexMeta {
	t.Helper()
	for _, idx := range listIndexesForTable(t, db, table) {
		cols := indexColumnsFor(t, db, idx.name)
		if len(cols) == 1 && strings.EqualFold(cols[0], column) {
			i := idx
			return &i
		}
	}
	return nil
}

// indexSQL returns the CREATE INDEX statement SQLite stored for this index,
// or "" if the index is an auto-index (no stored SQL).
func indexSQL(t *testing.T, db *sql.DB, indexName string) string {
	t.Helper()
	var sqlText sql.NullString
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, indexName).
		Scan(&sqlText); err != nil {
		t.Fatalf("sqlite_master sql lookup for %s: %v", indexName, err)
	}
	if !sqlText.Valid {
		return ""
	}
	return sqlText.String
}

// ── Tests ─────────────────────────────────────────────────────────────────

// TestFix3_BountyBoardHasPartialUniqueIdempotency verifies idx_bounty_idem
// (partial UNIQUE on idempotency_key) ships on every fresh DB.
func TestFix3_BountyBoardHasPartialUniqueIdempotency(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	idx := findIndexMetaByColumn(t, db, "BountyBoard", "idempotency_key")
	if idx == nil {
		t.Fatalf("Fix #3 regression: no index covers BountyBoard.idempotency_key")
	}
	if !idx.unique {
		t.Fatalf("Fix #3 regression: index %s on BountyBoard.idempotency_key is not UNIQUE", idx.name)
	}
	if !idx.partial {
		t.Fatalf("Fix #3 regression: index %s on BountyBoard.idempotency_key is not partial "+
			"(should be scoped to idempotency_key != '' AND status NOT IN "+
			"('Completed','Cancelled','Failed'))", idx.name)
	}
	def := indexSQL(t, db, idx.name)
	if !strings.Contains(def, "idempotency_key != ''") {
		t.Errorf("Fix #3 regression: %s definition missing "+
			"`idempotency_key != ''` predicate:\n%s", idx.name, def)
	}
	if !strings.Contains(def, "status NOT IN") {
		t.Errorf("Fix #3 regression: %s definition missing "+
			"`status NOT IN (...)` predicate:\n%s", idx.name, def)
	}
}

// TestFix3_EscalationsHasPartialUniqueOpenTaskID verifies the
// partial UNIQUE on Escalations(task_id) WHERE status='Open'.
func TestFix3_EscalationsHasPartialUniqueOpenTaskID(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	idx := findIndexMetaByColumn(t, db, "Escalations", "task_id")
	if idx == nil {
		t.Fatalf("Fix #3 regression: no index covers Escalations.task_id")
	}
	if !idx.unique {
		t.Fatalf("Fix #3 regression: index %s on Escalations.task_id is not UNIQUE", idx.name)
	}
	if !idx.partial {
		t.Fatalf("Fix #3 regression: index %s on Escalations.task_id is not partial "+
			"(should be scoped to status = 'Open')", idx.name)
	}
	def := indexSQL(t, db, idx.name)
	if !strings.Contains(def, `status = 'Open'`) && !strings.Contains(def, `status='Open'`) {
		t.Errorf("Fix #3 regression: %s definition missing "+
			"`status = 'Open'` predicate:\n%s", idx.name, def)
	}
}

// TestFix3_FeatureBlockersHasPartialUniqueUnresolved verifies the partial
// UNIQUE on FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE
// resolved_at IS NULL.
func TestFix3_FeatureBlockersHasPartialUniqueUnresolved(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	indexes := listIndexesForTable(t, db, "FeatureBlockers")
	var match *indexMeta
	for i := range indexes {
		cols := indexColumnsFor(t, db, indexes[i].name)
		if len(cols) != 2 {
			continue
		}
		if strings.EqualFold(cols[0], "blocked_convoy_id") &&
			strings.EqualFold(cols[1], "blocking_feature_id") &&
			indexes[i].unique && indexes[i].partial {
			m := indexes[i]
			match = &m
			break
		}
	}
	if match == nil {
		t.Fatalf("Fix #3 regression: no partial UNIQUE covers "+
			"FeatureBlockers(blocked_convoy_id, blocking_feature_id) — indexes: %+v", indexes)
	}
	def := indexSQL(t, db, match.name)
	if !strings.Contains(def, "resolved_at IS NULL") {
		t.Errorf("Fix #3 regression: %s missing `resolved_at IS NULL` predicate:\n%s",
			match.name, def)
	}
}

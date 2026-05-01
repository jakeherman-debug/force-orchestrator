package rules

import "testing"

func TestBOS008_Red_NoIndex(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func mk(db *sql.DB) {\n" +
		"\tdb.Exec(`CREATE TABLE IF NOT EXISTS NewThing (\n" +
		"\t\tid INTEGER PRIMARY KEY AUTOINCREMENT,\n" +
		"\t\towner TEXT NOT NULL,\n" +
		"\t\tcreated_at TEXT\n" +
		"\t);`)\n" +
		"}\n"
	out := runRule(t, bos008{}, "internal/store/schema.go", src)
	assertHasFinding(t, out, "BOS-008", "NewThing")
}

func TestBOS008_Green_WithIndex(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func mk(db *sql.DB) {\n" +
		"\tdb.Exec(`CREATE TABLE IF NOT EXISTS NewThing (\n" +
		"\t\tid INTEGER PRIMARY KEY AUTOINCREMENT,\n" +
		"\t\towner TEXT NOT NULL\n" +
		"\t);`)\n" +
		"\tdb.Exec(`CREATE INDEX IF NOT EXISTS idx_newthing_owner ON NewThing(owner);`)\n" +
		"}\n"
	out := runRule(t, bos008{}, "internal/store/schema.go", src)
	assertNoFindings(t, out)
}

package rules

import "testing"

// TestISB003_Red_ConcatenatedSelect — `"SELECT ..." + ident` triggers
// a finding.
func TestISB003_Red_ConcatenatedSelect(t *testing.T) {
	src := `package x
import "database/sql"
func F(db *sql.DB, name string) error {
	_, err := db.Exec("SELECT * FROM Users WHERE name = " + name)
	return err
}
`
	out := runRule(t, isb003{}, "internal/foo/q.go", src)
	assertHasFinding(t, out, "ISB-003", "")
}

// TestISB003_Green_Parameterized — `?` placeholder + arg list passes.
func TestISB003_Green_Parameterized(t *testing.T) {
	src := `package x
import "database/sql"
func F(db *sql.DB, name string) error {
	_, err := db.Exec("SELECT * FROM Users WHERE name = ?", name)
	return err
}
`
	out := runRule(t, isb003{}, "internal/foo/q.go", src)
	assertNoFindings(t, out)
}

// TestISB003_Green_NonSQLString — concatenating a non-SQL literal
// with a variable doesn't trip the rule.
func TestISB003_Green_NonSQLString(t *testing.T) {
	src := `package x
func F(name string) string {
	return "hello " + name
}
`
	out := runRule(t, isb003{}, "internal/foo/g.go", src)
	assertNoFindings(t, out)
}

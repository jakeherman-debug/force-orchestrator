package rules

import "testing"

// TestBOS001_Red — a void-returning store mutator triggers a finding.
func TestBOS001_Red(t *testing.T) {
	src := `
package store
import "database/sql"
func UpdateBountyStatus(db *sql.DB, id int, s string) {
	db.Exec("UPDATE BountyBoard SET status = ? WHERE id = ?", s, id)
}
`
	out := runRule(t, bos001{}, "internal/store/example.go", src)
	assertHasFinding(t, out, "BOS-001", "UpdateBountyStatus")
}

// TestBOS001_Green — the same shape but returning error passes.
func TestBOS001_Green(t *testing.T) {
	src := `
package store
import "database/sql"
func UpdateBountyStatus(db *sql.DB, id int, s string) error {
	_, err := db.Exec("UPDATE BountyBoard SET status = ? WHERE id = ?", s, id)
	return err
}
`
	out := runRule(t, bos001{}, "internal/store/example.go", src)
	assertNoFindings(t, out)
}

// TestBOS001_NotInStorePath — same void mutator, but not under
// internal/store/, is out of scope.
func TestBOS001_NotInStorePath(t *testing.T) {
	src := `
package something
func UpdateThing(x int) {}
`
	out := runRule(t, bos001{}, "internal/agents/something.go", src)
	assertNoFindings(t, out)
}

// TestBOS001_NonMutatorVerb — same void return but the prefix
// (Get, List, Find, ...) is not a mutator, so no finding.
func TestBOS001_NonMutatorVerb(t *testing.T) {
	src := `
package store
func GetBounty(id int) string { return "" }
`
	out := runRule(t, bos001{}, "internal/store/example.go", src)
	assertNoFindings(t, out)
}

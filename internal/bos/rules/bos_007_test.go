package rules

import "testing"

func TestBOS007_Red(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func ListByConvoy(db *sql.DB, id int) error {\n" +
		"\t_, err := db.Exec(`SELECT * FROM BountyBoard WHERE payload LIKE '%\"convoy_id\":1,%'`)\n" +
		"\treturn err\n" +
		"}\n"
	out := runRule(t, bos007{}, "internal/store/example.go", src)
	assertHasFinding(t, out, "BOS-007", "convoy_id")
}

func TestBOS007_Green_DirectColumnComparison(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func ListByConvoy(db *sql.DB, id int) error {\n" +
		"\t_, err := db.Exec(`SELECT * FROM BountyBoard WHERE convoy_id = ?`, id)\n" +
		"\treturn err\n" +
		"}\n"
	out := runRule(t, bos007{}, "internal/store/example.go", src)
	assertNoFindings(t, out)
}

// Other LIKE patterns are out of scope.
func TestBOS007_OtherLikes(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func ListByName(db *sql.DB, n string) error {\n" +
		"\t_, err := db.Exec(`SELECT * FROM Repositories WHERE name LIKE ?`, n)\n" +
		"\treturn err\n" +
		"}\n"
	out := runRule(t, bos007{}, "internal/store/example.go", src)
	assertNoFindings(t, out)
}

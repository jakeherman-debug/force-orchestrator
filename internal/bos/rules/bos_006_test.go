package rules

import "testing"

func TestBOS006_Red_NoValidator(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func InsertChild(db *sql.DB, parentID int) error {\n" +
		"\t_, err := db.Exec(`INSERT INTO Children (parent_id) VALUES (?)`, parentID)\n" +
		"\treturn err\n" +
		"}\n"
	out := runRule(t, bos006{}, "internal/store/example.go", src)
	assertHasFinding(t, out, "BOS-006", "")
}

func TestBOS006_Green_WithValidator(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func ValidateParentRef(id int) error { return nil }\n" +
		"func InsertChild(db *sql.DB, parentID int) error {\n" +
		"\tif err := ValidateParentRef(parentID); err != nil { return err }\n" +
		"\t_, err := db.Exec(`INSERT INTO Children (parent_id) VALUES (?)`, parentID)\n" +
		"\treturn err\n" +
		"}\n"
	out := runRule(t, bos006{}, "internal/store/example.go", src)
	assertNoFindings(t, out)
}

// SELECT statements are out of scope.
func TestBOS006_NotMutation(t *testing.T) {
	src := "package store\n" +
		"import \"database/sql\"\n" +
		"func GetChild(db *sql.DB, parentID int) error {\n" +
		"\t_, err := db.Exec(`SELECT * FROM Children WHERE parent_id = ?`, parentID)\n" +
		"\treturn err\n" +
		"}\n"
	out := runRule(t, bos006{}, "internal/store/example.go", src)
	assertNoFindings(t, out)
}

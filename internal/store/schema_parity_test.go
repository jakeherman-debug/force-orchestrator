package store

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestSchemaParity asserts that every column declared in createSchema (source
// of truth, internal/store/schema.go) also appears in schema/schema.sql (the
// reference/documentation file). Fix #8c / AUDIT-080 ratchets this invariant
// so a developer adding a new column to createSchema without updating the
// reference SQL fails CI.
//
// The comparison is symmetric: a column in schema.sql that is NOT in
// createSchema also fails — reference drift in either direction means the
// two documents disagree. runMigrations (the ALTER path) is NOT part of the
// parity check because it legitimately carries columns that only exist on
// the upgrade path (backfills, compat columns). For those, the rule is:
// any column ALTER-added must also be added to createSchema for fresh DBs
// (documented in CLAUDE.md § "Store / schema conventions").
func TestSchemaParity(t *testing.T) {
	root := schemaParityRoot(t)
	goSrc := readFileParity(t, filepath.Join(root, "internal", "store", "schema.go"))
	sqlSrc := readFileParity(t, filepath.Join(root, "schema", "schema.sql"))

	// schema.go embeds SQL inside Go raw string literals (backticks). Pull
	// out every `CREATE TABLE IF NOT EXISTS <name> (...);` block from the
	// raw-string bodies; each ends cleanly at `);` because the Go
	// formatting convention puts the closing `);` on its own line inside
	// the backtick literal.
	// schema.go embeds SQL inside raw string literals with `-- ...` inline
	// comments; strip them on both sides so a SQL-level comment tail
	// (e.g. `col TEXT DEFAULT '', -- comment`) doesn't register as a
	// subsequent column line.
	goTables := extractTableColumns(t, stripSQLComments(goSrc), "schema.go")
	sqlTables := extractTableColumns(t, stripSQLComments(sqlSrc), "schema.sql")

	// Every table in schema.go must appear in schema.sql with the same columns.
	for _, table := range sortedKeys(goTables) {
		goCols := goTables[table]
		sqlCols, present := sqlTables[table]
		if !present {
			t.Errorf("TestSchemaParity: table %q exists in createSchema but is missing from schema.sql", table)
			continue
		}
		diffMissing := schemaParitySetDiff(goCols, sqlCols)
		diffExtra := schemaParitySetDiff(sqlCols, goCols)
		if len(diffMissing) > 0 {
			t.Errorf("TestSchemaParity: table %q columns missing from schema.sql: %v\n"+
				"(createSchema is authoritative — update schema.sql to match)",
				table, diffMissing)
		}
		if len(diffExtra) > 0 {
			t.Errorf("TestSchemaParity: table %q has columns in schema.sql that are NOT in createSchema: %v\n"+
				"(reference drift — either remove from schema.sql, or add to createSchema)",
				table, diffExtra)
		}
	}

	// Tables in schema.sql but not in schema.go are also an error —
	// same divergence class.
	for _, table := range sortedKeys(sqlTables) {
		if _, present := goTables[table]; !present {
			t.Errorf("TestSchemaParity: table %q exists in schema.sql but is missing from createSchema", table)
		}
	}
}

// stripSQLComments removes `-- line comment` tails from every line. Keeps
// line structure intact so subsequent regex matches still work. Does NOT
// handle `/* ... */` block comments — schema.sql uses only `--` comments.
func stripSQLComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "--"); idx >= 0 {
			lines[i] = ln[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// extractTableColumns parses CREATE TABLE IF NOT EXISTS blocks and returns a
// map of table name → sorted set of column names.
//
// The parser walks each `CREATE TABLE IF NOT EXISTS <name>` occurrence,
// finds the first `(` that opens the column list, then balances parens to
// locate the matching `)`. The body between is split on top-level commas
// (paren depth == 0) so `DEFAULT (datetime('now'))` isn't mis-split.
// Multi-column constraint lines (PRIMARY KEY, UNIQUE, FOREIGN KEY, CHECK,
// CONSTRAINT) are recognised by their leading keyword and filtered out.
// Skips virtual tables (FTS5) — they participate in a separate sync path
// and don't have column-level parity semantics.
func extractTableColumns(t *testing.T, src, label string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	nameRe := regexp.MustCompile(`CREATE TABLE IF NOT EXISTS\s+(\w+)\s*\(`)
	locs := nameRe.FindAllStringSubmatchIndex(src, -1)
	if len(locs) == 0 {
		t.Fatalf("extractTableColumns(%s): found zero CREATE TABLE IF NOT EXISTS blocks — selector stale?", label)
	}
	for _, loc := range locs {
		name := src[loc[2]:loc[3]]
		// schema.go declares some tables twice — once in createSchema and
		// again in runMigrations (the `CREATE TABLE IF NOT EXISTS` idiom
		// is how idempotent upgrades handle compat-aware tables like
		// ProposedConvoys, FeatureBlockers, ConvoyHolds, ConvoyAskBranches,
		// AskBranchPRs, TaskNotes, PRReviewComments). createSchema is the
		// authoritative column list, so the first occurrence wins and
		// subsequent matches are ignored. runMigrations's ALTER statements
		// live outside the CREATE TABLE block and are not part of parity.
		if _, already := out[name]; already {
			continue
		}
		start := loc[1] - 1 // the `(` opener
		body, ok := extractParenBody(src, start)
		if !ok {
			t.Fatalf("extractTableColumns(%s): unbalanced parens in CREATE TABLE %s", label, name)
		}
		out[name] = parseColumnList(body)
	}
	return out
}

// extractParenBody returns the body between the paren at src[openIdx] and
// its matching close paren. openIdx must point at `(`. Returns (body, true)
// on success; ("", false) if unbalanced.
func extractParenBody(src string, openIdx int) (string, bool) {
	if openIdx >= len(src) || src[openIdx] != '(' {
		return "", false
	}
	depth := 0
	for i := openIdx; i < len(src); i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return src[openIdx+1 : i], true
			}
		}
	}
	return "", false
}

// parseColumnList splits a CREATE TABLE body on top-level commas and returns
// the first-word identifier from each column line. Constraint lines are
// filtered.
func parseColumnList(body string) []string {
	constraintPrefixes := []string{
		"PRIMARY KEY",
		"UNIQUE",
		"FOREIGN KEY",
		"CHECK",
		"CONSTRAINT",
	}
	var cols []string
	depth := 0
	var buf strings.Builder
	flush := func() {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s == "" {
			return
		}
		upper := strings.ToUpper(s)
		for _, pfx := range constraintPrefixes {
			if strings.HasPrefix(upper, pfx) {
				return
			}
		}
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return
		}
		cols = append(cols, fields[0])
	}
	for _, ch := range body {
		switch ch {
		case '(':
			depth++
			buf.WriteRune(ch)
		case ')':
			depth--
			buf.WriteRune(ch)
		case ',':
			if depth == 0 {
				flush()
			} else {
				buf.WriteRune(ch)
			}
		default:
			buf.WriteRune(ch)
		}
	}
	flush()
	sort.Strings(cols)
	return cols
}

func schemaParitySetDiff(a, b []string) []string {
	bset := map[string]struct{}{}
	for _, x := range b {
		bset[x] = struct{}{}
	}
	var diff []string
	for _, x := range a {
		if _, ok := bset[x]; !ok {
			diff = append(diff, x)
		}
	}
	sort.Strings(diff)
	return diff
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func readFileParity(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func schemaParityRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate project root from %s", file)
	return ""
}

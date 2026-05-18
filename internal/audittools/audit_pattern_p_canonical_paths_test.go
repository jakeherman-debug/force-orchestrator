// Pattern P_CanonicalPaths (Sweep F).
//
// Every runtime state file the daemon and CLI touch — the SQLite
// holocron, the legacy fleet.log writer, the holonet event stream,
// the singleton PID file, the per-task scratch log — MUST resolve
// through the internal/forcepath resolver. Pre-Sweep-F these all
// resolved from CWD, which produced the "force repos shows nothing"
// class of bug (operator invokes the CLI from a directory other than
// the daemon's; CLI silently opens a DIFFERENT empty DB).
//
// This pattern walks every production .go file and rejects any
// string literal whose shape matches a CWD-relative state file:
//
//   - "./holocron.db" / "./holocron.db-wal" / "./holocron.db-shm"
//   - "holocron.db"       (bare; not adjacent to a forcepath.Dir call)
//   - "./fleet.log"       / "fleet.log"
//   - "./holonet.jsonl"   / "holonet.jsonl"
//   - "./fleet.pid"       / "force.pid"   (bare)
//   - "./fleet-task-*.log" prefixes
//
// Allowed exceptions:
//
//   - internal/forcepath/*           — the resolver itself owns the
//     bare filenames it joins onto Dir().
//   - internal/daemon/singleton/*    — D12 PID path predates Sweep F
//     and intentionally builds its OWN canonical path; the resolver
//     forwards to it. Comments + the literal "force.pid" inside the
//     singleton helper are intentional.
//   - schema/sql/* and docs/         — not Go source.
//   - *_test.go                       — tests legitimately reference
//     literal filenames when asserting fixture state.
//
// The "drift sentinel" sibling test asserts the matcher is
// load-bearing by injecting a synthetic offence and expecting the
// matcher to flag it. If anyone weakens the matcher into a no-op the
// drift test fails first.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pCanonicalPathsForbidden returns true when val (the raw quoted form
// of a Go string literal, including the surrounding quotes) matches a
// CWD-relative state-file shape. The matcher is intentionally
// conservative — it looks for state-file SUFFIX patterns combined with
// a leading "./" prefix or a bare filename, so an unrelated path like
// "config/holocron.db.example" or "tools/fleet.log.tmpl" does NOT trip
// it.
func pCanonicalPathsForbidden(val string) (bool, string) {
	// Strip the surrounding double-quotes.
	if len(val) < 2 || val[0] != '"' || val[len(val)-1] != '"' {
		return false, ""
	}
	s := val[1 : len(val)-1]

	// CWD-relative forms — unambiguous offenders.
	switch s {
	case
		"./holocron.db", "./holocron.db-wal", "./holocron.db-shm",
		"./fleet.log",
		"./holonet.jsonl",
		"./fleet.pid", "./force.pid",
		"holocron.db", "holocron.db-wal", "holocron.db-shm",
		"fleet.log",
		"holonet.jsonl",
		"fleet.pid":
		return true, s
	}
	// fleet-task-<id>.log scratch-log shape, CWD-relative.
	if strings.HasPrefix(s, "./fleet-task-") && strings.HasSuffix(s, ".log") {
		return true, s
	}
	if strings.HasPrefix(s, "fleet-task-") && strings.HasSuffix(s, ".log") {
		return true, s
	}
	return false, ""
}

// pCanonicalPathsExemptPaths is the set of files / directory prefixes
// that may contain bare state-file literals. Paths are relative to
// the module root and use forward slashes.
var pCanonicalPathsExemptPaths = []string{
	"internal/forcepath/",
	"internal/daemon/singleton/",
	"internal/audittools/audit_pattern_p_canonical_paths_test.go", // self
}

func pCanonicalPathsExempt(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, p := range pCanonicalPathsExemptPaths {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// TestPattern_P_CanonicalPaths walks every non-test Go file under
// cmd/ and internal/ and rejects CWD-relative state-file string
// literals. Exempt packages are listed in pCanonicalPathsExemptPaths.
func TestPattern_P_CanonicalPaths(t *testing.T) {
	root := moduleRoot(t)
	type offence struct {
		File    string
		Line    int
		Literal string
	}
	var offences []offence

	scan := func(absPath string) {
		rel, _ := filepath.Rel(root, absPath)
		if pCanonicalPathsExempt(rel) {
			return
		}
		// Skip test files — fixtures legitimately reference literal
		// filenames when asserting on-disk state.
		if strings.HasSuffix(absPath, "_test.go") {
			return
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, absPath, nil, 0)
		if err != nil {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if hit, s := pCanonicalPathsForbidden(lit.Value); hit {
				offences = append(offences, offence{
					File:    filepath.ToSlash(rel),
					Line:    fset.Position(lit.Pos()).Line,
					Literal: s,
				})
			}
			return true
		})
	}

	for _, walkRoot := range []string{
		filepath.Join(root, "cmd"),
		filepath.Join(root, "internal"),
	} {
		_ = filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			scan(path)
			return nil
		})
	}

	if len(offences) > 0 {
		sort.Slice(offences, func(i, j int) bool {
			if offences[i].File != offences[j].File {
				return offences[i].File < offences[j].File
			}
			return offences[i].Line < offences[j].Line
		})
		t.Errorf("Pattern P_CanonicalPaths: %d CWD-relative state-file literal(s) found outside the forcepath resolver. Every runtime state file must resolve through internal/forcepath/ so the daemon and CLI agree on a single canonical location (otherwise an operator invoking the CLI from a different cwd than the daemon silently opens a DIFFERENT file):", len(offences))
		for _, o := range offences {
			t.Errorf("  %s:%d — %q", o.File, o.Line, o.Literal)
		}
	}
}

// TestPattern_P_CanonicalPaths_DetectsInjectedDrift proves the matcher
// is load-bearing: feed it a synthetic source snippet that contains
// every forbidden shape, parse it as if it lived outside the exempt
// list, and assert each one is flagged. If anyone weakens
// pCanonicalPathsForbidden into a tautology, this drift sentinel
// fails before the production matcher silently passes.
func TestPattern_P_CanonicalPaths_DetectsInjectedDrift(t *testing.T) {
	const synthetic = `package fake

func leak() {
	_ = "./holocron.db"
	_ = "./holocron.db-wal"
	_ = "./fleet.log"
	_ = "./holonet.jsonl"
	_ = "./fleet.pid"
	_ = "holocron.db"
	_ = "fleet.log"
	_ = "holonet.jsonl"
	_ = "./fleet-task-99.log"
	_ = "config/holocron.db.example" // must NOT trip
	_ = "tools/fleet.log.tmpl"        // must NOT trip
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", synthetic, 0)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if ok, s := pCanonicalPathsForbidden(lit.Value); ok {
			hits = append(hits, s)
		}
		return true
	})
	wantHits := []string{
		"./holocron.db",
		"./holocron.db-wal",
		"./fleet.log",
		"./holonet.jsonl",
		"./fleet.pid",
		"holocron.db",
		"fleet.log",
		"holonet.jsonl",
		"./fleet-task-99.log",
	}
	sort.Strings(hits)
	sort.Strings(wantHits)
	if len(hits) != len(wantHits) {
		t.Fatalf("drift sentinel: got %d hits, want %d.\n  got:  %v\n  want: %v", len(hits), len(wantHits), hits, wantHits)
	}
	for i := range hits {
		if hits[i] != wantHits[i] {
			t.Errorf("drift sentinel hit[%d] = %q; want %q", i, hits[i], wantHits[i])
		}
	}
	// Implicit: the false-positive cases ("config/holocron.db.example",
	// "tools/fleet.log.tmpl") MUST NOT appear in hits. The length check
	// above guarantees that.
}

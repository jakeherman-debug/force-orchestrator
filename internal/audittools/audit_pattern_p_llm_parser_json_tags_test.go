// Package audittools: pattern test for json-tag discipline on every
// struct used as a `json.Unmarshal` (or `json.Decoder.Decode`) target
// in production code.
//
// Anchor — commit 154411a:
//
//   `internal/clients/librarian/client.go`'s CandidateRule struct had no
//   json tags. The librarian's bootstrapSenatorRulesSystemPrompt asked
//   Claude to emit `rule_key` / `agent_scope` (snake_case) but the
//   struct fields were `RuleKey` / `AgentScope` (PascalCase). Go's
//   `encoding/json` matches case-insensitively but does NOT cross
//   underscores, so `rule_key` did NOT unmarshal into `RuleKey` —
//   silent empty strings, every candidate rejected at validation as
//   "missing rule_key or body" even on well-formed LLM output.
//
// The bug class is broader than LLM output: ANY struct used as a
// `json.Unmarshal` target whose multi-word PascalCase exported fields
// lack `json:"..."` tags is a latent silent-failure if the wire
// payload uses snake_case (or any other casing convention that breaks
// case-insensitive matching).
//
// Pattern P_LLMParserJSONTags walks production code via AST and flags
// any json-Unmarshal target struct with multi-word PascalCase exported
// fields that lack json tags. The check is intentionally broader than
// "LLM-fed only" — operator-fed payloads benefit from the same
// discipline, and false positives are documented via an allowlist.
//
// Allowlist conventions:
//
//   - Allowlisted entries MUST carry a one-line rationale explaining
//     why the struct is exempt (the bug doesn't apply because the
//     wire format uses PascalCase, the struct is never decoded from
//     a snake_case payload, etc.).
//   - Adding to the allowlist without a rationale is rejected by
//     TestPattern_P_LLMParserJSONTags_AllowlistReasonsTruthful.
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

// pLLMParserJSONTagsAllowlist names <package-path>.<TypeName> entries
// that are exempt from the json-tag rule. Each entry MUST carry a
// one-line rationale.
//
// As of the sweep-llm initial pass, no production struct that is
// unmarshaled from JSON has any multi-word PascalCase field lacking a
// json tag. The allowlist is empty by design — if a future change
// adds a new offender, the right move is almost always to add the
// json tag, not to expand this allowlist.
var pLLMParserJSONTagsAllowlist = map[string]string{
	// Intentionally empty.
	//
	// If you need to add an entry, document WHY the struct is exempt:
	// the wire format uses PascalCase (rare — most consumed JSON is
	// snake_case or camelCase), the struct is only decoded by tests,
	// or the multi-word field is intentionally meant to never bind
	// (in which case `json:"-"` is the right escape hatch).
}

// TestPattern_P_LLMParserJSONTags walks every production .go file,
// finds json.Unmarshal / json.Decoder.Decode call sites, resolves the
// target struct type, and fails if the struct has any exported
// multi-word PascalCase field that lacks a json tag.
//
// "Multi-word PascalCase" means the field name contains a transition
// from lowercase to uppercase letters (e.g. `RuleKey`, `AgentScope`).
// Single-word fields like `URL`, `ID`, `Email` are not flagged because
// Go's case-insensitive matcher correctly resolves `url` / `id` /
// `email` against them.
//
// The walker only inspects struct types defined in production code
// (cmd/, internal/). Anonymous (embedded) fields are skipped. Fields
// with `json:"-"` are skipped. Fields whose type is a struct from
// outside the production tree (third-party) are not analysed for
// their nested fields, only flagged at the top level.
func TestPattern_P_LLMParserJSONTags(t *testing.T) {
	root := moduleRoot(t)

	// Phase 1: collect every struct type definition reachable from
	// production .go files. Map: package-qualified name → struct fields
	// (with their json tags / lack thereof).
	structs := collectProductionStructs(t, root)

	// Phase 2: walk every json.Unmarshal / json.Decoder.Decode call
	// site, resolve the unmarshal-target type, and check tag discipline.
	type offender struct {
		file       string
		line       int
		structName string
		fieldName  string
	}
	var offenders []offender

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" ||
				name == ".d7-worktrees" || name == ".fix-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" ||
				name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath := rel(root, path)
		if !strings.HasPrefix(relPath, "cmd/") && !strings.HasPrefix(relPath, "internal/") {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}

		// For every json.Unmarshal call AND every Decode call on a
		// json.Decoder, find the target type's name.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			targetTypeName, ok := llmParserUnmarshalTarget(call, f)
			if !ok {
				return true
			}

			// targetTypeName is either an unqualified type ("CandidateRule")
			// or a package-qualified one ("librarian.CandidateRule" / via
			// SelectorExpr → resolved to its file).
			info, ok := structs[targetTypeName]
			if !ok {
				// Couldn't resolve — could be an anonymous struct,
				// generic, or external package type. Anonymous structs
				// declared inline are flagged separately below.
				return true
			}

			pos := fset.Position(call.Pos())
			for _, fld := range info.fields {
				if fld.problem == "" {
					continue
				}
				offenders = append(offenders, offender{
					file:       relPath,
					line:       pos.Line,
					structName: targetTypeName,
					fieldName:  fld.name,
				})
			}
			return true
		})

		// Inline struct unmarshals — anonymous struct literals are
		// declared at the call site. Walk their fields directly.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !isJSONUnmarshalLike(call) && !isJSONDecoderDecodeLike(call) {
				return true
			}
			structType, ok := inlineStructTargetType(call, f)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			for _, fld := range structType.Fields.List {
				if len(fld.Names) == 0 {
					continue // anonymous field
				}
				for _, nm := range fld.Names {
					if !nm.IsExported() {
						continue
					}
					if !isMultiWordPascalCase(nm.Name) {
						continue
					}
					if hasJSONTag(fld.Tag) {
						continue
					}
					offenders = append(offenders, offender{
						file:       relPath,
						line:       pos.Line,
						structName: "<inline anonymous>",
						fieldName:  nm.Name,
					})
				}
			}
			return true
		})

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	// Filter offenders against the allowlist.
	filtered := offenders[:0]
	for _, o := range offenders {
		if _, ok := pLLMParserJSONTagsAllowlist[o.structName]; ok {
			continue
		}
		filtered = append(filtered, o)
	}

	if len(filtered) == 0 {
		return
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].file != filtered[j].file {
			return filtered[i].file < filtered[j].file
		}
		return filtered[i].line < filtered[j].line
	})
	t.Errorf("Pattern P_LLMParserJSONTags: %d field(s) on json.Unmarshal target structs lack a json tag and have multi-word PascalCase names — Go's case-insensitive matcher does NOT cross underscores, so a wire payload like `rule_key` would silently unmarshal as an empty string into a field named `RuleKey`. Anchor: commit 154411a (CandidateRule).", len(filtered))
	for _, o := range filtered {
		t.Errorf("  %s:%d — struct %s, field %s — add `json:\"<wire_key>\"` tag", o.file, o.line, o.structName, o.fieldName)
	}
	t.Errorf("\nFix: add an explicit `json:\"<key>\"` tag to each field. If the field genuinely should never bind (i.e. derived after Unmarshal), use `json:\"-\"`.")
}

// TestPattern_P_LLMParserJSONTags_AllowlistReasonsTruthful enforces that
// every entry in pLLMParserJSONTagsAllowlist carries a non-empty
// rationale string.
func TestPattern_P_LLMParserJSONTags_AllowlistReasonsTruthful(t *testing.T) {
	for k, why := range pLLMParserJSONTagsAllowlist {
		if strings.TrimSpace(why) == "" {
			t.Errorf("Pattern P_LLMParserJSONTags allowlist entry %q has an empty rationale — "+
				"every allowlisted struct MUST document WHY it is exempt from the json-tag rule.", k)
		}
	}
}

// TestPattern_P_LLMParserJSONTags_DetectsInjectedDrift is the load-bearing
// sentinel. It synthesises a Go source file with an offending struct
// (multi-word PascalCase field, no json tag, used as a json.Unmarshal
// target) and runs the same AST-walk helpers used by the real test;
// the helpers MUST flag the synthetic field. If they don't, the real
// test is dead code (always-passing) and the sentinel signals it.
func TestPattern_P_LLMParserJSONTags_DetectsInjectedDrift(t *testing.T) {
	const synthetic = `package synthetic

import "encoding/json"

type SyntheticCandidate struct {
	RuleKey   string
	AgentScope string
	Body      string
}

func decode(raw []byte) (SyntheticCandidate, error) {
	var c SyntheticCandidate
	err := json.Unmarshal(raw, &c)
	return c, err
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", synthetic, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}

	// Build the per-file struct index for the synthetic file.
	structs := map[string]*structInfo{}
	collectStructsFromFile(f, structs)

	got, ok := structs["SyntheticCandidate"]
	if !ok {
		t.Fatalf("expected SyntheticCandidate struct to be indexed")
	}
	// We expect at least 2 problems: RuleKey + AgentScope (Body is
	// single-word, so case-insensitive matcher handles it).
	var problems []string
	for _, fld := range got.fields {
		if fld.problem != "" {
			problems = append(problems, fld.name)
		}
	}
	sort.Strings(problems)
	if len(problems) < 2 {
		t.Fatalf("sentinel: synthetic struct should have at least 2 flagged fields (RuleKey + AgentScope), got %v", problems)
	}
	want := []string{"AgentScope", "RuleKey"}
	if got1, got2 := problems[0], problems[1]; got1 != want[0] || got2 != want[1] {
		t.Fatalf("sentinel: expected %v among flagged fields, got %v", want, problems)
	}

	// Also verify the json.Unmarshal call resolves to SyntheticCandidate.
	var resolved string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := llmParserUnmarshalTarget(call, f)
		if !ok {
			return true
		}
		resolved = name
		return false
	})
	if resolved != "SyntheticCandidate" {
		t.Fatalf("sentinel: expected json.Unmarshal target to resolve to SyntheticCandidate, got %q", resolved)
	}

	// Inline-struct sentinel: synthesise a json.Unmarshal call against
	// an inline anonymous struct with an offending field, walk it, and
	// confirm the inline-struct path flags the field.
	const syntheticInline = `package synthetic

import "encoding/json"

func decodeInline(raw []byte) {
	var in struct {
		BriefingID   int64
		Decision     string
	}
	_ = json.Unmarshal(raw, &in)
}
`
	fset2 := token.NewFileSet()
	f2, err := parser.ParseFile(fset2, "synthetic_inline.go", syntheticInline, 0)
	if err != nil {
		t.Fatalf("parse inline synthetic: %v", err)
	}
	var inlineFlagged []string
	ast.Inspect(f2, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isJSONUnmarshalLike(call) {
			return true
		}
		st, ok := inlineStructTargetType(call, f2)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			for _, nm := range fld.Names {
				if !nm.IsExported() {
					continue
				}
				if !isMultiWordPascalCase(nm.Name) {
					continue
				}
				if hasJSONTag(fld.Tag) {
					continue
				}
				inlineFlagged = append(inlineFlagged, nm.Name)
			}
		}
		return true
	})
	if len(inlineFlagged) == 0 {
		t.Fatalf("sentinel: inline-struct path failed to flag BriefingID")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

type structField struct {
	name    string
	problem string // non-empty if the field violates the rule
}

type structInfo struct {
	name   string
	fields []structField
}

// collectProductionStructs walks every production .go file and returns
// a map from unqualified struct name to its field info. If two
// packages declare the same struct name, the LATER one wins — this is
// acceptable for the pattern test because we use the result to look
// up json.Unmarshal target types by their AST-visible name; the AST
// gives us the same unqualified token the file uses, and a name
// collision across packages would be flagged by both at once anyway.
func collectProductionStructs(t *testing.T, root string) map[string]*structInfo {
	t.Helper()
	out := make(map[string]*structInfo)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".build-worktrees" || name == ".force-worktrees" ||
				name == ".d7-worktrees" || name == ".fix-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" ||
				name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath := rel(root, path)
		if !strings.HasPrefix(relPath, "cmd/") && !strings.HasPrefix(relPath, "internal/") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}
		collectStructsFromFile(f, out)
		return nil
	})
	if err != nil {
		t.Fatalf("collectProductionStructs walk: %v", err)
	}
	return out
}

// collectStructsFromFile populates `out` with every named struct
// declared in `f`. For each exported field of each struct, we compute
// the field's "problem" status: empty string if the field is fine,
// non-empty if it violates the json-tag rule.
func collectStructsFromFile(f *ast.File, out map[string]*structInfo) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			info := &structInfo{name: ts.Name.Name}
			for _, fld := range st.Fields.List {
				if len(fld.Names) == 0 {
					continue // anonymous embedded field
				}
				for _, nm := range fld.Names {
					if !nm.IsExported() {
						continue
					}
					sf := structField{name: nm.Name}
					if isMultiWordPascalCase(nm.Name) && !hasJSONTag(fld.Tag) {
						sf.problem = "multi-word PascalCase field with no json tag"
					}
					info.fields = append(info.fields, sf)
				}
			}
			out[ts.Name.Name] = info
		}
	}
}

// isMultiWordPascalCase reports whether `name` contains a
// lowercase→uppercase transition AFTER the leading run of capitals.
//
// Examples that DO match (multi-word):
//
//	RuleKey, AgentScope, FixGuidance, BlockedBy, ConvoyID,
//	BriefingID, IsDraft
//
// Examples that DO NOT match (single-word, all-caps acronym, or
// purely lowercase):
//
//	URL, ID, Email, Name, IDs (the trailing 's' is lowercase but
//	preceded by all-caps initial run, so still treated as single-word).
//
// We treat IDs / URLs / OK as single-word — the case-insensitive
// matcher resolves them correctly against `id` / `url` / `ok`.
func isMultiWordPascalCase(name string) bool {
	if name == "" {
		return false
	}
	// Walk the runes. We're looking for a transition from a lowercase
	// rune to an uppercase rune anywhere in the name. That signals a
	// word boundary the case-insensitive matcher cannot reconstruct
	// from a snake_case wire payload.
	prevLower := false
	for _, r := range name {
		isUpper := r >= 'A' && r <= 'Z'
		isLower := r >= 'a' && r <= 'z'
		if isUpper && prevLower {
			return true
		}
		prevLower = isLower
	}
	return false
}

// hasJSONTag reports whether the field tag literal contains a `json:"..."`
// segment. A `json:"-"` tag also counts (the field is explicitly
// opted out of marshaling, which is the correct escape hatch).
func hasJSONTag(tag *ast.BasicLit) bool {
	if tag == nil {
		return false
	}
	v := tag.Value
	if len(v) < 2 {
		return false
	}
	v = v[1 : len(v)-1] // strip surrounding backticks / quotes
	return strings.Contains(v, "json:\"")
}

// isJSONUnmarshalLike reports whether a CallExpr is `json.Unmarshal(...)`.
func isJSONUnmarshalLike(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkgIdent.Name == "json" && sel.Sel.Name == "Unmarshal"
}

// isJSONDecoderDecodeLike reports whether a CallExpr is a
// `<decoder>.Decode(...)` call where the receiver is plausibly a
// `*json.Decoder`. Heuristic: any method named "Decode" on a single-
// argument call site within a context that imports encoding/json.
// We accept false positives here — additional Decode methods in the
// codebase that happen to take a struct pointer don't introduce
// new offenders unless their target struct already violates the rule
// (in which case they SHOULD be flagged regardless of source).
func isJSONDecoderDecodeLike(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Decode" {
		return false
	}
	if len(call.Args) != 1 {
		return false
	}
	return true
}

// llmParserUnmarshalTarget resolves the target struct type name passed
// to a json.Unmarshal or Decoder.Decode call. Returns "" if the call
// is not relevant or the target cannot be resolved to a named struct
// in the file's package or accessible imports. The "ok" return is
// false in those cases.
//
// We resolve only direct unmarshal forms:
//
//	json.Unmarshal(buf, &v)            → look up v's declared type
//	json.NewDecoder(...).Decode(&v)    → look up v's declared type
//
// where `v` is a local variable of a named struct type or `&Foo{}`
// pointing at a named struct.
func llmParserUnmarshalTarget(call *ast.CallExpr, f *ast.File) (string, bool) {
	var target ast.Expr
	switch {
	case isJSONUnmarshalLike(call):
		if len(call.Args) < 2 {
			return "", false
		}
		target = call.Args[1]
	case isJSONDecoderDecodeLike(call):
		target = call.Args[0]
	default:
		return "", false
	}
	// Strip the leading `&`.
	unary, ok := target.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return "", false
	}
	switch op := unary.X.(type) {
	case *ast.Ident:
		// `&v` — look up v's declared type in the enclosing function.
		// To avoid running a full type-checker, we walk the file for
		// `var v T` / `var v T = ...` / `v := T{...}` style declarations
		// and pick the first match.
		return resolveLocalIdentType(f, op.Name), true
	case *ast.CompositeLit:
		// `&Foo{}` — name is op.Type.
		switch t := op.Type.(type) {
		case *ast.Ident:
			return t.Name, true
		case *ast.SelectorExpr:
			return t.Sel.Name, true
		}
	}
	return "", false
}

// resolveLocalIdentType scans `f` for a declaration of `name` and
// returns the named-type token it was declared with. Returns "" when
// the declaration is anonymous (inline struct literal) or when no
// match is found.
//
// This is a simplified lexical resolver that catches the common
// shapes used in the codebase:
//
//	var v Foo
//	var v store.Foo
//	var v *Foo
//	v := loadSomething(...)         (we cannot resolve return types
//	                                 lexically — return "")
//	var v struct{...}               (anonymous — return "")
//
// Anonymous-struct unmarshals are caught separately via the
// `inlineStructTargetType` path.
func resolveLocalIdentType(f *ast.File, name string) string {
	var found string
	ast.Inspect(f, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.GenDecl:
			if decl.Tok != token.VAR {
				return true
			}
			for _, spec := range decl.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, ident := range vs.Names {
					if ident.Name != name {
						continue
					}
					if vs.Type == nil {
						continue
					}
					switch t := vs.Type.(type) {
					case *ast.Ident:
						found = t.Name
					case *ast.SelectorExpr:
						found = t.Sel.Name
					case *ast.StarExpr:
						switch inner := t.X.(type) {
						case *ast.Ident:
							found = inner.Name
						case *ast.SelectorExpr:
							found = inner.Sel.Name
						}
					}
				}
			}
		}
		return true
	})
	return found
}

// inlineStructTargetType returns the inline struct type AST node when
// the json.Unmarshal target is `&v` and v is declared via
// `var v struct{...}`. Returns (nil, false) for any other shape.
func inlineStructTargetType(call *ast.CallExpr, f *ast.File) (*ast.StructType, bool) {
	var target ast.Expr
	switch {
	case isJSONUnmarshalLike(call):
		if len(call.Args) < 2 {
			return nil, false
		}
		target = call.Args[1]
	case isJSONDecoderDecodeLike(call):
		target = call.Args[0]
	default:
		return nil, false
	}
	unary, ok := target.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return nil, false
	}
	ident, ok := unary.X.(*ast.Ident)
	if !ok {
		return nil, false
	}
	// Find the var decl for this identifier and look for an inline
	// struct type.
	var st *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, nm := range vs.Names {
				if nm.Name != ident.Name {
					continue
				}
				if vs.Type == nil {
					continue
				}
				if s, ok := vs.Type.(*ast.StructType); ok {
					st = s
				}
			}
		}
		return true
	})
	if st == nil {
		return nil, false
	}
	return st, true
}

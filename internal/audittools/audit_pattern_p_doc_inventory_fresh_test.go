// Pattern P_DocInventoryFresh — keeps docs/subsystems/dogs.md in lockstep
// with the canonical dog roster declared in internal/agents/dogs.go's
// `dogOrder` slice (the same list TestListDogs counts against).
//
// Hygiene Wave A finding: `docs/subsystems/dogs.md` carries an inventory
// table grouping all 40 dogs by purpose, but no test asserted the table
// stays in sync with `dogOrder` or the TestListDogs count. Adding or
// renaming a dog could leave the doc silently stale until an operator
// noticed the missing row by hand.
//
// This test closes that gap with a two-direction check:
//
//  1. Every dog name in `dogOrder` is mentioned somewhere in the doc's
//     Inventory section. Missing dogs => FAIL with the names listed.
//  2. Every dog-shaped name mentioned in the Inventory section refers
//     to a real dog (i.e. is present in `dogOrder`). Phantom dogs =>
//     FAIL with the names listed.
//
// The Inventory section is the H2 heading "## Inventory" through the
// next H2 boundary. We extract dog names by walking backtick-quoted
// tokens in that span — the doc's existing convention is to lead each
// bullet with `` `dog-name` ``. That keeps the parser robust to future
// reorganisation of the per-purpose grouping (a structural reshuffle
// inside Inventory is fine; the load-bearing fact is "every dog name
// shows up at least once between the boundaries").
//
// `dogOrder` is parsed via go/parser/go/ast over `internal/agents/dogs.go`
// (the same source TestListDogs reads). We don't import the agents
// package here — that would create a cycle through the test tree;
// AST-walking the file is the same approach P13 / P22 / P30 use.
//
// Sentinel sibling `TestPattern_P_DocInventoryFresh_DetectsInjectedDrift`
// proves the inner check actually fires both directions: it builds an
// in-memory `dogOrder` and a fake doc body that disagree in both
// directions and asserts the comparison surfaces the disagreement.
package audittools

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// inventoryHeading is the H2 heading that opens the dog inventory in
// docs/subsystems/dogs.md. The parser scans from the line AFTER this
// heading up to (but not including) the next "## " heading.
const inventoryHeading = "## Inventory"

// backtickedToken matches Markdown inline-code spans like `dog-name`.
// We keep the inner token loose (alnum + dash + underscore) to tolerate
// future renames; the cross-walk against dogOrder is the actual
// correctness gate, not the regex shape.
var backtickedToken = regexp.MustCompile("`([a-zA-Z][a-zA-Z0-9_-]*)`")

// TestPattern_P_DocInventoryFresh asserts docs/subsystems/dogs.md's
// Inventory section names every dog declared in dogOrder, and that
// every dog-shaped backticked token in the section refers to a real
// dog.
func TestPattern_P_DocInventoryFresh(t *testing.T) {
	root := moduleRoot(t)

	dogs, err := parseDogOrderFromSource(filepath.Join(root, "internal", "agents", "dogs.go"))
	if err != nil {
		t.Fatalf("parse dogOrder from internal/agents/dogs.go: %v", err)
	}
	if len(dogs) == 0 {
		t.Fatalf("parsed zero dogs from dogOrder — parser drift?")
	}

	docPath := filepath.Join(root, "docs", "subsystems", "dogs.md")
	docBody, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	inventory, err := extractInventorySection(string(docBody))
	if err != nil {
		t.Fatalf("extract Inventory section from %s: %v", docPath, err)
	}

	mentioned := backtickedTokensIn(inventory)
	dogSet := make(map[string]bool, len(dogs))
	for _, d := range dogs {
		dogSet[d] = true
	}

	missing, phantom := compareDogInventory(dogs, dogSet, mentioned)

	if len(missing) == 0 && len(phantom) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("docs/subsystems/dogs.md Inventory section is out of sync with internal/agents/dogs.go's dogOrder.\n\n")
	if len(missing) > 0 {
		fmt.Fprintf(&b, "Dogs in dogOrder but NOT mentioned in the Inventory section (%d):\n", len(missing))
		for _, n := range missing {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
		b.WriteString("\nFix: add a bullet under the appropriate Inventory subsection, leading with `dog-name` (cooldown) — purpose.\n\n")
	}
	if len(phantom) > 0 {
		fmt.Fprintf(&b, "Tokens that look like dog names in the Inventory section but are NOT in dogOrder (%d):\n", len(phantom))
		for _, n := range phantom {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
		b.WriteString("\nFix: either rename the entry to match the canonical dog name, or remove the bullet if the dog was deleted from internal/agents/dogs.go.\n")
	}
	t.Error(b.String())
}

// TestPattern_P_DocInventoryFresh_DetectsInjectedDrift proves the
// inner comparison helper fires in both directions. A future refactor
// that silently neutered `compareDogInventory` (e.g. returned nil
// always) would leave the freshness check toothless without this
// fixture.
func TestPattern_P_DocInventoryFresh_DetectsInjectedDrift(t *testing.T) {
	// Synthesise a dogOrder that disagrees with a fake doc body in BOTH
	// directions:
	//   - dogOrder has "ghost-dog" — the doc body does not mention it
	//     (missing direction)
	//   - the doc body mentions `phantom-dog` — dogOrder does not have
	//     it (phantom direction)
	fakeOrder := []string{"git-hygiene", "db-vacuum", "ghost-dog"}
	fakeDoc := `## Inventory

The dogs grouped by purpose, with cooldowns.

**Lifecycle hygiene:**

- ` + "`git-hygiene`" + ` (30 min) — orphan-branch cleanup.
- ` + "`db-vacuum`" + ` (6 h) — SQLite maintenance.
- ` + "`phantom-dog`" + ` (24 h) — does not exist in dogOrder.

## Invariants
`

	inventory, err := extractInventorySection(fakeDoc)
	if err != nil {
		t.Fatalf("extract fake Inventory section: %v", err)
	}
	mentioned := backtickedTokensIn(inventory)

	fakeSet := make(map[string]bool, len(fakeOrder))
	for _, d := range fakeOrder {
		fakeSet[d] = true
	}
	missing, phantom := compareDogInventory(fakeOrder, fakeSet, mentioned)

	if len(missing) != 1 || missing[0] != "ghost-dog" {
		t.Errorf("expected exactly [ghost-dog] missing, got %v", missing)
	}
	if len(phantom) != 1 || phantom[0] != "phantom-dog" {
		t.Errorf("expected exactly [phantom-dog] phantom, got %v", phantom)
	}

	// Sanity: when dogOrder + doc agree, both lists are empty.
	cleanOrder := []string{"git-hygiene", "db-vacuum"}
	cleanDoc := `## Inventory

- ` + "`git-hygiene`" + ` (30 min) — orphan cleanup.
- ` + "`db-vacuum`" + ` (6 h) — SQLite maintenance.

## Invariants
`
	cleanInv, err := extractInventorySection(cleanDoc)
	if err != nil {
		t.Fatalf("extract clean Inventory section: %v", err)
	}
	cleanMentioned := backtickedTokensIn(cleanInv)
	cleanSet := map[string]bool{"git-hygiene": true, "db-vacuum": true}
	cMissing, cPhantom := compareDogInventory(cleanOrder, cleanSet, cleanMentioned)
	if len(cMissing) != 0 || len(cPhantom) != 0 {
		t.Errorf("clean fixture should report zero drift; got missing=%v phantom=%v", cMissing, cPhantom)
	}
}

// parseDogOrderFromSource parses internal/agents/dogs.go and returns
// the string literal contents of the package-level `dogOrder` slice.
// AST-walks the file so we don't import the agents package (that would
// create a cycle: agents pulls in many internal packages, several of
// which in turn need the audittools test tree to be skipped to build).
func parseDogOrderFromSource(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var dogs []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			matched := false
			for _, name := range vs.Names {
				if name.Name == "dogOrder" {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			for _, val := range vs.Values {
				cl, ok := val.(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, el := range cl.Elts {
					lit, ok := el.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					// Strip surrounding quotes.
					unq := strings.Trim(lit.Value, "\"`")
					if unq != "" {
						dogs = append(dogs, unq)
					}
				}
			}
		}
	}
	if len(dogs) == 0 {
		return nil, fmt.Errorf("found no dogOrder var declaration in %s", path)
	}
	return dogs, nil
}

// extractInventorySection returns the substring of the doc body that
// lies between the "## Inventory" heading and the next "## " heading
// (or end-of-file, whichever comes first). The opening heading itself
// is excluded; the closing heading is excluded.
func extractInventorySection(doc string) (string, error) {
	idx := strings.Index(doc, inventoryHeading)
	if idx == -1 {
		return "", fmt.Errorf("docs/subsystems/dogs.md missing %q heading", inventoryHeading)
	}
	// Step past the heading line itself.
	rest := doc[idx+len(inventoryHeading):]
	// Find next H2 boundary. We look for "\n## " (with leading newline)
	// to avoid matching the inventoryHeading we just skipped or any
	// in-line "## " that happens to appear mid-paragraph.
	nextIdx := strings.Index(rest, "\n## ")
	if nextIdx == -1 {
		return rest, nil
	}
	return rest[:nextIdx], nil
}

// backtickedTokensIn returns the set of distinct backticked tokens in
// the supplied text that match the dog-name shape (alnum/dash/underscore).
func backtickedTokensIn(text string) map[string]bool {
	out := make(map[string]bool)
	for _, m := range backtickedToken.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}

// compareDogInventory cross-walks dogOrder against the set of tokens
// mentioned in the Inventory section. Returns:
//   - missing: dogs in dogOrder but not mentioned (sorted)
//   - phantom: tokens mentioned that look dog-name-shaped but are not
//     in dogOrder (sorted, filtered to multi-segment names — see below)
//
// The phantom filter requires at least one '-' in the token because
// the Inventory section legitimately quotes other identifiers like
// `dogCooldowns` and `dogOrder` (the wiring vars themselves) and SQL
// table names like `Dogs`. Every real dog name is multi-word with
// hyphens (e.g. `git-hygiene`, `spend-burn-watch`); enforcing that
// shape on the phantom side filters out unrelated quoted code tokens
// while still catching dog-shaped phantoms (renamed-but-not-removed
// bullets, typos, etc.).
func compareDogInventory(order []string, dogSet map[string]bool, mentioned map[string]bool) (missing, phantom []string) {
	for _, d := range order {
		if !mentioned[d] {
			missing = append(missing, d)
		}
	}
	for tok := range mentioned {
		if dogSet[tok] {
			continue
		}
		// Phantom filter: must look dog-name-shaped (contains a dash,
		// lowercase). Otherwise it's some other backticked code token
		// (e.g. `Dogs`, `dogCooldowns`).
		if !strings.Contains(tok, "-") {
			continue
		}
		if strings.ToLower(tok) != tok {
			continue
		}
		phantom = append(phantom, tok)
	}
	sort.Strings(missing)
	sort.Strings(phantom)
	return missing, phantom
}

// Package audittools: Pattern P23 — proposer write discipline.
//
// Roadmap reference: D3 § anti-cheat directive "No proposer mutation
// of archive/suppression state" (line 1300).
//
// Invariant: proposer code paths (Investigator, Captain mid-cycle
// amendment, Engineering Corps experiment wrap, ConvoyReview cross-
// classification, manual operator filing) only INSERT rows or use
// the dedup ON CONFLICT path. Direct writes to `archived_at`,
// `archive_reason`, or any column on `ProposedFeatureSuppressions`
// from a proposer code path fail this test. Only operator-routed
// handlers and the housekeeping dog may write archive state.
//
// Slice α of D3 fix-loop-1 authors this test as a SCAFFOLD that
// walks production Go source for SQL string literals matching
// archive/suppression mutations and reports any that originate
// from a proposer file (per the file-path classifier below).
// Today (pre-slice-β proposer write paths) production code has
// zero such mutations from proposer files — the test passes
// with zero offenders.
//
// The test is two-pronged:
//
//  1. ARCHIVE-WRITE CHECK. Any UPDATE / INSERT touching
//     ProposedFeatures.archived_at or ProposedFeatures.archive_reason
//     in a proposer file fails. Operator handlers
//     (`internal/dashboard/handlers_*`) and the housekeeping dog
//     (`internal/agents/*housekeeping*`) are the legitimate write
//     paths.
//
//  2. SUPPRESSION-WRITE CHECK. Any INSERT / UPDATE / DELETE on
//     ProposedFeatureSuppressions in a proposer file fails. The
//     suppression-CHECK at insert ingress is read-only against
//     this table; only operator-routed handlers may insert
//     suppression rules.
//
// Pattern P23 graduates to a BoS commit-time rule when D4 ships.
package audittools

import (
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

// p23ProposerFiles names the production files that ARE proposer
// code paths. Each file in this list is held to the proposer
// write-discipline contract; mutations to archive/suppression
// state from these files fail the test.
//
// The list captures all sources named in the roadmap directive:
// Investigator, Captain (mid-cycle amendment), Engineering Corps
// (experiment wrap), ConvoyReview (cross-classification).
//
// Slice α records the canonical proposer files. If slice β/δ
// renames or splits a proposer, the maintainer of the slice
// updates this list AND ensures the new file inherits the
// discipline.
var p23ProposerFiles = map[string]struct{}{
	"internal/agents/investigator.go":                          {},
	"internal/agents/captain.go":                               {},
	"internal/agents/convoy_review.go":                         {},
	"internal/agents/engineering_corps/experiment_author.go":   {},
	"internal/agents/engineering_corps/metric_author.go":       {},
	"internal/agents/engineering_corps/promotion_author.go":    {},
}

// p23ArchiveWriteRe matches SQL mutations touching the archive
// state on ProposedFeatures. Lenient by design — false positives
// surface for human review.
var p23ArchiveWriteRe = regexp.MustCompile(
	`(?is)(UPDATE\s+ProposedFeatures.*\b(archived_at|archive_reason)\b|INSERT\s+INTO\s+ProposedFeatures.*\b(archived_at|archive_reason)\b)`,
)

// p23SuppressionWriteRe matches any write to ProposedFeatureSuppressions.
var p23SuppressionWriteRe = regexp.MustCompile(
	`(?is)(INSERT\s+INTO\s+ProposedFeatureSuppressions|UPDATE\s+ProposedFeatureSuppressions|DELETE\s+FROM\s+ProposedFeatureSuppressions)`,
)

// TestPattern_P23_ProposerWriteDiscipline walks the proposer files
// and fails if any SQL string literal matches an archive-write or
// suppression-write shape.
func TestPattern_P23_ProposerWriteDiscipline(t *testing.T) {
	root := moduleRoot(t)

	type offender struct {
		file string
		line int
		text string
		why  string
	}
	var offenders []offender

	for relPath := range p23ProposerFiles {
		fullPath := filepath.Join(root, relPath)
		// If the proposer file doesn't exist yet (slice β hasn't
		// shipped, or the file was renamed), skip with a log line —
		// the test classifier in p23ProposerFiles is the canonical
		// list, but missing files are not offenders.
		if _, statErr := os.Stat(fullPath); statErr != nil {
			t.Logf("Pattern P23: proposer file %s not present in tree — skipping (slice β/δ may not have shipped it yet)", relPath)
			continue
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, fullPath, nil, 0)
		if parseErr != nil {
			t.Logf("Pattern P23: parse %s: %v (skipped)", relPath, parseErr)
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			raw := lit.Value
			val := strings.TrimPrefix(strings.TrimSuffix(raw, "`"), "`")
			val = strings.TrimPrefix(strings.TrimSuffix(val, `"`), `"`)

			if p23ArchiveWriteRe.MatchString(val) {
				pos := fset.Position(lit.Pos())
				preview := val
				if len(preview) > 200 {
					preview = preview[:200] + "…"
				}
				preview = strings.ReplaceAll(preview, "\n", " ")
				preview = strings.ReplaceAll(preview, "\t", " ")
				offenders = append(offenders, offender{
					file: relPath, line: pos.Line, text: preview,
					why: "archive-state write (archived_at / archive_reason) from a proposer file",
				})
			}
			if p23SuppressionWriteRe.MatchString(val) {
				pos := fset.Position(lit.Pos())
				preview := val
				if len(preview) > 200 {
					preview = preview[:200] + "…"
				}
				preview = strings.ReplaceAll(preview, "\n", " ")
				preview = strings.ReplaceAll(preview, "\t", " ")
				offenders = append(offenders, offender{
					file: relPath, line: pos.Line, text: preview,
					why: "suppression-table write from a proposer file",
				})
			}
			return true
		})
	}

	if len(offenders) == 0 {
		t.Logf("Pattern P23 (D3 anti-cheat): zero proposer-file writes to archive/suppression state. Proposer write discipline holds.")
		return
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].file != offenders[j].file {
			return offenders[i].file < offenders[j].file
		}
		return offenders[i].line < offenders[j].line
	})
	t.Errorf("Pattern P23 (D3 anti-cheat): %d proposer-file write(s) violate the write-discipline contract:", len(offenders))
	for _, o := range offenders {
		t.Errorf("  %s:%d — %s\n      preview: %s", o.file, o.line, o.why, o.text)
	}
	t.Errorf("\nFix: proposers only INSERT (or dedup ON CONFLICT). Archive state writes (archived_at, archive_reason) belong to the operator dashboard handler OR the proposed-features-housekeeping dog. Suppression writes are operator-only.")
}

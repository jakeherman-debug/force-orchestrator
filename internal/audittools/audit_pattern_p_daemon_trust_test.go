// Package audittools: Pattern P_DaemonTrustFile — `force daemon
// update` MUST gate binary rollover behind the trust file at
// ~/.force/trusted-binary-hashes, and the gate MUST default to
// paranoia mode (interactive confirmation required when the new
// binary's SHA is not already trusted).
//
// Three checks:
//
//  1. The trust package's DefaultPath returns a path ending in
//     "trusted-binary-hashes" under ~/.force/.
//
//  2. cmd/force/daemon_cmds.go's update path imports the trust
//     package and calls trust.Load + trust.Append.
//
//  3. The update flow's source contains the four operator-facing
//     diff hints (git log / git diff --stat / config drift / internal/
//     drift) and the literal "Trust this binary" prompt — the
//     operator-visible 4-diff preview required by D12 P1.
package audittools

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/daemon/trust"
)

// checkDaemonTrustFileSource asserts the daemon_cmds.go source body
// contains every literal token required by the D12 P1 trust-file
// contract: trust import, Load+Append calls, the 4-diff preview, the
// interactive prompt, and the --assume-yes / assumeYes pair.
//
// Returns nil when every required literal is present. Returns the
// first violation found otherwise. srcName is informational only.
func checkDaemonTrustFileSource(srcName, src string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcName, src, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", srcName, err)
	}
	wantImport := `"force-orchestrator/internal/daemon/trust"`
	hasImport := false
	for _, imp := range file.Imports {
		if imp.Path.Value == wantImport {
			hasImport = true
			break
		}
	}
	if !hasImport {
		return fmt.Errorf("%s does not import %s", srcName, wantImport)
	}
	// Scan body-only — strip import strings so the import line for
	// "force-orchestrator/internal/..." does not falsely satisfy the
	// "internal/" diff-hint check.
	body := stripImportLiterals(file, src)
	for _, want := range []string{"trust.Load(", "trust.Append("} {
		if !strings.Contains(body, want) {
			return fmt.Errorf("%s does not call %s — update flow is incomplete", srcName, want)
		}
	}
	for _, want := range []string{
		"git log",
		"git diff --stat",
		"config/*.yaml",
		"internal/",
	} {
		if !strings.Contains(body, want) {
			return fmt.Errorf("4-diff preview missing %q from update flow source (%s)", want, srcName)
		}
	}
	if !strings.Contains(body, "Trust this binary") {
		return fmt.Errorf("update flow missing the 'Trust this binary' interactive prompt — paranoia mode is the contract (%s)", srcName)
	}
	if !strings.Contains(body, "--assume-yes") {
		return fmt.Errorf("--assume-yes flag missing — required for non-interactive testing (%s)", srcName)
	}
	if !strings.Contains(body, "assumeYes") {
		return fmt.Errorf("assumeYes variable missing in update flow — interactive prompt cannot be skipped programmatically (%s)", srcName)
	}
	return nil
}

// stripImportLiterals returns src with every parsed import path
// literal replaced by an empty string. Used so the literal-token
// scans below don't accidentally match against an import path.
func stripImportLiterals(file *ast.File, src string) string {
	out := src
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		out = strings.ReplaceAll(out, imp.Path.Value, `""`)
	}
	return out
}

func TestPattern_P_DaemonTrustFile(t *testing.T) {
	// (1) DefaultPath shape.
	got := trust.DefaultPath()
	if !strings.HasSuffix(got, "trusted-binary-hashes") {
		t.Errorf("Pattern P_DaemonTrustFile: trust.DefaultPath() = %q, want suffix 'trusted-binary-hashes'", got)
	}
	// On any non-CI dev box HOME is set, so the path should contain
	// `.force`. We don't fail hard on that — the fallback to /tmp is
	// legitimate in CI — but log it.
	if home, _ := os.UserHomeDir(); home != "" && !strings.Contains(got, ".force") {
		t.Errorf("Pattern P_DaemonTrustFile: DefaultPath = %q does not contain '.force' on a HOME-having system", got)
	}

	// (2,3) Source-level checks on cmd/force/daemon_cmds.go.
	root := moduleRoot(t)
	target := filepath.Join(root, "cmd", "force", "daemon_cmds.go")
	srcBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if err := checkDaemonTrustFileSource(target, string(srcBytes)); err != nil {
		t.Errorf("Pattern P_DaemonTrustFile: %v", err)
	}
}

// TestPattern_P_DaemonTrustFile_DetectsInjectedDrift proves the source
// checker would actually fire when each contract clause is omitted.
// We feed checkDaemonTrustFileSource synthetic source that drops a
// single required token at a time and assert the matching error.
func TestPattern_P_DaemonTrustFile_DetectsInjectedDrift(t *testing.T) {
	// Compliant baseline that mentions every required token. Each
	// case below mutates the baseline to remove exactly one and
	// asserts the checker rejects it.
	// The baseline parses to a valid Go file AND mentions every
	// required token. Each removable token is on its own line as
	// the value of an entry in `_tokens`, so dropping one keeps
	// the file parseable. stripImportLiterals (in the production
	// checker) zeros out import paths before substring scanning so
	// the import path "force-orchestrator/internal/daemon/trust"
	// doesn't satisfy the "internal/" diff-hint check.
	baseline := `package force
import (
	_ "force-orchestrator/internal/daemon/trust"
)
var _tokens = []string{
	"trust.Load(",
	"trust.Append(",
	"git log",
	"git diff --stat",
	"config/*.yaml",
	"internal/",
	"Trust this binary",
	"--assume-yes",
}
var assumeYes bool
`
	cases := []struct {
		name    string
		drop    string
		wantSub string
	}{
		// Importer note: the "missing-import" case drops the trust
		// import line; the rest leave it in place.
		{"missing-import", "\t_ \"force-orchestrator/internal/daemon/trust\"\n", "does not import"},
		{"missing-load", "\t\"trust.Load(\",\n", "trust.Load("},
		{"missing-append", "\t\"trust.Append(\",\n", "trust.Append("},
		{"missing-git-log", "\t\"git log\",\n", "git log"},
		{"missing-git-diff-stat", "\t\"git diff --stat\",\n", "git diff --stat"},
		{"missing-config-yaml", "\t\"config/*.yaml\",\n", "config/*.yaml"},
		{"missing-internal", "\t\"internal/\",\n", "internal/"},
		{"missing-trust-prompt", "\t\"Trust this binary\",\n", "Trust this binary"},
		{"missing-assume-yes-flag", "\t\"--assume-yes\",\n", "--assume-yes"},
		{"missing-assumeYes-var", "var assumeYes bool\n", "assumeYes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(baseline, tc.drop, "", 1)
			if src == baseline {
				t.Fatalf("baseline did not contain %q — fixture broken", tc.drop)
			}
			err := checkDaemonTrustFileSource("synthetic.go", src)
			if err == nil {
				t.Fatalf("checker accepted source missing %q", tc.drop)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
	// Positive control.
	if err := checkDaemonTrustFileSource("synthetic.go", baseline); err != nil {
		t.Fatalf("checker rejected compliant baseline: %v", err)
	}
}

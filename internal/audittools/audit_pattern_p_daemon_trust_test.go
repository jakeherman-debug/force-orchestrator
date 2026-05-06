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
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/daemon/trust"
)

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
	src := string(srcBytes)

	// AST: imports trust package.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
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
		t.Fatalf("Pattern P_DaemonTrustFile: %s does not import %s", target, wantImport)
	}

	// trust.Load + trust.Append must both appear in the file.
	for _, want := range []string{"trust.Load(", "trust.Append("} {
		if !strings.Contains(src, want) {
			t.Errorf("Pattern P_DaemonTrustFile: %s does not call %s — update flow is incomplete", target, want)
		}
	}

	// 4-diff preview: each must show up in the source so an operator
	// running `force daemon update` against an untrusted SHA sees them.
	for _, want := range []string{
		"git log",
		"git diff --stat",
		"config/*.yaml",
		"internal/",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("Pattern P_DaemonTrustFile: 4-diff preview missing %q from update flow source", want)
		}
	}

	// Interactive paranoia gate.
	if !strings.Contains(src, "Trust this binary") {
		t.Errorf("Pattern P_DaemonTrustFile: update flow missing the 'Trust this binary' interactive prompt — paranoia mode is the contract")
	}

	// `--assume-yes` flag exists for testability, but the human path
	// must default to interactive (i.e. the source must check the flag
	// and only skip the prompt when set).
	if !strings.Contains(src, "--assume-yes") {
		t.Errorf("Pattern P_DaemonTrustFile: --assume-yes flag missing — required for non-interactive testing")
	}
	if !strings.Contains(src, "assumeYes") {
		t.Errorf("Pattern P_DaemonTrustFile: assumeYes variable missing in update flow — interactive prompt cannot be skipped programmatically")
	}
}

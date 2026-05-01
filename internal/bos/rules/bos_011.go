package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-011 — ClientsInterfaces (graduates D0 Pattern P16 to commit-time)
//
// CLAUDE.md anchor: "Cross-agent service interfaces".
//
// Rejects production agent files (internal/agents/*.go non-_test) that
// instantiate a concrete client struct from an `internal/clients/<svc>/`
// package via a composite literal — the same shape Pattern P16 has
// always rejected at CI-time. Mirrors the existing P16 AST walk in
// internal/audittools/audit_pattern_p16_clients_interfaces_test.go,
// graduating it to the commit-time gate (one step earlier in the
// pipeline).
//
// SEVERITY: BLOCK. This is the sole D4-P1 rule that ships at block-
// severity at launch — that's intentional, and documented in
// docs/roadmap.md § D4: BOS-011 graduates an already-CI-enforced check,
// so its precision is already known to be 100% (zero false positives
// observed since D0). New rules ship at advise; only rules that
// graduate from a separately-validated enforcement may ship at block.
type bos011 struct{}

func (bos011) ID() string             { return "BOS-011" }
func (bos011) CLAUDEMDAnchor() string { return "Cross-agent service interfaces" }
func (bos011) Severity() bos.Severity { return bos.SeverityBlock }

const clientsPkgPrefix = "force-orchestrator/internal/clients/"

func (bos011) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	// Scope: production code under internal/agents/, excluding tests.
	if !strings.Contains(path, "internal/agents") || strings.HasSuffix(path, "_test.go") {
		return nil
	}

	// Map import alias → service name for clients/<svc>/ imports.
	clientsImports := map[string]string{}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasPrefix(p, clientsPkgPrefix) {
			continue
		}
		svc := strings.TrimPrefix(p, clientsPkgPrefix)
		alias := svc
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		clientsImports[alias] = svc
	}
	if len(clientsImports) == 0 {
		return nil
	}

	var out []bos.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, isClientsPkg := clientsImports[pkgIdent.Name]; !isClientsPkg {
			return true
		}
		if !strings.HasSuffix(sel.Sel.Name, "Client") {
			return true
		}
		out = append(out, bos.Finding{
			RuleID:   "BOS-011",
			Severity: bos.SeverityBlock,
			Path:     path,
			Line:     positionLine(cl),
			Message:  "agent file constructs concrete client &" + pkgIdent.Name + "." + sel.Sel.Name + "{...}; use the package's NewInProcess/NewGRPC/NewMock factory instead per CLAUDE.md Cross-agent service interfaces",
		})
		return true
	})
	return out
}

func init() { bos.Register(bos011{}) }

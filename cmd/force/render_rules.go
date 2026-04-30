package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// cmdRenderRules implements `force render-rules [--check]`.
//
// Without --check: bootstrap-then-render. Reads the in-process
// FleetRules audit, ensures the FleetRules table is populated
// (idempotent), then writes CLAUDE.md / FIX-LOG.md / per-domain docs
// to disk. No-op when nothing changed.
//
// With --check: render every target in memory and compare to disk.
// Exit code 0 = no drift; exit code 1 = drift detected (lists the
// drifted paths). Used by the pre-commit hook to refuse hand-edits to
// auto-generated files.
func cmdRenderRules(ctx context.Context, db *sql.DB, args []string) {
	check := false
	// D3-P1 follow-up C: FIX-LOG.md is now rendered + drift-checked by
	// default. The audit covers every ## Fix #N narrative; the legacy
	// `--include-fix-log` flag is retained as a no-op for ergonomic
	// continuity (operators / scripts that pass it still work). The
	// new `--skip-fix-log` flag is the escape hatch if a future
	// situation calls for a partial render.
	includeFixLog := true
	for _, a := range args {
		switch a {
		case "--check":
			check = true
		case "--include-fix-log":
			// Now the default. Kept as a no-op so older invocations
			// don't trip on "unknown flag".
			includeFixLog = true
		case "--skip-fix-log":
			includeFixLog = false
		case "-h", "--help":
			fmt.Println("Usage: force render-rules [--check] [--skip-fix-log]")
			fmt.Println("  Without flags    : bootstraps FleetRules + renders CLAUDE.md + FIX-LOG.md + docs/* from the audit.")
			fmt.Println("  --check          : renders to memory and exits 1 if any on-disk file disagrees (drift detector).")
			fmt.Println("  --skip-fix-log   : skip FIX-LOG.md rendering on this invocation (useful for partial renders).")
			fmt.Println("  --include-fix-log: legacy no-op (now the default; flag retained for back-compat).")
			return
		default:
			fmt.Fprintf(os.Stderr, "render-rules: unknown flag %q\nUsage: force render-rules [--check] [--skip-fix-log]\n", a)
			os.Exit(2)
		}
	}

	repoRoot := findRepoRoot()

	// Bootstrap is idempotent — safe to run on every render. Pass empty
	// path to skip the all-sections-covered check; that guard is a
	// dev-time regression captured by the test suite, not a runtime
	// invariant. After Phase 3 renders CLAUDE.md, the on-disk file's
	// H2 headings are category labels, not the audit's original section
	// names — running the check at runtime would always trip.
	if _, err := store.BootstrapFleetRules(ctx, db, ""); err != nil {
		fmt.Fprintf(os.Stderr, "render-rules: bootstrap: %v\n", err)
		os.Exit(1)
	}

	if check {
		diverged, err := agents.CheckRenderDrift(ctx, db, repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "render-rules --check: %v\n", err)
			os.Exit(1)
		}
		if len(diverged) > 0 {
			fmt.Fprintln(os.Stderr, "render-rules --check: DRIFT detected. The following file(s) disagree with the rendered output:")
			for _, p := range diverged {
				fmt.Fprintf(os.Stderr, "  %s\n", p)
			}
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "These files are auto-generated from FleetRules. Either:")
			fmt.Fprintln(os.Stderr, "  - run `make render-rules` to regenerate, OR")
			fmt.Fprintln(os.Stderr, "  - if you want to change content, edit the FleetRules row in")
			fmt.Fprintln(os.Stderr, "    internal/store/fleet_rules_audit.go and re-render.")
			os.Exit(1)
		}
		fmt.Println("render-rules --check: OK (no drift)")
		return
	}

	// CLAUDE.md
	n, changed, err := agents.WriteRenderedClaudeMd(ctx, db, filepath.Join(repoRoot, "CLAUDE.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-rules: CLAUDE.md: %v\n", err)
		os.Exit(1)
	}
	report("CLAUDE.md", n, changed)

	// FIX-LOG.md — included by default (D3-P1 follow-up C). Operators
	// who want a partial render can pass --skip-fix-log.
	if includeFixLog {
		n, changed, err = agents.WriteRenderedFixLog(ctx, db, filepath.Join(repoRoot, "FIX-LOG.md"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "render-rules: FIX-LOG.md: %v\n", err)
			os.Exit(1)
		}
		report("FIX-LOG.md", n, changed)
	} else {
		fmt.Println("  skip    FIX-LOG.md (--skip-fix-log requested)")
	}

	// Per-domain docs
	sizes, changedPaths, err := agents.WriteRenderedPerDomainDocs(ctx, db, repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-rules: per-domain: %v\n", err)
		os.Exit(1)
	}
	keys := make([]string, 0, len(sizes))
	for k := range sizes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	changedSet := map[string]bool{}
	for _, p := range changedPaths {
		changedSet[p] = true
	}
	for _, k := range keys {
		report(k, sizes[k], changedSet[k])
	}
}

func report(path string, n int, changed bool) {
	if changed {
		fmt.Printf("  wrote   %s (%d bytes)\n", path, n)
	} else {
		fmt.Printf("  no-op   %s (%d bytes; identical)\n", path, n)
	}
}

// findRepoRoot returns the absolute path to the repo root by walking up
// from the current binary's source location looking for go.mod. Falls
// back to the current working directory.
func findRepoRoot() string {
	// Try CWD first — `force` is typically invoked from the repo root.
	if cwd, err := os.Getwd(); err == nil {
		if walked := walkForGoMod(cwd); walked != "" {
			return walked
		}
	}
	// Fall back to source-relative.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func walkForGoMod(start string) string {
	dir := start
	for i := 0; i < 16; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// cmdRenderRules implements `force render-rules [--check] [--use-runtime-db]`.
//
// Without --check: bootstrap-then-render. Reads the in-process
// FleetRules audit, ensures the FleetRules table is populated
// (convergent), then writes CLAUDE.md / FIX-LOG.md / per-domain docs
// to disk. No-op when nothing changed.
//
// With --check: render every target in memory and compare to disk.
// Exit code 0 = no drift; exit code 1 = drift detected (lists the
// drifted paths). Used by the pre-commit hook to refuse hand-edits to
// auto-generated files.
//
// The CLI's job is "render the audit slice." It must not depend on
// operator-side persistent DB state — every invocation should produce
// deterministic output regardless of any local holocron.db drift. So
// the default opens a fresh :memory: SQLite, runs schema migrations,
// runs (convergent) BootstrapFleetRules, then renders. The passed-in
// `db` (the daemon-shared persistent holocron.db) is used only when
// the operator explicitly asks via --use-runtime-db — typically to
// inspect renders that include operator-direct-write rules.
func cmdRenderRules(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("render-rules", flag.ContinueOnError)
	checkFlag := fs.Bool("check", false, "renders to memory and exits 1 if any on-disk file disagrees (drift detector)")
	useRuntimeDBFlag := fs.Bool("use-runtime-db", false, "use the persistent holocron.db instead of a fresh in-memory DB")
	includeFixLogFlag := fs.Bool("include-fix-log", true, "legacy no-op (now the default)")
	skipFixLogFlag := fs.Bool("skip-fix-log", false, "skip FIX-LOG.md rendering on this invocation")
	helped, perr := parseSubcommandFlags(fs, args, "render-rules",
		"Bootstrap FleetRules into a fresh in-memory DB + render CLAUDE.md + FIX-LOG.md + docs/* from the audit. --check exits non-zero on drift.",
		[]flagDoc{
			{Name: "--check", Desc: "exit 1 if any on-disk file disagrees with the rendered output"},
			{Name: "--use-runtime-db", Desc: "use the persistent holocron.db"},
			{Name: "--skip-fix-log", Desc: "skip FIX-LOG.md rendering on this invocation"},
			{Name: "--include-fix-log", Desc: "legacy no-op (now the default)"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force render-rules", "force render-rules --check"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	check := *checkFlag
	useRuntimeDB := *useRuntimeDBFlag
	includeFixLog := *includeFixLogFlag
	if *skipFixLogFlag {
		includeFixLog = false
	}

	repoRoot := findRepoRoot()

	// DB selection. Default: fresh :memory: — deterministic output,
	// independent of operator-side DB state. Opt-in: the persistent
	// holocron.db wired by main(). The fresh-DB path is the same code
	// path TestPattern_P18_RenderCoherence uses, just behind a CLI
	// entry point; convergent BootstrapFleetRules is what makes the
	// fresh-DB render produce identical output to the runtime DB on
	// subsequent invocations.
	renderDB := db
	if !useRuntimeDB {
		fresh := store.InitHolocronDSN(":memory:")
		defer fresh.Close()
		renderDB = fresh
	}

	// Bootstrap is convergent — safe to run on every render. Pass empty
	// path to skip the all-sections-covered check; that guard is a
	// dev-time regression captured by the test suite, not a runtime
	// invariant. After Phase 3 renders CLAUDE.md, the on-disk file's
	// H2 headings are category labels, not the audit's original section
	// names — running the check at runtime would always trip.
	if _, err := store.BootstrapFleetRules(ctx, renderDB, ""); err != nil {
		fmt.Fprintf(os.Stderr, "render-rules: bootstrap: %v\n", err)
		os.Exit(1)
	}

	if check {
		diverged, err := agents.CheckRenderDrift(ctx, renderDB, repoRoot)
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
	n, changed, err := agents.WriteRenderedClaudeMd(ctx, renderDB, filepath.Join(repoRoot, "CLAUDE.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-rules: CLAUDE.md: %v\n", err)
		os.Exit(1)
	}
	report("CLAUDE.md", n, changed)

	// FIX-LOG.md — included by default (D3-P1 follow-up C). Operators
	// who want a partial render can pass --skip-fix-log.
	if includeFixLog {
		n, changed, err = agents.WriteRenderedFixLog(ctx, renderDB, filepath.Join(repoRoot, "FIX-LOG.md"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "render-rules: FIX-LOG.md: %v\n", err)
			os.Exit(1)
		}
		report("FIX-LOG.md", n, changed)
	} else {
		fmt.Println("  skip    FIX-LOG.md (--skip-fix-log requested)")
	}

	// SENATE.md — per-repo Senator memory export. Empty rule set is
	// the steady state until the first PromotionProposal ratifies
	// (Pattern P34: ratification is operator-gated). The writer
	// reports (0, false, nil) in that case and we skip the report.
	n, changed, err = agents.WriteRenderedSenateMd(ctx, renderDB, filepath.Join(repoRoot, "SENATE.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-rules: SENATE.md: %v\n", err)
		os.Exit(1)
	}
	if n > 0 {
		report("SENATE.md", n, changed)
	}

	// Per-domain docs
	sizes, changedPaths, err := agents.WriteRenderedPerDomainDocs(ctx, renderDB, repoRoot)
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

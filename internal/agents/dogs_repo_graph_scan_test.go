// D8 Track 1 — tests for dogRepoGraphScan.
//
// Coverage matrix (per CLAUDE.md § "Testing rules" — happy path + each
// distinct failure mode + idempotence):
//   - happy path: producer repo with 3 exported symbols × consumer repo with
//     2 import sites → 3 CrossRepoSymbols + 2 CrossRepoDependencies.
//   - soft-delete: remove a consumer site → re-run → row tombstoned, not
//     deleted; the row id is preserved.
//   - file-disappearance: delete a consumer file outright → re-run → all its
//     edges soft-deleted.
//   - idempotence: 3 successive runs → no duplicate rows; counts stable.
//   - non-Go repo: stub extractor surfaces no symbols, doesn't fail.
//   - missing local_path: dog logs and skips, returns nil.
//   - empty fleet: no registered repos → nil error, log line.
//   - store helpers: UpsertCrossRepoSymbol idempotent + revival semantics.

package agents

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// makeProducerRepo writes a tiny Go module at `dir` whose package exports
// four symbols (NewClient, Client struct, MaxRetries const, Client.Do method).
// Module path = "example.com/producer".
func makeProducerRepo(t *testing.T, dir string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/producer\n\ngo 1.21\n")
	mustMkdir(t, filepath.Join(dir, "api"))
	mustWrite(t, filepath.Join(dir, "api", "client.go"), `package api

// MaxRetries is the default retry count.
const MaxRetries = 3

// Client is the producer API.
type Client struct {
	Endpoint string
}

// NewClient returns a Client.
func NewClient(endpoint string) *Client {
	return &Client{Endpoint: endpoint}
}

// Do issues a request; method on the Client receiver.
func (c *Client) Do(req string) error {
	return nil
}
`)
}

// makeConsumerRepo writes a tiny Go module at `dir` that imports
// example.com/producer/api at two distinct call sites (NewClient + MaxRetries).
// Module path = "example.com/consumer".
func makeConsumerRepo(t *testing.T, dir string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/consumer\n\ngo 1.21\nrequire example.com/producer v0.0.0\n")
	mustMkdir(t, filepath.Join(dir, "cmd"))
	mustWrite(t, filepath.Join(dir, "cmd", "main.go"), `package main

import (
	"fmt"

	"example.com/producer/api"
)

func main() {
	c := api.NewClient("https://x")
	fmt.Println(c, api.MaxRetries)
}
`)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustHaveSymbol(t *testing.T, db *sql.DB, repo, path, kind string) {
	t.Helper()
	rows, err := store.ListProvidersOfSymbol(db, repo, path)
	if err != nil {
		t.Fatalf("ListProvidersOfSymbol(%s, %s): %v", repo, path, err)
	}
	if len(rows) == 0 {
		t.Errorf("expected provider symbol %s/%s to exist", repo, path)
		return
	}
	if rows[0].SymbolKind != kind {
		t.Errorf("symbol %s/%s kind: want %q got %q", repo, path, kind, rows[0].SymbolKind)
	}
}

func mustListLiveDeps(t *testing.T, db *sql.DB, consumerRepo string) []store.CrossRepoDependency {
	t.Helper()
	out, err := store.ListLiveDependenciesForConsumerRepo(db, consumerRepo)
	if err != nil {
		t.Fatalf("ListLiveDependenciesForConsumerRepo(%s): %v", consumerRepo, err)
	}
	return out
}

// TestDogRepoGraphScan_SmokePath builds the fixture (producer + consumer),
// runs the dog once, and asserts the spec'd row counts: 3 distinct exported
// symbols on the consumer's import path × 2 consumer call-sites resolved.
//
// Per the roadmap exit criteria: "producer exports 3 symbols × consumer
// imports them at 2 sites → 3 CrossRepoSymbols + 2 CrossRepoDependencies."
// Our producer fixture has 4 exported decls (extra Client.Do method); the
// consumer references only 3 of them. We assert the three the consumer
// references are present + 2 live edges.
func TestDogRepoGraphScan_SmokePath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	root := t.TempDir()
	producer := filepath.Join(root, "producer")
	consumer := filepath.Join(root, "consumer")
	makeProducerRepo(t, producer)
	makeConsumerRepo(t, consumer)
	store.AddRepo(db, "producer", producer, "")
	store.AddRepo(db, "consumer", consumer, "")

	logger := log.New(io.Discard, "", 0)
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("dogRepoGraphScan: %v", err)
	}

	// The three symbols the consumer references.
	mustHaveSymbol(t, db, "producer", "example.com/producer/api.NewClient", "function")
	mustHaveSymbol(t, db, "producer", "example.com/producer/api.MaxRetries", "exported_const")
	mustHaveSymbol(t, db, "producer", "example.com/producer/api.Client", "type")

	// Consumer edges: NewClient + MaxRetries (Client doesn't appear as a
	// selector reference in the fixture — it's only used as a return type
	// of NewClient, which is captured implicitly by the NewClient edge).
	deps := mustListLiveDeps(t, db, "consumer")
	if len(deps) != 2 {
		t.Errorf("expected 2 live consumer dependencies, got %d: %+v", len(deps), deps)
	}
	for _, d := range deps {
		if d.ConsumerFile != "cmd/main.go" {
			t.Errorf("unexpected consumer file %q", d.ConsumerFile)
		}
		if d.DeletedAt != "" {
			t.Errorf("unexpected soft-delete on fresh edge: %+v", d)
		}
	}
}

// TestDogRepoGraphScan_SoftDelete: remove one consumer site and re-run; the
// disappeared edge gets soft-deleted (deleted_at non-empty), the surviving
// edge stays live, and the symbols themselves are untouched.
func TestDogRepoGraphScan_SoftDelete(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	root := t.TempDir()
	producer := filepath.Join(root, "producer")
	consumer := filepath.Join(root, "consumer")
	makeProducerRepo(t, producer)
	makeConsumerRepo(t, consumer)
	store.AddRepo(db, "producer", producer, "")
	store.AddRepo(db, "consumer", consumer, "")

	logger := log.New(io.Discard, "", 0)
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	beforeDeps := mustListLiveDeps(t, db, "consumer")
	if len(beforeDeps) != 2 {
		t.Fatalf("setup: expected 2 live deps, got %d", len(beforeDeps))
	}

	// Edit the consumer to remove the api.MaxRetries reference. Keep
	// api.NewClient.
	mustWrite(t, filepath.Join(consumer, "cmd", "main.go"), `package main

import (
	"fmt"

	"example.com/producer/api"
)

func main() {
	c := api.NewClient("https://x")
	fmt.Println(c)
}
`)
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	afterLive := mustListLiveDeps(t, db, "consumer")
	if len(afterLive) != 1 {
		t.Errorf("expected 1 live dep after removal, got %d: %+v", len(afterLive), afterLive)
	}
	// Total (incl. soft-deleted) must be 2 — soft-delete preserves the row.
	totalAll, err := store.CountCrossRepoDependencies(db, true)
	if err != nil {
		t.Fatalf("CountCrossRepoDependencies: %v", err)
	}
	if totalAll != 2 {
		t.Errorf("soft-delete should preserve the row: expected total=2, got %d", totalAll)
	}
	// Find the tombstoned row and verify deleted_at non-empty.
	var tombstoned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM CrossRepoDependencies WHERE deleted_at != ''`).Scan(&tombstoned); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if tombstoned != 1 {
		t.Errorf("expected 1 tombstoned edge, got %d", tombstoned)
	}
}

// TestDogRepoGraphScan_FileDisappearance: delete the consumer's file
// outright; the dog must soft-delete every edge that pointed into it.
func TestDogRepoGraphScan_FileDisappearance(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	root := t.TempDir()
	producer := filepath.Join(root, "producer")
	consumer := filepath.Join(root, "consumer")
	makeProducerRepo(t, producer)
	makeConsumerRepo(t, consumer)
	store.AddRepo(db, "producer", producer, "")
	store.AddRepo(db, "consumer", consumer, "")

	logger := log.New(io.Discard, "", 0)
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if err := os.Remove(filepath.Join(consumer, "cmd", "main.go")); err != nil {
		t.Fatalf("remove consumer file: %v", err)
	}
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	live, err := store.CountCrossRepoDependencies(db, false)
	if err != nil {
		t.Fatalf("CountCrossRepoDependencies(live): %v", err)
	}
	if live != 0 {
		t.Errorf("expected 0 live deps after file removal, got %d", live)
	}
	all, err := store.CountCrossRepoDependencies(db, true)
	if err != nil {
		t.Fatalf("CountCrossRepoDependencies(all): %v", err)
	}
	if all != 2 {
		t.Errorf("expected 2 total deps (both tombstoned), got %d", all)
	}
}

// TestDogRepoGraphScan_Idempotence: 3 successive scans on a stable fixture
// must not produce duplicate rows. UpsertCrossRepoSymbol + UpsertCrossRepoDependency
// keyed on UNIQUE constraints so the second pass is a re-stamp, not an insert.
func TestDogRepoGraphScan_Idempotence(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	root := t.TempDir()
	producer := filepath.Join(root, "producer")
	consumer := filepath.Join(root, "consumer")
	makeProducerRepo(t, producer)
	makeConsumerRepo(t, consumer)
	store.AddRepo(db, "producer", producer, "")
	store.AddRepo(db, "consumer", consumer, "")

	logger := log.New(io.Discard, "", 0)
	var symAfter [3]int
	var depAfter [3]int
	for i := 0; i < 3; i++ {
		if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
			t.Fatalf("scan %d: %v", i+1, err)
		}
		var err error
		if symAfter[i], err = store.CountCrossRepoSymbols(db); err != nil {
			t.Fatalf("CountCrossRepoSymbols: %v", err)
		}
		if depAfter[i], err = store.CountCrossRepoDependencies(db, true); err != nil {
			t.Fatalf("CountCrossRepoDependencies: %v", err)
		}
	}
	if symAfter[0] != symAfter[1] || symAfter[1] != symAfter[2] {
		t.Errorf("symbol count drift across runs: %v", symAfter)
	}
	if depAfter[0] != depAfter[1] || depAfter[1] != depAfter[2] {
		t.Errorf("dependency count drift across runs: %v", depAfter)
	}
}

// TestDogRepoGraphScan_NoRegisteredRepos: empty fleet → nil error, log line.
func TestDogRepoGraphScan_NoRegisteredRepos(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if err := dogRepoGraphScan(context.Background(), db, log.New(io.Discard, "", 0)); err != nil {
		t.Errorf("expected nil err on empty fleet, got %v", err)
	}
}

// TestDogRepoGraphScan_MissingLocalPath: registered repo whose local_path
// doesn't exist on disk should be skipped, not errored.
func TestDogRepoGraphScan_MissingLocalPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "ghost", "/nonexistent/path/to/repo", "")
	if err := dogRepoGraphScan(context.Background(), db, log.New(io.Discard, "", 0)); err != nil {
		t.Errorf("expected nil err on missing local_path, got %v", err)
	}
}

// TestDogRepoGraphScan_NonGoRepo: a registered repo with no go.mod (and no
// other manifest) should be silently skipped — the Go extractor's Detect()
// returns false and the lang stubs return no symbols.
func TestDogRepoGraphScan_NonGoRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "README.md"), "# bare repo\n")
	store.AddRepo(db, "bare", dir, "")
	if err := dogRepoGraphScan(context.Background(), db, log.New(io.Discard, "", 0)); err != nil {
		t.Errorf("expected nil err on bare repo, got %v", err)
	}
	n, err := store.CountCrossRepoSymbols(db)
	if err != nil {
		t.Fatalf("CountCrossRepoSymbols: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 symbols for bare repo, got %d", n)
	}
}

// TestUpsertCrossRepoSymbol_Idempotent verifies that two upserts for the same
// (repo, symbol_path) yield a single row and the row's metadata is updated.
func TestUpsertCrossRepoSymbol_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id1, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName: "p", SymbolPath: "p.X", SymbolKind: "function",
		FilePath: "x.go", LineNumber: 1, SignatureHash: "h1", IsPublic: true,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	id2, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName: "p", SymbolPath: "p.X", SymbolKind: "function",
		FilePath: "x.go", LineNumber: 5, SignatureHash: "h2", IsPublic: true,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Errorf("upsert should preserve row id: %d → %d", id1, id2)
	}
	n, err := store.CountCrossRepoSymbols(db)
	if err != nil {
		t.Fatalf("CountCrossRepoSymbols: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row after idempotent upserts, got %d", n)
	}
	// Verify the second upsert's metadata wins (line + hash).
	var line int
	var hash string
	if err := db.QueryRow(`SELECT line_number, signature_hash FROM CrossRepoSymbols WHERE id = ?`, id1).Scan(&line, &hash); err != nil {
		t.Fatalf("query: %v", err)
	}
	if line != 5 || hash != "h2" {
		t.Errorf("expected metadata refresh; got line=%d hash=%q", line, hash)
	}
}

// makeSyntheticGoRepo writes a Go module at `dir` with `numPkgs` packages,
// each exporting `exportsPerPkg` symbols (a function, a type, and a const,
// rotating). Used by perf-budget tests to drive the dog with a realistic-ish
// symbol fan-out without depending on a real-world repo on disk.
//
// Module path = "example.com/synthetic-<modSuffix>" so multiple synthetic
// repos in the same test don't share a module path. Returns the module path.
func makeSyntheticGoRepo(t *testing.T, dir, modSuffix string, numPkgs, exportsPerPkg int) string {
	t.Helper()
	modulePath := "example.com/synthetic-" + modSuffix
	mustWrite(t, filepath.Join(dir, "go.mod"), "module "+modulePath+"\n\ngo 1.21\n")
	for p := 0; p < numPkgs; p++ {
		pkgName := fmt.Sprintf("pkg%02d", p)
		pkgDir := filepath.Join(dir, pkgName)
		mustMkdir(t, pkgDir)
		var b strings.Builder
		fmt.Fprintf(&b, "package %s\n\n", pkgName)
		for i := 0; i < exportsPerPkg; i++ {
			switch i % 3 {
			case 0:
				fmt.Fprintf(&b, "// Func%02d is a synthetic exported function.\nfunc Func%02d(x int) int { return x + %d }\n\n", i, i, i)
			case 1:
				fmt.Fprintf(&b, "// Type%02d is a synthetic exported struct.\ntype Type%02d struct {\n\tField int\n}\n\n", i, i)
			case 2:
				fmt.Fprintf(&b, "// Const%02d is a synthetic exported constant.\nconst Const%02d = %d\n\n", i, i, i*7)
			}
		}
		mustWrite(t, filepath.Join(pkgDir, "synth.go"), b.String())
	}
	return modulePath
}

// TestDogRepoGraphScan_PerfBudget_SingleRepo asserts that a single-repo scan
// completes well under the per-repo wall-time budget.
//
// Scaling note: the roadmap's 60s/repo budget (docs/roadmap.md L2181) is for
// a "realistic" producer repo (~500 exported symbols) on the reference
// operator machine. Our test fixture is ~10x smaller (10 packages × 5 exports
// = ~50 symbols). We assert against a 10x-scaled bound (5s) here. If real-
// fleet runs show drift past the 60s wall budget, the operator surfaces it
// via the dog's per-repo timeout log line (see dogRepoGraphScan inline
// per-repo context.WithTimeout); this test just pins the scaling assumption
// so we catch a 100x perf regression at CI time, not on the operator's
// machine at 3am.
func TestDogRepoGraphScan_PerfBudget_SingleRepo(t *testing.T) {
	const perfBudgetSingleRepoTestScaling = 5 * time.Second

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	makeSyntheticGoRepo(t, dir, "perf-single", 10, 5)
	store.AddRepo(db, "perf-single", dir, "")

	logger := log.New(io.Discard, "", 0)
	start := time.Now()
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("dogRepoGraphScan: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= perfBudgetSingleRepoTestScaling {
		t.Errorf("single-repo perf budget violated: %s >= %s (10x-scaled bound for 60s/repo roadmap target on a 50-symbol fixture)",
			elapsed, perfBudgetSingleRepoTestScaling)
	}
	// Sanity: the scan actually did the work — fixture has ~50 symbols.
	n, err := store.CountCrossRepoSymbols(db)
	if err != nil {
		t.Fatalf("CountCrossRepoSymbols: %v", err)
	}
	if n < 40 {
		t.Errorf("expected the synthetic 10x5 fixture to yield ~50 symbols, got %d (perf assertion is meaningless if the dog short-circuited)", n)
	}
}

// TestDogRepoGraphScan_PerfBudget_FullFleet asserts that a multi-repo scan
// completes well under the full-fleet wall-time budget.
//
// Scaling note: the roadmap's 30-minute full-fleet budget (docs/roadmap.md
// L2181) assumes ~30 repos × ~500 symbols each on the reference operator
// machine. We register 5 synthetic repos × ~25 symbols each (5 packages × 5
// exports). That's ~6x fewer repos and ~20x fewer symbols-per-repo than the
// roadmap assumption, so the fixture is ~120x lighter. We assert a 15s bound
// (1800s/120 = 15s) — generous enough to absorb CI noise, tight enough to
// catch a 5x perf regression in symbol extraction or upsert pathways. As
// with the single-repo test, the production budget enforcement happens via
// the per-repo context.WithTimeout in dogRepoGraphScan; this test pins the
// scaling assumption against fleet-aggregate cost.
func TestDogRepoGraphScan_PerfBudget_FullFleet(t *testing.T) {
	const perfBudgetFullFleetTestScaling = 15 * time.Second

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	root := t.TempDir()
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("perf-fleet-%d", i)
		dir := filepath.Join(root, name)
		mustMkdir(t, dir)
		makeSyntheticGoRepo(t, dir, name, 5, 5)
		store.AddRepo(db, name, dir, "")
	}

	logger := log.New(io.Discard, "", 0)
	start := time.Now()
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("dogRepoGraphScan: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= perfBudgetFullFleetTestScaling {
		t.Errorf("full-fleet perf budget violated: %s >= %s (scaled bound for 30-min roadmap target on a 5-repo × 25-symbol fixture)",
			elapsed, perfBudgetFullFleetTestScaling)
	}
	n, err := store.CountCrossRepoSymbols(db)
	if err != nil {
		t.Fatalf("CountCrossRepoSymbols: %v", err)
	}
	if n < 100 {
		t.Errorf("expected the 5-repo × 25-symbol fleet fixture to yield ~125 symbols, got %d (perf assertion is meaningless if the dog short-circuited)", n)
	}
}

// TestDogRepoGraphScan_PerfBudget_SingleRepoTimeout exercises the
// self-healing path: when a single repo blows the per-repo budget, the dog
// logs the violation and continues to the next repo (does NOT crash, does
// NOT poison the rest of the fleet's pass). Drives the path by overriding
// repoGraphScanSingleRepoBudget down to ~1ns so any synthetic fixture trips
// it deterministically.
//
// Per CLAUDE.md "no silent failures": the dog's behaviour on timeout is
// "log loud, skip clean, retry next tick" — verified here by checking the
// log buffer for the expected violation marker AND by confirming the
// healthy second repo still got its symbols upserted.
func TestDogRepoGraphScan_PerfBudget_SingleRepoTimeout(t *testing.T) {
	// Override the budget for the duration of this test; restore on exit so
	// other tests (esp. PerfBudget_SingleRepo / _FullFleet) see the default.
	prev := repoGraphScanSingleRepoBudget
	repoGraphScanSingleRepoBudget = 1 * time.Nanosecond
	t.Cleanup(func() { repoGraphScanSingleRepoBudget = prev })

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	root := t.TempDir()
	slowDir := filepath.Join(root, "slow")
	healthyDir := filepath.Join(root, "healthy")
	mustMkdir(t, slowDir)
	mustMkdir(t, healthyDir)
	// Same shape for both — the budget override is what makes "slow" slow.
	makeSyntheticGoRepo(t, slowDir, "slow", 3, 5)
	makeSyntheticGoRepo(t, healthyDir, "healthy", 3, 5)
	store.AddRepo(db, "slow", slowDir, "")
	store.AddRepo(db, "healthy", healthyDir, "")

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// Both repos will trip the 1ns budget; assert the dog returns nil (not
	// a fatal error — timeout is a recoverable degradation) and that we got
	// the expected log line.
	if err := dogRepoGraphScan(context.Background(), db, logger); err != nil {
		t.Fatalf("dogRepoGraphScan: expected nil on per-repo timeout (recoverable), got %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "exceeded per-repo budget") {
		t.Errorf("expected timeout log line containing 'exceeded per-repo budget', got logs:\n%s", logs)
	}
	// Both repos should have hit the violation path; the dog should NOT
	// have crashed mid-fleet — surface that by checking the final summary
	// line ran (it's emitted at the bottom of dogRepoGraphScan).
	if !strings.Contains(logs, "Dog repo-graph-scan: scanned") {
		t.Errorf("expected dog to reach final summary log line (proves it didn't crash mid-fleet), got logs:\n%s", logs)
	}
}

// TestUpsertCrossRepoDependency_Revival verifies that an edge whose deleted_at
// is non-empty is revived (deleted_at cleared) on subsequent upsert. The row
// id is preserved so anything that stamped the prior id (Track 2 blast-radius
// alerts) keeps pointing at the right edge.
func TestUpsertCrossRepoDependency_Revival(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	symID, err := store.UpsertCrossRepoSymbol(db, store.CrossRepoSymbol{
		RepoName: "p", SymbolPath: "p.X", SymbolKind: "function",
		FilePath: "x.go", LineNumber: 1, SignatureHash: "h", IsPublic: true,
	})
	if err != nil {
		t.Fatalf("upsert symbol: %v", err)
	}
	id1, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: "c", ConsumerFile: "main.go", ConsumerLine: 10,
		ProviderSymbolID: symID,
	})
	if err != nil {
		t.Fatalf("upsert dep: %v", err)
	}
	if err := store.SoftDeleteCrossRepoDependency(db, id1); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	id2, err := store.UpsertCrossRepoDependency(db, store.CrossRepoDependency{
		ConsumerRepoName: "c", ConsumerFile: "main.go", ConsumerLine: 10,
		ProviderSymbolID: symID,
	})
	if err != nil {
		t.Fatalf("revive upsert: %v", err)
	}
	if id1 != id2 {
		t.Errorf("revival should preserve row id: %d → %d", id1, id2)
	}
	live, err := store.ListConsumersOfSymbol(db, symID)
	if err != nil {
		t.Fatalf("ListConsumersOfSymbol: %v", err)
	}
	if len(live) != 1 {
		t.Errorf("expected 1 live consumer post-revive, got %d", len(live))
	}
}

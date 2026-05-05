package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// TestD9ExitCriterion5_BlastRadiusListsAll20Sites is the integration
// test for D9 exit criterion #5 (roadmap.md ~L2327): seed 20 sites of a
// deprecated-API pattern across consumer repos; the Archaeologist's
// sweep on the producer should detect the cluster within one cycle,
// propose a Feature via librarian.EmitCandidate, and the proposal's
// evidence_summary_json.blast_radius block should identify all 20
// consumer call-sites.
//
// Wire-up (matches the production daemon flow):
//
//  1. Producer repo (`producer-d9`) — Go module example.com/producer-d9,
//     package api exporting Read1..Read5 — each function body uses
//     io/ioutil.ReadFile so ARCH-001 fires on 5 distinct lines (>= the
//     ARCH-001 MinHitsForFeature threshold).
//
//  2. Five consumer repos (`consumer-d9-N` for N in 1..5) — each Go
//     module importing example.com/producer-d9/api and calling 4 of
//     the 5 exported symbols (Read1..Read4 in this fixture). 5 × 4 =
//     20 distinct consumer call-sites across the fleet.
//
//  3. Run dogRepoGraphScan to populate CrossRepoSymbols (5 producer
//     symbols) + CrossRepoDependencies (20 consumer edges).
//
//  4. Queue + drain the Archaeologist sweep on the producer. The sweep
//     detects 5 ARCH-001 hits (>= threshold) and fans out an
//     ArchaeologistProposeMigration. The agent loop claims that next
//     and calls librarian.Client.EmitCandidate with the enriched
//     evidence — including a blast_radius block computed via
//     graph.BlastRadiusForModifications (D8 Track 2's reader path).
//
//  5. Decode the EmitCalls[0].EvidenceJSON and assert:
//       - blast_radius.affected_consumer_repos lists ALL 5 consumer
//         repos (de-duplicated, alphabetised).
//       - blast_radius.modified_symbols includes the deprecated symbol
//         (a producer.api.Read* function).
//       - The total per-symbol consumer-site count across
//         consumers_by_symbol is exactly 20.
//
// Anti-cheat alignment:
//
//   - The Archaeologist remains operator-gated — the test still drives
//     librarian.NewMock().EmitCalls (the same EmitCandidate seam
//     Pattern P-ArchaeologistOperatorGated audits), no new dispatch
//     surface is exercised.
//   - The blast-radius enrichment is pure data: it adds a key to the
//     EvidenceJSON, it does not gate the candidate emission on
//     graph success (a graph failure logs + emits without the block).
func TestD9ExitCriterion5_BlastRadiusListsAll20Sites(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	root := t.TempDir()

	// Producer fixture — five exported io/ioutil.ReadFile call-sites in
	// the api package. ARCH-001's MinHitsForFeature is 5, so these five
	// alone clear the threshold. Function bodies vary slightly to keep
	// ARCH-003 (duplicate-abstractions) from also firing — the test is
	// scoped to ARCH-001.
	producerDir := filepath.Join(root, "producer-d9")
	mustWrite(t, filepath.Join(producerDir, "go.mod"),
		"module example.com/producer-d9\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(producerDir, "api", "api.go"), `package api

import (
	"fmt"
	"io/ioutil"
)

// Read1 reads a file and returns its contents.
func Read1(p string) ([]byte, error) {
	return ioutil.ReadFile(p)
}

// Read2 reads and discards trailing whitespace.
func Read2(p string) (string, error) {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Read3 reads and counts the bytes.
func Read3(p string) (int, error) {
	data, err := ioutil.ReadFile(p)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// Read4 reads and prints.
func Read4(p string) error {
	contents, err := ioutil.ReadFile(p)
	if err != nil {
		return err
	}
	fmt.Println(string(contents))
	return nil
}

// Read5 reads and returns the first line.
func Read5(p string) (string, error) {
	raw, e := ioutil.ReadFile(p)
	if e != nil {
		return "", e
	}
	for i, c := range raw {
		if c == '\n' {
			return string(raw[:i]), nil
		}
	}
	return string(raw), nil
}
`)
	store.AddRepo(db, "producer-d9", producerDir, "ARCH-001 producer for D9 exit-5")

	// Consumer fixtures — 5 repos × 4 distinct selector references each
	// = 20 consumer call-sites total, distributed across producer
	// symbols Read1..Read4 (Read5 is exported but unconsumed; that's
	// realistic and exercises the "modified symbol with zero consumers"
	// branch of the JSON shape).
	const numConsumers = 5
	const sitesPerConsumer = 4
	expectedTotalSites := numConsumers * sitesPerConsumer
	for i := 1; i <= numConsumers; i++ {
		consumerName := fmt.Sprintf("consumer-d9-%d", i)
		consumerDir := filepath.Join(root, consumerName)
		mustWrite(t, filepath.Join(consumerDir, "go.mod"),
			fmt.Sprintf("module example.com/%s\n\ngo 1.21\nrequire example.com/producer-d9 v0.0.0\n", consumerName))
		mustWrite(t, filepath.Join(consumerDir, "main.go"), `package main

import (
	"fmt"

	"example.com/producer-d9/api"
)

func main() {
	a, _ := api.Read1("a")
	b, _ := api.Read2("b")
	c, _ := api.Read3("c")
	d := api.Read4("d")
	fmt.Println(a, b, c, d)
}
`)
		store.AddRepo(db, consumerName, consumerDir, fmt.Sprintf("D9 exit-5 consumer #%d", i))
	}

	// Step 3 — run the cross-repo graph dog. Populates CrossRepoSymbols
	// + CrossRepoDependencies in the in-memory holocron the rest of the
	// test reads via graph.NewInProcess.
	if err := dogRepoGraphScan(context.Background(), db, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("dogRepoGraphScan: %v", err)
	}
	// Pre-condition: the dog observed 4 live consumer edges per
	// consumer repo (one per selector reference). 5 consumers ×
	// 4 = 20.
	totalEdges := 0
	for i := 1; i <= numConsumers; i++ {
		consumerName := fmt.Sprintf("consumer-d9-%d", i)
		live, err := store.ListLiveDependenciesForConsumerRepo(db, consumerName)
		if err != nil {
			t.Fatalf("ListLiveDependenciesForConsumerRepo(%s): %v", consumerName, err)
		}
		if len(live) != sitesPerConsumer {
			t.Fatalf("consumer %s: expected %d live edges, got %d", consumerName, sitesPerConsumer, len(live))
		}
		totalEdges += len(live)
	}
	if totalEdges != expectedTotalSites {
		t.Fatalf("dog edge count: expected %d total live edges, got %d", expectedTotalSites, totalEdges)
	}

	// Step 4 — drive the Archaeologist sweep + propose-migration on the
	// producer. Same shape as TestArchaeologistSweep_FiresProposeMigrationOnThreshold.
	target := mustGetArchTarget(t, db, "producer-d9")
	mock := librarian.NewMock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SpawnArchaeologist(ctx, db, mock, "Archaeologist-d9-test")

	sweepID, qErr := store.QueueArchaeologistSweep(db, target.ID, target.Name)
	if qErr != nil {
		t.Fatalf("QueueArchaeologistSweep: %v", qErr)
	}

	// Wait for the chained pipeline (sweep → propose-migration →
	// EmitCandidate) to settle.
	deadline := time.Now().Add(8 * time.Second)
	settled := false
	for time.Now().Before(deadline) {
		var sweepStatus string
		_ = db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, sweepID).Scan(&sweepStatus)
		if sweepStatus == "Failed" {
			t.Fatalf("sweep #%d Failed unexpectedly", sweepID)
		}
		var pending, locked, completed int
		_ = db.QueryRow(`SELECT
			SUM(CASE WHEN status='Pending' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='Locked' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='Completed' THEN 1 ELSE 0 END)
			FROM BountyBoard WHERE type='ArchaeologistProposeMigration'`).Scan(&pending, &locked, &completed)
		if sweepStatus == "Completed" && pending == 0 && locked == 0 && completed >= 1 && len(mock.EmitCalls) >= 1 {
			settled = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !settled {
		t.Fatalf("pipeline did not settle within 8s; emits=%d", len(mock.EmitCalls))
	}

	// Step 5 — decode the candidate's EvidenceJSON and assert on the
	// blast_radius block.
	if len(mock.EmitCalls) != 1 {
		t.Fatalf("expected exactly 1 EmitCandidate call, got %d", len(mock.EmitCalls))
	}
	cand := mock.EmitCalls[0]

	if !strings.HasPrefix(cand.HypothesisKey, "archaeologist-arch-001-") {
		t.Errorf("HypothesisKey = %q, want prefix archaeologist-arch-001-", cand.HypothesisKey)
	}

	var evidence struct {
		BlastRadius struct {
			ModifiedSymbols []struct {
				Repo       string `json:"repo"`
				SymbolPath string `json:"symbol_path"`
				Kind       string `json:"kind"`
				FilePath   string `json:"file_path"`
				LineNumber int    `json:"line_number"`
			} `json:"modified_symbols"`
			AffectedConsumerRepos []string `json:"affected_consumer_repos"`
			ConsumersBySymbol     map[string][]struct {
				Repo     string `json:"repo"`
				FilePath string `json:"file_path"`
				Line     int    `json:"line"`
			} `json:"consumers_by_symbol"`
		} `json:"blast_radius"`
	}
	if err := json.Unmarshal([]byte(cand.EvidenceJSON), &evidence); err != nil {
		t.Fatalf("decode EvidenceJSON: %v\nEvidenceJSON = %s", err, cand.EvidenceJSON)
	}

	// Assertion 1 — modified_symbols contains the deprecated symbol(s).
	// The producer exports Read1..Read5, all in api/api.go, so all
	// five resolve as modifications. Assert at least Read1 is present
	// (the deprecated symbol the dashboard would render first) and that
	// every entry is a producer-d9 row whose file_path is api/api.go.
	if len(evidence.BlastRadius.ModifiedSymbols) == 0 {
		t.Fatalf("blast_radius.modified_symbols is empty; expected producer-d9 api.Read1..Read5\nEvidenceJSON = %s", cand.EvidenceJSON)
	}
	hasRead1 := false
	for _, m := range evidence.BlastRadius.ModifiedSymbols {
		if m.Repo != "producer-d9" {
			t.Errorf("modified_symbol entry has repo %q, want producer-d9: %+v", m.Repo, m)
		}
		if m.FilePath != "api/api.go" {
			t.Errorf("modified_symbol entry has file_path %q, want api/api.go: %+v", m.FilePath, m)
		}
		if strings.HasSuffix(m.SymbolPath, ".Read1") {
			hasRead1 = true
		}
	}
	if !hasRead1 {
		t.Errorf("blast_radius.modified_symbols missing Read1 (the deprecated-API head); got %+v", evidence.BlastRadius.ModifiedSymbols)
	}

	// Assertion 2 — affected_consumer_repos contains every consumer.
	gotRepos := map[string]bool{}
	for _, r := range evidence.BlastRadius.AffectedConsumerRepos {
		gotRepos[r] = true
	}
	if len(gotRepos) != numConsumers {
		t.Errorf("affected_consumer_repos: expected %d distinct consumers, got %d: %v",
			numConsumers, len(gotRepos), evidence.BlastRadius.AffectedConsumerRepos)
	}
	for i := 1; i <= numConsumers; i++ {
		want := fmt.Sprintf("consumer-d9-%d", i)
		if !gotRepos[want] {
			t.Errorf("affected_consumer_repos missing %s; got %v",
				want, evidence.BlastRadius.AffectedConsumerRepos)
		}
	}

	// Assertion 3 — total per-symbol consumer-site count across
	// consumers_by_symbol equals the planted 20.
	totalSites := 0
	for _, sites := range evidence.BlastRadius.ConsumersBySymbol {
		totalSites += len(sites)
	}
	if totalSites != expectedTotalSites {
		t.Errorf("consumers_by_symbol: total sites = %d, want %d (planted 5 consumers × 4 sites each)\nEvidenceJSON = %s",
			totalSites, expectedTotalSites, cand.EvidenceJSON)
	}

	// Cross-check (idempotence sanity) — each modified Read1..Read4
	// symbol's bucket should have exactly numConsumers (=5) sites.
	// Read5 is unconsumed so its bucket is absent (or empty); that's
	// fine and exercises the "modified symbol with zero consumers"
	// branch of the JSON shape.
	for sym, sites := range evidence.BlastRadius.ConsumersBySymbol {
		if !strings.HasPrefix(sym, "example.com/producer-d9/api.Read") {
			t.Errorf("consumers_by_symbol unexpected key %q", sym)
			continue
		}
		if len(sites) != numConsumers {
			t.Errorf("consumers_by_symbol[%s]: expected %d sites (one per consumer), got %d: %+v",
				sym, numConsumers, len(sites), sites)
		}
	}
}

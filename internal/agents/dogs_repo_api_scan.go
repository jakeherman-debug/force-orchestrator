// Package agents — D15 repo-api-scan dog.
//
// Walks every registered repo with a local_path and runs the apiextract
// scanner to populate CrossRepoAPIs (provider pass) and then resolve
// CrossRepoAPIDependencies (consumer pass + path-matcher). Runs daily (24h
// cooldown) and is ordered after repo-graph-scan so the symbol graph is
// fresh before the API graph is updated.
//
// Design invariant: this dog does NOT import any concrete extractor package
// (rails, spring, express, …). It uses only the apiextract.ExtractorRegistry
// and scanner.Scanner interfaces so the extraction layer remains swappable.
// The daemon startup (cmd/force/fleet_cmds.go) is the ONLY file that imports
// concrete extractors and wires them into the registry via
// RegisterAPIExtractorRegistry.
//
// Dependency injection: RegisterAPIExtractorRegistry sets the package-level
// registry once at daemon startup (same pattern as RegisterSupplyRecheckDeps
// in dogs_supply_token_recheck.go). Tests that exercise dogRepoAPIScan
// directly can call RegisterAPIExtractorRegistry with a test registry.
// When the registry is nil (e.g. a CLI "force dogs run repo-api-scan"
// without daemon wiring) the dog logs a one-line warning and returns nil.
package agents

import (
	"context"
	"database/sql"
	"os"
	"sync"

	"force-orchestrator/internal/apiextract"
	"force-orchestrator/internal/apiextract/scanner"
)

// ── Dependency injection ─────────────────────────────────────────────────

var (
	apiExtractRegistryMu  sync.RWMutex
	apiExtractRegistryVar *apiextract.ExtractorRegistry
)

// RegisterAPIExtractorRegistry installs the registry used by dogRepoAPIScan.
// Must be called once at daemon startup before the first inquisitor tick.
// Safe to call multiple times (last write wins) for test purposes.
func RegisterAPIExtractorRegistry(r *apiextract.ExtractorRegistry) {
	apiExtractRegistryMu.Lock()
	defer apiExtractRegistryMu.Unlock()
	apiExtractRegistryVar = r
}

func getAPIExtractorRegistry() *apiextract.ExtractorRegistry {
	apiExtractRegistryMu.RLock()
	defer apiExtractRegistryMu.RUnlock()
	return apiExtractRegistryVar
}

// ── Dog body ─────────────────────────────────────────────────────────────

func dogRepoAPIScan(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	reg := getAPIExtractorRegistry()
	if reg == nil {
		logger.Printf("Dog repo-api-scan: ExtractorRegistry not registered — skipping (call RegisterAPIExtractorRegistry at daemon startup)")
		return nil
	}
	if len(reg.AllProviders()) == 0 && len(reg.AllConsumers()) == 0 {
		logger.Printf("Dog repo-api-scan: no extractors registered — skipping")
		return nil
	}

	// Load registered repos that have an accessible local_path.
	repos, err := loadRegisteredRepos(db)
	if err != nil {
		return &dogRepoAPIScanError{"load repos", err}
	}
	if len(repos) == 0 {
		logger.Printf("Dog repo-api-scan: no registered repos with local_path — nothing to scan")
		return nil
	}

	sc := scanner.New(db, reg)
	totalProviders, totalConsumers, totalResolved := 0, 0, 0

	for _, r := range repos {
		if ctx.Err() != nil {
			break
		}
		if _, statErr := os.Stat(r.path); statErr != nil {
			logger.Printf("Dog repo-api-scan: skipping %s — local_path %q not accessible: %v", r.name, r.path, statErr)
			continue
		}

		// Provider pass.
		n, pErr := sc.ScanProviders(ctx, r.name, r.path)
		if pErr != nil {
			logger.Printf("Dog repo-api-scan: ScanProviders(%s): %v — continuing", r.name, pErr)
		} else {
			totalProviders += n
			logger.Printf("Dog repo-api-scan: %s: upserted %d provider API(s)", r.name, n)
		}

		// Consumer pass.
		m, cErr := sc.ScanConsumers(ctx, r.name, r.path)
		if cErr != nil {
			logger.Printf("Dog repo-api-scan: ScanConsumers(%s): %v — continuing", r.name, cErr)
		} else {
			totalConsumers += m
			logger.Printf("Dog repo-api-scan: %s: upserted %d consumer dependency(s)", r.name, m)
		}
	}

	// Path-matcher: attempt to resolve any remaining unresolved deps.
	resolved, rErr := scanner.ResolveConsumerDependencies(ctx, db)
	if rErr != nil {
		logger.Printf("Dog repo-api-scan: ResolveConsumerDependencies: %v — continuing", rErr)
	} else {
		totalResolved += resolved
	}

	logger.Printf("Dog repo-api-scan: done — %d provider APIs, %d consumer deps upserted, %d deps resolved this cycle",
		totalProviders, totalConsumers, totalResolved)
	return nil
}

// dogRepoAPIScanError is a structured error from the repo-api-scan dog that
// preserves the phase name so log aggregators can bucket failures.
type dogRepoAPIScanError struct {
	phase string
	cause error
}

func (e *dogRepoAPIScanError) Error() string {
	return "repo-api-scan: " + e.phase + ": " + e.cause.Error()
}

func (e *dogRepoAPIScanError) Unwrap() error { return e.cause }

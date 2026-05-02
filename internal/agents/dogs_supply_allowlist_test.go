package agents

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/store"
)

// allowlistStub is a test-only codeartifact.Client that lets the tests
// program per-ecosystem ListPackages results (success / error / token
// expired). All other Client methods are stubbed to return zero values
// since the supply-allowlist-refresh dog only consumes ListPackages.
type allowlistStub struct {
	results map[codeartifact.Ecosystem][]codeartifact.Package
	errs    map[codeartifact.Ecosystem]error
	calls   map[codeartifact.Ecosystem]int
}

func newAllowlistStub() *allowlistStub {
	return &allowlistStub{
		results: map[codeartifact.Ecosystem][]codeartifact.Package{},
		errs:    map[codeartifact.Ecosystem]error{},
		calls:   map[codeartifact.Ecosystem]int{},
	}
}

func (s *allowlistStub) ListPackages(_ context.Context, eco codeartifact.Ecosystem) ([]codeartifact.Package, error) {
	s.calls[eco]++
	if err, ok := s.errs[eco]; ok {
		return nil, err
	}
	return s.results[eco], nil
}

func (s *allowlistStub) DescribePackageVersion(_ context.Context, _ codeartifact.Ecosystem, _, _ string) (codeartifact.PackageVersionInfo, error) {
	return codeartifact.PackageVersionInfo{}, nil
}

func (s *allowlistStub) Health(_ context.Context) error { return nil }

// Compile-time assertion: allowlistStub satisfies the Client interface.
var _ codeartifact.Client = (*allowlistStub)(nil)

// TestSupplyAllowlistRefresh_HappyPath — three packages per ecosystem,
// all four ecosystems succeed; SystemConfig allowlist + last_refresh
// rows are populated.
func TestSupplyAllowlistRefresh_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	stub := newAllowlistStub()
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemNPM, codeartifact.EcosystemPyPI,
		codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		// Intentionally unsorted — the dog must sort.
		stub.results[eco] = []codeartifact.Package{
			{Ecosystem: eco, Name: "zeta"},
			{Ecosystem: eco, Name: "alpha"},
			{Ecosystem: eco, Name: "mu"},
		}
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogSupplyAllowlistRefresh(context.Background(), db, stub, logger); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}

	// Each ecosystem ListPackages must have been called exactly once.
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemNPM, codeartifact.EcosystemPyPI,
		codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		if stub.calls[eco] != 1 {
			t.Errorf("ListPackages calls for %s = %d, want 1", eco, stub.calls[eco])
		}

		// SystemConfig allowlist row must contain the sorted, newline-joined names.
		got := store.GetConfig(db, "supply_allowlist_"+string(eco), "")
		want := "alpha\nmu\nzeta"
		if got != want {
			t.Errorf("SystemConfig.supply_allowlist_%s = %q, want %q", eco, got, want)
		}

		// last_refresh row must be set and parse as a SQLite timestamp.
		ts := store.GetConfig(db, "supply_allowlist_"+string(eco)+"_last_refresh", "")
		if ts == "" {
			t.Errorf("supply_allowlist_%s_last_refresh not set", eco)
			continue
		}
		parsed, err := store.ParseSQLiteTime(ts)
		if err != nil {
			t.Errorf("supply_allowlist_%s_last_refresh = %q is not a parseable SQLite ts: %v", eco, ts, err)
			continue
		}
		if time.Since(parsed) > time.Minute {
			t.Errorf("supply_allowlist_%s_last_refresh = %q is more than 1 minute in the past", eco, ts)
		}
	}
}

// TestSupplyAllowlistRefresh_TokenExpired_SkipEcosystem — the npm
// ecosystem returns ErrTokenExpired; the dog logs and skips it but
// still populates the other three.
func TestSupplyAllowlistRefresh_TokenExpired_SkipEcosystem(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	stub := newAllowlistStub()
	stub.errs[codeartifact.EcosystemNPM] = codeartifact.ErrTokenExpired
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemPyPI, codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		stub.results[eco] = []codeartifact.Package{{Ecosystem: eco, Name: "ok-pkg"}}
	}

	logger := log.New(io.Discard, "", 0)
	if err := dogSupplyAllowlistRefresh(context.Background(), db, stub, logger); err != nil {
		// Token expired must NOT bubble up as an error — it's deferral, not failure.
		t.Fatalf("dog returned err on token-expired (should be silent skip): %v", err)
	}

	// npm allowlist must be empty (skipped, never written).
	if got := store.GetConfig(db, "supply_allowlist_npm", "<missing>"); got != "<missing>" {
		t.Errorf("npm allowlist should be unset on token-expired skip, got %q", got)
	}
	if got := store.GetConfig(db, "supply_allowlist_npm_last_refresh", "<missing>"); got != "<missing>" {
		t.Errorf("npm last_refresh should be unset on token-expired skip, got %q", got)
	}

	// Other ecosystems must populate normally.
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemPyPI, codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		got := store.GetConfig(db, "supply_allowlist_"+string(eco), "")
		if got != "ok-pkg" {
			t.Errorf("SystemConfig.supply_allowlist_%s = %q, want %q", eco, got, "ok-pkg")
		}
		if ts := store.GetConfig(db, "supply_allowlist_"+string(eco)+"_last_refresh", ""); ts == "" {
			t.Errorf("supply_allowlist_%s_last_refresh not set after happy-path branch", eco)
		}
	}
}

// TestSupplyAllowlistRefresh_AwsError_OtherEcosystemsContinue — pypi
// returns a generic error (not token-expired); the dog returns
// errors.Join carrying that error, but the other ecosystems still
// populate their allowlists. CLAUDE.md "no silent failures" — the
// returned error reaches the dog dispatcher's operator-mail path.
func TestSupplyAllowlistRefresh_AwsError_OtherEcosystemsContinue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	stub := newAllowlistStub()
	awsErr := errors.New("AWS: AccessDeniedException on us-east-1")
	stub.errs[codeartifact.EcosystemPyPI] = awsErr
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemNPM, codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		stub.results[eco] = []codeartifact.Package{{Ecosystem: eco, Name: "ok-pkg"}}
	}

	logger := log.New(io.Discard, "", 0)
	err := dogSupplyAllowlistRefresh(context.Background(), db, stub, logger)
	if err == nil {
		t.Fatalf("expected non-nil error from generic ecosystem failure")
	}
	if !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Errorf("returned error does not surface the underlying AWS error: %v", err)
	}
	if !strings.Contains(err.Error(), "pypi") {
		t.Errorf("returned error should mention the failing ecosystem, got: %v", err)
	}

	// pypi allowlist must NOT have been written on error.
	if got := store.GetConfig(db, "supply_allowlist_pypi", "<missing>"); got != "<missing>" {
		t.Errorf("pypi allowlist should be unset on error, got %q", got)
	}

	// Other ecosystems must populate normally.
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemNPM, codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		if got := store.GetConfig(db, "supply_allowlist_"+string(eco), ""); got != "ok-pkg" {
			t.Errorf("SystemConfig.supply_allowlist_%s = %q, want %q", eco, got, "ok-pkg")
		}
	}
}

// TestSupplyAllowlistRefresh_NilClient — when the daemon couldn't
// construct a CodeArtifact client (e.g. AWS config missing), the dog
// must log and exit nil rather than panic. The dog reschedules
// normally and the operator can re-attempt once AWS config is fixed.
func TestSupplyAllowlistRefresh_NilClient(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(io.Discard, "", 0)
	if err := dogSupplyAllowlistRefresh(context.Background(), db, nil, logger); err != nil {
		t.Errorf("nil client should not be an error (skip with log), got: %v", err)
	}
	// Ensure no allowlist rows were written.
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemNPM, codeartifact.EcosystemPyPI,
		codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		if got := store.GetConfig(db, "supply_allowlist_"+string(eco), "<missing>"); got != "<missing>" {
			t.Errorf("supply_allowlist_%s should be unset on nil client, got %q", eco, got)
		}
	}
}

// TestSupplyAllowlistRefresh_RegisteredAtCorrectCadence — assert the
// dog is registered in dogOrder + dogCooldowns at 24h cadence and is
// reachable through ListDogs.
func TestSupplyAllowlistRefresh_RegisteredAtCorrectCadence(t *testing.T) {
	const name = "supply-allowlist-refresh"

	// dogCooldowns
	cooldown, ok := dogCooldowns[name]
	if !ok {
		t.Fatalf("%s not present in dogCooldowns", name)
	}
	if cooldown != 24*time.Hour {
		t.Errorf("%s cooldown = %v, want 24h", name, cooldown)
	}

	// dogOrder
	found := false
	for _, n := range dogOrder {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("%s not present in dogOrder", name)
	}

	// ListDogs reflects registration.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	dogs := ListDogs(db)
	var matched *DogStatus
	for i := range dogs {
		if dogs[i].Name == name {
			matched = &dogs[i]
			break
		}
	}
	if matched == nil {
		t.Fatalf("%s not surfaced by ListDogs", name)
	}
	if matched.Cooldown != 24*time.Hour {
		t.Errorf("ListDogs %s cooldown = %v, want 24h", name, matched.Cooldown)
	}
}

// TestSupplyAllowlistRefresh_GoEcosystemNotInList — Go is intentionally
// excluded (CodeArtifact has no Go format). Any future drift that adds
// it would surface here; conversely, this test documents the constraint.
func TestSupplyAllowlistRefresh_GoEcosystemNotInList(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	stub := newAllowlistStub()
	logger := log.New(io.Discard, "", 0)
	if err := dogSupplyAllowlistRefresh(context.Background(), db, stub, logger); err != nil {
		t.Fatalf("dog returned err: %v", err)
	}

	// The four supported ecosystems should each have been called exactly once.
	for _, eco := range []codeartifact.Ecosystem{
		codeartifact.EcosystemNPM, codeartifact.EcosystemPyPI,
		codeartifact.EcosystemRubyGems, codeartifact.EcosystemMaven,
	} {
		if stub.calls[eco] != 1 {
			t.Errorf("ListPackages(%s) called %d times, want 1", eco, stub.calls[eco])
		}
	}
	// "go" should never appear as a key — verify by total call count.
	totalCalls := 0
	for _, n := range stub.calls {
		totalCalls += n
	}
	if totalCalls != 4 {
		t.Errorf("dog issued %d ListPackages calls, want 4 (one per supported ecosystem)", totalCalls)
	}
}

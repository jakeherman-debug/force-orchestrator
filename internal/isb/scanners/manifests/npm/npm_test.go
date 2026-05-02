package npm

import (
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/isb/scanners/manifests"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body
}

func TestParser_Detect(t *testing.T) {
	p := Parser{}
	for path, want := range map[string]bool{
		"package.json":      true,
		"package-lock.json": true,
		"yarn.lock":         true,
		"pnpm-lock.yaml":    true,
		"go.mod":            false,
		"requirements.txt":  false,
	} {
		if got := p.Detect(path); got != want {
			t.Errorf("Detect(%q)=%v want %v", path, got, want)
		}
	}
}

func TestParser_PackageJSON_Direct(t *testing.T) {
	deps, err := Parser{}.Parse("package.json", read(t, "package.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
		if d.Source != manifests.SourceDirect {
			t.Errorf("package.json dep should be Direct: %+v", d)
		}
	}
	if got["react"] != "^18.2.0" || got["axios"] != "1.6.2" || got["typescript"] != "5.3.3" {
		t.Errorf("missing direct deps: %v", got)
	}
}

func TestParser_PackageLockV3_Transitive(t *testing.T) {
	deps, err := Parser{}.Parse("package-lock.json", read(t, "package-lock-v3.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
		if d.Source != manifests.SourceTransitive {
			t.Errorf("lock dep should be Transitive: %+v", d)
		}
	}
	if got["react"] != "18.2.0" {
		t.Errorf("missing react: %v", got)
	}
	if got["@babel/core"] != "7.23.7" {
		t.Errorf("missing @babel/core (scoped): %v", got)
	}
}

func TestParser_PackageLockV1_RecursiveTree(t *testing.T) {
	deps, err := Parser{}.Parse("package-lock.json", read(t, "package-lock-v1.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["lodash"] != "4.17.21" {
		t.Errorf("missing lodash: %v", got)
	}
	if got["body-parser"] != "1.20.1" {
		t.Errorf("missing nested body-parser: %v", got)
	}
}

func TestParser_YarnLock(t *testing.T) {
	deps, err := Parser{}.Parse("yarn.lock", read(t, "yarn.lock"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["lodash"] != "4.17.21" {
		t.Errorf("missing lodash: %v", got)
	}
	if got["react"] != "18.2.0" {
		t.Errorf("missing react: %v", got)
	}
}

func TestParser_PnpmLock(t *testing.T) {
	deps, err := Parser{}.Parse("pnpm-lock.yaml", read(t, "pnpm-lock.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["react"] != "18.2.0" {
		t.Errorf("missing react: %v", got)
	}
	if got["lodash"] != "4.17.21" {
		t.Errorf("missing lodash: %v", got)
	}
}

func TestParser_PackageJSON_DiffAddsLodash(t *testing.T) {
	before := read(t, "package.json")
	after := read(t, "package.json.after")
	added, removed, err := Parser{}.ParseDiff("package.json", before, after)
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	foundLodash := false
	for _, d := range added {
		if d.Name == "lodash" && d.Version == "4.17.21" {
			foundLodash = true
		}
	}
	if !foundLodash {
		t.Errorf("expected lodash@4.17.21 in added: %+v", added)
	}
	if len(removed) != 0 {
		t.Errorf("no removed deps expected: %+v", removed)
	}
}

func TestParser_Malformed_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on malformed: %v", r)
		}
	}()
	_, err := Parser{}.Parse("package.json", []byte("{not valid json"))
	if err == nil {
		t.Errorf("expected error on malformed package.json")
	}
}

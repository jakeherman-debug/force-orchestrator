package gomod

import (
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/isb/scanners/manifests"
)

func mustReadFile(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body
}

func TestParser_Detect(t *testing.T) {
	p := Parser{}
	for _, want := range []string{"go.mod", "go.sum", "/repo/go.mod", "vendor/modules.txt-not-this-one"} {
		shouldMatch := want == "go.mod" || want == "go.sum" || want == "/repo/go.mod"
		if got := p.Detect(want); got != shouldMatch {
			t.Errorf("Detect(%q)=%v want %v", want, got, shouldMatch)
		}
	}
}

func TestParser_ParseGoMod_Happy(t *testing.T) {
	body := mustReadFile(t, "go.mod")
	deps, err := Parser{}.Parse("go.mod", body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gotDirect := map[string]string{}
	gotIndirect := map[string]string{}
	for _, d := range deps {
		if d.Source == manifests.SourceDirect {
			gotDirect[d.Name] = d.Version
		} else {
			gotIndirect[d.Name] = d.Version
		}
	}
	if gotDirect["github.com/stretchr/testify"] != "v1.9.0" {
		t.Errorf("missing direct testify@v1.9.0: %v", gotDirect)
	}
	if gotIndirect["github.com/davecgh/go-spew"] != "v1.1.1" {
		t.Errorf("missing indirect go-spew@v1.1.1: %v", gotIndirect)
	}
}

func TestParser_ParseGoSum_DedupesGoModSuffix(t *testing.T) {
	body := mustReadFile(t, "go.sum")
	deps, err := Parser{}.Parse("go.sum", body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	count := map[string]int{}
	for _, d := range deps {
		count[d.Name+"@"+d.Version]++
		if d.Source != manifests.SourceTransitive {
			t.Errorf("go.sum dep should be Transitive: %+v", d)
		}
	}
	if count["github.com/stretchr/testify@v1.9.0"] != 1 {
		t.Errorf("expected exactly 1 entry for testify, got %d (full count: %v)", count["github.com/stretchr/testify@v1.9.0"], count)
	}
}

func TestParser_ParseDiff_AddedRemoved(t *testing.T) {
	before := mustReadFile(t, "go.mod")
	after := mustReadFile(t, "go.mod.after")
	added, removed, err := Parser{}.ParseDiff("go.mod", before, after)
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	foundCobra := false
	for _, d := range added {
		if d.Name == "github.com/spf13/cobra" && d.Version == "v1.8.0" {
			foundCobra = true
		}
	}
	if !foundCobra {
		t.Errorf("expected cobra@v1.8.0 in added, got: %+v", added)
	}
	if len(removed) != 0 {
		t.Errorf("expected no removed deps, got: %+v", removed)
	}
}

func TestParser_Malformed_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Parse panicked: %v", r)
		}
	}()
	_, err := Parser{}.Parse("go.mod", []byte("garbage not a go.mod"))
	if err == nil {
		t.Errorf("expected error on malformed go.mod")
	}
}

func TestParser_EmptyContent(t *testing.T) {
	deps, err := Parser{}.Parse("go.mod", nil)
	if err != nil {
		t.Errorf("empty content should not error: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("empty content should produce zero deps")
	}
}

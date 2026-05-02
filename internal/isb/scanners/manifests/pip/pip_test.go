package pip

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
		"requirements.txt": true,
		"Pipfile":          true,
		"Pipfile.lock":     true,
		"pyproject.toml":   true,
		"poetry.lock":      true,
		"setup.py":         true,
		"package.json":     false,
		"go.mod":           false,
	} {
		if got := p.Detect(path); got != want {
			t.Errorf("Detect(%q)=%v want %v", path, got, want)
		}
	}
}

func TestParser_Requirements(t *testing.T) {
	deps, err := Parser{}.Parse("requirements.txt", read(t, "requirements.txt"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
		if d.Source != manifests.SourceDirect {
			t.Errorf("requirements.txt should be Direct: %+v", d)
		}
	}
	if got["requests"] != "2.31.0" {
		t.Errorf("missing requests==2.31.0: %v", got)
	}
	if got["click"] != "8.1.3" {
		t.Errorf("missing click==8.1.3: %v", got)
	}
	// Range-only entries land with Version="" by design.
	if v, ok := got["flask"]; !ok || v != "" {
		t.Errorf("flask should be present with empty Version: %v", got)
	}
}

func TestParser_Pipfile(t *testing.T) {
	deps, err := Parser{}.Parse("Pipfile", read(t, "Pipfile"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["requests"] != "2.31.0" {
		t.Errorf("missing requests: %v", got)
	}
	if got["click"] != "8.1.3" {
		t.Errorf("missing click (object form): %v", got)
	}
}

func TestParser_PipfileLock(t *testing.T) {
	deps, err := Parser{}.Parse("Pipfile.lock", read(t, "Pipfile.lock"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["requests"] != "2.31.0" {
		t.Errorf("missing requests in lock: %v", got)
	}
	if got["pytest"] != "7.4.0" {
		t.Errorf("missing pytest dev in lock: %v", got)
	}
}

func TestParser_PyProject_PEP621(t *testing.T) {
	deps, err := Parser{}.Parse("pyproject.toml", read(t, "pyproject-pep621.toml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["requests"] != "2.31.0" {
		t.Errorf("missing requests: %v", got)
	}
	if got["rich"] != "13.7.0" {
		t.Errorf("missing rich: %v", got)
	}
}

func TestParser_PyProject_Poetry(t *testing.T) {
	deps, err := Parser{}.Parse("pyproject.toml", read(t, "pyproject-poetry.toml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["click"] != "8.1.3" {
		t.Errorf("missing click@8.1.3: %v", got)
	}
	if _, ok := got["python"]; ok {
		t.Errorf("python pin should be skipped: %v", got)
	}
}

func TestParser_PoetryLock(t *testing.T) {
	deps, err := Parser{}.Parse("poetry.lock", read(t, "poetry.lock"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["requests"] != "2.31.0" {
		t.Errorf("missing requests: %v", got)
	}
	if got["click"] != "8.1.3" {
		t.Errorf("missing click: %v", got)
	}
}

func TestParser_SetupPy(t *testing.T) {
	deps, err := Parser{}.Parse("setup.py", read(t, "setup.py"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["requests"] != "2.31.0" {
		t.Errorf("missing requests==2.31.0: %v", got)
	}
}

func TestParser_DiffAddsHttpx(t *testing.T) {
	before := read(t, "requirements.txt")
	after := read(t, "requirements.txt.after")
	added, _, err := Parser{}.ParseDiff("requirements.txt", before, after)
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	found := false
	for _, d := range added {
		if d.Name == "httpx" && d.Version == "0.27.0" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected httpx@0.27.0 in added: %+v", added)
	}
}

func TestParser_Malformed_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on malformed: %v", r)
		}
	}()
	_, _ = Parser{}.Parse("requirements.txt", []byte("\x00\x01garbage"))
	_, err := Parser{}.Parse("Pipfile.lock", []byte("not-json{"))
	if err == nil {
		t.Errorf("expected error on malformed Pipfile.lock")
	}
}

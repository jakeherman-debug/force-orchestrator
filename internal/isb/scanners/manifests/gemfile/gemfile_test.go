package gemfile

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
		"Gemfile":          true,
		"Gemfile.lock":     true,
		"foo.gemspec":      true,
		"src/foo.gemspec":  true,
		"package.json":     false,
		"requirements.txt": false,
	} {
		if got := p.Detect(path); got != want {
			t.Errorf("Detect(%q)=%v want %v", path, got, want)
		}
	}
}

func TestParser_ParseGemfile_Direct(t *testing.T) {
	body := read(t, "Gemfile")
	deps, err := Parser{}.Parse("Gemfile", body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
		if d.Source != manifests.SourceDirect {
			t.Errorf("Gemfile dep should be Direct: %+v", d)
		}
	}
	if got["rails"] != "7.0.4" {
		t.Errorf("missing rails@7.0.4: %v", got)
	}
	if _, ok := got["puma"]; !ok {
		t.Errorf("missing puma (no version): %v", got)
	}
	if got["rspec-rails"] != "6.0.1" {
		t.Errorf("missing rspec-rails@6.0.1: %v", got)
	}
}

func TestParser_ParseGemfileLock_Transitive(t *testing.T) {
	body := read(t, "Gemfile.lock")
	deps, err := Parser{}.Parse("Gemfile.lock", body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
		if d.Source != manifests.SourceTransitive {
			t.Errorf("Gemfile.lock dep should be Transitive: %+v", d)
		}
	}
	if got["nio4r"] != "2.7.0" {
		t.Errorf("missing nio4r@2.7.0 (transitive): %v", got)
	}
	if got["rails"] != "7.0.4" {
		t.Errorf("missing rails@7.0.4 in lock: %v", got)
	}
}

func TestParser_ParseGemspec_AddDependency(t *testing.T) {
	body := read(t, "myproject.gemspec")
	deps, err := Parser{}.Parse("myproject.gemspec", body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["activesupport"] != "~> 7.0" {
		t.Errorf("missing activesupport: %v", got)
	}
	if got["json"] != "2.6.3" {
		t.Errorf("missing json@2.6.3: %v", got)
	}
	if got["rspec"] != "~> 3.12" {
		t.Errorf("missing rspec dev dep: %v", got)
	}
}

func TestParser_ParseDiff_AddedRedis(t *testing.T) {
	before := read(t, "Gemfile")
	after := read(t, "Gemfile.after")
	added, removed, err := Parser{}.ParseDiff("Gemfile", before, after)
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	foundRedis := false
	for _, d := range added {
		if d.Name == "redis" && d.Version == "5.0.0" {
			foundRedis = true
		}
	}
	if !foundRedis {
		t.Errorf("expected redis@5.0.0 in added: %+v", added)
	}
	if len(removed) != 0 {
		t.Errorf("expected no removed deps: %+v", removed)
	}
}

func TestParser_Malformed_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on malformed: %v", r)
		}
	}()
	deps, err := Parser{}.Parse("Gemfile", []byte("totally malformed Ruby \x00\x01"))
	if err != nil {
		t.Errorf("Gemfile parser is regex-best-effort, should not error on garbage: %v", err)
	}
	_ = deps
}

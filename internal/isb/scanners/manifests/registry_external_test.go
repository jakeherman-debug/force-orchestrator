package manifests_test

// External test package: verifies the default registry has every
// production ecosystem parser loaded once the per-ecosystem
// subpackages are imported (init side-effect). Lives in
// `_test` package so the side-effect imports in loaders_test.go don't
// create an import cycle with the manifests package itself.

import (
	"testing"

	"force-orchestrator/internal/isb/scanners/manifests"
)

func TestDefaultRegistry_LoadsAllProductionParsers(t *testing.T) {
	cases := map[string]manifests.Ecosystem{
		"go.mod":           manifests.EcosystemGo,
		"package.json":     manifests.EcosystemNPM,
		"requirements.txt": manifests.EcosystemPyPI,
		"pom.xml":          manifests.EcosystemMaven,
		"Gemfile":          manifests.EcosystemRubyGems,
		"build.gradle":     manifests.EcosystemMaven,
		"build.gradle.kts": manifests.EcosystemMaven,
		"yarn.lock":        manifests.EcosystemNPM,
		"pnpm-lock.yaml":   manifests.EcosystemNPM,
		"poetry.lock":      manifests.EcosystemPyPI,
		"pyproject.toml":   manifests.EcosystemPyPI,
		"setup.py":         manifests.EcosystemPyPI,
		"Pipfile":          manifests.EcosystemPyPI,
		"Pipfile.lock":     manifests.EcosystemPyPI,
		"Gemfile.lock":     manifests.EcosystemRubyGems,
		"some.gemspec":     manifests.EcosystemRubyGems,
		"go.sum":           manifests.EcosystemGo,
	}
	for path, want := range cases {
		got, ok := manifests.IsManifest(path)
		if !ok {
			t.Errorf("IsManifest(%q): expected match for ecosystem %q", path, want)
			continue
		}
		if got != want {
			t.Errorf("IsManifest(%q): want %q got %q", path, want, got)
		}
	}

	if _, ok := manifests.IsManifest("main.go"); ok {
		t.Errorf("main.go must NOT be a manifest")
	}
	if _, ok := manifests.IsManifest("README.md"); ok {
		t.Errorf("README.md must NOT be a manifest")
	}
}

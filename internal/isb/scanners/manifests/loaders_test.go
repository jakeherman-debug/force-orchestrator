package manifests_test

// loadAllProductionParsers exists in an external test package so the
// init()-side-effect imports of the per-ecosystem subpackages don't
// trigger an import cycle (the subpackages import the manifests
// package). Tests that need the production registry pre-loaded
// import this _test.go file's package.
//
// This file is intentionally tiny; the side-effect imports are the
// load-bearing piece.

import (
	_ "force-orchestrator/internal/isb/scanners/manifests/gemfile"
	_ "force-orchestrator/internal/isb/scanners/manifests/gomod"
	_ "force-orchestrator/internal/isb/scanners/manifests/maven"
	_ "force-orchestrator/internal/isb/scanners/manifests/npm"
	_ "force-orchestrator/internal/isb/scanners/manifests/pip"
)

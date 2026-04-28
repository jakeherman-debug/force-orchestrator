// Package capabilityprofiles holds the per-agent capability profile
// YAML files at the operator-facing repo path agents/capabilities/.
// The internal loader (internal/agents/capabilities/loader.go) reads
// them via this embedded FS so the binary is self-contained and the
// fleet does not depend on the operator's working directory at runtime.
//
// This package is intentionally tiny: only the embed.FS variable. All
// parsing, validation, and lookup logic lives in the loader package.
package capabilityprofiles

import "embed"

// FS exposes the per-agent profile YAMLs, REGISTRY.yaml, and
// .forceblocklist.yaml as an embedded filesystem. The leading-dot
// blocklist file is included by listing it explicitly — Go's embed
// directive excludes dotfiles by default for *.yaml globs.
//
//go:embed *.yaml
//go:embed .forceblocklist.yaml
var FS embed.FS

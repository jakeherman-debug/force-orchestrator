// Package provenance exposes the build-time provenance vars wired
// from cmd/force/main.go via -ldflags. Centralised here so any package
// (audit, dashboard, daemon, status) can read them without re-importing
// main.
//
// Setters are exported because main.go's `var GitSHA string` lives in
// the main package — we mirror them via initialisers.
package provenance

import (
	"fmt"
	"runtime"
)

// Info is the canonical provenance bundle. All fields default to
// "unknown" when the binary was not built via the project Makefile
// (which injects values via -ldflags).
type Info struct {
	GitSHA    string
	BuildTime string
	GitBranch string
	GoVersion string
}

var (
	gitSHA    = "unknown"
	buildTime = "unknown"
	gitBranch = "unknown"
)

// Set is called from main.init (or from main directly via
// SetFromMain) to mirror the -ldflags-set vars into this package.
func Set(sha, btime, branch string) {
	if sha != "" {
		gitSHA = sha
	}
	if btime != "" {
		buildTime = btime
	}
	if branch != "" {
		gitBranch = branch
	}
}

// Get returns the current provenance bundle. Safe to call before Set
// (returns "unknown" defaults).
func Get() Info {
	return Info{
		GitSHA:    gitSHA,
		BuildTime: buildTime,
		GitBranch: gitBranch,
		GoVersion: runtime.Version(),
	}
}

// String renders the provenance as a single human-readable line,
// suitable for `force version` and `force daemon status`.
func (i Info) String() string {
	return fmt.Sprintf("git=%s branch=%s built=%s go=%s",
		i.GitSHA, i.GitBranch, i.BuildTime, i.GoVersion)
}

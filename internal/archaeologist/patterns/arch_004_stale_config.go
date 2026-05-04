// Pattern ARCH-004 — stale-config-files.
//
// Detects config files under known config-rooted directories whose
// last-modified time exceeds a "stale" threshold (default 365d).
// The signal is "no operator has touched this in over a year" —
// likely abandoned and ripe for review/removal.
//
// Language-agnostic by design (config files are not source code), but
// the file extension allowlist keeps it scoped to recognised config
// formats so a stale `.go` source file doesn't get caught.

package patterns

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"force-orchestrator/internal/archaeologist"
)

// arch004ConfigExtensions lists the file extensions ARCH-004
// considers "config". Adding more is safe; removing requires a
// migration story for any existing findings.
var arch004ConfigExtensions = []string{
	".yaml", ".yml",
	".toml",
	".ini",
	".cfg",
	".conf",
	".json", // config-shaped JSON (e.g. tsconfig.json) — we accept some noise
	".env",
}

// arch004StaleThreshold is the age past which a config file is
// flagged. 365 days is the v1 default; a future iteration may make it
// per-repo configurable via SystemConfig.
const arch004StaleThreshold = 365 * 24 * time.Hour

type arch004 struct{}

// NewARCH004 returns the ARCH-004 pattern.
func NewARCH004() archaeologist.Pattern { return arch004{} }

func (arch004) ID() string             { return "ARCH-004" }
func (arch004) MinHitsForFeature() int { return 6 }

func (p arch004) Scan(repo *archaeologist.Repo) []archaeologist.Hit {
	if repo == nil || repo.LocalPath == "" {
		return nil
	}
	threshold := time.Now().Add(-arch004StaleThreshold)
	var hits []archaeologist.Hit
	_ = walkRepoFiles(repo.LocalPath, arch004ConfigExtensions, func(absPath, relPath string) error {
		info, err := os.Stat(absPath)
		if err != nil || info.IsDir() {
			return nil
		}
		if !info.ModTime().Before(threshold) {
			return nil
		}
		// Skip generated / lock files — the staleness signal there is
		// expected (the lockfile only changes when deps change).
		base := strings.ToLower(info.Name())
		if strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".sum") {
			return nil
		}
		detail, _ := json.Marshal(map[string]any{
			"mod_time":   info.ModTime().UTC().Format(time.RFC3339),
			"age_days":   int(time.Since(info.ModTime()).Hours() / 24),
			"size_bytes": info.Size(),
		})
		hits = append(hits, archaeologist.Hit{
			FilePath:   relPath,
			LineNumber: 1, // file-level finding
			DetailJSON: string(detail),
		})
		return nil
	})
	return hits
}

func init() { Register(NewARCH004()) }

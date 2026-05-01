// D3 P6A.13 — Pattern P30: high-stakes auto-execute cooldown.
//
// Every high-stakes auto-execute call site MUST route through
// agents.ScheduleCooldown. The audit asserts the helper exists and
// is exported; future migration of existing auto-execute sites
// (Council auto-merge on critical convoy, Medic auto-fix, etc.)
// is tracked separately as backlog.
package audittools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPattern_P30_HighStakesCooldown_HelperExists(t *testing.T) {
	root := repoRootP30(t)
	src, err := os.ReadFile(filepath.Join(root, "internal/agents/cooldown_scheduler.go"))
	if err != nil {
		t.Fatalf("read cooldown_scheduler.go: %v", err)
	}
	body := string(src)
	for _, want := range []string{
		"func ScheduleCooldown(",
		"func PauseCooldown(",
		"func ResumeCooldown(",
		"func CancelCooldown(",
		"func MarkCooldownExecuted(",
		"func ListPendingCooldowns(",
		"const CooldownDuration",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Pattern P30: %s missing from cooldown_scheduler.go", want)
		}
	}
}

func repoRootP30(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found from %s", wd)
	return ""
}

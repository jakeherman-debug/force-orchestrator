package rules

import "testing"

func TestBOS004_Red_NoGuards(t *testing.T) {
	src := `
package agents
import (
	"context"
	"database/sql"
)
func SpawnFoo(ctx context.Context, db *sql.DB, name string) {
	for {}
}
`
	out := runRule(t, bos004{}, "internal/agents/foo.go", src)
	assertHasFinding(t, out, "BOS-004", "SpawnFoo")
}

func TestBOS004_Green_AllGuards(t *testing.T) {
	src := `
package agents
import (
	"context"
	"database/sql"
	"time"
)
func IsEstopped(db *sql.DB) bool      { return false }
func SpendCapExceeded(db *sql.DB) bool { return false }
func SpawnFoo(ctx context.Context, db *sql.DB, name string) {
	for {
		if ctx.Err() != nil { return }
		if IsEstopped(db) { time.Sleep(time.Second); continue }
		if SpendCapExceeded(db) { time.Sleep(time.Second); continue }
	}
}
`
	out := runRule(t, bos004{}, "internal/agents/foo.go", src)
	assertNoFindings(t, out)
}

// Non-Spawn function is out of scope.
func TestBOS004_NotSpawn(t *testing.T) {
	src := `
package agents
import "context"
func RunStuff(ctx context.Context) {}
`
	out := runRule(t, bos004{}, "internal/agents/foo.go", src)
	assertNoFindings(t, out)
}

package rules

import "testing"

func TestBOS009_Red(t *testing.T) {
	src := `
package agents
import (
	"database/sql"
	"time"
)
func IsEstopped(db *sql.DB) bool { return false }
func loop(db *sql.DB) {
	for {
		if IsEstopped(db) { time.Sleep(time.Second); continue }
	}
}
`
	out := runRule(t, bos009{}, "internal/agents/foo.go", src)
	assertHasFinding(t, out, "BOS-009", "")
}

func TestBOS009_Green_NoSleep(t *testing.T) {
	src := `
package agents
import "database/sql"
func IsEstopped(db *sql.DB) bool { return false }
func loop(db *sql.DB) {
	for {
		if IsEstopped(db) { return }
	}
}
`
	out := runRule(t, bos009{}, "internal/agents/foo.go", src)
	assertNoFindings(t, out)
}

func TestBOS009_Green_NoEstop(t *testing.T) {
	src := `
package agents
import "time"
func loop() {
	for {
		time.Sleep(time.Second)
	}
}
`
	out := runRule(t, bos009{}, "internal/agents/foo.go", src)
	assertNoFindings(t, out)
}

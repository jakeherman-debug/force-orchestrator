package rules

import "testing"

func TestBOS003_Red_TwoMutationsNoTx(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/store"
func DoTwoWrites(taskID int) error {
	if err := store.UpdateBountyStatus(nil, taskID, "X"); err != nil {
		return err
	}
	if err := store.InsertSecurityFinding(nil, store.SecurityFinding{}); err != nil {
		return err
	}
	return nil
}
`
	out := runRule(t, bos003{}, "internal/agents/something.go", src)
	assertHasFinding(t, out, "BOS-003", "DoTwoWrites")
}

func TestBOS003_Green_TxWrapped(t *testing.T) {
	src := `
package agents
import (
	"database/sql"
	"force-orchestrator/internal/store"
)
func DoTwoWrites(db *sql.DB, taskID int) error {
	tx, err := db.Begin()
	if err != nil { return err }
	if err := store.UpdateBountyStatus(nil, taskID, "X"); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := store.InsertSecurityFinding(nil, store.SecurityFinding{}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
`
	out := runRule(t, bos003{}, "internal/agents/something.go", src)
	assertNoFindings(t, out)
}

// Single mutation is fine even without tx — the rule is about >=2.
func TestBOS003_GreenSingleMutation(t *testing.T) {
	src := `
package agents
import "force-orchestrator/internal/store"
func DoOneWrite(id int) error {
	return store.UpdateBountyStatus(nil, id, "X")
}
`
	out := runRule(t, bos003{}, "internal/agents/something.go", src)
	assertNoFindings(t, out)
}

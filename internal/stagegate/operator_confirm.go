package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// OperatorConfirm gates stage advancement on an explicit operator
// click in the dashboard. The dashboard's "advance" button writes a
// SystemConfig key:
//
//	stage_advance_<convoy_id>_<stage_num> = "<operator-name>:<timestamp>"
//
// The gate reads that key and passes when it's non-empty. Config
// carries an optional prompt string surfaced to the dashboard so the
// operator knows what they're confirming.
//
// Config: {"prompt": string}  (optional; rendered in the dashboard)
//
// Per the D5.5 anti-cheat directive ("No Slack-message-triggers-stage-
// advance"), only the explicit dashboard write — or a real operator
// invocation of the API endpoint — flips this gate. Slack signal is
// read-only.
type OperatorConfirm struct{}

// Type implements Gate.
func (OperatorConfirm) Type() string { return "operator_confirm" }

// Evaluate implements Gate.
func (OperatorConfirm) Evaluate(_ context.Context, db *sql.DB, stage StageContext) (bool, string, error) {
	key := fmt.Sprintf("stage_advance_%d_%d", stage.ConvoyID, stage.StageNum)
	val := getConfigForStageAdvance(db, key)
	if val == "" {
		var cfg struct {
			Prompt string `json:"prompt"`
		}
		// Best-effort parse: missing/malformed config is fine — the
		// pending reason just omits the prompt. We don't fail on bad
		// JSON here because operator_confirm is the most user-facing
		// gate and a partial config shouldn't deadlock the stage; the
		// validator at planning time (P2) catches structural issues.
		_ = json.Unmarshal(stage.GateConfig, &cfg)
		if cfg.Prompt == "" {
			return false, "awaiting operator confirm", ErrPending
		}
		return false, fmt.Sprintf("awaiting operator confirm: %s", cfg.Prompt), ErrPending
	}
	return true, fmt.Sprintf("operator confirmed: %s", val), nil
}

// getConfigForStageAdvance reads a SystemConfig row directly. Using a
// local function (rather than depending on store.GetConfig) keeps
// internal/stagegate free of an internal/store import — store imports
// types from many places and pulling it in here would risk an import
// cycle once store helpers start consulting the registry.
func getConfigForStageAdvance(db *sql.DB, key string) string {
	if db == nil {
		return ""
	}
	var v string
	err := db.QueryRow(`SELECT value FROM SystemConfig WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

package adversarial

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"force-orchestrator/internal/store"
)

// CriticOutcome is what the critic LLM returned: a structured decision
// + the prompt-version tag that produced it. Both fields are required;
// an empty PromptVersion fails closed via ErrIdenticalPromptVersions
// (treating "" == primary's "" as a sham match).
type CriticOutcome struct {
	Outcome       string
	PromptVersion string
}

// CriticFn is the critic-side LLM caller, injected at the call site.
// Real wiring (council.go / medic.go / convoy.go) provides a function
// that loads the agent's `*-critic` profile, builds an opposite-framing
// prompt, calls claude.AskClaudeCLIContext, and parses the response
// into a CriticOutcome.
//
// Tests inject a deterministic stub so CI never makes real LLM calls.
type CriticFn func(ctx context.Context, primary PrimaryDecision) (CriticOutcome, error)

// RunAdversarialPairWith runs the critic via the provided CriticFn,
// computes agreement, persists the AdversarialPairings row, and
// returns the resulting Pair. This is the testable variant; production
// call sites use one of the wrapper functions in council.go /
// medic.go / convoy.go which load the appropriate critic profile and
// pass the wired CriticFn.
//
// Anti-cheat: returns ErrIdenticalPromptVersions when the primary and
// critic prompt versions match (or either is empty). The whole point
// of the pair is independent prompts; same-prompt pairs collapse the
// disagreement signal to noise and would falsely inflate the
// agreement-rate dashboard metric.
func RunAdversarialPairWith(ctx context.Context, db *sql.DB, primary PrimaryDecision, critic CriticFn) (*Pair, error) {
	if db == nil {
		return nil, fmt.Errorf("adversarial.RunAdversarialPairWith: db is required")
	}
	if critic == nil {
		return nil, fmt.Errorf("adversarial.RunAdversarialPairWith: critic function is required")
	}
	if primary.PromptVersion == "" {
		return nil, ErrIdenticalPromptVersions
	}
	if primary.Agent == "" {
		return nil, fmt.Errorf("adversarial.RunAdversarialPairWith: primary.Agent is required")
	}

	co, err := critic(ctx, primary)
	if err != nil {
		return nil, fmt.Errorf("adversarial: critic call failed: %w", err)
	}
	if co.PromptVersion == "" || co.PromptVersion == primary.PromptVersion {
		return nil, ErrIdenticalPromptVersions
	}

	agreement := outcomesAgree(primary.Outcome, co.Outcome)
	agreementInt := 0
	if agreement {
		agreementInt = 1
	}

	res, err := db.ExecContext(ctx, `
		INSERT INTO AdversarialPairings (
			decision_id, agent,
			primary_outcome, critic_outcome,
			prompt_version_primary, prompt_version_critic,
			agreement, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		primary.DecisionID, string(primary.Agent),
		primary.Outcome, co.Outcome,
		primary.PromptVersion, co.PromptVersion,
		agreementInt, store.NowSQLite(),
	)
	if err != nil {
		return nil, fmt.Errorf("adversarial: insert pairing: %w", err)
	}
	pairID, _ := res.LastInsertId()

	pair := &Pair{
		ID:                   pairID,
		DecisionID:           primary.DecisionID,
		Agent:                primary.Agent,
		PrimaryOutcome:       primary.Outcome,
		CriticOutcome:        co.Outcome,
		PromptVersionPrimary: primary.PromptVersion,
		PromptVersionCritic:  co.PromptVersion,
		Agreement:            agreement,
		CreatedAt:            store.NowSQLite(),
	}
	return pair, nil
}

// outcomesAgree compares two structured-decision payloads (typically
// JSON). The comparison is whitespace-insensitive but otherwise exact —
// we don't try to be clever about field ordering, since the production
// LLM outputs are produced by a single strict-JSON-marshalling code
// path on each side.
func outcomesAgree(a, b string) bool {
	return strings.Join(strings.Fields(a), "") == strings.Join(strings.Fields(b), "")
}

// SurfaceDisagreementToOperatorWith writes a Fleet_Mail row for the
// operator + sets AdversarialPairings.surfaced_at on the pair. The
// optional `body` argument lets call sites add context-rich rendering;
// pass "" for the canonical format.
//
// Idempotent: a pair already surfaced (surfaced_at != '') becomes a
// no-op. Returns nil on a no-op rather than an error so callers don't
// need to special-case re-runs.
func SurfaceDisagreementToOperatorWith(ctx context.Context, db *sql.DB, pairID int64, operatorAgent string) error {
	if db == nil {
		return fmt.Errorf("adversarial.SurfaceDisagreementToOperator: db is required")
	}
	if pairID <= 0 {
		return fmt.Errorf("adversarial.SurfaceDisagreementToOperator: pairID must be > 0")
	}
	if operatorAgent == "" {
		operatorAgent = "operator"
	}

	var (
		surfacedAt        string
		decisionID        int64
		agentName         string
		primaryOutcome    string
		criticOutcome     string
		promptPrimary     string
		promptCritic      string
		agreement         int
	)
	err := db.QueryRowContext(ctx, `
		SELECT IFNULL(surfaced_at, ''), decision_id, agent,
		       primary_outcome, critic_outcome,
		       IFNULL(prompt_version_primary, ''), IFNULL(prompt_version_critic, ''),
		       IFNULL(agreement, 0)
		FROM AdversarialPairings WHERE id = ?`, pairID,
	).Scan(&surfacedAt, &decisionID, &agentName, &primaryOutcome, &criticOutcome, &promptPrimary, &promptCritic, &agreement)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("adversarial.SurfaceDisagreementToOperator: pair %d not found", pairID)
	}
	if err != nil {
		return fmt.Errorf("adversarial.SurfaceDisagreementToOperator: load pair %d: %w", pairID, err)
	}

	if surfacedAt != "" {
		// Already surfaced — idempotent no-op.
		return nil
	}
	if agreement == 1 {
		// Agreement = no surface needed; operator only sees disagreements.
		return nil
	}

	subject := fmt.Sprintf("Adversarial pair disagreement: %s decision %d", agentName, decisionID)
	body := fmt.Sprintf(
		"The %s primary prompt (%s) and critic prompt (%s) disagreed on decision %d.\n\n"+
			"PRIMARY OUTCOME:\n%s\n\nCRITIC OUTCOME:\n%s\n\n"+
			"Pair ID: %d. Resolve via the operator dashboard.",
		agentName, promptPrimary, promptCritic, decisionID,
		truncate(primaryOutcome, 2000), truncate(criticOutcome, 2000), pairID,
	)
	// P27 burn-down: budget-gate the operator emit before SendMail.
	// Adversarial-pair surfacing is a high-stakes operator-action
	// signal — StakesHigh always punches through, so the gate's
	// allowed=false branch only fires on a real config-set 0-cap row.
	if allowed, _ := store.RespectNotificationBudget(
		ctx, db, "operator", "adversarial-pairing", "email", "{}",
		store.StakesHigh,
	); !allowed {
		return fmt.Errorf("adversarial.SurfaceDisagreementToOperator: budget governor refused emit for pair %d", pairID)
	}
	mailID := store.SendMail(db, "adversarial-pairing", operatorAgent, subject, body, int(decisionID), store.MailTypeAlert)
	if mailID <= 0 {
		return fmt.Errorf("adversarial.SurfaceDisagreementToOperator: SendMail returned mailID=%d", mailID)
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE AdversarialPairings SET surfaced_at = ? WHERE id = ? AND IFNULL(surfaced_at, '') = ''`,
		store.NowSQLite(), pairID,
	); err != nil {
		return fmt.Errorf("adversarial.SurfaceDisagreementToOperator: mark surfaced: %w", err)
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// LoadPair re-reads an AdversarialPairings row by id. Used by surface
// callbacks + replay tooling.
func LoadPair(ctx context.Context, db *sql.DB, id int64) (*Pair, error) {
	if db == nil {
		return nil, fmt.Errorf("adversarial.LoadPair: db is required")
	}
	var (
		p Pair
		ag int
	)
	var agentStr string
	err := db.QueryRowContext(ctx, `
		SELECT id, decision_id, agent,
		       primary_outcome, critic_outcome,
		       IFNULL(prompt_version_primary, ''), IFNULL(prompt_version_critic, ''),
		       IFNULL(agreement, 0), IFNULL(surfaced_at, ''),
		       IFNULL(operator_resolution, ''), IFNULL(created_at, '')
		FROM AdversarialPairings WHERE id = ?`, id,
	).Scan(&p.ID, &p.DecisionID, &agentStr,
		&p.PrimaryOutcome, &p.CriticOutcome,
		&p.PromptVersionPrimary, &p.PromptVersionCritic,
		&ag, &p.SurfacedAt, &p.OperatorResolution, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("adversarial.LoadPair: id %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	p.Agent = Agent(agentStr)
	p.Agreement = ag == 1
	return &p, nil
}

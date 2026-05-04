-- pr_review_triage_cost_per_call@2026-05-02 fixture test.
--
-- One run against one task; two pr-review-triage transcripts at $0.001 +
-- $0.003 → expected mean = $0.002.

INSERT INTO Experiments
    (id, name, hypothesis_text, stakes_tier, subject_agent, assignment_unit, status, created_by)
VALUES (1, 'fixture-pr_review_triage', 'fixture', 'low', 'pr-review-triage', 'task', 'running', 'fixture');

INSERT INTO ExperimentRuns
    (id, experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, completed_at)
VALUES
    (10, 1, 1, 'task', 100, 'paired_real', datetime('now'));

INSERT INTO LLMCallTranscripts
    (id, task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt, response_text, cost_usd)
VALUES
    (1, 100, 'pr-review-triage', 'pr-review-triage-v1', datetime('now'), 'sys', 'usr', 'ok', 0.001),
    (2, 100, 'pr-review-triage', 'pr-review-triage-v1', datetime('now'), 'sys', 'usr', 'ok', 0.003);

-- pr_review_triage_accuracy@2026-05-02 fixture test.

INSERT INTO Experiments
    (id, name, hypothesis_text, stakes_tier, subject_agent, assignment_unit, status, created_by)
VALUES (1, 'fixture-pr_review_triage_accuracy', 'fixture', 'low', 'pr-review-triage', 'task', 'running', 'fixture');

INSERT INTO ExperimentRuns
    (id, experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, completed_at)
VALUES (10, 1, 1, 'task', 100, 'paired_real', datetime('now'));

INSERT INTO LLMCallTranscripts
    (id, task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt, response_text)
VALUES (1, 100, 'pr-review-triage', 'pr-review-triage-v1', datetime('now'), 'sys', 'usr', 'ok response');

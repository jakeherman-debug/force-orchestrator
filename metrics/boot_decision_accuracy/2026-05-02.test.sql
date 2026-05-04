-- boot_decision_accuracy@2026-05-02 fixture test.
--
-- Two runs against two tasks; one Boot transcript clean, one tagged
-- with [error]. Expected per-run score: 1.0 and 0.0.

INSERT INTO Experiments
    (id, name, hypothesis_text, stakes_tier, subject_agent, assignment_unit, status, created_by)
VALUES (1, 'fixture-boot', 'fixture', 'low', 'boot', 'task', 'running', 'fixture');

INSERT INTO ExperimentRuns
    (id, experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, completed_at)
VALUES
    (10, 1, 1, 'task', 100, 'paired_real', datetime('now')),
    (11, 1, 1, 'task', 101, 'paired_real', datetime('now'));

INSERT INTO LLMCallTranscripts
    (id, task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt, response_text)
VALUES
    (1, 100, 'boot', 'boot-triage-v1', datetime('now'), 'sys', 'usr', '{"decision":"WARN","reason":"ok"}'),
    (2, 101, 'boot', 'boot-triage-v1', datetime('now'), 'sys', 'usr', 'partial output\n[error] claude CLI failed');

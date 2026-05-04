-- commander_plan_validity@2026-05-02 fixture test.

INSERT INTO Experiments
    (id, name, hypothesis_text, stakes_tier, subject_agent, assignment_unit, status, created_by)
VALUES (1, 'fixture-commander_plan_validity', 'fixture', 'low', 'commander', 'task', 'running', 'fixture');

INSERT INTO ExperimentRuns
    (id, experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, completed_at)
VALUES (10, 1, 1, 'task', 100, 'paired_real', datetime('now'));

INSERT INTO LLMCallTranscripts
    (id, task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt, response_text)
VALUES (1, 100, 'commander', 'commander-v1', datetime('now'), 'sys', 'usr', 'ok response');

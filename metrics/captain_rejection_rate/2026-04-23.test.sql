-- captain_rejection_rate@2026-04-23 fixture test.
--
-- Seeds a tiny experiment + 3 runs (2 with Captain rejections, 1 clean)
-- and verifies the SQL returns the expected rate. Run by the runner's
-- ephemeral fixture-DB on daemon start; metrics whose test fails are
-- disabled until fixed (per paired-runs.md § Metric Registry).

-- Fixture: one experiment, three runs against three tasks. Two of the
-- three Captain reviews rejected; one approved → expected score 2/3.

INSERT INTO Experiments
    (id, name, hypothesis_text, stakes_tier, subject_agent, assignment_unit, status, created_by)
VALUES (1, 'fixture-exp', 'fixture', 'low', 'captain', 'task', 'running', 'fixture');

INSERT INTO ExperimentRuns
    (id, experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, completed_at)
VALUES
    (10, 1, 1, 'task', 100, 'paired_real', datetime('now')),
    (11, 1, 1, 'task', 101, 'paired_real', datetime('now')),
    (12, 1, 1, 'task', 102, 'paired_real', datetime('now'));

INSERT INTO BountyBoard (id, type, status, payload)
VALUES
    (100, 'CodeEdit', 'Completed', 'fixture'),
    (101, 'CodeEdit', 'Completed', 'fixture'),
    (102, 'CodeEdit', 'Completed', 'fixture');

INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome)
VALUES
    (100, 1, 'Captain-1', 'sess-A', '{}', 'Rejected'),
    (101, 1, 'Captain-1', 'sess-B', '{}', 'Rejected'),
    (102, 1, 'Captain-1', 'sess-C', '{}', 'Completed');

-- Expectation: each run has exactly one Captain review.
-- Run 10: rejected (1/1 = 1.0)
-- Run 11: rejected (1/1 = 1.0)
-- Run 12: not-rejected (0/1 = 0.0)
-- Per-run scores aggregate to a single value when the runner takes the
-- mean: (1+1+0)/3 = 0.6667.

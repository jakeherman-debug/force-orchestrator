-- captain_rejection_rate@2026-04-23
--
-- Fraction of Captain reviews that rejected, per (run_id, score) for
-- experiments scoring against this metric. Run-level scoping treats
-- one ExperimentRuns row as the natural unit; the join through
-- BountyBoard surfaces the Captain rulings on tasks attached to the
-- run's natural unit.
--
-- Convention: returned value is in [0.0, 1.0]; lower is better
-- (fewer rejections == higher Captain agreement with the agent).
--
-- The :experiment_id parameter is bound by the runner; the SQL uses
-- the named-parameter syntax SQLite accepts.

WITH run_tasks AS (
    SELECT
        er.id AS run_id,
        bb.id AS task_id,
        bb.status AS final_status
    FROM ExperimentRuns er
    JOIN BountyBoard bb
      ON er.natural_unit_kind = 'task' AND bb.id = er.natural_unit_id
    WHERE er.experiment_id = :experiment_id
      AND er.completed_at != ''
)
SELECT
    rt.run_id,
    CAST(SUM(CASE WHEN th.outcome = 'Rejected' AND th.agent LIKE 'Captain%' THEN 1 ELSE 0 END) AS REAL)
        /
    NULLIF(SUM(CASE WHEN th.agent LIKE 'Captain%' THEN 1 ELSE 0 END), 0)
        AS score
FROM run_tasks rt
LEFT JOIN TaskHistory th ON th.task_id = rt.task_id
GROUP BY rt.run_id;

-- commander_plan_validity@2026-05-02
--
-- Per-run score in [0, 1]; higher is better. The :experiment_id
-- parameter is bound by the runner. Engineering-deliverable proxy:
-- LLMCallTranscripts row presence + non-error response_text. D7's
-- promoted experiments will graduate to ground-truth-backed metric
-- versions before terminating.

WITH run_calls AS (
    SELECT
        er.id AS run_id,
        t.id  AS transcript_id,
        t.response_text
    FROM ExperimentRuns er
    LEFT JOIN LLMCallTranscripts t
      ON t.task_id = er.natural_unit_id
     AND t.agent = 'commander'
    WHERE er.experiment_id = :experiment_id
      AND er.natural_unit_kind = 'task'
)
SELECT
    rc.run_id,
    CAST(SUM(CASE WHEN rc.transcript_id IS NOT NULL
                  AND rc.response_text NOT LIKE '%[error]%'
                  THEN 1 ELSE 0 END) AS REAL)
        /
    NULLIF(SUM(CASE WHEN rc.transcript_id IS NOT NULL THEN 1 ELSE 0 END), 0)
        AS score
FROM run_calls rc
GROUP BY rc.run_id;

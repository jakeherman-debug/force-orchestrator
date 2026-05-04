-- boot_decision_accuracy@2026-05-02
--
-- Fraction of Boot triage decisions that completed without an
-- error-tagged response_text in LLMCallTranscripts (a strong proxy
-- for "the call returned a parseable BootVerdict"). Score in [0, 1];
-- higher is better. The :experiment_id parameter is bound by the
-- runner.
--
-- D7: this is the structurally-analogous quality metric the model-
-- downgrade ship gate evaluates against. The actual ground-truth
-- ratification (operator-flagged "Boot was wrong" cases) is added in
-- a later metric version once D7's experiments terminate; for the
-- engineering deliverable we use the parse-success proxy so the
-- harness has a metric to bind to.

WITH run_calls AS (
    SELECT
        er.id AS run_id,
        t.id  AS transcript_id,
        t.response_text
    FROM ExperimentRuns er
    LEFT JOIN LLMCallTranscripts t
      ON t.task_id = er.natural_unit_id
     AND t.agent = 'boot'
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

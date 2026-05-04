-- librarian_cost_per_call@2026-05-02
--
-- Mean cost in USD per Claude call attributed to agent='librarian'
-- inside the experiment's runs. Per-run score: average cost across
-- all LLMCallTranscripts rows whose task_id maps to the run's
-- natural_unit. The ship gate compares haiku (treatment) cost-per-
-- call against control cost-per-call as a fraction (haiku < 0.4 ×
-- control). The :experiment_id parameter is bound by the runner.
--
-- Anti-cheat (paired-runs.md § Anti-cheat directives): cost alone
-- never declares a winner. PromotionAuthor evaluates this metric
-- AND the primary quality metric — both must clear before a
-- PromotionProposal mints.

WITH run_calls AS (
    SELECT
        er.id AS run_id,
        t.cost_usd
    FROM ExperimentRuns er
    LEFT JOIN LLMCallTranscripts t
      ON t.task_id = er.natural_unit_id
     AND t.agent = 'librarian'
    WHERE er.experiment_id = :experiment_id
      AND er.natural_unit_kind = 'task'
)
SELECT
    rc.run_id,
    CAST(IFNULL(AVG(rc.cost_usd), 0) AS REAL) AS score
FROM run_calls rc
GROUP BY rc.run_id;

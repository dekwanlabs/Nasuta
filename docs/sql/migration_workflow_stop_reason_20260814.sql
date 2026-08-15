-- Persist the structured convergence reason on terminal workflow runs.
--
-- Requirements:
--   1. MySQL 8.0.13 or newer.
--   2. Select the target database first, for example: USE nasuta;
--   3. Back up the database and stop Nasuta/CodeLoom writes during migration.

ALTER TABLE workflow_runs
    ADD COLUMN stop_reason VARCHAR(64) NOT NULL DEFAULT '' AFTER error_code;

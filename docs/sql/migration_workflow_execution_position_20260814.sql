-- Persist the immutable round and depth position used for dispatch admission.
--
-- Requirements:
--   1. MySQL 8.0.13 or newer.
--   2. Select the target database first, for example: USE nasuta;
--   3. Back up the database and stop Nasuta/CodeLoom writes during migration.

ALTER TABLE workflow_runs
    ADD COLUMN round_number INT NOT NULL DEFAULT 1 AFTER parent_run_id,
    ADD COLUMN base_depth INT NOT NULL DEFAULT 0 AFTER round_number;

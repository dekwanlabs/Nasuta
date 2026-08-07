ALTER TABLE workflow_runs
    ADD COLUMN parent_run_id VARCHAR(64) NOT NULL DEFAULT '' AFTER id,
    ADD KEY idx_parent_run (parent_run_id, started_at, id);

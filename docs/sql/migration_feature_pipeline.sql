ALTER TABLE feature_generation_runs
    ADD COLUMN workflow_run_id VARCHAR(64) NOT NULL DEFAULT '' AFTER parent_artifact_id,
    ADD COLUMN workflow_node_id VARCHAR(128) NOT NULL DEFAULT '' AFTER workflow_run_id,
    ADD COLUMN workflow_attempt INT NOT NULL DEFAULT 0 AFTER workflow_node_id,
    ADD COLUMN artifact_id VARCHAR(64) NOT NULL DEFAULT '' AFTER workflow_attempt,
    ADD UNIQUE KEY uniq_workflow_node_attempt (
        workflow_run_id,
        workflow_node_id,
        workflow_attempt
    ),
    ADD KEY idx_generation_artifact (artifact_id),
    ADD KEY idx_workflow_node_success (
        workflow_run_id,
        workflow_node_id,
        status,
        workflow_attempt
    );

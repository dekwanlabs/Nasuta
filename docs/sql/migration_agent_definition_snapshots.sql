ALTER TABLE agent_runs
    ADD COLUMN agent_id VARCHAR(128) NOT NULL DEFAULT '' AFTER session_id,
    ADD COLUMN definition_version BIGINT NOT NULL DEFAULT 0 AFTER agent_id,
    ADD COLUMN definition_hash CHAR(64) NOT NULL DEFAULT '' AFTER definition_version,
    ADD COLUMN tool_snapshot_id VARCHAR(80) NOT NULL DEFAULT '' AFTER definition_hash,
    ADD COLUMN input_schema_version BIGINT NOT NULL DEFAULT 0 AFTER tool_snapshot_id,
    ADD COLUMN output_schema_version BIGINT NOT NULL DEFAULT 0 AFTER input_schema_version,
    ADD COLUMN parent_run_id VARCHAR(64) NOT NULL DEFAULT '' AFTER output_schema_version,
    ADD COLUMN workflow_run_id VARCHAR(64) NOT NULL DEFAULT '' AFTER parent_run_id,
    ADD COLUMN workflow_node_id VARCHAR(128) NOT NULL DEFAULT '' AFTER workflow_run_id,
    ADD COLUMN error_code VARCHAR(64) NOT NULL DEFAULT '' AFTER status,
    ADD KEY idx_agent_version (agent_id, definition_version),
    ADD KEY idx_workflow_node (workflow_run_id, workflow_node_id);

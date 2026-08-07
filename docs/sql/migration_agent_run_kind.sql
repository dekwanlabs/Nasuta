ALTER TABLE agent_runs
    ADD COLUMN run_kind VARCHAR(16) NOT NULL DEFAULT 'agent' AFTER id,
    ADD KEY idx_run_kind (run_kind);

CREATE TABLE IF NOT EXISTS workflow_runs (
    id VARCHAR(64) PRIMARY KEY,
    workflow_id VARCHAR(128) NOT NULL,
    workflow_version BIGINT NOT NULL,
    workflow_hash CHAR(64) NOT NULL,
    selection_json JSON NOT NULL,
    input_hash CHAR(64) NOT NULL,
    actor_user_id BIGINT NOT NULL DEFAULT 0,
    actor_tenant_id VARCHAR(128) NOT NULL DEFAULT '',
    actor_permissions_json JSON NOT NULL,
    scenario VARCHAR(128) NOT NULL DEFAULT '',
    scenario_permissions_json JSON NOT NULL,
    status VARCHAR(24) NOT NULL,
    budget_json JSON NOT NULL,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0,
    tool_call_count BIGINT NOT NULL DEFAULT 0,
    cost_micros BIGINT NOT NULL DEFAULT 0,
    retry_count BIGINT NOT NULL DEFAULT 0,
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ended_at TIMESTAMP NULL,
    KEY idx_workflow_started (workflow_id, workflow_version, started_at, id),
    KEY idx_status_started (status, started_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_node_runs (
    workflow_run_id VARCHAR(64) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    attempt INT NOT NULL,
    kind VARCHAR(24) NOT NULL,
    agent_run_id VARCHAR(64) NOT NULL DEFAULT '',
    input_handoff_ids_json JSON NOT NULL,
    output_handoff_id VARCHAR(64) NOT NULL DEFAULT '',
    status VARCHAR(24) NOT NULL,
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0,
    tool_call_count BIGINT NOT NULL DEFAULT 0,
    cost_micros BIGINT NOT NULL DEFAULT 0,
    retry_count BIGINT NOT NULL DEFAULT 0,
    started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ended_at TIMESTAMP NULL,
    PRIMARY KEY (workflow_run_id, node_id, attempt),
    KEY idx_agent_run (agent_run_id),
    KEY idx_status_started (status, started_at, workflow_run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS handoff_artifacts (
    id VARCHAR(64) PRIMARY KEY,
    workflow_run_id VARCHAR(64) NOT NULL,
    producer_node_id VARCHAR(128) NOT NULL,
    producer_run_id VARCHAR(64) NOT NULL DEFAULT '',
    schema_id VARCHAR(128) NOT NULL,
    schema_version BIGINT NOT NULL,
    payload_json JSON NOT NULL,
    references_json JSON NOT NULL,
    completeness VARCHAR(16) NOT NULL,
    content_hash CHAR(64) NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_run_node_hash (workflow_run_id, producer_node_id, content_hash),
    KEY idx_run_created (workflow_run_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_events (
    workflow_run_id VARCHAR(64) NOT NULL,
    seq BIGINT NOT NULL,
    kind VARCHAR(32) NOT NULL,
    node_id VARCHAR(128) NOT NULL DEFAULT '',
    summary TEXT NOT NULL,
    detail_json JSON NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workflow_run_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_approvals (
    workflow_run_id VARCHAR(64) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    decision VARCHAR(16) NOT NULL,
    approver_user_id BIGINT NOT NULL,
    approver_tenant_id VARCHAR(128) NOT NULL,
    comment TEXT NOT NULL,
    decided_at TIMESTAMP NOT NULL,
    PRIMARY KEY (workflow_run_id, node_id),
    KEY idx_approval_decided (workflow_run_id, decided_at, node_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS gate_decisions (
    id VARCHAR(64) PRIMARY KEY,
    workflow_run_id VARCHAR(64) NOT NULL,
    node_id VARCHAR(128) NOT NULL,
    gate_id VARCHAR(128) NOT NULL,
    subject_hash CHAR(64) NOT NULL,
    decision VARCHAR(32) NOT NULL,
    reason_codes_json JSON NOT NULL,
    finding_ids_json JSON NOT NULL,
    evaluated_at TIMESTAMP NOT NULL,
    UNIQUE KEY uniq_run_node_gate (workflow_run_id, node_id, gate_id),
    KEY idx_subject_gate (subject_hash, gate_id, evaluated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

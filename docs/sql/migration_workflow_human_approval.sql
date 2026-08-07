ALTER TABLE workflow_runs
    ADD COLUMN actor_permissions_json JSON NULL AFTER actor_tenant_id,
    ADD COLUMN scenario_permissions_json JSON NULL AFTER scenario;

UPDATE workflow_runs
SET actor_permissions_json = JSON_OBJECT()
WHERE actor_permissions_json IS NULL;

UPDATE workflow_runs
SET scenario_permissions_json = JSON_OBJECT()
WHERE scenario_permissions_json IS NULL;

ALTER TABLE workflow_runs
    MODIFY COLUMN actor_permissions_json JSON NOT NULL,
    MODIFY COLUMN scenario_permissions_json JSON NOT NULL;

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

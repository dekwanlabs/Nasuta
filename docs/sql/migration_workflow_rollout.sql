ALTER TABLE workflow_runs
    ADD COLUMN selection_json JSON NULL AFTER workflow_hash;

UPDATE workflow_runs
SET selection_json = JSON_OBJECT()
WHERE selection_json IS NULL;

ALTER TABLE workflow_runs
    MODIFY COLUMN selection_json JSON NOT NULL;

CREATE TABLE IF NOT EXISTS workflow_definition_rollouts (
    workflow_id       VARCHAR(128) NOT NULL,
    rule_version      BIGINT NOT NULL,
    candidate_version BIGINT NOT NULL,
    percentage_bps    INT NOT NULL,
    salt              VARCHAR(255) NOT NULL,
    rule_hash         CHAR(64) NOT NULL,
    active            TINYINT(1) NOT NULL DEFAULT 1,
    created_by        BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workflow_id),
    UNIQUE KEY uniq_workflow_rollout_hash (rule_hash),
    KEY idx_workflow_rollout_active (workflow_id, active, rule_version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_definition_rollout_audit (
    seq               BIGINT AUTO_INCREMENT PRIMARY KEY,
    workflow_id       VARCHAR(128) NOT NULL,
    rule_version      BIGINT NOT NULL,
    candidate_version BIGINT NOT NULL,
    percentage_bps    INT NOT NULL,
    rule_hash         CHAR(64) NOT NULL,
    action            VARCHAR(32) NOT NULL,
    actor_user_id     BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_workflow_rollout_audit (workflow_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

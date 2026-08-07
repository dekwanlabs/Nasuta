CREATE TABLE IF NOT EXISTS agent_definitions (
    id              VARCHAR(128) NOT NULL,
    version         BIGINT NOT NULL,
    definition_json JSON NOT NULL,
    content_hash    CHAR(64) NOT NULL,
    active          TINYINT(1) NOT NULL DEFAULT 1,
    is_default      TINYINT(1) NOT NULL DEFAULT 0,
    created_by      BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    default_key     VARCHAR(128) AS (
        CASE WHEN is_default=1 THEN id ELSE NULL END
    ) STORED,
    PRIMARY KEY (id, version),
    UNIQUE KEY uniq_agent_definition_hash (content_hash),
    UNIQUE KEY uniq_agent_definition_default (default_key),
    KEY idx_agent_definition_list (id, version),
    KEY idx_agent_definition_active (id, active, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS agent_definition_audit (
    seq           BIGINT AUTO_INCREMENT PRIMARY KEY,
    definition_id VARCHAR(128) NOT NULL,
    version       BIGINT NOT NULL,
    action        VARCHAR(32) NOT NULL,
    actor_user_id BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_agent_definition_audit (definition_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS agent_definition_rollouts (
    agent_id          VARCHAR(128) NOT NULL,
    rule_version      BIGINT NOT NULL,
    candidate_version BIGINT NOT NULL,
    percentage_bps    INT NOT NULL,
    salt              VARCHAR(255) NOT NULL,
    rule_hash         CHAR(64) NOT NULL,
    active            TINYINT(1) NOT NULL DEFAULT 1,
    created_by        BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (agent_id),
    UNIQUE KEY uniq_agent_rollout_hash (rule_hash),
    KEY idx_agent_rollout_active (agent_id, active, rule_version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS agent_definition_rollout_audit (
    seq               BIGINT AUTO_INCREMENT PRIMARY KEY,
    agent_id          VARCHAR(128) NOT NULL,
    rule_version      BIGINT NOT NULL,
    candidate_version BIGINT NOT NULL,
    percentage_bps    INT NOT NULL,
    rule_hash         CHAR(64) NOT NULL,
    action            VARCHAR(32) NOT NULL,
    actor_user_id     BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_agent_rollout_audit (agent_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_definitions (
    id              VARCHAR(128) NOT NULL,
    version         BIGINT NOT NULL,
    definition_json JSON NOT NULL,
    content_hash    CHAR(64) NOT NULL,
    active          TINYINT(1) NOT NULL DEFAULT 1,
    is_default      TINYINT(1) NOT NULL DEFAULT 0,
    created_by      BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    default_key     VARCHAR(128) AS (
        CASE WHEN is_default=1 THEN id ELSE NULL END
    ) STORED,
    PRIMARY KEY (id, version),
    UNIQUE KEY uniq_workflow_definition_hash (content_hash),
    UNIQUE KEY uniq_workflow_definition_default (default_key),
    KEY idx_workflow_definition_list (id, version),
    KEY idx_workflow_definition_active (id, active, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_definition_audit (
    seq           BIGINT AUTO_INCREMENT PRIMARY KEY,
    definition_id VARCHAR(128) NOT NULL,
    version       BIGINT NOT NULL,
    action        VARCHAR(32) NOT NULL,
    actor_user_id BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_workflow_definition_audit (definition_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

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

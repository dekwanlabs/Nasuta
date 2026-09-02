-- Add the durable run snapshots, delegation ledger, artifacts, and Workflow
-- escalation records required by dynamic QA delegation.
--
-- Requirements:
--   1. MySQL 8.0.13 or newer.
--   2. Select the target database first, for example: USE nasuta;
--   3. Apply migration_align_schema_20260810.sql first.
--   4. Back up the database and stop Nasuta/CodeLoom writes during migration.
--
-- The migration is additive and idempotent.

DROP PROCEDURE IF EXISTS migration_add_column_20260816;
DROP PROCEDURE IF EXISTS migration_add_index_20260816;
DROP PROCEDURE IF EXISTS migrate_dynamic_delegation_20260816;

DELIMITER //

CREATE PROCEDURE migration_add_column_20260816(
    IN p_table_name VARCHAR(64),
    IN p_column_name VARCHAR(64),
    IN p_ddl TEXT
)
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = p_table_name
          AND column_name = p_column_name
    ) THEN
        SET @migration_ddl = p_ddl;
        PREPARE migration_statement FROM @migration_ddl;
        EXECUTE migration_statement;
        DEALLOCATE PREPARE migration_statement;
    END IF;
END//

CREATE PROCEDURE migration_add_index_20260816(
    IN p_table_name VARCHAR(64),
    IN p_index_name VARCHAR(64),
    IN p_ddl TEXT
)
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.statistics
        WHERE table_schema = DATABASE()
          AND table_name = p_table_name
          AND index_name = p_index_name
    ) THEN
        SET @migration_ddl = p_ddl;
        PREPARE migration_statement FROM @migration_ddl;
        EXECUTE migration_statement;
        DEALLOCATE PREPARE migration_statement;
    END IF;
END//

CREATE PROCEDURE migrate_dynamic_delegation_20260816()
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'agent_runs'
          AND column_name = 'run_kind'
    ) OR NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'agent_steps'
          AND column_name = 'answer_contract_json'
    ) THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'apply migration_align_schema_20260810.sql before this migration';
    END IF;

    -- Persist the exact capability and budget contract used by each Agent Run.
    CALL migration_add_column_20260816(
        'agent_runs',
        'capability_id',
        'ALTER TABLE agent_runs ADD COLUMN capability_id VARCHAR(128) NOT NULL DEFAULT '''' AFTER parent_run_id'
    );
    CALL migration_add_column_20260816(
        'agent_runs',
        'capability_version',
        'ALTER TABLE agent_runs ADD COLUMN capability_version BIGINT NOT NULL DEFAULT 0 AFTER capability_id'
    );
    CALL migration_add_column_20260816(
        'agent_runs',
        'capability_content_hash',
        'ALTER TABLE agent_runs ADD COLUMN capability_content_hash CHAR(64) NOT NULL DEFAULT '''' AFTER capability_version'
    );
    CALL migration_add_column_20260816(
        'agent_runs',
        'delegation_id',
        'ALTER TABLE agent_runs ADD COLUMN delegation_id VARCHAR(64) NOT NULL DEFAULT '''' AFTER capability_content_hash'
    );
    CALL migration_add_column_20260816(
        'agent_runs',
        'delegation_depth',
        'ALTER TABLE agent_runs ADD COLUMN delegation_depth INT NOT NULL DEFAULT 0 AFTER delegation_id'
    );
    CALL migration_add_column_20260816(
        'agent_runs',
        'run_limits_json',
        'ALTER TABLE agent_runs ADD COLUMN run_limits_json JSON NULL AFTER delegation_depth'
    );
    CALL migration_add_column_20260816(
        'agent_runs',
        'capability_registry_revision',
        'ALTER TABLE agent_runs ADD COLUMN capability_registry_revision BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER run_limits_json'
    );
    CALL migration_add_column_20260816(
        'agent_runs',
        'cost_micros',
        'ALTER TABLE agent_runs ADD COLUMN cost_micros BIGINT NOT NULL DEFAULT 0 AFTER total_tokens'
    );
    CALL migration_add_column_20260816(
        'agent_run_budget_ledger',
        'lease_fence',
        'ALTER TABLE agent_run_budget_ledger ADD COLUMN lease_fence BIGINT NOT NULL DEFAULT 0 AFTER lease_expires_at'
    );

    CREATE TABLE IF NOT EXISTS agent_run_checkpoints (
        run_id VARCHAR(64) PRIMARY KEY,
        step_no INT NOT NULL DEFAULT 0,
        phase VARCHAR(32) NOT NULL,
        input_hash CHAR(64) NOT NULL DEFAULT '',
        prompt_hash CHAR(64) NOT NULL DEFAULT '',
        state_json JSON NOT NULL,
        lease_fence BIGINT NOT NULL DEFAULT 0,
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
        KEY idx_run_checkpoint_updated (updated_at)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    CREATE TABLE IF NOT EXISTS agent_work_items (
        work_id VARCHAR(128) PRIMARY KEY,
        run_id VARCHAR(64) NOT NULL,
        parent_run_id VARCHAR(64) NOT NULL DEFAULT '',
        delegation_id VARCHAR(64) NOT NULL DEFAULT '',
        task_index INT NOT NULL DEFAULT 0,
        attempt_no INT NOT NULL DEFAULT 1,
        kind VARCHAR(32) NOT NULL,
        payload_json JSON NOT NULL,
        state VARCHAR(24) NOT NULL DEFAULT 'ready',
        lease_owner VARCHAR(128) NOT NULL DEFAULT '',
        lease_fence BIGINT NOT NULL DEFAULT 0,
        lease_expires_at TIMESTAMP NULL DEFAULT NULL,
        available_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        attempt_count INT NOT NULL DEFAULT 0,
        last_error TEXT NOT NULL,
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
        UNIQUE KEY uniq_work_delegation_attempt (parent_run_id,delegation_id,task_index,attempt_no),
        KEY idx_work_ready (state,available_at,lease_expires_at),
        KEY idx_work_run (run_id,state)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    UPDATE agent_runs
    SET run_limits_json = JSON_OBJECT()
    WHERE run_limits_json IS NULL;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'agent_runs'
          AND column_name = 'run_limits_json'
          AND is_nullable = 'YES'
    ) THEN
        ALTER TABLE agent_runs
            MODIFY COLUMN run_limits_json JSON NOT NULL;
    END IF;

    CALL migration_add_index_20260816(
        'agent_runs',
        'idx_delegation',
        'CREATE INDEX idx_delegation ON agent_runs (parent_run_id, delegation_id)'
    );

    -- Durable hierarchical budget state. The root row is authoritative for
    -- cross-process admission; reservation rows make task grants and physical
    -- provider calls recoverable and idempotent.
    CREATE TABLE IF NOT EXISTS agent_run_budget_ledger (
        root_run_id VARCHAR(64) PRIMARY KEY,
        limits_json JSON NOT NULL,
        used_usage_json JSON NOT NULL,
        reserved_usage_json JSON NOT NULL,
        price_version VARCHAR(64) NOT NULL DEFAULT '',
        lease_owner VARCHAR(128) NOT NULL DEFAULT '',
        lease_expires_at TIMESTAMP NULL DEFAULT NULL,
        version BIGINT NOT NULL DEFAULT 0,
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
        KEY idx_agent_budget_ledger_updated (updated_at)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    CREATE TABLE IF NOT EXISTS agent_run_budget_reservations (
        reservation_id VARCHAR(160) PRIMARY KEY,
        root_run_id VARCHAR(64) NOT NULL,
        parent_reservation_id VARCHAR(160) NOT NULL DEFAULT '',
        kind VARCHAR(24) NOT NULL,
        phase VARCHAR(24) NOT NULL DEFAULT 'default',
        estimate_usage_json JSON NOT NULL,
        grant_usage_json JSON NOT NULL,
        used_usage_json JSON NOT NULL,
        state VARCHAR(24) NOT NULL,
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        settled_at TIMESTAMP NULL DEFAULT NULL,
        UNIQUE KEY uniq_agent_budget_reservation_root (root_run_id, reservation_id),
        KEY idx_agent_budget_reservation_parent (root_run_id, parent_reservation_id, state),
        KEY idx_agent_budget_reservation_state (root_run_id, state, settled_at)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    -- Record admission, reservation, and settlement for every delegated task.
    CREATE TABLE IF NOT EXISTS agent_delegation_tasks (
        id BIGINT AUTO_INCREMENT PRIMARY KEY,
        parent_run_id VARCHAR(64) NOT NULL,
        delegation_id VARCHAR(64) NOT NULL,
        task_index INT NOT NULL,
        child_run_id VARCHAR(64) NOT NULL DEFAULT '',
        capability_id VARCHAR(128) NOT NULL,
        capability_version BIGINT NOT NULL,
        capability_content_hash CHAR(64) NOT NULL,
        objective_hash CHAR(64) NOT NULL,
        admitted BOOLEAN NOT NULL DEFAULT FALSE,
        rejection_code VARCHAR(64) NOT NULL DEFAULT '',
        reservation_json JSON NOT NULL,
        settled_usage_json JSON NULL,
        report_artifact_id VARCHAR(64) NOT NULL DEFAULT '',
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        settled_at TIMESTAMP NULL DEFAULT NULL,
        child_run_key VARCHAR(64) AS (NULLIF(child_run_id, '')) STORED,
        report_artifact_key VARCHAR(64) AS (NULLIF(report_artifact_id, '')) STORED,
        UNIQUE KEY uniq_delegation_task (parent_run_id, delegation_id, task_index),
        UNIQUE KEY uniq_delegation_child_run (child_run_key),
        UNIQUE KEY uniq_delegation_report (report_artifact_key),
        KEY idx_delegation_parent (parent_run_id, created_at)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    -- Store authoritative reports and verification results outside step traces.
    CREATE TABLE IF NOT EXISTS agent_delegation_attempts (
        id BIGINT AUTO_INCREMENT PRIMARY KEY,
        parent_run_id VARCHAR(64) NOT NULL,
        delegation_id VARCHAR(64) NOT NULL,
        task_index INT NOT NULL,
        attempt_no INT NOT NULL,
        attempt_id VARCHAR(64) NOT NULL,
        child_run_id VARCHAR(64) NOT NULL,
        status VARCHAR(24) NOT NULL,
        retryable BOOLEAN NOT NULL DEFAULT FALSE,
        error_code VARCHAR(64) NOT NULL DEFAULT '',
        error_message TEXT NOT NULL,
        started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        ended_at TIMESTAMP NULL DEFAULT NULL,
        next_attempt_at TIMESTAMP NULL DEFAULT NULL,
        usage_json JSON NULL,
        report_artifact_id VARCHAR(64) NOT NULL DEFAULT '',
        UNIQUE KEY uniq_delegation_attempt (parent_run_id, delegation_id, task_index, attempt_no),
        UNIQUE KEY uniq_delegation_attempt_id (attempt_id),
        UNIQUE KEY uniq_delegation_attempt_child (child_run_id),
        KEY idx_delegation_attempt_latest (parent_run_id, delegation_id, task_index, attempt_no),
        KEY idx_delegation_attempt_status (status, next_attempt_at)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    CREATE TABLE IF NOT EXISTS agent_delegation_checkpoints (
        parent_run_id VARCHAR(64) NOT NULL,
        delegation_id VARCHAR(64) NOT NULL,
        task_index INT NOT NULL,
        invocation_id VARCHAR(128) NOT NULL DEFAULT '',
        request_hash CHAR(64) NOT NULL DEFAULT '',
        status VARCHAR(24) NOT NULL,
        child_run_id VARCHAR(64) NOT NULL DEFAULT '',
        report_artifact_id VARCHAR(64) NOT NULL DEFAULT '',
        error_code VARCHAR(64) NOT NULL DEFAULT '',
        error_message TEXT NOT NULL,
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
        PRIMARY KEY (parent_run_id, delegation_id, task_index),
        KEY idx_delegation_checkpoint_status (status, updated_at),
        KEY idx_delegation_checkpoint_invocation (invocation_id)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    CREATE TABLE IF NOT EXISTS agent_run_artifacts (
        artifact_id VARCHAR(64) PRIMARY KEY,
        run_id VARCHAR(64) NOT NULL,
        kind VARCHAR(64) NOT NULL,
        schema_id VARCHAR(128) NOT NULL,
        schema_version BIGINT NOT NULL,
        content_hash CHAR(64) NOT NULL,
        content LONGBLOB NOT NULL,
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        UNIQUE KEY uniq_run_artifact_kind (run_id, kind),
        KEY idx_run_artifact_run (run_id, created_at)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

    -- Persist which delegation reports were adopted by each Agent step.
    CALL migration_add_column_20260816(
        'agent_steps',
        'delegation_adoptions_json',
        'ALTER TABLE agent_steps ADD COLUMN delegation_adoptions_json JSON NULL AFTER answer_contract_json'
    );

    -- Make a QA-to-Workflow escalation idempotent and recoverable.
    CREATE TABLE IF NOT EXISTS workflow_escalations (
        parent_run_id VARCHAR(64) NOT NULL,
        request_id VARCHAR(128) NOT NULL,
        request_hash CHAR(64) NOT NULL,
        workflow_run_id VARCHAR(64) NOT NULL,
        binding_id VARCHAR(128) NOT NULL DEFAULT '',
        binding_version BIGINT NOT NULL DEFAULT 0,
        status VARCHAR(24) NOT NULL,
        error_code VARCHAR(64) NOT NULL DEFAULT '',
        created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
        PRIMARY KEY (parent_run_id, request_id),
        UNIQUE KEY uniq_workflow_escalation_run (workflow_run_id),
        KEY idx_workflow_escalation_binding (binding_id, binding_version, created_at)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
END//

DELIMITER ;

CALL migrate_dynamic_delegation_20260816();

DROP PROCEDURE migration_add_column_20260816;
DROP PROCEDURE migration_add_index_20260816;
DROP PROCEDURE migrate_dynamic_delegation_20260816;

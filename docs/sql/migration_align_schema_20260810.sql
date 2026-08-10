-- Align the larger legacy schema with the compact Nasuta schema exported on
-- 2026-08-10.
--
-- Requirements:
--   1. MySQL 8.0.13 or newer.
--   2. Select the target database first, for example: USE nasuta;
--   3. Back up the database and stop Nasuta/CodeLoom writes during migration.
--
-- The migration is additive and idempotent. Legacy source tables are kept as
-- rollback copies after their data has been merged into the compact tables.

DROP PROCEDURE IF EXISTS migration_add_column_20260810;
DROP PROCEDURE IF EXISTS migration_add_index_20260810;
DROP PROCEDURE IF EXISTS migrate_compact_schema_20260810;

DELIMITER //

CREATE PROCEDURE migration_add_column_20260810(
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

CREATE PROCEDURE migration_add_index_20260810(
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

CREATE PROCEDURE migrate_compact_schema_20260810()
BEGIN
    DECLARE required_table_count INT DEFAULT 0;
    DECLARE source_table_exists INT DEFAULT 0;
    DECLARE invalid_count BIGINT DEFAULT 0;
    DECLARE conflict_count BIGINT DEFAULT 0;
    DECLARE remaining_count BIGINT DEFAULT 0;

    SELECT COUNT(DISTINCT table_name)
    INTO required_table_count
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name IN (
          'agent_runs',
          'agent_steps',
          'feature_artifacts',
          'feature_generation_runs',
          'feature_implementation_runs',
          'qa_turns',
          'runtime_events'
      );

    IF required_table_count <> 7 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'compact schema migration is missing required target tables';
    END IF;

    -- Agent runs: preserve the legacy LONGTEXT question column and add only
    -- the execution identity/snapshot fields required by the compact schema.
    CALL migration_add_column_20260810(
        'agent_runs',
        'run_kind',
        'ALTER TABLE agent_runs ADD COLUMN run_kind VARCHAR(16) NOT NULL DEFAULT ''agent'' AFTER id'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'agent_id',
        'ALTER TABLE agent_runs ADD COLUMN agent_id VARCHAR(128) NOT NULL DEFAULT '''' AFTER session_id'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'definition_version',
        'ALTER TABLE agent_runs ADD COLUMN definition_version BIGINT NOT NULL DEFAULT 0 AFTER agent_id'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'definition_hash',
        'ALTER TABLE agent_runs ADD COLUMN definition_hash CHAR(64) NOT NULL DEFAULT '''' AFTER definition_version'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'selection_json',
        'ALTER TABLE agent_runs ADD COLUMN selection_json JSON NULL AFTER definition_hash'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'tool_snapshot_id',
        'ALTER TABLE agent_runs ADD COLUMN tool_snapshot_id VARCHAR(80) NOT NULL DEFAULT '''' AFTER selection_json'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'input_schema_version',
        'ALTER TABLE agent_runs ADD COLUMN input_schema_version BIGINT NOT NULL DEFAULT 0 AFTER tool_snapshot_id'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'output_schema_version',
        'ALTER TABLE agent_runs ADD COLUMN output_schema_version BIGINT NOT NULL DEFAULT 0 AFTER input_schema_version'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'parent_run_id',
        'ALTER TABLE agent_runs ADD COLUMN parent_run_id VARCHAR(64) NOT NULL DEFAULT '''' AFTER output_schema_version'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'workflow_run_id',
        'ALTER TABLE agent_runs ADD COLUMN workflow_run_id VARCHAR(64) NOT NULL DEFAULT '''' AFTER parent_run_id'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'workflow_node_id',
        'ALTER TABLE agent_runs ADD COLUMN workflow_node_id VARCHAR(128) NOT NULL DEFAULT '''' AFTER workflow_run_id'
    );
    CALL migration_add_column_20260810(
        'agent_runs',
        'error_code',
        'ALTER TABLE agent_runs ADD COLUMN error_code VARCHAR(64) NOT NULL DEFAULT '''' AFTER status'
    );

    UPDATE agent_runs
    SET selection_json = JSON_OBJECT()
    WHERE selection_json IS NULL;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'agent_runs'
          AND column_name = 'selection_json'
          AND is_nullable = 'YES'
    ) THEN
        ALTER TABLE agent_runs
            MODIFY COLUMN selection_json JSON NOT NULL;
    END IF;

    CALL migration_add_index_20260810(
        'agent_runs',
        'idx_run_kind',
        'CREATE INDEX idx_run_kind ON agent_runs (run_kind)'
    );
    CALL migration_add_index_20260810(
        'agent_runs',
        'idx_agent_version',
        'CREATE INDEX idx_agent_version ON agent_runs (agent_id, definition_version)'
    );
    CALL migration_add_index_20260810(
        'agent_runs',
        'idx_workflow_node',
        'CREATE INDEX idx_workflow_node ON agent_runs (workflow_run_id, workflow_node_id)'
    );

    -- Agent step artifacts: merge the legacy blob table into its owning step.
    CALL migration_add_column_20260810(
        'agent_steps',
        'artifact_content',
        'ALTER TABLE agent_steps ADD COLUMN artifact_content LONGBLOB NULL AFTER duration_ms'
    );
    CALL migration_add_column_20260810(
        'agent_steps',
        'artifact_content_type',
        'ALTER TABLE agent_steps ADD COLUMN artifact_content_type VARCHAR(128) NULL AFTER artifact_content'
    );

    SELECT COUNT(*)
    INTO source_table_exists
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name = 'agent_tool_result_artifacts';

    IF source_table_exists <> 0 THEN
        SELECT COUNT(*)
        INTO invalid_count
        FROM (
            SELECT source.id
            FROM agent_tool_result_artifacts source
            LEFT JOIN agent_steps target
                ON target.artifact_id = source.id
                OR (
                    target.run_id = source.run_id
                    AND target.tool_call_id = source.tool_call_id
                )
            GROUP BY source.id
            HAVING COUNT(target.id) <> 1
        ) invalid_artifact_bindings;

        IF invalid_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'artifact migration found missing or ambiguous agent steps';
        END IF;

        SELECT COUNT(*)
        INTO conflict_count
        FROM agent_tool_result_artifacts source
        INNER JOIN agent_steps target
            ON target.artifact_id = source.id
            OR (
                target.run_id = source.run_id
                AND target.tool_call_id = source.tool_call_id
            )
        WHERE (target.artifact_id <> '' AND target.artifact_id <> source.id)
           OR (
               target.artifact_content IS NOT NULL
               AND NOT (target.artifact_content <=> source.content)
           )
           OR (
               target.artifact_content_type IS NOT NULL
               AND NOT (target.artifact_content_type <=> source.content_type)
           )
           OR (
               target.authoritative_sha256 <> ''
               AND target.authoritative_sha256 <> source.sha256
           )
           OR (
               target.content_bytes <> 0
               AND target.content_bytes <> source.size_bytes
           );

        IF conflict_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'artifact migration found conflicting agent step data';
        END IF;

        UPDATE agent_steps target
        INNER JOIN agent_tool_result_artifacts source
            ON target.artifact_id = source.id
            OR (
                target.run_id = source.run_id
                AND target.tool_call_id = source.tool_call_id
            )
        SET target.artifact_id = source.id,
            target.artifact_content = source.content,
            target.artifact_content_type = source.content_type,
            target.authoritative_sha256 = IF(
                target.authoritative_sha256 = '',
                source.sha256,
                target.authoritative_sha256
            ),
            target.content_bytes = IF(
                target.content_bytes = 0,
                source.size_bytes,
                target.content_bytes
            );

        SELECT COUNT(*)
        INTO remaining_count
        FROM agent_tool_result_artifacts source
        LEFT JOIN agent_steps target
            ON target.artifact_id = source.id
           AND target.run_id = source.run_id
           AND target.tool_call_id = source.tool_call_id
           AND target.artifact_content <=> source.content
           AND target.artifact_content_type <=> source.content_type
        WHERE target.id IS NULL;

        IF remaining_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'artifact migration did not copy every legacy artifact';
        END IF;
    END IF;

    SELECT COUNT(*)
    INTO invalid_count
    FROM (
        SELECT artifact_id
        FROM agent_steps
        WHERE artifact_id <> ''
        GROUP BY artifact_id
        HAVING COUNT(*) > 1
    ) duplicate_artifact_ids;

    IF invalid_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'agent steps contain duplicate non-empty artifact ids';
    END IF;

    SELECT COUNT(*)
    INTO invalid_count
    FROM (
        SELECT run_id, tool_call_id
        FROM agent_steps
        WHERE artifact_id <> ''
        GROUP BY run_id, tool_call_id
        HAVING COUNT(*) > 1
    ) duplicate_artifact_calls;

    IF invalid_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'agent steps contain duplicate artifact tool calls';
    END IF;

    CALL migration_add_column_20260810(
        'agent_steps',
        'artifact_id_key',
        'ALTER TABLE agent_steps ADD COLUMN artifact_id_key VARCHAR(64) GENERATED ALWAYS AS (NULLIF(artifact_id, '''')) STORED'
    );
    CALL migration_add_column_20260810(
        'agent_steps',
        'artifact_tool_call_key',
        'ALTER TABLE agent_steps ADD COLUMN artifact_tool_call_key VARCHAR(128) GENERATED ALWAYS AS (CASE WHEN artifact_id <> '''' THEN tool_call_id ELSE NULL END) STORED'
    );
    CALL migration_add_index_20260810(
        'agent_steps',
        'uniq_agent_step_artifact',
        'CREATE UNIQUE INDEX uniq_agent_step_artifact ON agent_steps (artifact_id_key)'
    );
    CALL migration_add_index_20260810(
        'agent_steps',
        'uniq_agent_step_artifact_call',
        'CREATE UNIQUE INDEX uniq_agent_step_artifact_call ON agent_steps (run_id, artifact_tool_call_key)'
    );

    -- Feature artifact reviews: fold the old one-to-one review row into the
    -- artifact. Historical rows cannot reconstruct Review/Gate bindings, so
    -- those three binding fields use empty strings.
    CALL migration_add_column_20260810(
        'feature_artifacts',
        'review_subject_hash',
        'ALTER TABLE feature_artifacts ADD COLUMN review_subject_hash CHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_artifacts',
        'review_round_id',
        'ALTER TABLE feature_artifacts ADD COLUMN review_round_id VARCHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_artifacts',
        'review_gate_result_id',
        'ALTER TABLE feature_artifacts ADD COLUMN review_gate_result_id VARCHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_artifacts',
        'review_decision',
        'ALTER TABLE feature_artifacts ADD COLUMN review_decision VARCHAR(16) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_artifacts',
        'review_comment',
        'ALTER TABLE feature_artifacts ADD COLUMN review_comment TEXT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_artifacts',
        'review_reviewer',
        'ALTER TABLE feature_artifacts ADD COLUMN review_reviewer BIGINT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_artifacts',
        'review_created_at',
        'ALTER TABLE feature_artifacts ADD COLUMN review_created_at TIMESTAMP NULL'
    );

    SELECT COUNT(*)
    INTO source_table_exists
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name = 'feature_artifact_reviews';

    IF source_table_exists <> 0 THEN
        SELECT COUNT(*)
        INTO invalid_count
        FROM feature_artifact_reviews source
        LEFT JOIN feature_artifacts target
            ON target.id = source.artifact_id
        WHERE target.id IS NULL;

        IF invalid_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'artifact review migration found orphan artifacts';
        END IF;

        SELECT COUNT(*)
        INTO conflict_count
        FROM feature_artifact_reviews source
        INNER JOIN feature_artifacts target
            ON target.id = source.artifact_id
        WHERE (
            target.review_subject_hash IS NOT NULL
            OR target.review_round_id IS NOT NULL
            OR target.review_gate_result_id IS NOT NULL
            OR target.review_decision IS NOT NULL
            OR target.review_comment IS NOT NULL
            OR target.review_reviewer IS NOT NULL
            OR target.review_created_at IS NOT NULL
        )
        AND NOT (
            target.review_subject_hash <=> ''
            AND target.review_round_id <=> ''
            AND target.review_gate_result_id <=> ''
            AND target.review_decision <=> source.decision
            AND target.review_comment <=> source.comment
            AND target.review_reviewer <=> source.reviewer
            AND target.review_created_at <=> source.created_at
        );

        IF conflict_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'artifact review migration found conflicting target data';
        END IF;

        UPDATE feature_artifacts target
        INNER JOIN feature_artifact_reviews source
            ON target.id = source.artifact_id
        SET target.review_subject_hash = '',
            target.review_round_id = '',
            target.review_gate_result_id = '',
            target.review_decision = source.decision,
            target.review_comment = source.comment,
            target.review_reviewer = source.reviewer,
            target.review_created_at = source.created_at;

        SELECT COUNT(*)
        INTO remaining_count
        FROM feature_artifact_reviews source
        LEFT JOIN feature_artifacts target
            ON target.id = source.artifact_id
           AND target.review_subject_hash <=> ''
           AND target.review_round_id <=> ''
           AND target.review_gate_result_id <=> ''
           AND target.review_decision <=> source.decision
           AND target.review_comment <=> source.comment
           AND target.review_reviewer <=> source.reviewer
           AND target.review_created_at <=> source.created_at
        WHERE target.id IS NULL;

        IF remaining_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'artifact review migration did not copy every review';
        END IF;
    END IF;

    -- Feature generation runs: bind Workflow node attempts and generated
    -- artifacts. The functional unique index ignores unbound API runs.
    CALL migration_add_column_20260810(
        'feature_generation_runs',
        'workflow_run_id',
        'ALTER TABLE feature_generation_runs ADD COLUMN workflow_run_id VARCHAR(64) NOT NULL DEFAULT '''' AFTER parent_artifact_id'
    );
    CALL migration_add_column_20260810(
        'feature_generation_runs',
        'workflow_node_id',
        'ALTER TABLE feature_generation_runs ADD COLUMN workflow_node_id VARCHAR(128) NOT NULL DEFAULT '''' AFTER workflow_run_id'
    );
    CALL migration_add_column_20260810(
        'feature_generation_runs',
        'workflow_attempt',
        'ALTER TABLE feature_generation_runs ADD COLUMN workflow_attempt INT NOT NULL DEFAULT 0 AFTER workflow_node_id'
    );
    CALL migration_add_column_20260810(
        'feature_generation_runs',
        'artifact_id',
        'ALTER TABLE feature_generation_runs ADD COLUMN artifact_id VARCHAR(64) NOT NULL DEFAULT '''' AFTER workflow_attempt'
    );

    SELECT COUNT(*)
    INTO invalid_count
    FROM feature_generation_runs
    WHERE (workflow_run_id = '' AND (
               workflow_node_id <> ''
               OR workflow_attempt <> 0
           ))
       OR (workflow_run_id <> '' AND (
               workflow_node_id = ''
               OR workflow_attempt <= 0
           ));

    IF invalid_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'generation runs contain partial workflow bindings';
    END IF;

    SELECT COUNT(*)
    INTO invalid_count
    FROM (
        SELECT workflow_run_id, workflow_node_id, workflow_attempt
        FROM feature_generation_runs
        WHERE workflow_run_id <> ''
        GROUP BY workflow_run_id, workflow_node_id, workflow_attempt
        HAVING COUNT(*) > 1
    ) duplicate_workflow_attempts;

    IF invalid_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'generation runs contain duplicate workflow node attempts';
    END IF;

    CALL migration_add_index_20260810(
        'feature_generation_runs',
        'idx_generation_artifact',
        'CREATE INDEX idx_generation_artifact ON feature_generation_runs (artifact_id)'
    );
    CALL migration_add_index_20260810(
        'feature_generation_runs',
        'uniq_workflow_node_attempt',
        'CREATE UNIQUE INDEX uniq_workflow_node_attempt ON feature_generation_runs ((NULLIF(workflow_run_id, '''')), (NULLIF(workflow_node_id, '''')), (NULLIF(workflow_attempt, 0)))'
    );
    CALL migration_add_index_20260810(
        'feature_generation_runs',
        'idx_workflow_node_success',
        'CREATE INDEX idx_workflow_node_success ON feature_generation_runs (workflow_run_id, workflow_node_id, status, workflow_attempt)'
    );

    -- Feature implementation change sets and reviews: merge both legacy
    -- one-to-one tables into their owning implementation run.
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'worktree_head',
        'ALTER TABLE feature_implementation_runs ADD COLUMN worktree_head VARCHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'patch_rel_path',
        'ALTER TABLE feature_implementation_runs ADD COLUMN patch_rel_path VARCHAR(1024) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'patch_sha256',
        'ALTER TABLE feature_implementation_runs ADD COLUMN patch_sha256 CHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'patch_bytes',
        'ALTER TABLE feature_implementation_runs ADD COLUMN patch_bytes BIGINT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'files_changed',
        'ALTER TABLE feature_implementation_runs ADD COLUMN files_changed INT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'additions',
        'ALTER TABLE feature_implementation_runs ADD COLUMN additions INT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'deletions',
        'ALTER TABLE feature_implementation_runs ADD COLUMN deletions INT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'files_json',
        'ALTER TABLE feature_implementation_runs ADD COLUMN files_json JSON NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'plan_deviations_json',
        'ALTER TABLE feature_implementation_runs ADD COLUMN plan_deviations_json JSON NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'validation_results_json',
        'ALTER TABLE feature_implementation_runs ADD COLUMN validation_results_json JSON NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'provider_summary',
        'ALTER TABLE feature_implementation_runs ADD COLUMN provider_summary TEXT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'change_set_created_at',
        'ALTER TABLE feature_implementation_runs ADD COLUMN change_set_created_at TIMESTAMP NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'review_subject_hash',
        'ALTER TABLE feature_implementation_runs ADD COLUMN review_subject_hash CHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'review_round_id',
        'ALTER TABLE feature_implementation_runs ADD COLUMN review_round_id VARCHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'review_gate_result_id',
        'ALTER TABLE feature_implementation_runs ADD COLUMN review_gate_result_id VARCHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'review_decision',
        'ALTER TABLE feature_implementation_runs ADD COLUMN review_decision VARCHAR(16) NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'review_comment',
        'ALTER TABLE feature_implementation_runs ADD COLUMN review_comment TEXT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'review_reviewer',
        'ALTER TABLE feature_implementation_runs ADD COLUMN review_reviewer BIGINT NULL'
    );
    CALL migration_add_column_20260810(
        'feature_implementation_runs',
        'review_created_at',
        'ALTER TABLE feature_implementation_runs ADD COLUMN review_created_at TIMESTAMP NULL'
    );

    SELECT COUNT(*)
    INTO source_table_exists
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name = 'feature_change_sets';

    IF source_table_exists <> 0 THEN
        SELECT COUNT(*)
        INTO invalid_count
        FROM feature_change_sets source
        LEFT JOIN feature_implementation_runs target
            ON target.id = source.run_id
        WHERE target.id IS NULL;

        IF invalid_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'change-set migration found orphan implementation runs';
        END IF;

        SELECT COUNT(*)
        INTO conflict_count
        FROM feature_change_sets source
        INNER JOIN feature_implementation_runs target
            ON target.id = source.run_id
        WHERE (
            target.worktree_head IS NOT NULL
            OR target.patch_rel_path IS NOT NULL
            OR target.patch_sha256 IS NOT NULL
            OR target.patch_bytes IS NOT NULL
            OR target.files_changed IS NOT NULL
            OR target.additions IS NOT NULL
            OR target.deletions IS NOT NULL
            OR target.files_json IS NOT NULL
            OR target.plan_deviations_json IS NOT NULL
            OR target.validation_results_json IS NOT NULL
            OR target.provider_summary IS NOT NULL
            OR target.change_set_created_at IS NOT NULL
        )
        AND NOT (
            target.worktree_head <=> source.worktree_head
            AND target.patch_rel_path <=> source.patch_rel_path
            AND target.patch_sha256 <=> source.patch_sha256
            AND target.patch_bytes <=> source.patch_bytes
            AND target.files_changed <=> source.files_changed
            AND target.additions <=> source.additions
            AND target.deletions <=> source.deletions
            AND target.files_json <=> source.files_json
            AND target.plan_deviations_json <=> source.plan_deviations_json
            AND target.validation_results_json <=> source.validation_results_json
            AND target.provider_summary <=> source.provider_summary
            AND target.change_set_created_at <=> source.created_at
        );

        IF conflict_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'change-set migration found conflicting target data';
        END IF;

        UPDATE feature_implementation_runs target
        INNER JOIN feature_change_sets source
            ON target.id = source.run_id
        SET target.worktree_head = source.worktree_head,
            target.patch_rel_path = source.patch_rel_path,
            target.patch_sha256 = source.patch_sha256,
            target.patch_bytes = source.patch_bytes,
            target.files_changed = source.files_changed,
            target.additions = source.additions,
            target.deletions = source.deletions,
            target.files_json = source.files_json,
            target.plan_deviations_json = source.plan_deviations_json,
            target.validation_results_json = source.validation_results_json,
            target.provider_summary = source.provider_summary,
            target.change_set_created_at = source.created_at;

        SELECT COUNT(*)
        INTO remaining_count
        FROM feature_change_sets source
        LEFT JOIN feature_implementation_runs target
            ON target.id = source.run_id
           AND target.worktree_head <=> source.worktree_head
           AND target.patch_rel_path <=> source.patch_rel_path
           AND target.patch_sha256 <=> source.patch_sha256
           AND target.patch_bytes <=> source.patch_bytes
           AND target.files_changed <=> source.files_changed
           AND target.additions <=> source.additions
           AND target.deletions <=> source.deletions
           AND target.files_json <=> source.files_json
           AND target.plan_deviations_json <=> source.plan_deviations_json
           AND target.validation_results_json <=> source.validation_results_json
           AND target.provider_summary <=> source.provider_summary
           AND target.change_set_created_at <=> source.created_at
        WHERE target.id IS NULL;

        IF remaining_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'change-set migration did not copy every change set';
        END IF;
    END IF;

    SELECT COUNT(*)
    INTO source_table_exists
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name = 'feature_change_reviews';

    IF source_table_exists <> 0 THEN
        SELECT COUNT(*)
        INTO invalid_count
        FROM feature_change_reviews source
        LEFT JOIN feature_implementation_runs target
            ON target.id = source.run_id
        WHERE target.id IS NULL;

        IF invalid_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'change review migration found orphan implementation runs';
        END IF;

        SELECT COUNT(*)
        INTO conflict_count
        FROM feature_change_reviews source
        INNER JOIN feature_implementation_runs target
            ON target.id = source.run_id
        WHERE (
            target.review_subject_hash IS NOT NULL
            OR target.review_round_id IS NOT NULL
            OR target.review_gate_result_id IS NOT NULL
            OR target.review_decision IS NOT NULL
            OR target.review_comment IS NOT NULL
            OR target.review_reviewer IS NOT NULL
            OR target.review_created_at IS NOT NULL
        )
        AND NOT (
            target.review_subject_hash <=> ''
            AND target.review_round_id <=> ''
            AND target.review_gate_result_id <=> ''
            AND target.review_decision <=> source.decision
            AND target.review_comment <=> source.comment
            AND target.review_reviewer <=> source.reviewer
            AND target.review_created_at <=> source.created_at
        );

        IF conflict_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'change review migration found conflicting target data';
        END IF;

        UPDATE feature_implementation_runs target
        INNER JOIN feature_change_reviews source
            ON target.id = source.run_id
        SET target.review_subject_hash = '',
            target.review_round_id = '',
            target.review_gate_result_id = '',
            target.review_decision = source.decision,
            target.review_comment = source.comment,
            target.review_reviewer = source.reviewer,
            target.review_created_at = source.created_at;

        SELECT COUNT(*)
        INTO remaining_count
        FROM feature_change_reviews source
        LEFT JOIN feature_implementation_runs target
            ON target.id = source.run_id
           AND target.review_subject_hash <=> ''
           AND target.review_round_id <=> ''
           AND target.review_gate_result_id <=> ''
           AND target.review_decision <=> source.decision
           AND target.review_comment <=> source.comment
           AND target.review_reviewer <=> source.reviewer
           AND target.review_created_at <=> source.created_at
        WHERE target.id IS NULL;

        IF remaining_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'change review migration did not copy every review';
        END IF;
    END IF;

    -- Runtime events: preserve sequence numbers while moving the old Feature
    -- stream into the shared runtime event store.
    SELECT COUNT(*)
    INTO source_table_exists
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name = 'feature_run_events';

    IF source_table_exists <> 0 THEN
        SELECT COUNT(*)
        INTO conflict_count
        FROM feature_run_events source
        INNER JOIN runtime_events target
            ON target.stream_kind = 'feature_implementation'
           AND target.stream_id = source.run_id
           AND target.seq = source.seq
        WHERE target.kind <> source.kind
           OR target.node_id <> ''
           OR NOT (target.summary <=> source.summary)
           OR NOT (target.detail_json <=> source.detail_json)
           OR NOT (target.created_at <=> source.created_at);

        IF conflict_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'feature event migration found conflicting runtime events';
        END IF;

        INSERT IGNORE INTO runtime_events (
            stream_kind,
            stream_id,
            seq,
            kind,
            node_id,
            summary,
            detail_json,
            created_at
        )
        SELECT
            'feature_implementation',
            source.run_id,
            source.seq,
            source.kind,
            '',
            source.summary,
            source.detail_json,
            source.created_at
        FROM feature_run_events source;

        SELECT COUNT(*)
        INTO remaining_count
        FROM feature_run_events source
        LEFT JOIN runtime_events target
            ON target.stream_kind = 'feature_implementation'
           AND target.stream_id = source.run_id
           AND target.seq = source.seq
           AND target.kind = source.kind
           AND target.node_id = ''
           AND target.summary <=> source.summary
           AND target.detail_json <=> source.detail_json
           AND target.created_at <=> source.created_at
        WHERE target.stream_id IS NULL;

        IF remaining_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'feature event migration did not copy every event';
        END IF;
    END IF;

    -- QA turn contexts: merge archived context into the turn that owns it.
    CALL migration_add_column_20260810(
        'qa_turns',
        'context_ref',
        'ALTER TABLE qa_turns ADD COLUMN context_ref VARCHAR(64) NULL'
    );
    CALL migration_add_column_20260810(
        'qa_turns',
        'context_detail_json',
        'ALTER TABLE qa_turns ADD COLUMN context_detail_json JSON NULL'
    );
    CALL migration_add_column_20260810(
        'qa_turns',
        'context_summary_text',
        'ALTER TABLE qa_turns ADD COLUMN context_summary_text TEXT NULL'
    );
    CALL migration_add_column_20260810(
        'qa_turns',
        'context_summary_tokens',
        'ALTER TABLE qa_turns ADD COLUMN context_summary_tokens INT NULL'
    );
    CALL migration_add_column_20260810(
        'qa_turns',
        'context_source_tokens',
        'ALTER TABLE qa_turns ADD COLUMN context_source_tokens INT NULL'
    );
    CALL migration_add_column_20260810(
        'qa_turns',
        'context_retained_tokens',
        'ALTER TABLE qa_turns ADD COLUMN context_retained_tokens INT NULL'
    );
    CALL migration_add_column_20260810(
        'qa_turns',
        'context_archived_at',
        'ALTER TABLE qa_turns ADD COLUMN context_archived_at TIMESTAMP NULL'
    );

    SELECT COUNT(*)
    INTO source_table_exists
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name = 'qa_turn_contexts';

    IF source_table_exists <> 0 THEN
        SELECT COUNT(*)
        INTO invalid_count
        FROM qa_turn_contexts source
        LEFT JOIN qa_turns target
            ON target.session_id = source.session_id
           AND target.turn_no = source.turn_number
        WHERE target.session_id IS NULL;

        IF invalid_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'QA context migration found orphan turns';
        END IF;

        SELECT COUNT(*)
        INTO conflict_count
        FROM qa_turn_contexts source
        INNER JOIN qa_turns target
            ON target.session_id = source.session_id
           AND target.turn_no = source.turn_number
        WHERE source.run_id <> ''
          AND target.run_id <> ''
          AND target.run_id <> source.run_id;

        IF conflict_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'QA context migration found conflicting run ids';
        END IF;

        SELECT COUNT(*)
        INTO conflict_count
        FROM qa_turn_contexts source
        INNER JOIN qa_turns target
            ON target.session_id = source.session_id
           AND target.turn_no = source.turn_number
        WHERE (
            target.context_ref IS NOT NULL
            OR target.context_detail_json IS NOT NULL
            OR target.context_summary_text IS NOT NULL
            OR target.context_summary_tokens IS NOT NULL
            OR target.context_source_tokens IS NOT NULL
            OR target.context_retained_tokens IS NOT NULL
            OR target.context_archived_at IS NOT NULL
        )
        AND NOT (
            target.context_ref <=> source.ref
            AND target.context_detail_json <=> source.detail_json
            AND target.context_summary_text <=> source.summary_text
            AND target.context_summary_tokens <=> source.summary_tokens
            AND target.context_source_tokens <=> source.source_tokens
            AND target.context_retained_tokens <=> source.retained_tokens
            AND target.context_archived_at <=> source.created_at
        );

        IF conflict_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'QA context migration found conflicting target data';
        END IF;

        UPDATE qa_turns target
        INNER JOIN qa_turn_contexts source
            ON target.session_id = source.session_id
           AND target.turn_no = source.turn_number
        SET target.run_id = IF(
                target.run_id = '',
                source.run_id,
                target.run_id
            ),
            target.context_ref = source.ref,
            target.context_detail_json = source.detail_json,
            target.context_summary_text = source.summary_text,
            target.context_summary_tokens = source.summary_tokens,
            target.context_source_tokens = source.source_tokens,
            target.context_retained_tokens = source.retained_tokens,
            target.context_archived_at = source.created_at;

        SELECT COUNT(*)
        INTO remaining_count
        FROM qa_turn_contexts source
        LEFT JOIN qa_turns target
            ON target.session_id = source.session_id
           AND target.turn_no = source.turn_number
           AND target.context_ref <=> source.ref
           AND target.context_detail_json <=> source.detail_json
           AND target.context_summary_text <=> source.summary_text
           AND target.context_summary_tokens <=> source.summary_tokens
           AND target.context_source_tokens <=> source.source_tokens
           AND target.context_retained_tokens <=> source.retained_tokens
           AND target.context_archived_at <=> source.created_at
        WHERE target.session_id IS NULL;

        IF remaining_count <> 0 THEN
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'QA context migration did not copy every context';
        END IF;
    END IF;

    SELECT COUNT(*)
    INTO invalid_count
    FROM (
        SELECT session_id, run_id
        FROM qa_turns
        WHERE run_id <> ''
        GROUP BY session_id, run_id
        HAVING COUNT(*) > 1
    ) duplicate_qa_run_ids;

    IF invalid_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'QA turns contain duplicate non-empty run ids';
    END IF;

    SELECT COUNT(*)
    INTO invalid_count
    FROM (
        SELECT context_ref
        FROM qa_turns
        WHERE context_ref IS NOT NULL
        GROUP BY context_ref
        HAVING COUNT(*) > 1
    ) duplicate_context_refs;

    IF invalid_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'QA turns contain duplicate context refs';
    END IF;

    CALL migration_add_column_20260810(
        'qa_turns',
        'run_id_key',
        'ALTER TABLE qa_turns ADD COLUMN run_id_key VARCHAR(64) GENERATED ALWAYS AS (NULLIF(run_id, '''')) STORED'
    );
    CALL migration_add_index_20260810(
        'qa_turns',
        'uniq_session_run',
        'CREATE UNIQUE INDEX uniq_session_run ON qa_turns (session_id, run_id_key)'
    );
    CALL migration_add_index_20260810(
        'qa_turns',
        'uniq_qa_turn_context_ref',
        'CREATE UNIQUE INDEX uniq_qa_turn_context_ref ON qa_turns (context_ref)'
    );

    SELECT
        DATABASE() AS migrated_schema,
        'legacy source tables retained' AS source_table_policy,
        (
            SELECT COUNT(*)
            FROM information_schema.columns
            WHERE table_schema = DATABASE()
              AND (
                  (table_name = 'agent_runs' AND column_name IN (
                      'run_kind',
                      'agent_id',
                      'definition_version',
                      'definition_hash',
                      'selection_json',
                      'tool_snapshot_id',
                      'input_schema_version',
                      'output_schema_version',
                      'parent_run_id',
                      'workflow_run_id',
                      'workflow_node_id',
                      'error_code'
                  ))
                  OR (table_name = 'agent_steps' AND column_name IN (
                      'artifact_content',
                      'artifact_content_type',
                      'artifact_id_key',
                      'artifact_tool_call_key'
                  ))
                  OR (table_name = 'feature_artifacts' AND column_name LIKE 'review_%')
                  OR (table_name = 'feature_generation_runs' AND column_name IN (
                      'workflow_run_id',
                      'workflow_node_id',
                      'workflow_attempt',
                      'artifact_id'
                  ))
                  OR (table_name = 'feature_implementation_runs' AND column_name IN (
                      'worktree_head',
                      'patch_rel_path',
                      'patch_sha256',
                      'patch_bytes',
                      'files_changed',
                      'additions',
                      'deletions',
                      'files_json',
                      'plan_deviations_json',
                      'validation_results_json',
                      'provider_summary',
                      'change_set_created_at',
                      'review_subject_hash',
                      'review_round_id',
                      'review_gate_result_id',
                      'review_decision',
                      'review_comment',
                      'review_reviewer',
                      'review_created_at'
                  ))
                  OR (table_name = 'qa_turns' AND column_name IN (
                      'run_id_key',
                      'context_ref',
                      'context_detail_json',
                      'context_summary_text',
                      'context_summary_tokens',
                      'context_source_tokens',
                      'context_retained_tokens',
                      'context_archived_at'
                  ))
              )
        ) AS aligned_column_count;
END//

DELIMITER ;

CALL migrate_compact_schema_20260810();

DROP PROCEDURE migrate_compact_schema_20260810;
DROP PROCEDURE migration_add_index_20260810;
DROP PROCEDURE migration_add_column_20260810;

-- Expected result: no rows. These are the compact-schema fields that must
-- exist after migration.
SELECT required.table_name, required.column_name
FROM (
    SELECT 'agent_runs' AS table_name, 'selection_json' AS column_name
    UNION ALL SELECT 'agent_steps', 'artifact_content'
    UNION ALL SELECT 'feature_artifacts', 'review_decision'
    UNION ALL SELECT 'feature_generation_runs', 'workflow_run_id'
    UNION ALL SELECT 'feature_implementation_runs', 'change_set_created_at'
    UNION ALL SELECT 'feature_implementation_runs', 'review_decision'
    UNION ALL SELECT 'qa_turns', 'run_id_key'
    UNION ALL SELECT 'qa_turns', 'context_ref'
) required
LEFT JOIN information_schema.columns existing
    ON existing.table_schema = DATABASE()
   AND existing.table_name = required.table_name
   AND existing.column_name = required.column_name
WHERE existing.column_name IS NULL;

-- Widen agent_runs.question so a workflow-node run can persist its full
-- joined-evidence input. The column was TEXT (65KiB), but a synthesize
-- node's input is bounded by MaxHandoffBytes (1MiB). The stored value is the
-- run's JSON input, so truncating it would corrupt the document.
--
-- Requirements:
--   1. MySQL 8.0.13 or newer.
--   2. Select the target database first, for example: USE nasuta;
--   3. Back up the database and stop Nasuta/CodeLoom writes during migration.
--
-- The migration is idempotent.

DROP PROCEDURE IF EXISTS migration_widen_agent_runs_question_20260817;

DELIMITER //

CREATE PROCEDURE migration_widen_agent_runs_question_20260817()
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'agent_runs'
          AND column_name = 'question'
          AND data_type <> 'mediumtext'
    ) THEN
        ALTER TABLE agent_runs
            MODIFY COLUMN question MEDIUMTEXT NOT NULL;
    END IF;
END//

DELIMITER ;

CALL migration_widen_agent_runs_question_20260817();
DROP PROCEDURE migration_widen_agent_runs_question_20260817;

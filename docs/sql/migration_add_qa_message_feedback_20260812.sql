-- Store QA answer feedback on the existing message row.
-- Select the target database before running this migration.

DROP PROCEDURE IF EXISTS migration_add_qa_message_feedback_20260812;

DELIMITER //

CREATE PROCEDURE migration_add_qa_message_feedback_20260812()
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'qa_messages'
          AND column_name = 'feedback'
    ) THEN
        ALTER TABLE qa_messages
            ADD COLUMN feedback VARCHAR(8) NOT NULL DEFAULT '' AFTER tool_name;
    END IF;
END//

DELIMITER ;

CALL migration_add_qa_message_feedback_20260812();
DROP PROCEDURE migration_add_qa_message_feedback_20260812;

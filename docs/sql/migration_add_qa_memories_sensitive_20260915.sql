-- Mark sensitive (health/custom) memories so recall can keep them out of the
-- default QA context. Select the target database before running this migration.
-- The column is named is_sensitive because SENSITIVE is a MySQL reserved word.

DROP PROCEDURE IF EXISTS migration_add_qa_memories_sensitive_20260915;

DELIMITER //

CREATE PROCEDURE migration_add_qa_memories_sensitive_20260915()
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'qa_memories'
          AND column_name = 'is_sensitive'
    ) THEN
        ALTER TABLE qa_memories
            ADD COLUMN is_sensitive TINYINT(1) NOT NULL DEFAULT 0 AFTER use_count;
    END IF;
END//

DELIMITER ;

CALL migration_add_qa_memories_sensitive_20260915();
DROP PROCEDURE migration_add_qa_memories_sensitive_20260915;

-- Stop Nasuta and CodeLoom before running this migration.
-- The migration is idempotent after feature_subject_reviews has been dropped.

DROP PROCEDURE IF EXISTS migrate_feature_subject_reviews;

DELIMITER //

CREATE PROCEDURE migrate_feature_subject_reviews()
migration: BEGIN
    DECLARE old_table_exists INT DEFAULT 0;
    DECLARE unknown_kind_count BIGINT DEFAULT 0;
    DECLARE orphan_artifact_count BIGINT DEFAULT 0;
    DECLARE orphan_change_set_count BIGINT DEFAULT 0;
    DECLARE artifact_conflict_count BIGINT DEFAULT 0;
    DECLARE change_set_conflict_count BIGINT DEFAULT 0;
    DECLARE old_artifact_count BIGINT DEFAULT 0;
    DECLARE migrated_artifact_count BIGINT DEFAULT 0;
    DECLARE old_change_set_count BIGINT DEFAULT 0;
    DECLARE migrated_change_set_count BIGINT DEFAULT 0;

    DECLARE EXIT HANDLER FOR SQLEXCEPTION
    BEGIN
        ROLLBACK;
        RESIGNAL;
    END;

    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_artifacts'
          AND column_name = 'review_subject_hash'
    ) THEN
        ALTER TABLE feature_artifacts
            ADD COLUMN review_subject_hash CHAR(64) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_artifacts'
          AND column_name = 'review_round_id'
    ) THEN
        ALTER TABLE feature_artifacts
            ADD COLUMN review_round_id VARCHAR(64) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_artifacts'
          AND column_name = 'review_gate_result_id'
    ) THEN
        ALTER TABLE feature_artifacts
            ADD COLUMN review_gate_result_id VARCHAR(64) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_artifacts'
          AND column_name = 'review_decision'
    ) THEN
        ALTER TABLE feature_artifacts
            ADD COLUMN review_decision VARCHAR(16) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_artifacts'
          AND column_name = 'review_comment'
    ) THEN
        ALTER TABLE feature_artifacts
            ADD COLUMN review_comment TEXT NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_artifacts'
          AND column_name = 'review_reviewer'
    ) THEN
        ALTER TABLE feature_artifacts
            ADD COLUMN review_reviewer BIGINT NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_artifacts'
          AND column_name = 'review_created_at'
    ) THEN
        ALTER TABLE feature_artifacts
            ADD COLUMN review_created_at TIMESTAMP NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_implementation_runs'
          AND column_name = 'review_subject_hash'
    ) THEN
        ALTER TABLE feature_implementation_runs
            ADD COLUMN review_subject_hash CHAR(64) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_implementation_runs'
          AND column_name = 'review_round_id'
    ) THEN
        ALTER TABLE feature_implementation_runs
            ADD COLUMN review_round_id VARCHAR(64) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_implementation_runs'
          AND column_name = 'review_gate_result_id'
    ) THEN
        ALTER TABLE feature_implementation_runs
            ADD COLUMN review_gate_result_id VARCHAR(64) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_implementation_runs'
          AND column_name = 'review_decision'
    ) THEN
        ALTER TABLE feature_implementation_runs
            ADD COLUMN review_decision VARCHAR(16) NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_implementation_runs'
          AND column_name = 'review_comment'
    ) THEN
        ALTER TABLE feature_implementation_runs
            ADD COLUMN review_comment TEXT NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_implementation_runs'
          AND column_name = 'review_reviewer'
    ) THEN
        ALTER TABLE feature_implementation_runs
            ADD COLUMN review_reviewer BIGINT NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE()
          AND table_name = 'feature_implementation_runs'
          AND column_name = 'review_created_at'
    ) THEN
        ALTER TABLE feature_implementation_runs
            ADD COLUMN review_created_at TIMESTAMP NULL;
    END IF;

    SELECT COUNT(*)
    INTO old_table_exists
    FROM information_schema.tables
    WHERE table_schema = DATABASE()
      AND table_name = 'feature_subject_reviews';

    IF old_table_exists = 0 THEN
        LEAVE migration;
    END IF;

    START TRANSACTION;

    SELECT COUNT(*)
    INTO unknown_kind_count
    FROM feature_subject_reviews
    WHERE subject_kind NOT IN ('artifact', 'change_set');

    IF unknown_kind_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'feature review migration found unknown subject kinds';
    END IF;

    SELECT COUNT(*)
    INTO orphan_artifact_count
    FROM feature_subject_reviews old_review
    LEFT JOIN feature_artifacts artifact
        ON artifact.id = old_review.subject_id
    WHERE old_review.subject_kind = 'artifact'
      AND artifact.id IS NULL;

    IF orphan_artifact_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'feature review migration found orphan artifacts';
    END IF;

    SELECT COUNT(*)
    INTO orphan_change_set_count
    FROM feature_subject_reviews old_review
    LEFT JOIN feature_implementation_runs implementation
        ON implementation.id = old_review.subject_id
    WHERE old_review.subject_kind = 'change_set'
      AND implementation.id IS NULL;

    IF orphan_change_set_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'feature review migration found orphan change sets';
    END IF;

    SELECT COUNT(*)
    INTO artifact_conflict_count
    FROM feature_subject_reviews old_review
    INNER JOIN feature_artifacts artifact
        ON artifact.id = old_review.subject_id
    WHERE old_review.subject_kind = 'artifact'
      AND (
          artifact.review_subject_hash IS NOT NULL
          OR artifact.review_round_id IS NOT NULL
          OR artifact.review_gate_result_id IS NOT NULL
          OR artifact.review_decision IS NOT NULL
          OR artifact.review_comment IS NOT NULL
          OR artifact.review_reviewer IS NOT NULL
          OR artifact.review_created_at IS NOT NULL
      )
      AND NOT (
          artifact.review_subject_hash <=> old_review.subject_hash
          AND artifact.review_round_id <=> old_review.review_round_id
          AND artifact.review_gate_result_id <=> old_review.gate_result_id
          AND artifact.review_decision <=> old_review.decision
          AND artifact.review_comment <=> old_review.comment
          AND artifact.review_reviewer <=> old_review.reviewer
          AND artifact.review_created_at <=> old_review.created_at
      );

    IF artifact_conflict_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'feature review migration found artifact conflicts';
    END IF;

    SELECT COUNT(*)
    INTO change_set_conflict_count
    FROM feature_subject_reviews old_review
    INNER JOIN feature_implementation_runs implementation
        ON implementation.id = old_review.subject_id
    WHERE old_review.subject_kind = 'change_set'
      AND (
          implementation.review_subject_hash IS NOT NULL
          OR implementation.review_round_id IS NOT NULL
          OR implementation.review_gate_result_id IS NOT NULL
          OR implementation.review_decision IS NOT NULL
          OR implementation.review_comment IS NOT NULL
          OR implementation.review_reviewer IS NOT NULL
          OR implementation.review_created_at IS NOT NULL
      )
      AND NOT (
          implementation.review_subject_hash <=> old_review.subject_hash
          AND implementation.review_round_id <=> old_review.review_round_id
          AND implementation.review_gate_result_id <=> old_review.gate_result_id
          AND implementation.review_decision <=> old_review.decision
          AND implementation.review_comment <=> old_review.comment
          AND implementation.review_reviewer <=> old_review.reviewer
          AND implementation.review_created_at <=> old_review.created_at
      );

    IF change_set_conflict_count <> 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'feature review migration found change set conflicts';
    END IF;

    UPDATE feature_artifacts artifact
    INNER JOIN feature_subject_reviews old_review
        ON old_review.subject_kind = 'artifact'
       AND old_review.subject_id = artifact.id
    SET artifact.review_subject_hash = old_review.subject_hash,
        artifact.review_round_id = old_review.review_round_id,
        artifact.review_gate_result_id = old_review.gate_result_id,
        artifact.review_decision = old_review.decision,
        artifact.review_comment = old_review.comment,
        artifact.review_reviewer = old_review.reviewer,
        artifact.review_created_at = old_review.created_at
    WHERE artifact.review_subject_hash IS NULL;

    UPDATE feature_implementation_runs implementation
    INNER JOIN feature_subject_reviews old_review
        ON old_review.subject_kind = 'change_set'
       AND old_review.subject_id = implementation.id
    SET implementation.review_subject_hash = old_review.subject_hash,
        implementation.review_round_id = old_review.review_round_id,
        implementation.review_gate_result_id = old_review.gate_result_id,
        implementation.review_decision = old_review.decision,
        implementation.review_comment = old_review.comment,
        implementation.review_reviewer = old_review.reviewer,
        implementation.review_created_at = old_review.created_at
    WHERE implementation.review_subject_hash IS NULL;

    SELECT COUNT(*)
    INTO old_artifact_count
    FROM feature_subject_reviews
    WHERE subject_kind = 'artifact';

    SELECT COUNT(*)
    INTO migrated_artifact_count
    FROM feature_subject_reviews old_review
    INNER JOIN feature_artifacts artifact
        ON artifact.id = old_review.subject_id
    WHERE old_review.subject_kind = 'artifact'
      AND artifact.review_subject_hash <=> old_review.subject_hash
      AND artifact.review_round_id <=> old_review.review_round_id
      AND artifact.review_gate_result_id <=> old_review.gate_result_id
      AND artifact.review_decision <=> old_review.decision
      AND artifact.review_comment <=> old_review.comment
      AND artifact.review_reviewer <=> old_review.reviewer
      AND artifact.review_created_at <=> old_review.created_at;

    IF old_artifact_count <> migrated_artifact_count THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'feature artifact review migration validation failed';
    END IF;

    SELECT COUNT(*)
    INTO old_change_set_count
    FROM feature_subject_reviews
    WHERE subject_kind = 'change_set';

    SELECT COUNT(*)
    INTO migrated_change_set_count
    FROM feature_subject_reviews old_review
    INNER JOIN feature_implementation_runs implementation
        ON implementation.id = old_review.subject_id
    WHERE old_review.subject_kind = 'change_set'
      AND implementation.review_subject_hash <=> old_review.subject_hash
      AND implementation.review_round_id <=> old_review.review_round_id
      AND implementation.review_gate_result_id <=> old_review.gate_result_id
      AND implementation.review_decision <=> old_review.decision
      AND implementation.review_comment <=> old_review.comment
      AND implementation.review_reviewer <=> old_review.reviewer
      AND implementation.review_created_at <=> old_review.created_at;

    IF old_change_set_count <> migrated_change_set_count THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'feature change review migration validation failed';
    END IF;

    COMMIT;
    DROP TABLE feature_subject_reviews;
END//

DELIMITER ;

CALL migrate_feature_subject_reviews();
DROP PROCEDURE migrate_feature_subject_reviews;

SELECT
    (SELECT COUNT(*) FROM feature_artifacts
     WHERE review_subject_hash IS NOT NULL) AS artifact_review_count,
    (SELECT COUNT(*) FROM feature_implementation_runs
     WHERE review_subject_hash IS NOT NULL) AS change_set_review_count;

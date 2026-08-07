-- Pre-release migration: identity-independent hashes cannot be reconstructed
-- from historical Report rows without replaying their original canonicalizer.
DELIMITER //

CREATE PROCEDURE migrate_review_report_reuse()
BEGIN
    IF EXISTS (SELECT 1 FROM review_reports LIMIT 1) THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'migration_review_report_reuse requires an empty review_reports table';
    END IF;

    ALTER TABLE review_reports
        ADD COLUMN report_hash CHAR(64) NOT NULL AFTER report_json,
        ADD KEY idx_report_reuse (report_hash, id);

    CREATE TABLE review_report_reuses (
        id                   VARCHAR(64) PRIMARY KEY,
        round_id             VARCHAR(64) NOT NULL,
        assignment_id        VARCHAR(64) NOT NULL,
        report_id            VARCHAR(64) NOT NULL,
        reviewer_id          VARCHAR(128) NOT NULL,
        source_round_id      VARCHAR(64) NOT NULL,
        source_assignment_id VARCHAR(64) NOT NULL,
        source_report_id     VARCHAR(64) NOT NULL,
        subject_hash         CHAR(64) NOT NULL,
        policy_hash          CHAR(64) NOT NULL,
        definition_hash      CHAR(64) NOT NULL,
        report_hash          CHAR(64) NOT NULL,
        reason               TEXT NOT NULL,
        actor_id             BIGINT NOT NULL,
        created_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        UNIQUE KEY uniq_review_report_reuse_assignment (assignment_id),
        UNIQUE KEY uniq_review_report_reuse_report (report_id),
        KEY idx_review_report_reuse_source (source_report_id, created_at, id),
        KEY idx_review_report_reuse_round (round_id, created_at, id)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
END//

DELIMITER ;

CALL migrate_review_report_reuse();
DROP PROCEDURE migrate_review_report_reuse;

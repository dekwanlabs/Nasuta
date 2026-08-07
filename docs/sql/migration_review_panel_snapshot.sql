-- Pre-release migration: historical Rounds cannot be backfilled because the risk
-- facts used at creation time were not recorded. Abort instead of inventing them.
DELIMITER //

CREATE PROCEDURE migrate_review_panel_snapshot()
BEGIN
    IF EXISTS (SELECT 1 FROM review_rounds LIMIT 1) THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'migration_review_panel_snapshot requires an empty review_rounds table';
    END IF;

    ALTER TABLE review_rounds
        ADD COLUMN risk_facts_json JSON NOT NULL AFTER policy_hash,
        ADD COLUMN risk_hash CHAR(64) NOT NULL AFTER risk_facts_json,
        ADD COLUMN selection_rule_version VARCHAR(64) NOT NULL DEFAULT '' AFTER risk_hash,
        ADD COLUMN selected_reviewers_json JSON NOT NULL AFTER selection_rule_version,
        ADD COLUMN panel_hash CHAR(64) NOT NULL AFTER selected_reviewers_json;
END//

DELIMITER ;

CALL migrate_review_panel_snapshot();
DROP PROCEDURE migrate_review_panel_snapshot;

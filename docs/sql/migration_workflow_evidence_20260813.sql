-- Persist workflow evidence across handoff queries and checkpoint recovery.
--
-- Requirements:
--   1. MySQL 8.0.13 or newer.
--   2. Select the target database first, for example: USE nasuta;
--   3. Back up the database and stop Nasuta/CodeLoom writes during migration.

ALTER TABLE handoff_artifacts
    ADD COLUMN evidence_units_json JSON NULL AFTER references_json,
    ADD COLUMN evidence_conflicts_json JSON NULL AFTER evidence_units_json;

UPDATE handoff_artifacts
SET evidence_units_json = JSON_ARRAY()
WHERE evidence_units_json IS NULL;

UPDATE handoff_artifacts
SET evidence_conflicts_json = JSON_ARRAY()
WHERE evidence_conflicts_json IS NULL;

ALTER TABLE handoff_artifacts
    MODIFY COLUMN evidence_units_json JSON NOT NULL,
    MODIFY COLUMN evidence_conflicts_json JSON NOT NULL;

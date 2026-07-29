ALTER TABLE feature_change_sets
    ADD COLUMN plan_deviations_json JSON NULL AFTER files_json;

UPDATE feature_change_sets
SET plan_deviations_json = JSON_ARRAY()
WHERE plan_deviations_json IS NULL;

ALTER TABLE feature_change_sets
    MODIFY COLUMN plan_deviations_json JSON NOT NULL;

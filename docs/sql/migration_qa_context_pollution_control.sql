ALTER TABLE qa_turns
  ADD COLUMN question_text VARCHAR(2048) NOT NULL DEFAULT '' AFTER token_estimate,
  ADD COLUMN topic_key VARCHAR(512) NOT NULL DEFAULT '' AFTER question_text,
  ADD COLUMN entities_json JSON NULL AFTER topic_key,
  ADD COLUMN question_terms_json JSON NULL AFTER entities_json,
  ADD COLUMN evidence_manifest_json JSON NULL AFTER question_terms_json;

UPDATE qa_turns
SET entities_json = JSON_ARRAY(),
    question_terms_json = JSON_ARRAY(),
    evidence_manifest_json = JSON_OBJECT(
      'status', 'manifest_unavailable',
      'items', JSON_ARRAY()
    )
WHERE entities_json IS NULL
   OR question_terms_json IS NULL
   OR evidence_manifest_json IS NULL;

ALTER TABLE qa_turns
  MODIFY COLUMN entities_json JSON NOT NULL,
  MODIFY COLUMN question_terms_json JSON NOT NULL,
  MODIFY COLUMN evidence_manifest_json JSON NOT NULL;

-- The JSON-only compaction contract intentionally discards legacy summaries.
-- Raw qa_messages and qa_turns remain intact and will be compacted again on demand.
DELETE FROM qa_turn_contexts;

UPDATE qa_sessions
SET summary = NULL,
    compacted_through_turn = 0;

ALTER TABLE qa_sessions
  MODIFY COLUMN summary JSON NULL;

ALTER TABLE qa_turn_contexts
  CHANGE COLUMN text detail_json JSON NOT NULL;

DROP TABLE IF EXISTS qa_session_compactions;

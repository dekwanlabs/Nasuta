-- Replace the unbounded rolling summary with rebuildable per-turn history
-- indexes. This intentionally resets old
-- compaction snapshots; raw qa_messages and qa_turns remain authoritative.

DELETE FROM qa_turn_contexts;

UPDATE qa_sessions
SET summary = NULL,
    compacted_through_turn = 0;

ALTER TABLE qa_sessions
  DROP COLUMN summary,
  ADD COLUMN archived_summary_tokens BIGINT NOT NULL DEFAULT 0 AFTER title;

ALTER TABLE qa_turn_contexts
  ADD COLUMN summary_tokens INT NOT NULL DEFAULT 0 AFTER summary_text;

CREATE TABLE IF NOT EXISTS qa_session_history_terms (
  session_id  VARCHAR(64) NOT NULL,
  user_id     BIGINT NOT NULL,
  term        VARCHAR(191) NOT NULL,
  ref         VARCHAR(64) NOT NULL,
  turn_number INT NOT NULL,
  weight      SMALLINT NOT NULL DEFAULT 1,
  PRIMARY KEY (session_id, term, ref),
  KEY idx_ref (ref),
  KEY idx_user_session_turn (user_id, session_id, turn_number)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS qa_session_history_index_outbox (
  id           BIGINT AUTO_INCREMENT PRIMARY KEY,
  operation    VARCHAR(16) NOT NULL,
  ref          VARCHAR(64) NOT NULL,
  session_id   VARCHAR(64) NOT NULL,
  user_id      BIGINT NOT NULL,
  attempts     INT NOT NULL DEFAULT 0,
  next_attempt TIMESTAMP NULL,
  last_error   VARCHAR(1024) NOT NULL DEFAULT '',
  created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uniq_operation_ref (operation, ref),
  KEY idx_due (next_attempt, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

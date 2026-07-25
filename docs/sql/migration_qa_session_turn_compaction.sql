ALTER TABLE qa_sessions
  ADD COLUMN compacted_through_turn INT NOT NULL DEFAULT 0 AFTER summary;

ALTER TABLE qa_messages
  ADD COLUMN turn_no INT NOT NULL DEFAULT 0 AFTER seq,
  ADD KEY idx_session_turn (session_id, turn_no, seq);

UPDATE qa_messages m
JOIN (
  SELECT id,
         GREATEST(1, SUM(CASE WHEN role = 'user' THEN 1 ELSE 0 END)
           OVER (PARTITION BY session_id ORDER BY seq ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)) AS derived_turn
  FROM qa_messages
) numbered ON numbered.id = m.id
SET m.turn_no = numbered.derived_turn;

CREATE TABLE IF NOT EXISTS qa_turns (
  session_id     VARCHAR(64) NOT NULL,
  turn_no        INT NOT NULL,
  run_id         VARCHAR(64) NOT NULL DEFAULT '',
  first_seq      INT NOT NULL,
  last_seq       INT NOT NULL,
  token_estimate INT NOT NULL DEFAULT 0,
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (session_id, turn_no),
  KEY idx_session_last (session_id, turn_no DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO qa_turns(session_id,turn_no,run_id,first_seq,last_seq,token_estimate,created_at)
SELECT session_id,turn_no,'',MIN(seq),MAX(seq),
       GREATEST(1, SUM(
         OCTET_LENGTH(content) - CHAR_LENGTH(content) + CEIL(CHAR_LENGTH(content) / 2) +
         OCTET_LENGTH(COALESCE(tool_calls_json,'')) - CHAR_LENGTH(COALESCE(tool_calls_json,'')) +
         CEIL(CHAR_LENGTH(COALESCE(tool_calls_json,'')) / 2)
       )),
       MIN(created_at)
FROM qa_messages
GROUP BY session_id,turn_no;

UPDATE qa_sessions
SET summary=NULL, compacted_through_turn=0;

ALTER TABLE qa_sessions
  MODIFY COLUMN summary JSON NULL;

CREATE TABLE IF NOT EXISTS qa_turn_contexts (
  ref             VARCHAR(64) PRIMARY KEY,
  session_id      VARCHAR(64) NOT NULL,
  user_id         BIGINT NOT NULL,
  run_id          VARCHAR(64) NOT NULL DEFAULT '',
  turn_number     INT NOT NULL,
  detail_json     JSON NOT NULL,
  summary_text    TEXT NOT NULL,
  source_tokens   INT NOT NULL DEFAULT 0,
  retained_tokens INT NOT NULL DEFAULT 0,
  created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uniq_session_turn_context (session_id, turn_number),
  KEY idx_user_session (user_id, session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Baseline: the Nasuta schema exported on 2026-08-07.
-- Requirement: MySQL 8.0.13 or newer (functional index support).
-- Back up the database and pause Nasuta/CodeLoom writes before execution.

USE Nasuta;

-- 1. Email/password users have no Feishu UID. Empty emails remain repeatable,
-- while non-empty emails are unique through the generated key.
ALTER TABLE users
    MODIFY COLUMN feishu_uid VARCHAR(64) NULL,
    ADD COLUMN password_hash VARCHAR(255) NOT NULL DEFAULT '' AFTER email,
    ADD COLUMN email_key VARCHAR(256)
        GENERATED ALWAYS AS (NULLIF(email, '')) STORED AFTER updated_at,
    ADD UNIQUE KEY uniq_email (email_key);

-- 2. Make one Agent Run correspond to at most one turn in a QA session.
ALTER TABLE qa_turns
    ADD COLUMN run_id_key VARCHAR(64)
        GENERATED ALWAYS AS (NULLIF(run_id, '')) STORED AFTER run_id,
    ADD UNIQUE KEY uniq_session_run (session_id, run_id_key);

-- 3. Bind Feature generation records to durable Workflow node attempts and
-- their generated artifacts.
ALTER TABLE feature_generation_runs
    ADD COLUMN workflow_run_id VARCHAR(64) NOT NULL DEFAULT '' AFTER parent_artifact_id,
    ADD COLUMN workflow_node_id VARCHAR(128) NOT NULL DEFAULT '' AFTER workflow_run_id,
    ADD COLUMN workflow_attempt INT NOT NULL DEFAULT 0 AFTER workflow_node_id,
    ADD COLUMN artifact_id VARCHAR(64) NOT NULL DEFAULT '' AFTER workflow_attempt,
    ADD KEY idx_generation_artifact (artifact_id),
    ADD KEY idx_workflow_node_success (
        workflow_run_id,
        workflow_node_id,
        status,
        workflow_attempt
    );

-- Empty bindings belong to the non-Workflow generation API and must not
-- conflict with one another. Only real Workflow node attempts are unique.
CREATE UNIQUE INDEX uniq_workflow_node_attempt
ON feature_generation_runs (
    (NULLIF(workflow_run_id, '')),
    (NULLIF(workflow_node_id, '')),
    (NULLIF(workflow_attempt, 0))
);

-- 4. Bind historical Artifact approvals to the new Review/Gate model.
-- Existing approvals cannot be reconstructed and are marked as legacy with
-- empty binding values.
ALTER TABLE feature_artifact_reviews
    ADD COLUMN subject_hash CHAR(64) NULL AFTER artifact_id,
    ADD COLUMN review_round_id VARCHAR(64) NULL AFTER subject_hash,
    ADD COLUMN gate_result_id VARCHAR(64) NULL AFTER review_round_id;

UPDATE feature_artifact_reviews
SET subject_hash = '', review_round_id = '', gate_result_id = ''
WHERE subject_hash IS NULL
   OR review_round_id IS NULL
   OR gate_result_id IS NULL;

ALTER TABLE feature_artifact_reviews
    MODIFY COLUMN subject_hash CHAR(64) NOT NULL,
    MODIFY COLUMN review_round_id VARCHAR(64) NOT NULL,
    MODIFY COLUMN gate_result_id VARCHAR(64) NOT NULL;

-- 5. Bind historical Change approvals to the new Review/Gate model.
ALTER TABLE feature_change_reviews
    ADD COLUMN subject_hash CHAR(64) NULL AFTER run_id,
    ADD COLUMN review_round_id VARCHAR(64) NULL AFTER subject_hash,
    ADD COLUMN gate_result_id VARCHAR(64) NULL AFTER review_round_id;

UPDATE feature_change_reviews
SET subject_hash = '', review_round_id = '', gate_result_id = ''
WHERE subject_hash IS NULL
   OR review_round_id IS NULL
   OR gate_result_id IS NULL;

ALTER TABLE feature_change_reviews
    MODIFY COLUMN subject_hash CHAR(64) NOT NULL,
    MODIFY COLUMN review_round_id VARCHAR(64) NOT NULL,
    MODIFY COLUMN gate_result_id VARCHAR(64) NOT NULL;

-- 6. Verification: the first query must return 13 rows, the second 5 rows,
-- and all three counters in the final result must be 0.
SELECT table_name, column_name, column_type, is_nullable,
       column_default, generation_expression
FROM information_schema.columns
WHERE table_schema = 'Nasuta'
  AND (
      (table_name = 'users'
       AND column_name IN ('password_hash', 'email_key'))
   OR (table_name = 'qa_turns'
       AND column_name = 'run_id_key')
   OR (table_name = 'feature_generation_runs'
       AND column_name IN (
           'workflow_run_id', 'workflow_node_id',
           'workflow_attempt', 'artifact_id'
       ))
   OR (table_name = 'feature_artifact_reviews'
       AND column_name IN (
           'subject_hash', 'review_round_id', 'gate_result_id'
       ))
   OR (table_name = 'feature_change_reviews'
       AND column_name IN (
           'subject_hash', 'review_round_id', 'gate_result_id'
       ))
  )
ORDER BY table_name, ordinal_position;

SELECT table_name, index_name, non_unique,
       GROUP_CONCAT(
           COALESCE(column_name, expression)
           ORDER BY seq_in_index
       ) AS indexed_columns
FROM information_schema.statistics
WHERE table_schema = 'Nasuta'
  AND index_name IN (
      'uniq_email',
      'uniq_session_run',
      'idx_generation_artifact',
      'uniq_workflow_node_attempt',
      'idx_workflow_node_success'
  )
GROUP BY table_name, index_name, non_unique
ORDER BY table_name, index_name;

SELECT
    (SELECT COUNT(*)
     FROM feature_artifact_reviews
     WHERE subject_hash IS NULL
        OR review_round_id IS NULL
        OR gate_result_id IS NULL) AS artifact_review_null_bindings,
    (SELECT COUNT(*)
     FROM feature_change_reviews
     WHERE subject_hash IS NULL
        OR review_round_id IS NULL
        OR gate_result_id IS NULL) AS change_review_null_bindings,
    (SELECT COUNT(*)
     FROM feature_generation_runs
     WHERE workflow_run_id <> ''
       AND (workflow_node_id = '' OR workflow_attempt <= 0))
        AS invalid_workflow_generation_bindings;

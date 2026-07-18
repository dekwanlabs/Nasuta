-- Codeloom time-column migration
--
-- This is an explicit database migration for installations created before
-- the schema switched API timestamps from strings to TIMESTAMP columns.
-- It is intentionally NOT executed by application startup and does not create
-- a stored function, so it works with binary logging and without SUPER.
--
-- Run after taking a backup. The session is forced to UTC so offset-bearing
-- values such as `2026-07-11 00:35:20+08:00` keep their actual instant.

SET time_zone = '+00:00';
SET @legacy_zone_pattern = '(Z|[+-][0-9]{2}:[0-9]{2})$';

-- Each expression strips T/Z/offset syntax before CAST. DATETIME(6) accepts
-- both second precision and fractional seconds. Required columns use NOW for
-- empty/invalid legacy values; nullable columns remain NULL.

-- auth
UPDATE users
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    updated_at = COALESCE(CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

UPDATE sessions
SET expires_at = COALESCE(CASE WHEN NULLIF(TRIM(expires_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(expires_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(expires_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(expires_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(expires_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

UPDATE observe_history
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

UPDATE settings
SET updated_at = COALESCE(CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

-- rbac
UPDATE rbac_roles
SET created_at = CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END,
    updated_at = CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END;

UPDATE rbac_menus
SET created_at = CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END;

UPDATE rbac_mcp_keys
SET created_at = CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END,
    expires_at = CASE WHEN NULLIF(TRIM(expires_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(expires_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(expires_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(expires_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(expires_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END;

-- documents and QA
UPDATE documents
SET created_at = CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END,
    updated_at = CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END;

UPDATE qa_sessions
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    updated_at = COALESCE(CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

UPDATE qa_messages
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

UPDATE agent_runs
SET started_at = COALESCE(CASE WHEN NULLIF(TRIM(started_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(started_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(started_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(started_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(started_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    ended_at = CASE WHEN NULLIF(TRIM(ended_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(ended_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(ended_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(ended_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(ended_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END;

UPDATE agent_steps
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

-- The legacy schema rejected the NULL produced for memories never recalled.
ALTER TABLE qa_memories MODIFY COLUMN last_used VARCHAR(40) NULL DEFAULT NULL;

UPDATE qa_memories
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    updated_at = COALESCE(CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    last_used = CASE WHEN NULLIF(TRIM(last_used), '') IS NULL THEN NULL WHEN RIGHT(TRIM(last_used), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(last_used), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(last_used), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(last_used), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END;

UPDATE pending_actions
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    decided_at = CASE WHEN NULLIF(TRIM(decided_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(decided_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(decided_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(decided_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(decided_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END,
    expires_at = COALESCE(CASE WHEN NULLIF(TRIM(expires_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(expires_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(expires_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(expires_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(expires_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

-- incident and observe
UPDATE incident_records
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    updated_at = COALESCE(CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    fix_started_at = CASE WHEN NULLIF(TRIM(fix_started_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(fix_started_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(fix_started_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(fix_started_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(fix_started_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END,
    fixed_at = CASE WHEN NULLIF(TRIM(fixed_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(fixed_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(fixed_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(fixed_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(fixed_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END;

UPDATE observe_sources
SET created_at = COALESCE(CASE WHEN NULLIF(TRIM(created_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(created_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(created_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(created_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP),
    updated_at = COALESCE(CASE WHEN NULLIF(TRIM(updated_at), '') IS NULL THEN NULL WHEN RIGHT(TRIM(updated_at), 6) REGEXP '^[+-][0-9]{2}:[0-9]{2}$' THEN CONVERT_TZ(CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)), RIGHT(TRIM(updated_at), 6), '+00:00') ELSE CAST(REPLACE(REGEXP_REPLACE(TRIM(updated_at), @legacy_zone_pattern, ''), 'T', ' ') AS DATETIME(6)) END, CURRENT_TIMESTAMP);

-- schema changes
ALTER TABLE users MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP;
ALTER TABLE sessions MODIFY COLUMN expires_at TIMESTAMP NOT NULL, MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE observe_history MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE settings MODIFY COLUMN updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP;
ALTER TABLE rbac_roles MODIFY COLUMN created_at TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN updated_at TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP;
ALTER TABLE rbac_menus MODIFY COLUMN created_at TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE rbac_mcp_keys MODIFY COLUMN created_at TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN expires_at TIMESTAMP NULL DEFAULT NULL;
ALTER TABLE documents MODIFY COLUMN created_at TIMESTAMP NULL DEFAULT NULL, MODIFY COLUMN updated_at TIMESTAMP NULL DEFAULT NULL;
ALTER TABLE qa_sessions MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP;
ALTER TABLE qa_messages MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE agent_runs MODIFY COLUMN started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN ended_at TIMESTAMP NULL DEFAULT NULL;
ALTER TABLE agent_steps MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE qa_memories MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, MODIFY COLUMN last_used DATETIME NULL DEFAULT NULL;
ALTER TABLE pending_actions MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN decided_at TIMESTAMP NULL DEFAULT NULL, MODIFY COLUMN expires_at TIMESTAMP NOT NULL;
ALTER TABLE incident_records MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, MODIFY COLUMN fix_started_at TIMESTAMP NULL DEFAULT NULL, MODIFY COLUMN fixed_at TIMESTAMP NULL DEFAULT NULL;
ALTER TABLE observe_sources MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, MODIFY COLUMN updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP;

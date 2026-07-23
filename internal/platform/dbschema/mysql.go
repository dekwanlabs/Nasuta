package dbschema

import (
	"database/sql"
	"fmt"
)

// MySQLGroup identifies one logical slice of the shared MySQL schema.
type MySQLGroup string

const (
	GroupAuth      MySQLGroup = "auth"
	GroupRBAC      MySQLGroup = "rbac"
	GroupDocuments MySQLGroup = "documents"
	GroupQASession MySQLGroup = "qa_session"
	GroupQARun     MySQLGroup = "qa_run"
	GroupQAMemory  MySQLGroup = "qa_memory"
	GroupIncident  MySQLGroup = "incident"
	GroupApproval  MySQLGroup = "approval"
)

// allMySQLGroups lists every schema group in dependency order. GroupRBAC follows
// GroupAuth because its tables carry foreign keys into users, created by GroupAuth.
var allMySQLGroups = []MySQLGroup{
	GroupAuth,
	GroupRBAC,
	GroupDocuments,
	GroupQASession,
	GroupQARun,
	GroupQAMemory,
	GroupIncident,
	GroupApproval,
}

// AllGroups returns every known MySQL schema group.
func AllGroups() []MySQLGroup { return allMySQLGroups }

var mysqlSchema = map[MySQLGroup][]string{
	GroupAuth: {
		`CREATE TABLE IF NOT EXISTS users (
				id            BIGINT AUTO_INCREMENT PRIMARY KEY,
				feishu_uid    VARCHAR(64)  NULL UNIQUE,
				open_id       VARCHAR(128) NOT NULL DEFAULT '',
				name          VARCHAR(128) NOT NULL DEFAULT '',
				email         VARCHAR(256) NOT NULL DEFAULT '',
				password_hash VARCHAR(255) NOT NULL DEFAULT '',
				avatar_url    VARCHAR(512) NOT NULL DEFAULT '',
				department    VARCHAR(256) NOT NULL DEFAULT '',
				is_admin      TINYINT(1)   NOT NULL DEFAULT 0,
				created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				email_key     VARCHAR(256) AS (NULLIF(email, '')) STORED,
				UNIQUE KEY uniq_email (email_key)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS sessions (
				token        VARCHAR(64)  NOT NULL PRIMARY KEY,
				user_id      BIGINT       NOT NULL,
				expires_at   TIMESTAMP    NOT NULL,
				created_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
				INDEX idx_user (user_id),
				INDEX idx_expires (expires_at)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS settings (
				k          VARCHAR(128) NOT NULL PRIMARY KEY,
				v          TEXT         NOT NULL,
				updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
	GroupRBAC: {
		`CREATE TABLE IF NOT EXISTS rbac_roles (
				id          BIGINT AUTO_INCREMENT PRIMARY KEY,
				name        VARCHAR(64) NOT NULL UNIQUE,
				description VARCHAR(255) DEFAULT '',
				prompt      TEXT,
				created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
				updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS rbac_user_roles (
				id       BIGINT AUTO_INCREMENT PRIMARY KEY,
				user_id  BIGINT NOT NULL,
				role_id  BIGINT NOT NULL,
				UNIQUE KEY uk_user_role (user_id, role_id),
				FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
				FOREIGN KEY (role_id) REFERENCES rbac_roles(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS rbac_menus (
				id          BIGINT AUTO_INCREMENT PRIMARY KEY,
				parent_id   BIGINT DEFAULT 0,
				name        VARCHAR(64) NOT NULL,
				path        VARCHAR(128) DEFAULT '',
				icon        VARCHAR(64) DEFAULT '',
				sort_order  INT DEFAULT 0,
				created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS rbac_role_menus (
				id      BIGINT AUTO_INCREMENT PRIMARY KEY,
				role_id BIGINT NOT NULL,
				menu_id BIGINT NOT NULL,
				UNIQUE KEY uk_role_menu (role_id, menu_id),
				FOREIGN KEY (role_id) REFERENCES rbac_roles(id) ON DELETE CASCADE,
				FOREIGN KEY (menu_id) REFERENCES rbac_menus(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS rbac_mcp_keys (
				id          BIGINT AUTO_INCREMENT PRIMARY KEY,
				user_id     BIGINT NOT NULL,
				key_name    VARCHAR(128) NOT NULL,
				api_key     VARCHAR(64) NOT NULL UNIQUE,
				is_active   TINYINT DEFAULT 1,
				created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
				expires_at  TIMESTAMP DEFAULT NULL,
				FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
	GroupDocuments: {
		`CREATE TABLE IF NOT EXISTS documents (
				id          VARCHAR(64) PRIMARY KEY,
				title       TEXT,
				filename    VARCHAR(256),
				kind        VARCHAR(32)  NOT NULL DEFAULT 'document',
				content     MEDIUMTEXT,
				chunk_count INT DEFAULT 0,
				created_at  TIMESTAMP NULL DEFAULT NULL,
				updated_at  TIMESTAMP NULL DEFAULT NULL
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
	GroupQASession: {
		`CREATE TABLE IF NOT EXISTS qa_sessions (
				id         VARCHAR(64) PRIMARY KEY,
				user_id    BIGINT NOT NULL DEFAULT 0,
				title      VARCHAR(512) NOT NULL DEFAULT '',
				summary    TEXT,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				KEY idx_user (user_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS qa_messages (
				id         BIGINT AUTO_INCREMENT PRIMARY KEY,
				session_id VARCHAR(64) NOT NULL,
				seq        INT NOT NULL,
				role       VARCHAR(32) NOT NULL,
				content    MEDIUMTEXT NOT NULL,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_session_seq (session_id, seq),
				KEY idx_session (session_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
	GroupQARun: {
		`CREATE TABLE IF NOT EXISTS agent_runs (
				id         VARCHAR(64) PRIMARY KEY,
				user_id    BIGINT NOT NULL DEFAULT 0,
				session_id VARCHAR(64) NOT NULL DEFAULT '',
				question   TEXT NOT NULL,
				status     VARCHAR(16) NOT NULL,
				mode       VARCHAR(16) NOT NULL DEFAULT 'single',
				max_steps  INT NOT NULL DEFAULT 0,
				step_count INT NOT NULL DEFAULT 0,
				token_used INT NOT NULL DEFAULT 0,
				input_tokens BIGINT NOT NULL DEFAULT 0,
				cached_input_tokens BIGINT NOT NULL DEFAULT 0,
				output_tokens BIGINT NOT NULL DEFAULT 0,
				reasoning_tokens BIGINT NOT NULL DEFAULT 0,
				total_tokens BIGINT NOT NULL DEFAULT 0,
				llm_call_count INT NOT NULL DEFAULT 0,
				peak_input_tokens INT NOT NULL DEFAULT 0,
				peak_reserved_tokens INT NOT NULL DEFAULT 0,
				started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				ended_at   TIMESTAMP NULL DEFAULT NULL,
				KEY idx_user (user_id),
				KEY idx_session (session_id),
				KEY idx_status (status)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS agent_steps (
				id               BIGINT AUTO_INCREMENT PRIMARY KEY,
				run_id           VARCHAR(64) NOT NULL,
				step_no          INT NOT NULL,
				kind             VARCHAR(16) NOT NULL,
				tool             VARCHAR(64) NOT NULL DEFAULT '',
				args             TEXT,
				result_summary   TEXT,
				content          MEDIUMTEXT,
				token_delta      INT NOT NULL DEFAULT 0,
				reasoning_tokens INT NOT NULL DEFAULT 0,
				duration_ms      INT NOT NULL DEFAULT 0,
				created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_run_step (run_id, step_no),
				KEY idx_run (run_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS agent_llm_calls (
				id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
				run_id              VARCHAR(64) NOT NULL,
				call_seq            INT NOT NULL,
				phase               VARCHAR(32) NOT NULL,
				provider            VARCHAR(32) NOT NULL,
				model               VARCHAR(128) NOT NULL,
				input_tokens        INT NOT NULL DEFAULT 0,
				cached_input_tokens INT NOT NULL DEFAULT 0,
				output_tokens       INT NOT NULL DEFAULT 0,
				reasoning_tokens    INT NOT NULL DEFAULT 0,
				total_tokens        INT NOT NULL DEFAULT 0,
				max_output_tokens   INT NOT NULL DEFAULT 0,
				duration_ms         INT NOT NULL DEFAULT 0,
				status              VARCHAR(16) NOT NULL,
				created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_run_call (run_id, call_seq),
				KEY idx_run (run_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
	GroupQAMemory: {
		`CREATE TABLE IF NOT EXISTS qa_memories (
				id             VARCHAR(64) PRIMARY KEY,
				user_id        BIGINT NOT NULL DEFAULT 0,
				fact_key       VARCHAR(255) NOT NULL,
				kind           VARCHAR(32) NOT NULL,
				content        TEXT NOT NULL,
				source_type    VARCHAR(32) NOT NULL,
				authority      INT NOT NULL DEFAULT 0,
				status         VARCHAR(16) NOT NULL DEFAULT 'active',
				superseded_by  VARCHAR(64) NULL,
				source_session VARCHAR(64) NOT NULL DEFAULT '',
				confidence     FLOAT NOT NULL DEFAULT 1.0,
				expires_at     DATETIME NULL DEFAULT NULL,
				created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				last_used      DATETIME NULL DEFAULT NULL,
				use_count      INT NOT NULL DEFAULT 0,
				active_fact_key VARCHAR(255)
					GENERATED ALWAYS AS (CASE WHEN status = 'active' THEN fact_key ELSE NULL END) STORED,
				UNIQUE KEY uniq_user_factkey_active (user_id, active_fact_key),
				KEY idx_user_status (user_id, status),
				KEY idx_kind (kind)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
	GroupIncident: {
		`CREATE TABLE IF NOT EXISTS incident_records (
				id                 VARCHAR(64)  NOT NULL PRIMARY KEY,
				dedup_key          VARCHAR(512) NOT NULL DEFAULT '',
				created_at         TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at         TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				status             VARCHAR(32)  NOT NULL,
				source             VARCHAR(64)  NOT NULL,
				alert_title        VARCHAR(512) NOT NULL,
				alert_payload_json LONGTEXT,
				error_logs_json    LONGTEXT,
				traces_json        LONGTEXT,
				affected_svcs_json TEXT,
				root_cause         TEXT,
				solution           TEXT,
				analysis_doc       LONGTEXT,
				assigned_to        VARCHAR(128) NOT NULL DEFAULT '',
				fix_branches_json  LONGTEXT,
				fix_started_at     TIMESTAMP NULL DEFAULT NULL,
				fixed_at           TIMESTAMP NULL DEFAULT NULL,
				created_unix       BIGINT NOT NULL DEFAULT 0,
				updated_unix       BIGINT NOT NULL DEFAULT 0,
				KEY idx_dedup (dedup_key),
				KEY idx_status (status),
				KEY idx_created (created_unix)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
	GroupApproval: {
		`CREATE TABLE IF NOT EXISTS pending_actions (
				id           VARCHAR(64) PRIMARY KEY,
				tool         VARCHAR(64) NOT NULL,
				incident_id  VARCHAR(64) NOT NULL DEFAULT '',
				args_json    TEXT NOT NULL,
				rationale    TEXT NOT NULL,
				impact       TEXT NOT NULL,
				status       VARCHAR(16) NOT NULL DEFAULT 'pending',
				requested_by BIGINT NOT NULL DEFAULT 0,
				approver     BIGINT NOT NULL DEFAULT 0,
				result_json  TEXT,
				created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				decided_at   TIMESTAMP NULL DEFAULT NULL,
				expires_at   TIMESTAMP NOT NULL,
				KEY idx_status (status),
				KEY idx_incident (incident_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	},
}

// MigrateMySQL creates missing schema groups. Changes to existing installations
// are explicit migrations under docs/sql and are not applied at startup.
func MigrateMySQL(db *sql.DB, groups ...MySQLGroup) error {
	for _, group := range dedupeGroups(groups) {
		stmts, ok := mysqlSchema[group]
		if !ok {
			return fmt.Errorf("mysql schema: unknown group %q", group)
		}
		if err := createGroupTables(db, group, stmts); err != nil {
			return err
		}
	}
	return nil
}

func dedupeGroups(groups []MySQLGroup) []MySQLGroup {
	seen := map[MySQLGroup]bool{}
	out := make([]MySQLGroup, 0, len(groups))
	for _, group := range groups {
		if seen[group] {
			continue
		}
		seen[group] = true
		out = append(out, group)
	}
	return out
}

func createGroupTables(db *sql.DB, group MySQLGroup, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("mysql schema %s: create table: %w", group, err)
		}
	}
	return nil
}

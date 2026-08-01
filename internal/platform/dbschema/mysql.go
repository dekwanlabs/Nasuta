package dbschema

import (
	"database/sql"
	"fmt"
)

// MySQLGroup identifies one logical slice of the shared MySQL schema.
type MySQLGroup string

const (
	GroupAuth            MySQLGroup = "auth"
	GroupRBAC            MySQLGroup = "rbac"
	GroupDocuments       MySQLGroup = "documents"
	GroupQASession       MySQLGroup = "qa_session"
	GroupQARun           MySQLGroup = "qa_run"
	GroupQAMemory        MySQLGroup = "qa_memory"
	GroupIncident        MySQLGroup = "incident"
	GroupApproval        MySQLGroup = "approval"
	GroupFeatureDelivery MySQLGroup = "feature_delivery"
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
	GroupFeatureDelivery,
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
				archived_summary_tokens BIGINT NOT NULL DEFAULT 0,
				compacted_through_turn INT NOT NULL DEFAULT 0,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				KEY idx_user (user_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS qa_messages (
				id         BIGINT AUTO_INCREMENT PRIMARY KEY,
				session_id VARCHAR(64) NOT NULL,
				seq        INT NOT NULL,
				turn_no    INT NOT NULL,
				role       VARCHAR(32) NOT NULL,
				content    MEDIUMTEXT NOT NULL,
				tool_calls_json MEDIUMTEXT NULL,
				tool_call_id VARCHAR(128) NOT NULL DEFAULT '',
				tool_name   VARCHAR(128) NOT NULL DEFAULT '',
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_session_seq (session_id, seq),
				KEY idx_session (session_id),
				KEY idx_session_turn (session_id, turn_no, seq)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS qa_turns (
				session_id    VARCHAR(64) NOT NULL,
				turn_no       INT NOT NULL,
				run_id        VARCHAR(64) NOT NULL DEFAULT '',
				first_seq     INT NOT NULL,
				last_seq      INT NOT NULL,
				token_estimate INT NOT NULL DEFAULT 0,
				question_text VARCHAR(2048) NOT NULL DEFAULT '',
				topic_key VARCHAR(512) NOT NULL DEFAULT '',
				entities_json JSON NOT NULL,
				question_terms_json JSON NOT NULL,
				evidence_manifest_json JSON NOT NULL,
				created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (session_id, turn_no),
				KEY idx_session_last (session_id, turn_no DESC)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS qa_turn_contexts (
				ref           VARCHAR(64) PRIMARY KEY,
				session_id    VARCHAR(64) NOT NULL,
				user_id       BIGINT NOT NULL,
				run_id        VARCHAR(64) NOT NULL DEFAULT '',
				turn_number   INT NOT NULL,
				detail_json   JSON NOT NULL,
				summary_text  TEXT NOT NULL,
				summary_tokens INT NOT NULL DEFAULT 0,
				source_tokens INT NOT NULL DEFAULT 0,
				retained_tokens INT NOT NULL DEFAULT 0,
				created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_session_turn_context (session_id, turn_number),
				KEY idx_user_session (user_id, session_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS qa_session_history_terms (
				session_id  VARCHAR(64) NOT NULL,
				user_id     BIGINT NOT NULL,
				term        VARCHAR(191) NOT NULL,
				ref         VARCHAR(64) NOT NULL,
				turn_number INT NOT NULL,
				weight      SMALLINT NOT NULL DEFAULT 1,
				PRIMARY KEY (session_id, term, ref),
				KEY idx_ref (ref),
				KEY idx_user_session_turn (user_id, session_id, turn_number)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS qa_session_history_index_outbox (
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
				evidence_status VARCHAR(16) NOT NULL DEFAULT 'unavailable',
				forced_conclusion BOOLEAN NOT NULL DEFAULT FALSE,
				evidence_result_count INT NOT NULL DEFAULT 0,
				tool_call_count INT NOT NULL DEFAULT 0,
				tool_failure_count INT NOT NULL DEFAULT 0,
				partial_result_count INT NOT NULL DEFAULT 0,
				omitted_evidence_count INT NOT NULL DEFAULT 0,
				started_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				ended_at   TIMESTAMP NULL DEFAULT NULL,
				KEY idx_user (user_id),
				KEY idx_session (session_id),
				KEY idx_status (status)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS agent_steps (
				id                   BIGINT AUTO_INCREMENT PRIMARY KEY,
				run_id               VARCHAR(64) NOT NULL,
				step_no              INT NOT NULL,
				kind                 VARCHAR(16) NOT NULL,
				trace_id             VARCHAR(64) NOT NULL DEFAULT '',
				artifact_id          VARCHAR(64) NOT NULL DEFAULT '',
				tool_call_id         VARCHAR(128) NOT NULL DEFAULT '',
				tool                 VARCHAR(64) NOT NULL DEFAULT '',
				args                 TEXT,
				content              MEDIUMTEXT,
				prompt_content       MEDIUMTEXT,
				authoritative_sha256 CHAR(64) NOT NULL DEFAULT '',
				prompt_sha256        CHAR(64) NOT NULL DEFAULT '',
				content_bytes        BIGINT NOT NULL DEFAULT 0,
				coverage_json        JSON NOT NULL,
				answer_contract_json JSON NOT NULL,
				failed               BOOLEAN NOT NULL DEFAULT FALSE,
				delivery_error       VARCHAR(128) NOT NULL DEFAULT '',
				token_delta          INT NOT NULL DEFAULT 0,
				reasoning_tokens     INT NOT NULL DEFAULT 0,
				duration_ms          INT NOT NULL DEFAULT 0,
				created_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_run_step (run_id, step_no),
				KEY idx_trace (trace_id),
				KEY idx_run (run_id),
				KEY idx_artifact (artifact_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS agent_tool_result_artifacts (
				id           VARCHAR(64) PRIMARY KEY,
				user_id      BIGINT NOT NULL,
				session_id   VARCHAR(64) NOT NULL,
				run_id       VARCHAR(64) NOT NULL,
				tool_call_id VARCHAR(128) NOT NULL,
				content      LONGBLOB NOT NULL,
				content_type VARCHAR(128) NOT NULL,
				sha256       CHAR(64) NOT NULL,
				size_bytes   BIGINT NOT NULL,
				coverage_json JSON NOT NULL,
				created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_run_tool_call (run_id, tool_call_id),
				KEY idx_user_session (user_id, session_id),
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
				KEY idx_dedup (dedup_key),
				KEY idx_status (status),
				KEY idx_created_at_id (created_at, id)
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
	GroupFeatureDelivery: {
		`CREATE TABLE IF NOT EXISTS feature_user_workspaces (
				user_id           BIGINT PRIMARY KEY,
				username_key      VARCHAR(128) NOT NULL,
				username_snapshot VARCHAR(128) NOT NULL,
				created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_workspace_username (username_key)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_requests (
				id          VARCHAR(64) PRIMARY KEY,
				title       VARCHAR(512) NOT NULL,
				created_by  BIGINT NOT NULL,
				archived_at TIMESTAMP NULL,
				created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				KEY idx_owner_updated (created_by, updated_at, id),
				KEY idx_updated (updated_at, id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_artifacts (
				id                 VARCHAR(64) PRIMARY KEY,
				request_id         VARCHAR(64) NOT NULL,
				kind               VARCHAR(32) NOT NULL,
				version            INT NOT NULL,
				parent_artifact_id VARCHAR(64) NOT NULL DEFAULT '',
				origin             VARCHAR(16) NOT NULL,
				document_json      JSON NOT NULL,
				rendered_markdown  MEDIUMTEXT NOT NULL,
				evidence_json      JSON NOT NULL,
				content_hash       CHAR(64) NOT NULL,
				created_by         BIGINT NOT NULL,
				created_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
					UNIQUE KEY uniq_request_kind_version (request_id, kind, version),
					KEY idx_request_kind_parent_version (request_id, kind, parent_artifact_id, version),
					KEY idx_request_created (request_id, created_at, id),
				KEY idx_parent (parent_artifact_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_artifact_reviews (
				artifact_id VARCHAR(64) PRIMARY KEY,
				decision    VARCHAR(16) NOT NULL,
				comment     TEXT NOT NULL,
				reviewer    BIGINT NOT NULL,
				created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				KEY idx_reviewer_created (reviewer, created_at)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_generation_runs (
				id                 VARCHAR(64) PRIMARY KEY,
				request_id         VARCHAR(64) NOT NULL,
				artifact_kind      VARCHAR(32) NOT NULL,
				parent_artifact_id VARCHAR(64) NOT NULL,
				status             VARCHAR(16) NOT NULL,
				provider           VARCHAR(32) NOT NULL,
				model              VARCHAR(128) NOT NULL,
				requested_by       BIGINT NOT NULL,
				input_tokens       BIGINT NOT NULL DEFAULT 0,
				output_tokens      BIGINT NOT NULL DEFAULT 0,
				error_summary      VARCHAR(2048) NOT NULL DEFAULT '',
				started_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				ended_at           TIMESTAMP NULL,
				KEY idx_request_started (request_id, started_at, id),
				KEY idx_status_started (status, started_at, id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_implementation_runs (
				id                     VARCHAR(64) PRIMARY KEY,
				request_id             VARCHAR(64) NOT NULL,
				client_request_id      VARCHAR(128) NOT NULL,
				request_hash           CHAR(64) NOT NULL,
				design_artifact_id     VARCHAR(64) NOT NULL,
				plan_artifact_id       VARCHAR(64) NOT NULL,
				parent_run_id          VARCHAR(64) NOT NULL DEFAULT '',
				repo                   VARCHAR(512) NOT NULL,
				base_ref               VARCHAR(255) NOT NULL,
				base_commit            VARCHAR(64) NOT NULL,
				workspace_user_id      BIGINT NOT NULL,
				workspace_username     VARCHAR(128) NOT NULL,
				provider               VARCHAR(32) NOT NULL,
				model                  VARCHAR(128) NOT NULL DEFAULT '',
				provider_version       VARCHAR(64) NOT NULL DEFAULT '',
				network_enabled        TINYINT(1) NOT NULL DEFAULT 0,
				status                 VARCHAR(16) NOT NULL,
				worker_id              VARCHAR(128) NOT NULL DEFAULT '',
				lease_expires_at       TIMESTAMP NULL,
				cancel_requested_at    TIMESTAMP NULL,
				provider_session_id    VARCHAR(255) NOT NULL DEFAULT '',
				exit_code              INT NULL,
				error_summary          VARCHAR(2048) NOT NULL DEFAULT '',
				requested_by           BIGINT NOT NULL,
				started_at             TIMESTAMP NULL,
				ended_at               TIMESTAMP NULL,
				retain_until           TIMESTAMP NULL,
				worktree_cleaned_at    TIMESTAMP NULL,
				cleanup_error          VARCHAR(2048) NOT NULL DEFAULT '',
				created_at             TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE KEY uniq_requester_client_request (requested_by, client_request_id),
				KEY idx_request_created (request_id, created_at, id),
				KEY idx_status_created (status, created_at, id),
				KEY idx_lease (status, lease_expires_at)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_run_events (
				run_id      VARCHAR(64) NOT NULL,
				seq         BIGINT NOT NULL,
				kind        VARCHAR(32) NOT NULL,
				summary     TEXT NOT NULL,
				detail_json JSON NULL,
				created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (run_id, seq)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_change_sets (
				run_id                  VARCHAR(64) PRIMARY KEY,
				worktree_head           VARCHAR(64) NOT NULL,
				patch_rel_path          VARCHAR(1024) NOT NULL,
				patch_sha256            CHAR(64) NOT NULL,
				patch_bytes             BIGINT NOT NULL,
				files_changed           INT NOT NULL,
				additions               INT NOT NULL,
				deletions               INT NOT NULL,
				files_json              JSON NOT NULL,
				plan_deviations_json    JSON NOT NULL,
				validation_results_json JSON NOT NULL,
				provider_summary        TEXT NOT NULL,
				created_at              TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS feature_change_reviews (
				run_id     VARCHAR(64) PRIMARY KEY,
				decision   VARCHAR(16) NOT NULL,
				comment    TEXT NOT NULL,
				reviewer   BIGINT NOT NULL,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				KEY idx_reviewer_created (reviewer, created_at)
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

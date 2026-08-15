package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// Store persists immutable definitions and mutable rollout metadata.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("agent catalog database is required")
	}
	return &Store{db: db}, nil
}

func (catalogStore *Store) Publish(
	ctx context.Context,
	definitions []agentapi.Definition,
	actorUserID int64,
) ([]DefinitionRecord, error) {
	tx, err := catalogStore.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin agent definition publication: %w", err)
	}
	defer tx.Rollback()
	records := make([]DefinitionRecord, 0, len(definitions))
	for _, definition := range definitions {
		record, created, err := publishDefinitionTx(ctx, tx, definition, actorUserID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		if created {
			if err := appendAuditTx(
				ctx, tx, definition.ID, definition.Version, "published", actorUserID,
			); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit agent definition publication: %w", err)
	}
	return records, nil
}

func publishDefinitionTx(
	ctx context.Context,
	tx *sql.Tx,
	definition agentapi.Definition,
	actorUserID int64,
) (DefinitionRecord, bool, error) {
	var existing DefinitionRecord
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT
		definition_json,content_hash,active,is_default,created_by,created_at
		FROM agent_definitions
		WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		definition.ID, definition.Version,
	).Scan(
		&raw, &existing.ContentHash, &existing.Active, &existing.Default,
		&existing.CreatedBy, &existing.CreatedAt,
	)
	if err == nil {
		if existing.ContentHash != definition.ContentHash {
			return DefinitionRecord{}, false, fmt.Errorf(
				"agent definition %q version %d is already published: %w",
				definition.ID, definition.Version, ErrConflict,
			)
		}
		if err := json.Unmarshal(raw, &existing.Definition); err != nil {
			return DefinitionRecord{}, false, fmt.Errorf(
				"decode agent definition %q version %d: %w",
				definition.ID, definition.Version, err,
			)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DefinitionRecord{}, false, fmt.Errorf(
			"get agent definition %q version %d: %w",
			definition.ID, definition.Version, err,
		)
	}
	var defaultVersion int64
	err = tx.QueryRowContext(ctx, `SELECT version
		FROM agent_definitions
		WHERE id=? AND is_default=1 LIMIT 1 FOR UPDATE`,
		definition.ID,
	).Scan(&defaultVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DefinitionRecord{}, false, fmt.Errorf(
			"lock agent definition %q default: %w", definition.ID, err,
		)
	}
	makeDefault := defaultVersion == 0 || definition.Version > defaultVersion
	if makeDefault && defaultVersion > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_definitions
			SET is_default=0 WHERE id=? AND is_default=1`, definition.ID); err != nil {
			return DefinitionRecord{}, false, fmt.Errorf(
				"clear agent definition %q default: %w", definition.ID, err,
			)
		}
	}
	raw, err = json.Marshal(definition)
	if err != nil {
		return DefinitionRecord{}, false, fmt.Errorf(
			"marshal agent definition %q version %d: %w",
			definition.ID, definition.Version, err,
		)
	}
	createdAt := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_definitions(
		id,version,definition_json,content_hash,active,is_default,created_by,created_at)
		VALUES(?,?,?,?,1,?,?,?)`,
		definition.ID, definition.Version, raw, definition.ContentHash,
		makeDefault, actorUserID,
		store.DatabaseTime(createdAt.Format(time.RFC3339Nano)),
	); err != nil {
		return DefinitionRecord{}, false, fmt.Errorf(
			"save agent definition %q version %d: %w",
			definition.ID, definition.Version, err,
		)
	}
	return DefinitionRecord{
		Definition: definition, Active: true, Default: makeDefault,
		CreatedBy: actorUserID, CreatedAt: createdAt,
	}, true, nil
}

// LoadFullCatalog is an explicit startup recovery read.
func (catalogStore *Store) LoadFullCatalog(
	ctx context.Context,
) ([]DefinitionRecord, error) {
	rows, err := catalogStore.db.QueryContext(ctx, `SELECT
		definition_json,content_hash,active,is_default,created_by,created_at
		FROM agent_definitions ORDER BY id,version`)
	if err != nil {
		return nil, fmt.Errorf("load full agent catalog: %w", err)
	}
	defer rows.Close()
	records := make([]DefinitionRecord, 0)
	for rows.Next() {
		record, err := scanDefinitionRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate full agent catalog: %w", err)
	}
	return records, nil
}

func (catalogStore *Store) LoadRollouts(
	ctx context.Context,
) ([]RolloutRule, error) {
	rows, err := catalogStore.db.QueryContext(ctx, `SELECT
		subject_id,rule_version,candidate_version,percentage_bps,salt,rule_hash,
		active,created_by,created_at
		FROM catalog_rollouts
		WHERE catalog_kind='agent'
		ORDER BY subject_id`)
	if err != nil {
		return nil, fmt.Errorf("load agent rollouts: %w", err)
	}
	defer rows.Close()
	rules := make([]RolloutRule, 0)
	for rows.Next() {
		var rule RolloutRule
		if err := rows.Scan(
			&rule.AgentID, &rule.RuleVersion, &rule.CandidateVersion,
			&rule.PercentageBPS, &rule.Salt, &rule.RuleHash, &rule.Active,
			&rule.CreatedBy, &rule.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent rollout: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent rollouts: %w", err)
	}
	return rules, nil
}

func (catalogStore *Store) ListDefinitions(
	ctx context.Context,
	cursor DefinitionCursor,
	limit int,
) ([]DefinitionRecord, error) {
	rows, err := catalogStore.db.QueryContext(ctx, `SELECT
		definition_json,content_hash,active,is_default,created_by,created_at
		FROM agent_definitions
		WHERE id>? OR (id=? AND version>?)
		ORDER BY id,version LIMIT ?`,
		cursor.ID, cursor.ID, cursor.Version, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list agent definitions: %w", err)
	}
	defer rows.Close()
	records := make([]DefinitionRecord, 0, limit)
	for rows.Next() {
		record, err := scanDefinitionRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent definitions: %w", err)
	}
	return records, nil
}

func (catalogStore *Store) SetDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
) error {
	tx, err := catalogStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent default update: %w", err)
	}
	defer tx.Rollback()
	var active, current bool
	if err := tx.QueryRowContext(ctx, `SELECT active,is_default
		FROM agent_definitions WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		id, version,
	).Scan(&active, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf(
				"agent definition %q version %d not found: %w",
				id, version, ErrNotFound,
			)
		}
		return fmt.Errorf("lock agent definition %q version %d: %w", id, version, err)
	}
	if !active {
		return fmt.Errorf(
			"agent definition %q version %d is disabled: %w",
			id, version, ErrConflict,
		)
	}
	if !current {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_definitions
			SET is_default=0 WHERE id=? AND is_default=1`, id); err != nil {
			return fmt.Errorf("clear agent definition %q default: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_definitions
			SET is_default=1 WHERE id=? AND version=?`, id, version); err != nil {
			return fmt.Errorf(
				"set agent definition %q version %d default: %w",
				id, version, err,
			)
		}
		if err := appendAuditTx(ctx, tx, id, version, "default_set", actorUserID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent default update: %w", err)
	}
	return nil
}

func (catalogStore *Store) SetActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
) error {
	tx, err := catalogStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent status update: %w", err)
	}
	defer tx.Rollback()
	var current, isDefault bool
	if err := tx.QueryRowContext(ctx, `SELECT active,is_default
		FROM agent_definitions WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		id, version,
	).Scan(&current, &isDefault); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf(
				"agent definition %q version %d not found: %w",
				id, version, ErrNotFound,
			)
		}
		return fmt.Errorf("lock agent definition %q version %d: %w", id, version, err)
	}
	if !active && isDefault {
		return fmt.Errorf(
			"agent definition %q version %d is the default: %w",
			id, version, ErrConflict,
		)
	}
	if current != active {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_definitions
			SET active=? WHERE id=? AND version=?`, active, id, version); err != nil {
			return fmt.Errorf(
				"set agent definition %q version %d status: %w",
				id, version, err,
			)
		}
		action := "disabled"
		if active {
			action = "enabled"
		}
		if err := appendAuditTx(ctx, tx, id, version, action, actorUserID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent status update: %w", err)
	}
	return nil
}

func (catalogStore *Store) SetRollout(
	ctx context.Context,
	rule RolloutRule,
	actorUserID int64,
) error {
	tx, err := catalogStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent rollout update: %w", err)
	}
	defer tx.Rollback()
	var existingVersion int64
	err = tx.QueryRowContext(ctx, `SELECT rule_version
		FROM catalog_rollouts
		WHERE catalog_kind='agent' AND subject_id=? LIMIT 1 FOR UPDATE`,
		rule.AgentID,
	).Scan(&existingVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock agent rollout %q: %w", rule.AgentID, err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if rule.RuleVersion != 1 {
			return fmt.Errorf(
				"agent rollout %q expected initial rule version 1, got %d: %w",
				rule.AgentID, rule.RuleVersion, ErrConflict,
			)
		}
	} else if rule.RuleVersion != existingVersion+1 {
		return fmt.Errorf(
			"agent rollout %q expected rule version %d, got %d: %w",
			rule.AgentID, existingVersion+1, rule.RuleVersion, ErrConflict,
		)
	}
	createdAt := rule.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO catalog_rollouts(
			catalog_kind,subject_id,rule_version,candidate_id,candidate_version,
			percentage_bps,salt,rule_hash,active,created_by,created_at)
			VALUES('agent',?,?,?,?,?,?,?,?,?,?)`,
			rule.AgentID, rule.RuleVersion, rule.AgentID, rule.CandidateVersion, rule.PercentageBPS,
			rule.Salt, rule.RuleHash, rule.Active, actorUserID,
			store.DatabaseTime(createdAt.Format(time.RFC3339Nano)),
		)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE catalog_rollouts SET
			rule_version=?,candidate_id=?,candidate_version=?,percentage_bps=?,salt=?,rule_hash=?,
			active=?,created_by=?,created_at=?
			WHERE catalog_kind='agent' AND subject_id=?`,
			rule.RuleVersion, rule.AgentID, rule.CandidateVersion, rule.PercentageBPS, rule.Salt,
			rule.RuleHash, rule.Active, actorUserID,
			store.DatabaseTime(createdAt.Format(time.RFC3339Nano)), rule.AgentID,
		)
	}
	if err != nil {
		return fmt.Errorf("save agent rollout %q: %w", rule.AgentID, err)
	}
	action := "rollout_disabled"
	if rule.Active {
		action = "rollout_enabled"
	}
	if err := appendRolloutAuditTx(ctx, tx, rule, action, actorUserID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent rollout update: %w", err)
	}
	return nil
}

func (catalogStore *Store) ListAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]AuditEvent, error) {
	rows, err := catalogStore.db.QueryContext(ctx, `SELECT
		seq,subject_id,version,action,actor_user_id,created_at
		FROM catalog_audit
		WHERE catalog_kind='agent' AND event_kind='definition'
			AND subject_id=? AND seq>?
		ORDER BY seq LIMIT ?`, id, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list agent definition %q audit: %w", id, err)
	}
	defer rows.Close()
	events := make([]AuditEvent, 0, limit)
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(
			&event.Seq, &event.DefinitionID, &event.Version, &event.Action,
			&event.ActorUserID, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent definition %q audit: %w", id, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent definition %q audit: %w", id, err)
	}
	return events, nil
}

func (catalogStore *Store) ListRolloutAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]RolloutAuditEvent, error) {
	rows, err := catalogStore.db.QueryContext(ctx, `SELECT
		seq,subject_id,version,candidate_version,percentage_bps,rule_hash,
		action,actor_user_id,created_at
		FROM catalog_audit
		WHERE catalog_kind='agent' AND event_kind='rollout'
			AND subject_id=? AND seq>?
		ORDER BY seq LIMIT ?`, id, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list agent rollout %q audit: %w", id, err)
	}
	defer rows.Close()
	events := make([]RolloutAuditEvent, 0, limit)
	for rows.Next() {
		var event RolloutAuditEvent
		if err := rows.Scan(
			&event.Seq, &event.AgentID, &event.RuleVersion,
			&event.CandidateVersion, &event.PercentageBPS, &event.RuleHash,
			&event.Action, &event.ActorUserID, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent rollout %q audit: %w", id, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent rollout %q audit: %w", id, err)
	}
	return events, nil
}

func scanDefinitionRecord(row interface{ Scan(...any) error }) (DefinitionRecord, error) {
	var record DefinitionRecord
	var raw []byte
	if err := row.Scan(
		&raw, &record.ContentHash, &record.Active, &record.Default,
		&record.CreatedBy, &record.CreatedAt,
	); err != nil {
		return DefinitionRecord{}, fmt.Errorf("scan agent definition: %w", err)
	}
	if err := json.Unmarshal(raw, &record.Definition); err != nil {
		return DefinitionRecord{}, fmt.Errorf("decode agent definition: %w", err)
	}
	return record, nil
}

func appendAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	version int64,
	action string,
	actorUserID int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_audit(
		catalog_kind,event_kind,subject_id,version,action,actor_user_id,created_at)
		VALUES('agent','definition',?,?,?,?,?)`,
		id, version, action, actorUserID,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf(
			"append agent definition %q version %d audit: %w",
			id, version, err,
		)
	}
	return nil
}

func appendRolloutAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	rule RolloutRule,
	action string,
	actorUserID int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_audit(
		catalog_kind,event_kind,subject_id,version,candidate_id,candidate_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at)
		VALUES('agent','rollout',?,?,?,?,?,?,?,?,?)`,
		rule.AgentID, rule.RuleVersion, rule.AgentID, rule.CandidateVersion, rule.PercentageBPS,
		rule.RuleHash, action, actorUserID,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf(
			"append agent rollout %q version %d audit: %w",
			rule.AgentID, rule.RuleVersion, err,
		)
	}
	return nil
}

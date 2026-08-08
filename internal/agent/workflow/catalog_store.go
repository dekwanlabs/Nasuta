package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	platformstore "github.com/dekwanlabs/nasuta/internal/platform/store"
)

func (workflowStore *Store) PublishDefinitions(
	ctx context.Context,
	definitions []WorkflowDefinition,
	actorUserID int64,
) ([]DefinitionRecord, error) {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin workflow definition publication: %w", err)
	}
	defer tx.Rollback()
	records := make([]DefinitionRecord, 0, len(definitions))
	for _, definition := range definitions {
		record, created, err := publishWorkflowDefinitionTx(
			ctx, tx, definition, actorUserID,
		)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		if created {
			if err := appendWorkflowDefinitionAuditTx(
				ctx, tx, definition.ID, definition.Version, "published", actorUserID,
			); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit workflow definition publication: %w", err)
	}
	return records, nil
}

func publishWorkflowDefinitionTx(
	ctx context.Context,
	tx *sql.Tx,
	definition WorkflowDefinition,
	actorUserID int64,
) (DefinitionRecord, bool, error) {
	var existing DefinitionRecord
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT
		definition_json,content_hash,active,is_default,created_by,created_at
		FROM workflow_definitions
		WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		definition.ID, definition.Version,
	).Scan(
		&raw, &existing.ContentHash, &existing.Active, &existing.Default,
		&existing.CreatedBy, &existing.CreatedAt,
	)
	if err == nil {
		if existing.ContentHash != definition.ContentHash {
			return DefinitionRecord{}, false, fmt.Errorf(
				"workflow %q version %d is already published: %w",
				definition.ID, definition.Version, ErrConflict,
			)
		}
		if err := json.Unmarshal(raw, &existing.WorkflowDefinition); err != nil {
			return DefinitionRecord{}, false, fmt.Errorf(
				"decode workflow %q version %d: %w",
				definition.ID, definition.Version, err,
			)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DefinitionRecord{}, false, fmt.Errorf(
			"get workflow %q version %d: %w",
			definition.ID, definition.Version, err,
		)
	}
	var defaultVersion int64
	err = tx.QueryRowContext(ctx, `SELECT version
		FROM workflow_definitions
		WHERE id=? AND is_default=1 LIMIT 1 FOR UPDATE`,
		definition.ID,
	).Scan(&defaultVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DefinitionRecord{}, false, fmt.Errorf(
			"lock workflow %q default: %w", definition.ID, err,
		)
	}
	makeDefault := defaultVersion == 0 || definition.Version > defaultVersion
	if makeDefault && defaultVersion > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_definitions
			SET is_default=0 WHERE id=? AND is_default=1`, definition.ID); err != nil {
			return DefinitionRecord{}, false, fmt.Errorf(
				"clear workflow %q default: %w", definition.ID, err,
			)
		}
	}
	raw, err = json.Marshal(definition)
	if err != nil {
		return DefinitionRecord{}, false, fmt.Errorf(
			"marshal workflow %q version %d: %w",
			definition.ID, definition.Version, err,
		)
	}
	createdAt := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_definitions(
		id,version,definition_json,content_hash,active,is_default,created_by,created_at)
		VALUES(?,?,?,?,1,?,?,?)`,
		definition.ID, definition.Version, raw, definition.ContentHash,
		makeDefault, actorUserID,
		platformstore.DatabaseTime(createdAt.Format(time.RFC3339Nano)),
	); err != nil {
		return DefinitionRecord{}, false, fmt.Errorf(
			"save workflow %q version %d: %w",
			definition.ID, definition.Version, err,
		)
	}
	return DefinitionRecord{
		WorkflowDefinition: definition,
		Active:             true,
		Default:            makeDefault,
		CreatedBy:          actorUserID,
		CreatedAt:          createdAt,
	}, true, nil
}

// LoadFullCatalog is reserved for startup recovery of immutable definitions.
func (workflowStore *Store) LoadFullCatalog(
	ctx context.Context,
) ([]DefinitionRecord, error) {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		definition_json,content_hash,active,is_default,created_by,created_at
		FROM workflow_definitions ORDER BY id,version`)
	if err != nil {
		return nil, fmt.Errorf("load full workflow catalog: %w", err)
	}
	defer rows.Close()
	records := make([]DefinitionRecord, 0)
	for rows.Next() {
		record, err := scanWorkflowDefinitionRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate full workflow catalog: %w", err)
	}
	return records, nil
}

func (workflowStore *Store) LoadDefinitionRollouts(
	ctx context.Context,
) ([]RolloutRule, error) {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		subject_id,rule_version,candidate_version,percentage_bps,salt,rule_hash,
		active,created_by,created_at
		FROM catalog_rollouts
		WHERE catalog_kind='workflow'
		ORDER BY subject_id`)
	if err != nil {
		return nil, fmt.Errorf("load workflow rollouts: %w", err)
	}
	defer rows.Close()
	rules := make([]RolloutRule, 0)
	for rows.Next() {
		var rule RolloutRule
		if err := rows.Scan(
			&rule.WorkflowID, &rule.RuleVersion, &rule.CandidateVersion,
			&rule.PercentageBPS, &rule.Salt, &rule.RuleHash, &rule.Active,
			&rule.CreatedBy, &rule.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workflow rollout: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow rollouts: %w", err)
	}
	return rules, nil
}

func (workflowStore *Store) ListDefinitions(
	ctx context.Context,
	cursor DefinitionCursor,
	limit int,
) ([]DefinitionRecord, error) {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		definition_json,content_hash,active,is_default,created_by,created_at
		FROM workflow_definitions
		WHERE id>? OR (id=? AND version>?)
		ORDER BY id,version LIMIT ?`,
		cursor.ID, cursor.ID, cursor.Version, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list workflow definitions: %w", err)
	}
	defer rows.Close()
	records := make([]DefinitionRecord, 0, limit)
	for rows.Next() {
		record, err := scanWorkflowDefinitionRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow definitions: %w", err)
	}
	return records, nil
}

func (workflowStore *Store) SetDefinitionDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
) error {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow default update: %w", err)
	}
	defer tx.Rollback()
	var active, current bool
	if err := tx.QueryRowContext(ctx, `SELECT active,is_default
		FROM workflow_definitions WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		id, version,
	).Scan(&active, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf(
				"workflow %q version %d not found: %w",
				id, version, ErrNotFound,
			)
		}
		return fmt.Errorf("lock workflow %q version %d: %w", id, version, err)
	}
	if !active {
		return fmt.Errorf(
			"workflow %q version %d is disabled: %w",
			id, version, ErrConflict,
		)
	}
	if !current {
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_definitions
			SET is_default=0 WHERE id=? AND is_default=1`, id); err != nil {
			return fmt.Errorf("clear workflow %q default: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_definitions
			SET is_default=1 WHERE id=? AND version=?`, id, version); err != nil {
			return fmt.Errorf(
				"set workflow %q version %d default: %w",
				id, version, err,
			)
		}
		if err := appendWorkflowDefinitionAuditTx(
			ctx, tx, id, version, "default_set", actorUserID,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow default update: %w", err)
	}
	return nil
}

func (workflowStore *Store) SetDefinitionActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
) error {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow status update: %w", err)
	}
	defer tx.Rollback()
	var current, isDefault bool
	if err := tx.QueryRowContext(ctx, `SELECT active,is_default
		FROM workflow_definitions WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		id, version,
	).Scan(&current, &isDefault); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf(
				"workflow %q version %d not found: %w",
				id, version, ErrNotFound,
			)
		}
		return fmt.Errorf("lock workflow %q version %d: %w", id, version, err)
	}
	if !active && isDefault {
		return fmt.Errorf(
			"workflow %q version %d is the default: %w",
			id, version, ErrConflict,
		)
	}
	if current != active {
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_definitions
			SET active=? WHERE id=? AND version=?`, active, id, version); err != nil {
			return fmt.Errorf(
				"set workflow %q version %d status: %w",
				id, version, err,
			)
		}
		action := "disabled"
		if active {
			action = "enabled"
		}
		if err := appendWorkflowDefinitionAuditTx(
			ctx, tx, id, version, action, actorUserID,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow status update: %w", err)
	}
	return nil
}

func (workflowStore *Store) SetDefinitionRollout(
	ctx context.Context,
	rule RolloutRule,
	actorUserID int64,
) error {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow rollout update: %w", err)
	}
	defer tx.Rollback()
	var existingVersion int64
	err = tx.QueryRowContext(ctx, `SELECT rule_version
		FROM catalog_rollouts
		WHERE catalog_kind='workflow' AND subject_id=? LIMIT 1 FOR UPDATE`,
		rule.WorkflowID,
	).Scan(&existingVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock workflow rollout %q: %w", rule.WorkflowID, err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if rule.RuleVersion != 1 {
			return fmt.Errorf(
				"workflow rollout %q expected initial rule version 1, got %d: %w",
				rule.WorkflowID, rule.RuleVersion, ErrConflict,
			)
		}
	} else if rule.RuleVersion != existingVersion+1 {
		return fmt.Errorf(
			"workflow rollout %q expected rule version %d, got %d: %w",
			rule.WorkflowID, existingVersion+1, rule.RuleVersion, ErrConflict,
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
			VALUES('workflow',?,?,?,?,?,?,?,?,?,?)`,
			rule.WorkflowID, rule.RuleVersion, rule.WorkflowID, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			actorUserID,
			platformstore.DatabaseTime(createdAt.Format(time.RFC3339Nano)),
		)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE catalog_rollouts SET
			rule_version=?,candidate_id=?,candidate_version=?,percentage_bps=?,salt=?,rule_hash=?,
			active=?,created_by=?,created_at=?
			WHERE catalog_kind='workflow' AND subject_id=?`,
			rule.RuleVersion, rule.WorkflowID, rule.CandidateVersion, rule.PercentageBPS,
			rule.Salt, rule.RuleHash, rule.Active, actorUserID,
			platformstore.DatabaseTime(createdAt.Format(time.RFC3339Nano)),
			rule.WorkflowID,
		)
	}
	if err != nil {
		return fmt.Errorf("save workflow rollout %q: %w", rule.WorkflowID, err)
	}
	action := "rollout_disabled"
	if rule.Active {
		action = "rollout_enabled"
	}
	if err := appendWorkflowDefinitionRolloutAuditTx(
		ctx, tx, rule, action, actorUserID,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow rollout update: %w", err)
	}
	return nil
}

func (workflowStore *Store) ListDefinitionAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]DefinitionAuditEvent, error) {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		seq,subject_id,version,action,actor_user_id,created_at
		FROM catalog_audit
		WHERE catalog_kind='workflow' AND event_kind='definition'
			AND subject_id=? AND seq>?
		ORDER BY seq LIMIT ?`, id, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list workflow %q audit: %w", id, err)
	}
	defer rows.Close()
	events := make([]DefinitionAuditEvent, 0, limit)
	for rows.Next() {
		var event DefinitionAuditEvent
		if err := rows.Scan(
			&event.Seq, &event.DefinitionID, &event.Version, &event.Action,
			&event.ActorUserID, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workflow %q audit: %w", id, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow %q audit: %w", id, err)
	}
	return events, nil
}

func (workflowStore *Store) ListDefinitionRolloutAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]RolloutAuditEvent, error) {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		seq,subject_id,version,candidate_version,percentage_bps,rule_hash,
		action,actor_user_id,created_at
		FROM catalog_audit
		WHERE catalog_kind='workflow' AND event_kind='rollout'
			AND subject_id=? AND seq>?
		ORDER BY seq LIMIT ?`, id, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list workflow rollout %q audit: %w", id, err)
	}
	defer rows.Close()
	events := make([]RolloutAuditEvent, 0, limit)
	for rows.Next() {
		var event RolloutAuditEvent
		if err := rows.Scan(
			&event.Seq, &event.WorkflowID, &event.RuleVersion,
			&event.CandidateVersion, &event.PercentageBPS, &event.RuleHash,
			&event.Action, &event.ActorUserID, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workflow rollout %q audit: %w", id, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow rollout %q audit: %w", id, err)
	}
	return events, nil
}

func scanWorkflowDefinitionRecord(
	row interface{ Scan(...any) error },
) (DefinitionRecord, error) {
	var record DefinitionRecord
	var raw []byte
	if err := row.Scan(
		&raw, &record.ContentHash, &record.Active, &record.Default,
		&record.CreatedBy, &record.CreatedAt,
	); err != nil {
		return DefinitionRecord{}, fmt.Errorf("scan workflow definition: %w", err)
	}
	if err := json.Unmarshal(raw, &record.WorkflowDefinition); err != nil {
		return DefinitionRecord{}, fmt.Errorf("decode workflow definition: %w", err)
	}
	return record, nil
}

func appendWorkflowDefinitionAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	version int64,
	action string,
	actorUserID int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_audit(
		catalog_kind,event_kind,subject_id,version,action,actor_user_id,created_at)
		VALUES('workflow','definition',?,?,?,?,?)`,
		id, version, action, actorUserID,
		platformstore.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf(
			"append workflow %q version %d audit: %w",
			id, version, err,
		)
	}
	return nil
}

func appendWorkflowDefinitionRolloutAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	rule RolloutRule,
	action string,
	actorUserID int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_audit(
		catalog_kind,event_kind,subject_id,version,candidate_id,candidate_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at)
		VALUES('workflow','rollout',?,?,?,?,?,?,?,?,?)`,
		rule.WorkflowID, rule.RuleVersion, rule.WorkflowID, rule.CandidateVersion,
		rule.PercentageBPS, rule.RuleHash, action, actorUserID,
		platformstore.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf(
			"append workflow rollout %q version %d audit: %w",
			rule.WorkflowID, rule.RuleVersion, err,
		)
	}
	return nil
}

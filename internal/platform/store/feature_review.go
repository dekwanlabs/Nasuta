package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

const (
	maxReviewAssignmentPage   = 16
	maxReviewFindingPage      = 100
	maxReviewEvidence         = 20
	maxReviewEventPage        = 500
	maxReviewAdjudicationPage = 1600
	maxReviewResolutionPage   = 100
	maxReviewPolicyPage       = 100
	maxReviewRoundPage        = 100
)

func (store *FeatureDeliveryStore) SaveReviewPolicies(
	ctx context.Context,
	policies []featuredelivery.ReviewPolicy,
) error {
	prepared := make([]featuredelivery.ReviewPolicy, 0, len(policies))
	seen := make(map[string]struct{}, len(policies))
	for _, policy := range policies {
		item, err := featuredelivery.PrepareReviewPolicy(policy)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%s\x00%d", item.ID, item.Version)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate review policy %q version %d: %w", item.ID, item.Version, featuredelivery.ErrInvalid)
		}
		seen[key] = struct{}{}
		prepared = append(prepared, item)
	}
	if len(prepared) == 0 {
		return nil
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review policy publication: %w", err)
	}
	defer tx.Rollback()
	for _, policy := range prepared {
		raw, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("marshal review policy %q: %w", policy.ID, err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO review_policies(id,version,subject_kind,definition_json,content_hash,created_at)
			 VALUES(?,?,?,?,?,?)`,
			policy.ID, policy.Version, policy.SubjectKind, raw, policy.ContentHash, policy.CreatedAt,
		)
		if err == nil {
			continue
		}
		if !duplicateKey(err) {
			return fmt.Errorf("save review policy %q version %d: %w", policy.ID, policy.Version, err)
		}
		var existingHash string
		readErr := tx.QueryRowContext(ctx,
			`SELECT content_hash FROM review_policies WHERE id=? AND version=? LIMIT 1`,
			policy.ID, policy.Version,
		).Scan(&existingHash)
		if readErr != nil {
			return fmt.Errorf("read existing review policy %q version %d: %w", policy.ID, policy.Version, readErr)
		}
		if existingHash != policy.ContentHash {
			return featuredelivery.ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review policy publication: %w", err)
	}
	return nil
}

// PublishReviewPolicies records actor and rollout metadata atomically.
func (store *FeatureDeliveryStore) PublishReviewPolicies(
	ctx context.Context,
	policies []featuredelivery.ReviewPolicy,
	actorUserID int64,
) error {
	prepared := make([]featuredelivery.ReviewPolicy, 0, len(policies))
	seen := make(map[string]struct{}, len(policies))
	for _, policy := range policies {
		item, err := featuredelivery.PrepareReviewPolicy(policy)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%s\x00%d", item.ID, item.Version)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate review policy %q version %d: %w", item.ID, item.Version, featuredelivery.ErrInvalid)
		}
		seen[key] = struct{}{}
		prepared = append(prepared, item)
	}
	if len(prepared) == 0 {
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review policy control publication: %w", err)
	}
	defer tx.Rollback()
	for _, policy := range prepared {
		var existingHash string
		var existingRaw []byte
		err := tx.QueryRowContext(ctx, `SELECT definition_json,content_hash
			FROM review_policies WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
			policy.ID, policy.Version,
		).Scan(&existingRaw, &existingHash)
		if err == nil {
			if existingHash != policy.ContentHash {
				return featuredelivery.ErrConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock review policy %q version %d: %w", policy.ID, policy.Version, err)
		}
		var defaultID string
		defaultErr := tx.QueryRowContext(ctx, `SELECT id FROM review_policies
			WHERE subject_kind=? AND is_default=1 LIMIT 1 FOR UPDATE`,
			policy.SubjectKind,
		).Scan(&defaultID)
		if defaultErr != nil && !errors.Is(defaultErr, sql.ErrNoRows) {
			return fmt.Errorf("lock default review policy for %q: %w", policy.SubjectKind, defaultErr)
		}
		raw, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("marshal review policy %q: %w", policy.ID, err)
		}
		makeDefault := errors.Is(defaultErr, sql.ErrNoRows)
		_, err = tx.ExecContext(ctx, `INSERT INTO review_policies(
			id,version,subject_kind,definition_json,content_hash,active,is_default,created_by,created_at)
			VALUES(?,?,?,?,?,1,?,?,?)`,
			policy.ID, policy.Version, policy.SubjectKind, raw, policy.ContentHash,
			makeDefault, actorUserID, policy.CreatedAt,
		)
		if err != nil {
			if duplicateKey(err) {
				return featuredelivery.ErrConflict
			}
			return fmt.Errorf("save review policy %q version %d: %w", policy.ID, policy.Version, err)
		}
		if err := appendReviewPolicyAuditTx(
			ctx, tx, policy.ID, policy.Version, "published", actorUserID,
		); err != nil {
			return err
		}
		if makeDefault {
			if err := appendReviewPolicyAuditTx(
				ctx, tx, policy.ID, policy.Version, "default_set", actorUserID,
			); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review policy control publication: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) ListReviewPolicyRecords(
	ctx context.Context,
	cursor featuredelivery.ReviewPolicyCursor,
	limit int,
) ([]featuredelivery.ReviewPolicyRecord, error) {
	limit = boundedLimit(limit, 20, maxReviewPolicyPage)
	query := `SELECT definition_json,active,is_default,created_by,created_at
		FROM review_policies`
	args := make([]any, 0, 4)
	if cursor.ID != "" {
		query += ` WHERE id>? OR (id=? AND version>?)`
		args = append(args, cursor.ID, cursor.ID, cursor.Version)
	}
	query += ` ORDER BY id,version LIMIT ?`
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list review policy records: %w", err)
	}
	defer rows.Close()
	records := make([]featuredelivery.ReviewPolicyRecord, 0, limit)
	for rows.Next() {
		var raw []byte
		var record featuredelivery.ReviewPolicyRecord
		var createdAt sql.NullTime
		if err := rows.Scan(
			&raw, &record.Active, &record.Default, &record.CreatedBy, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan review policy record: %w", err)
		}
		if err := json.Unmarshal(raw, &record.ReviewPolicy); err != nil {
			return nil, fmt.Errorf("decode review policy record: %w", err)
		}
		if record.CreatedAt.IsZero() && createdAt.Valid {
			record.CreatedAt = createdAt.Time
		}
		prepared, err := featuredelivery.PrepareReviewPolicy(record.ReviewPolicy)
		if err != nil {
			return nil, fmt.Errorf("validate stored review policy %q@%d: %w", record.ID, record.Version, err)
		}
		record.ReviewPolicy = prepared
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review policy records: %w", err)
	}
	return records, nil
}

func (store *FeatureDeliveryStore) GetReviewPolicyRecord(
	ctx context.Context,
	id string,
	version int64,
) (featuredelivery.ReviewPolicyRecord, error) {
	var (
		raw       []byte
		record    featuredelivery.ReviewPolicyRecord
		createdAt sql.NullTime
	)
	err := store.db.QueryRowContext(ctx, `SELECT
		definition_json,active,is_default,created_by,created_at
		FROM review_policies WHERE id=? AND version=? LIMIT 1`,
		id, version,
	).Scan(
		&raw, &record.Active, &record.Default, &record.CreatedBy, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ReviewPolicyRecord{}, featuredelivery.ErrNotFound
	}
	if err != nil {
		return featuredelivery.ReviewPolicyRecord{}, fmt.Errorf(
			"get review policy record %q@%d: %w", id, version, err,
		)
	}
	if err := json.Unmarshal(raw, &record.ReviewPolicy); err != nil {
		return featuredelivery.ReviewPolicyRecord{}, fmt.Errorf(
			"decode review policy record %q@%d: %w", id, version, err,
		)
	}
	if record.CreatedAt.IsZero() && createdAt.Valid {
		record.CreatedAt = createdAt.Time
	}
	prepared, err := featuredelivery.PrepareReviewPolicy(record.ReviewPolicy)
	if err != nil {
		return featuredelivery.ReviewPolicyRecord{}, fmt.Errorf(
			"validate stored review policy %q@%d: %w", id, version, err,
		)
	}
	record.ReviewPolicy = prepared
	return record, nil
}

func (store *FeatureDeliveryStore) GetDefaultReviewPolicy(
	ctx context.Context,
	kind featuredelivery.SubjectKind,
) (featuredelivery.ReviewPolicyRef, error) {
	var ref featuredelivery.ReviewPolicyRef
	err := store.db.QueryRowContext(ctx, `SELECT id,version FROM review_policies
		WHERE subject_kind=? AND active=1 AND is_default=1 LIMIT 1`, kind).
		Scan(&ref.ID, &ref.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ReviewPolicyRef{}, featuredelivery.ErrNotFound
	}
	if err != nil {
		return featuredelivery.ReviewPolicyRef{}, fmt.Errorf("get default review policy for %q: %w", kind, err)
	}
	return ref, nil
}

func (store *FeatureDeliveryStore) EnsureReviewPolicyDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review policy default ensure: %w", err)
	}
	defer tx.Rollback()
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT subject_kind FROM review_policies
		WHERE id=? AND version=? LIMIT 1 FOR UPDATE`, id, version).Scan(&kind); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock review policy %q@%d: %w", id, version, err)
	}
	var currentID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM review_policies
		WHERE subject_kind=? AND active=1 AND is_default=1 LIMIT 1 FOR UPDATE`, kind).
		Scan(&currentID)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read default review policy for %q: %w", kind, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_policies
		SET is_default=1 WHERE id=? AND version=? AND active=1`, id, version); err != nil {
		return fmt.Errorf("set review policy %q@%d default: %w", id, version, err)
	}
	if err := appendReviewPolicyAuditTx(ctx, tx, id, version, "default_set", actorUserID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review policy default ensure: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) SetReviewPolicyDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review policy default update: %w", err)
	}
	defer tx.Rollback()
	var kind string
	var active, current bool
	if err := tx.QueryRowContext(ctx, `SELECT subject_kind,active,is_default
		FROM review_policies WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		id, version).Scan(&kind, &active, &current); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock review policy %q@%d: %w", id, version, err)
	}
	if !active {
		return fmt.Errorf("review policy %q@%d is disabled: %w", id, version, featuredelivery.ErrConflict)
	}
	if !current {
		if _, err := tx.ExecContext(ctx, `UPDATE review_policies
			SET is_default=0 WHERE subject_kind=? AND is_default=1`, kind); err != nil {
			return fmt.Errorf("clear default review policy for %q: %w", kind, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_policies
			SET is_default=1 WHERE id=? AND version=?`, id, version); err != nil {
			return fmt.Errorf("set review policy %q@%d default: %w", id, version, err)
		}
		if err := appendReviewPolicyAuditTx(ctx, tx, id, version, "default_set", actorUserID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review policy default update: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) SetReviewPolicyActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review policy status update: %w", err)
	}
	defer tx.Rollback()
	var current, isDefault bool
	if err := tx.QueryRowContext(ctx, `SELECT active,is_default
		FROM review_policies WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		id, version).Scan(&current, &isDefault); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock review policy %q@%d: %w", id, version, err)
	}
	if !active && isDefault {
		return fmt.Errorf("review policy %q@%d is the default: %w", id, version, featuredelivery.ErrConflict)
	}
	if current != active {
		if _, err := tx.ExecContext(ctx, `UPDATE review_policies
			SET active=? WHERE id=? AND version=?`, active, id, version); err != nil {
			return fmt.Errorf("set review policy %q@%d status: %w", id, version, err)
		}
		action := "disabled"
		if active {
			action = "enabled"
		}
		if err := appendReviewPolicyAuditTx(ctx, tx, id, version, action, actorUserID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review policy status update: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) ListReviewPolicyAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]featuredelivery.ReviewPolicyAuditEvent, error) {
	limit = boundedLimit(limit, 20, maxReviewPolicyPage)
	rows, err := store.db.QueryContext(ctx, `SELECT seq,policy_id,version,action,actor_user_id,created_at
		FROM review_policy_audit WHERE policy_id=? AND seq>? ORDER BY seq LIMIT ?`,
		id, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list review policy %q audit: %w", id, err)
	}
	defer rows.Close()
	events := make([]featuredelivery.ReviewPolicyAuditEvent, 0, limit)
	for rows.Next() {
		var event featuredelivery.ReviewPolicyAuditEvent
		if err := rows.Scan(
			&event.Seq, &event.PolicyID, &event.Version, &event.Action,
			&event.ActorUserID, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan review policy %q audit: %w", id, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review policy %q audit: %w", id, err)
	}
	return events, nil
}

func (store *FeatureDeliveryStore) GetReviewPolicyRollout(
	ctx context.Context,
	kind featuredelivery.SubjectKind,
) (featuredelivery.ReviewPolicyRolloutRule, bool, error) {
	var rule featuredelivery.ReviewPolicyRolloutRule
	err := store.db.QueryRowContext(ctx, `SELECT
		subject_kind,rule_version,candidate_policy_id,candidate_policy_version,
		percentage_bps,salt,rule_hash,active,created_by,created_at
		FROM review_policy_rollouts WHERE subject_kind=? LIMIT 1`,
		kind,
	).Scan(
		&rule.SubjectKind, &rule.RuleVersion, &rule.CandidatePolicyID,
		&rule.CandidatePolicyVersion, &rule.PercentageBPS, &rule.Salt,
		&rule.RuleHash, &rule.Active, &rule.CreatedBy, &rule.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ReviewPolicyRolloutRule{}, false, nil
	}
	if err != nil {
		return featuredelivery.ReviewPolicyRolloutRule{}, false, fmt.Errorf(
			"get review policy rollout for %q: %w", kind, err,
		)
	}
	return rule, true, nil
}

func (store *FeatureDeliveryStore) SetReviewPolicyRollout(
	ctx context.Context,
	rule featuredelivery.ReviewPolicyRolloutRule,
	actorUserID int64,
) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review policy rollout update: %w", err)
	}
	defer tx.Rollback()
	var existingVersion int64
	err = tx.QueryRowContext(ctx, `SELECT rule_version
		FROM review_policy_rollouts
		WHERE subject_kind=? LIMIT 1 FOR UPDATE`,
		rule.SubjectKind,
	).Scan(&existingVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"lock review policy rollout for %q: %w", rule.SubjectKind, err,
		)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if rule.RuleVersion != 1 {
			return fmt.Errorf(
				"review policy rollout for %q expected initial rule version 1, got %d: %w",
				rule.SubjectKind, rule.RuleVersion, featuredelivery.ErrConflict,
			)
		}
	} else if rule.RuleVersion != existingVersion+1 {
		return fmt.Errorf(
			"review policy rollout for %q expected rule version %d, got %d: %w",
			rule.SubjectKind,
			existingVersion+1,
			rule.RuleVersion,
			featuredelivery.ErrConflict,
		)
	}
	createdAt := rule.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO review_policy_rollouts(
			subject_kind,rule_version,candidate_policy_id,candidate_policy_version,
			percentage_bps,salt,rule_hash,active,created_by,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			rule.SubjectKind, rule.RuleVersion, rule.CandidatePolicyID,
			rule.CandidatePolicyVersion, rule.PercentageBPS, rule.Salt,
			rule.RuleHash, rule.Active, actorUserID, createdAt,
		)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE review_policy_rollouts SET
			rule_version=?,candidate_policy_id=?,candidate_policy_version=?,
			percentage_bps=?,salt=?,rule_hash=?,active=?,created_by=?,created_at=?
			WHERE subject_kind=?`,
			rule.RuleVersion, rule.CandidatePolicyID,
			rule.CandidatePolicyVersion, rule.PercentageBPS, rule.Salt,
			rule.RuleHash, rule.Active, actorUserID, createdAt, rule.SubjectKind,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"save review policy rollout for %q: %w", rule.SubjectKind, err,
		)
	}
	action := "rollout_disabled"
	if rule.Active {
		action = "rollout_enabled"
	}
	if err := appendReviewPolicyRolloutAuditTx(
		ctx, tx, rule, action, actorUserID,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf(
			"commit review policy rollout for %q: %w", rule.SubjectKind, err,
		)
	}
	return nil
}

func (store *FeatureDeliveryStore) ListReviewPolicyRolloutAudit(
	ctx context.Context,
	kind featuredelivery.SubjectKind,
	afterSeq int64,
	limit int,
) ([]featuredelivery.ReviewPolicyRolloutAuditEvent, error) {
	limit = boundedLimit(limit, 20, maxReviewPolicyPage)
	rows, err := store.db.QueryContext(ctx, `SELECT
		seq,subject_kind,rule_version,candidate_policy_id,candidate_policy_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at
		FROM review_policy_rollout_audit
		WHERE subject_kind=? AND seq>? ORDER BY seq LIMIT ?`,
		kind, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"list review policy rollout audit for %q: %w", kind, err,
		)
	}
	defer rows.Close()
	events := make([]featuredelivery.ReviewPolicyRolloutAuditEvent, 0, limit)
	for rows.Next() {
		var event featuredelivery.ReviewPolicyRolloutAuditEvent
		if err := rows.Scan(
			&event.Seq, &event.SubjectKind, &event.RuleVersion,
			&event.CandidatePolicyID, &event.CandidatePolicyVersion,
			&event.PercentageBPS, &event.RuleHash, &event.Action,
			&event.ActorUserID, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf(
				"scan review policy rollout audit for %q: %w", kind, err,
			)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterate review policy rollout audit for %q: %w", kind, err,
		)
	}
	return events, nil
}

func appendReviewPolicyAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	version int64,
	action string,
	actorUserID int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_policy_audit(
		policy_id,version,action,actor_user_id,created_at) VALUES(?,?,?,?,?)`,
		id, version, action, actorUserID, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("append review policy %q@%d audit: %w", id, version, err)
	}
	return nil
}

func appendReviewPolicyRolloutAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	rule featuredelivery.ReviewPolicyRolloutRule,
	action string,
	actorUserID int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_policy_rollout_audit(
		subject_kind,rule_version,candidate_policy_id,candidate_policy_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		rule.SubjectKind, rule.RuleVersion, rule.CandidatePolicyID,
		rule.CandidatePolicyVersion, rule.PercentageBPS, rule.RuleHash,
		action, actorUserID, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf(
			"append review policy rollout audit for %q version %d: %w",
			rule.SubjectKind, rule.RuleVersion, err,
		)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetReviewPolicy(ctx context.Context, id string, version int64) (*featuredelivery.ReviewPolicy, error) {
	var raw []byte
	err := store.db.QueryRowContext(ctx,
		`SELECT definition_json FROM review_policies WHERE id=? AND version=? LIMIT 1`,
		id, version,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review policy %q version %d: %w", id, version, err)
	}
	var policy featuredelivery.ReviewPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("decode review policy %q version %d: %w", id, version, err)
	}
	prepared, err := featuredelivery.PrepareReviewPolicy(policy)
	if err != nil {
		return nil, fmt.Errorf("validate stored review policy %q version %d: %w", id, version, err)
	}
	return &prepared, nil
}

func (store *FeatureDeliveryStore) CreateReviewRound(
	ctx context.Context,
	round featuredelivery.ReviewRound,
	assignments []featuredelivery.ReviewAssignment,
) error {
	return store.createReviewRound(ctx, round, assignments, nil, nil)
}

func (store *FeatureDeliveryStore) CreateReviewRoundWithReuses(
	ctx context.Context,
	round featuredelivery.ReviewRound,
	assignments []featuredelivery.ReviewAssignment,
	reports []featuredelivery.ReviewReport,
	reuses []featuredelivery.ReviewReportReuse,
) error {
	if len(reports) == 0 || len(reports) != len(reuses) {
		return featuredelivery.ErrInvalid
	}
	return store.createReviewRound(ctx, round, assignments, reports, reuses)
}

func (store *FeatureDeliveryStore) createReviewRound(
	ctx context.Context,
	round featuredelivery.ReviewRound,
	assignments []featuredelivery.ReviewAssignment,
	reports []featuredelivery.ReviewReport,
	reuses []featuredelivery.ReviewReportReuse,
) error {
	if len(assignments) < 2 || len(assignments) > maxReviewAssignmentPage {
		return featuredelivery.ErrInvalid
	}
	assignmentByID := make(map[string]featuredelivery.ReviewAssignment, len(assignments))
	reusedAssignments := make(map[string]struct{}, len(reports))
	for _, assignment := range assignments {
		if assignment.RoundID != round.ID {
			return featuredelivery.ErrConflict
		}
		switch assignment.Status {
		case featuredelivery.AssignmentQueued:
		case featuredelivery.AssignmentReused:
			reusedAssignments[assignment.ID] = struct{}{}
		default:
			return featuredelivery.ErrConflict
		}
		assignmentByID[assignment.ID] = assignment
	}
	reportByID := make(map[string]featuredelivery.ReviewReport, len(reports))
	for _, report := range reports {
		assignment, ok := assignmentByID[report.AssignmentID]
		if !ok || assignment.Status != featuredelivery.AssignmentReused ||
			report.RoundID != round.ID ||
			report.ReviewerID != assignment.ReviewerID ||
			report.SubjectHash != round.Subject.ContentHash ||
			report.Reuse == nil {
			return featuredelivery.ErrConflict
		}
		reportByID[report.ID] = report
	}
	if len(reusedAssignments) != len(reports) {
		return featuredelivery.ErrConflict
	}
	for _, reuse := range reuses {
		report, ok := reportByID[reuse.ReportID]
		assignment := assignmentByID[reuse.AssignmentID]
		if !ok || reuse.RoundID != round.ID ||
			reuse.AssignmentID != report.AssignmentID ||
			reuse.ReviewerID != report.ReviewerID ||
			reuse.SubjectHash != round.Subject.ContentHash ||
			reuse.PolicyHash != round.PolicyHash ||
			reuse.DefinitionHash != assignment.DefinitionHash ||
			reuse.ReportHash != report.ReportHash ||
			report.Reuse.SourceReportID != reuse.SourceReportID ||
			report.Reuse.SourceRoundID != reuse.SourceRoundID ||
			report.Reuse.SourceAssignmentID != reuse.SourceAssignmentID ||
			report.Reuse.Reason != reuse.Reason {
			return featuredelivery.ErrConflict
		}
	}
	subjectJSON, err := json.Marshal(round.Subject)
	if err != nil {
		return fmt.Errorf("marshal review subject: %w", err)
	}
	riskFactsJSON, err := json.Marshal(round.RiskFacts)
	if err != nil {
		return fmt.Errorf("marshal review risk facts: %w", err)
	}
	policySelectionJSON, err := json.Marshal(round.PolicySelection)
	if err != nil {
		return fmt.Errorf("marshal review policy selection: %w", err)
	}
	reviewersJSON, err := json.Marshal(round.Reviewers)
	if err != nil {
		return fmt.Errorf("marshal review panel: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review round creation: %w", err)
	}
	defer tx.Rollback()
	var policyHash string
	if err := tx.QueryRowContext(ctx,
		`SELECT content_hash FROM review_policies WHERE id=? AND version=? LIMIT 1 FOR UPDATE`,
		round.PolicyID, round.PolicyVersion,
	).Scan(&policyHash); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock review policy: %w", err)
	}
	if policyHash != round.PolicyHash {
		return featuredelivery.ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_rounds(
			id,workflow_run_id,subject_kind,subject_id,subject_version,subject_hash,subject_json,
			policy_id,policy_version,policy_hash,policy_selection_json,risk_facts_json,risk_hash,
			selection_rule_version,selected_reviewers_json,panel_hash,
			status,created_by,created_at,completed_at
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		round.ID, round.WorkflowRunID, round.Subject.Kind, round.Subject.ID, round.Subject.Version,
		round.Subject.ContentHash, subjectJSON, round.PolicyID, round.PolicyVersion,
		round.PolicyHash, policySelectionJSON, riskFactsJSON, round.RiskHash, round.RuleVersion,
		reviewersJSON, round.PanelHash, round.Status, round.CreatedBy,
		round.CreatedAt, round.CompletedAt,
	); err != nil {
		if duplicateKey(err) {
			return featuredelivery.ErrConflict
		}
		return fmt.Errorf("insert review round %q: %w", round.ID, err)
	}
	for _, assignment := range assignments {
		categoriesJSON, err := json.Marshal(assignment.Categories)
		if err != nil {
			return fmt.Errorf("marshal reviewer categories: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO review_assignments(
				id,round_id,reviewer_id,agent_id,agent_version,definition_hash,categories_json,
				required_review,status,attempt,agent_run_id,error_code,created_at,started_at,completed_at
			 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			assignment.ID, assignment.RoundID, assignment.ReviewerID,
			assignment.Agent.ID, assignment.Agent.Version, assignment.DefinitionHash,
			categoriesJSON, assignment.Required, assignment.Status, assignment.Attempt,
			assignment.AgentRunID, assignment.ErrorCode, assignment.CreatedAt,
			assignment.StartedAt, assignment.CompletedAt,
		); err != nil {
			if duplicateKey(err) {
				return featuredelivery.ErrConflict
			}
			return fmt.Errorf("insert review assignment %q: %w", assignment.ID, err)
		}
	}
	for _, report := range reports {
		if err := insertReviewReport(ctx, tx, report); err != nil {
			return err
		}
	}
	for _, reuse := range reuses {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO review_report_reuses(
				id,round_id,assignment_id,report_id,reviewer_id,
				source_round_id,source_assignment_id,source_report_id,
				subject_hash,policy_hash,definition_hash,report_hash,
				reason,actor_id,created_at
			 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			reuse.ID, reuse.RoundID, reuse.AssignmentID, reuse.ReportID,
			reuse.ReviewerID, reuse.SourceRoundID, reuse.SourceAssignmentID,
			reuse.SourceReportID, reuse.SubjectHash, reuse.PolicyHash,
			reuse.DefinitionHash, reuse.ReportHash, reuse.Reason,
			reuse.ActorID, reuse.CreatedAt,
		); err != nil {
			if duplicateKey(err) {
				return featuredelivery.ErrConflict
			}
			return fmt.Errorf("insert review report reuse %q: %w", reuse.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review round %q: %w", round.ID, err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetReviewRound(ctx context.Context, id string) (*featuredelivery.ReviewRound, error) {
	round, err := scanReviewRound(store.db.QueryRowContext(ctx,
		`SELECT id,workflow_run_id,subject_json,policy_id,policy_version,policy_hash,
			policy_selection_json,risk_facts_json,risk_hash,selection_rule_version,
			selected_reviewers_json,panel_hash,status,created_by,created_at,completed_at
		 FROM review_rounds WHERE id=? LIMIT 1`,
		id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review round %q: %w", id, err)
	}
	return &round, nil
}

func (store *FeatureDeliveryStore) ListReviewRoundSummaries(
	ctx context.Context,
	filter featuredelivery.ReviewRoundFilter,
	cursor featuredelivery.ReviewRoundCursor,
	limit int,
	userID int64,
	admin bool,
) ([]featuredelivery.ReviewRoundSummary, bool, error) {
	limit = boundedLimit(limit, 20, maxReviewRoundPage)
	query := `SELECT r.id,r.workflow_run_id,f.id,r.subject_kind,r.subject_id,
		r.subject_version,r.subject_hash,r.policy_id,r.policy_version,r.policy_hash,
		r.risk_hash,r.selection_rule_version,r.panel_hash,
		JSON_LENGTH(r.selected_reviewers_json),r.status,r.created_by,r.created_at,
		r.completed_at
		FROM review_rounds r
		LEFT JOIN feature_artifacts a ON a.id=r.subject_id
			AND r.subject_kind IN (
				'requirement_artifact','requirement_analysis_artifact',
				'technical_proposal_artifact','system_design_artifact',
				'implementation_plan_artifact'
			)
		LEFT JOIN feature_implementation_runs i ON i.id=r.subject_id
			AND r.subject_kind IN ('change_set','validation_bundle','delivery_bundle')
		JOIN feature_requests f ON f.id=COALESCE(a.request_id,i.request_id)
		WHERE 1=1`
	args := make([]any, 0, 12)
	if !admin {
		query += ` AND f.created_by=?`
		args = append(args, userID)
	}
	if filter.FeatureID != "" {
		query += ` AND f.id=?`
		args = append(args, filter.FeatureID)
	}
	if filter.SubjectKind != "" {
		query += ` AND r.subject_kind=?`
		args = append(args, filter.SubjectKind)
	}
	if filter.SubjectID != "" {
		query += ` AND r.subject_id=?`
		args = append(args, filter.SubjectID)
	}
	if filter.Status != "" {
		query += ` AND r.status=?`
		args = append(args, filter.Status)
	}
	if !cursor.CreatedAt.IsZero() {
		query += ` AND (r.created_at<? OR (r.created_at=? AND r.id<?))`
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	query += ` ORDER BY r.created_at DESC,r.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list review round summaries: %w", err)
	}
	defer rows.Close()
	summaries := make([]featuredelivery.ReviewRoundSummary, 0, limit+1)
	for rows.Next() {
		var summary featuredelivery.ReviewRoundSummary
		var completed sql.NullTime
		if err := rows.Scan(
			&summary.ID, &summary.WorkflowRunID, &summary.FeatureID,
			&summary.SubjectKind, &summary.SubjectID, &summary.SubjectVersion,
			&summary.SubjectHash, &summary.PolicyID,
			&summary.PolicyVersion, &summary.PolicyHash, &summary.RiskHash,
			&summary.RuleVersion, &summary.PanelHash, &summary.ReviewerCount,
			&summary.Status, &summary.CreatedBy, &summary.CreatedAt, &completed,
		); err != nil {
			return nil, false, fmt.Errorf("scan review round summary: %w", err)
		}
		summary.CompletedAt = nullableTime(completed)
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate review round summaries: %w", err)
	}
	hasMore := len(summaries) > limit
	if hasMore {
		summaries = summaries[:limit]
	}
	return summaries, hasMore, nil
}

// GetLatestCompletedReviewRoundBySubjectHash resolves the current reviewed replacement.
func (store *FeatureDeliveryStore) GetLatestCompletedReviewRoundBySubjectHash(
	ctx context.Context,
	subjectHash string,
) (*featuredelivery.ReviewRound, error) {
	round, err := scanReviewRound(store.db.QueryRowContext(ctx,
		`SELECT r.id,r.workflow_run_id,r.subject_json,r.policy_id,r.policy_version,
		        r.policy_hash,r.policy_selection_json,r.risk_facts_json,r.risk_hash,
		        r.selection_rule_version,r.selected_reviewers_json,r.panel_hash,
		        r.status,r.created_by,r.created_at,r.completed_at
		 FROM review_gate_results g
		 JOIN review_rounds r ON r.id=g.round_id
		 WHERE g.subject_hash=? AND r.status='completed'
		 ORDER BY g.created_at DESC,g.id DESC LIMIT 1`,
		subjectHash,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get latest completed review round for subject %q: %w", subjectHash, err)
	}
	return &round, nil
}

func scanReviewRound(row rowScanner) (featuredelivery.ReviewRound, error) {
	var round featuredelivery.ReviewRound
	var (
		subjectJSON         []byte
		policySelectionJSON []byte
		riskFactsJSON       []byte
		reviewersJSON       []byte
	)
	var completed sql.NullTime
	err := row.Scan(
		&round.ID, &round.WorkflowRunID, &subjectJSON, &round.PolicyID, &round.PolicyVersion, &round.PolicyHash,
		&policySelectionJSON, &riskFactsJSON, &round.RiskHash, &round.RuleVersion, &reviewersJSON,
		&round.PanelHash, &round.Status, &round.CreatedBy, &round.CreatedAt, &completed,
	)
	if err != nil {
		return round, err
	}
	if err := json.Unmarshal(subjectJSON, &round.Subject); err != nil {
		return round, fmt.Errorf("decode review round %q subject: %w", round.ID, err)
	}
	if err := json.Unmarshal(policySelectionJSON, &round.PolicySelection); err != nil {
		return round, fmt.Errorf(
			"decode review round %q policy selection: %w", round.ID, err,
		)
	}
	if err := json.Unmarshal(riskFactsJSON, &round.RiskFacts); err != nil {
		return round, fmt.Errorf("decode review round %q risk facts: %w", round.ID, err)
	}
	if err := json.Unmarshal(reviewersJSON, &round.Reviewers); err != nil {
		return round, fmt.Errorf("decode review round %q panel: %w", round.ID, err)
	}
	round.CompletedAt = nullableTime(completed)
	return round, nil
}

func (store *FeatureDeliveryStore) BindReviewRoundWorkflow(
	ctx context.Context,
	roundID, workflowRunID string,
	at time.Time,
) error {
	if roundID == "" || workflowRunID == "" || at.IsZero() {
		return featuredelivery.ErrInvalid
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review workflow binding: %w", err)
	}
	defer tx.Rollback()
	var existing string
	if err := tx.QueryRowContext(ctx,
		`SELECT workflow_run_id FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`,
		roundID,
	).Scan(&existing); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock review round %q workflow binding: %w", roundID, err)
	}
	if existing != "" {
		if existing != workflowRunID {
			return featuredelivery.ErrConflict
		}
		return nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE review_rounds SET workflow_run_id=? WHERE id=? AND workflow_run_id=''`,
		workflowRunID, roundID,
	)
	if err != nil {
		return fmt.Errorf("bind review round %q to workflow %q: %w", roundID, workflowRunID, err)
	}
	if err := requireSingleAffected(result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review round %q workflow binding: %w", roundID, err)
	}
	return nil
}

func (store *FeatureDeliveryStore) ListReviewAssignments(
	ctx context.Context,
	roundID string,
	cursor featuredelivery.ReviewAssignmentCursor,
	limit int,
) ([]featuredelivery.ReviewAssignment, error) {
	limit = boundedLimit(limit, maxReviewAssignmentPage, maxReviewAssignmentPage)
	query := `SELECT id,round_id,reviewer_id,agent_id,agent_version,definition_hash,categories_json,
		required_review,status,attempt,agent_run_id,error_code,created_at,started_at,completed_at
		FROM review_assignments WHERE round_id=?`
	args := []any{roundID}
	if !cursor.CreatedAt.IsZero() {
		query += ` AND (created_at>? OR (created_at=? AND id>?))`
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	query += ` ORDER BY created_at,id LIMIT ?`
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list review assignments for round %q: %w", roundID, err)
	}
	defer rows.Close()
	assignments := make([]featuredelivery.ReviewAssignment, 0, limit)
	for rows.Next() {
		assignment, err := scanReviewAssignment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan review assignment: %w", err)
		}
		assignments = append(assignments, assignment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review assignments: %w", err)
	}
	return assignments, nil
}

func (store *FeatureDeliveryStore) GetLatestReviewAssignment(
	ctx context.Context,
	roundID, reviewerID string,
) (*featuredelivery.ReviewAssignment, error) {
	row := store.db.QueryRowContext(ctx,
		`SELECT id,round_id,reviewer_id,agent_id,agent_version,definition_hash,categories_json,
		 required_review,status,attempt,agent_run_id,error_code,created_at,started_at,completed_at
		 FROM review_assignments
		 WHERE round_id=? AND reviewer_id=? ORDER BY attempt DESC LIMIT 1`,
		roundID, reviewerID,
	)
	assignment, err := scanReviewAssignment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf(
			"get latest review assignment for round %q reviewer %q: %w",
			roundID,
			reviewerID,
			err,
		)
	}
	return &assignment, nil
}

func (store *FeatureDeliveryStore) StartReviewAssignmentAttempt(
	ctx context.Context,
	roundID, reviewerID string,
	attempt int,
	agentRunID string,
	at time.Time,
) (*featuredelivery.ReviewAssignment, error) {
	if roundID == "" || reviewerID == "" || attempt <= 0 || agentRunID == "" || at.IsZero() {
		return nil, featuredelivery.ErrInvalid
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin review assignment attempt: %w", err)
	}
	defer tx.Rollback()
	var roundStatus featuredelivery.ReviewRoundStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`,
		roundID,
	).Scan(&roundStatus); errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lock review round %q: %w", roundID, err)
	}
	if roundStatus != featuredelivery.RoundRunning {
		return nil, featuredelivery.ErrConflict
	}
	latest, err := scanReviewAssignment(tx.QueryRowContext(ctx,
		`SELECT id,round_id,reviewer_id,agent_id,agent_version,definition_hash,categories_json,
		 required_review,status,attempt,agent_run_id,error_code,created_at,started_at,completed_at
		 FROM review_assignments
		 WHERE round_id=? AND reviewer_id=? ORDER BY attempt DESC LIMIT 1 FOR UPDATE`,
		roundID, reviewerID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf(
			"lock latest review assignment for round %q reviewer %q: %w",
			roundID,
			reviewerID,
			err,
		)
	}
	if latest.Attempt == attempt && latest.Status == featuredelivery.AssignmentRunning &&
		latest.AgentRunID == agentRunID {
		return &latest, nil
	}
	if attempt == 1 {
		if latest.Attempt != 1 || latest.Status != featuredelivery.AssignmentQueued {
			return nil, featuredelivery.ErrConflict
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE review_assignments
			 SET status='running',agent_run_id=?,error_code='',started_at=?,completed_at=NULL
			 WHERE id=? AND status='queued' AND attempt=1`,
			agentRunID, at, latest.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("start review assignment %q: %w", latest.ID, err)
		}
		if err := requireSingleAffected(result); err != nil {
			return nil, err
		}
		latest.Status = featuredelivery.AssignmentRunning
		latest.AgentRunID = agentRunID
		latest.ErrorCode = ""
		latest.StartedAt = &at
		latest.CompletedAt = nil
	} else {
		if latest.Attempt != attempt-1 ||
			(latest.Status != featuredelivery.AssignmentQueued &&
				latest.Status != featuredelivery.AssignmentRunning &&
				latest.Status != featuredelivery.AssignmentFailed) {
			return nil, featuredelivery.ErrConflict
		}
		if latest.Status != featuredelivery.AssignmentFailed {
			result, err := tx.ExecContext(ctx,
				`UPDATE review_assignments
				 SET status='failed',error_code='workflow_restarted',completed_at=?
				 WHERE id=? AND status IN ('queued','running')`,
				at, latest.ID,
			)
			if err != nil {
				return nil, fmt.Errorf("close interrupted review assignment %q: %w", latest.ID, err)
			}
			if err := requireSingleAffected(result); err != nil {
				return nil, err
			}
		}
		assignmentID, err := featuredelivery.NewID("assignment")
		if err != nil {
			return nil, err
		}
		categoriesJSON, err := json.Marshal(latest.Categories)
		if err != nil {
			return nil, fmt.Errorf("marshal reviewer categories: %w", err)
		}
		latest = featuredelivery.ReviewAssignment{
			ID: assignmentID, RoundID: roundID, ReviewerID: reviewerID,
			Agent: latest.Agent, DefinitionHash: latest.DefinitionHash,
			Categories: append([]string(nil), latest.Categories...), Required: latest.Required,
			Status: featuredelivery.AssignmentRunning, Attempt: attempt,
			AgentRunID: agentRunID, CreatedAt: at, StartedAt: &at,
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO review_assignments(
				id,round_id,reviewer_id,agent_id,agent_version,definition_hash,categories_json,
				required_review,status,attempt,agent_run_id,error_code,created_at,started_at,completed_at
			 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			latest.ID, latest.RoundID, latest.ReviewerID,
			latest.Agent.ID, latest.Agent.Version, latest.DefinitionHash,
			categoriesJSON, latest.Required, latest.Status, latest.Attempt,
			latest.AgentRunID, latest.ErrorCode, latest.CreatedAt,
			latest.StartedAt, latest.CompletedAt,
		); err != nil {
			if duplicateKey(err) {
				return nil, featuredelivery.ErrConflict
			}
			return nil, fmt.Errorf("insert review assignment attempt %q: %w", latest.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit review assignment attempt: %w", err)
	}
	return &latest, nil
}

func (store *FeatureDeliveryStore) GetSuccessfulReviewReport(
	ctx context.Context,
	roundID, reviewerID string,
) (*featuredelivery.ReviewReport, error) {
	var raw []byte
	err := store.db.QueryRowContext(ctx,
		`SELECT r.report_json
		 FROM review_reports r
		 JOIN review_assignments a ON a.id=r.assignment_id
		 WHERE a.round_id=? AND a.reviewer_id=?
		   AND a.status IN ('succeeded','reused')
		 ORDER BY a.attempt DESC LIMIT 1`,
		roundID, reviewerID,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf(
			"get successful review report for round %q reviewer %q: %w",
			roundID,
			reviewerID,
			err,
		)
	}
	var report featuredelivery.ReviewReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("decode successful review report: %w", err)
	}
	return &report, nil
}

func (store *FeatureDeliveryStore) GetReviewReportReuseSources(
	ctx context.Context,
	reportIDs []string,
) ([]featuredelivery.ReviewReportReuseSource, error) {
	if len(reportIDs) == 0 || len(reportIDs) > maxReviewAssignmentPage {
		return nil, featuredelivery.ErrInvalid
	}
	args := make([]any, 0, len(reportIDs)+1)
	for _, reportID := range reportIDs {
		args = append(args, reportID)
	}
	args = append(args, len(reportIDs))
	rows, err := store.db.QueryContext(ctx,
		`SELECT report.id,report.round_id,report.assignment_id,report.reviewer_id,
		        report.subject_hash,report.report_json,report.report_hash,report.content_hash,
		        assignment.agent_id,assignment.agent_version,assignment.definition_hash,
		        assignment.status,round.policy_id,round.policy_version,round.policy_hash
		 FROM review_reports report
		 JOIN review_assignments assignment ON assignment.id=report.assignment_id
		 JOIN review_rounds round ON round.id=report.round_id
		 WHERE report.id IN (`+placeholders(len(reportIDs))+`)
		   AND assignment.status IN ('succeeded','reused')
		 ORDER BY report.id LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("get reusable review reports: %w", err)
	}
	defer rows.Close()
	sources := make([]featuredelivery.ReviewReportReuseSource, 0, len(reportIDs))
	for rows.Next() {
		var source featuredelivery.ReviewReportReuseSource
		var (
			id, roundID, assignmentID, reviewerID string
			subjectHash, reportHash, contentHash  string
			raw                                   []byte
		)
		if err := rows.Scan(
			&id, &roundID, &assignmentID, &reviewerID,
			&subjectHash, &raw, &reportHash, &contentHash,
			&source.Assignment.Agent.ID, &source.Assignment.Agent.Version,
			&source.Assignment.DefinitionHash, &source.Assignment.Status,
			&source.PolicyID, &source.PolicyVersion, &source.PolicyHash,
		); err != nil {
			return nil, fmt.Errorf("scan reusable review report: %w", err)
		}
		if err := json.Unmarshal(raw, &source.Report); err != nil {
			return nil, fmt.Errorf("decode reusable review report %q: %w", id, err)
		}
		source.Assignment.ID = assignmentID
		source.Assignment.RoundID = roundID
		source.Assignment.ReviewerID = reviewerID
		if source.Report.ID != id ||
			source.Report.RoundID != roundID ||
			source.Report.AssignmentID != assignmentID ||
			source.Report.ReviewerID != reviewerID ||
			source.Report.SubjectHash != subjectHash ||
			source.Report.ReportHash != reportHash ||
			source.Report.ContentHash != contentHash {
			return nil, fmt.Errorf(
				"stored reusable review report %q has inconsistent identity: %w",
				id,
				featuredelivery.ErrConflict,
			)
		}
		if err := featuredelivery.ValidateReviewReportSnapshot(
			source.Report,
			source.Assignment,
			subjectHash,
		); err != nil {
			return nil, fmt.Errorf("validate reusable review report %q: %w", id, err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reusable review reports: %w", err)
	}
	return sources, nil
}

// GetReviewReportByAssignment loads one immutable report through its unique assignment.
func (store *FeatureDeliveryStore) GetReviewReportByAssignment(
	ctx context.Context,
	roundID, assignmentID string,
) (*featuredelivery.ReviewReport, error) {
	var (
		id, storedRoundID, storedAssignmentID string
		reviewerID, subjectHash, reportHash   string
		contentHash                           string
		raw                                   []byte
	)
	err := store.db.QueryRowContext(ctx,
		`SELECT id,round_id,assignment_id,reviewer_id,subject_hash,
		        report_json,report_hash,content_hash
		 FROM review_reports WHERE round_id=? AND assignment_id=? LIMIT 1`,
		roundID, assignmentID,
	).Scan(
		&id,
		&storedRoundID,
		&storedAssignmentID,
		&reviewerID,
		&subjectHash,
		&raw,
		&reportHash,
		&contentHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf(
			"get review report for round %q assignment %q: %w",
			roundID,
			assignmentID,
			err,
		)
	}
	var report featuredelivery.ReviewReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf(
			"decode review report for assignment %q: %w",
			assignmentID,
			err,
		)
	}
	if report.ID != id ||
		report.RoundID != storedRoundID ||
		report.AssignmentID != storedAssignmentID ||
		report.ReviewerID != reviewerID ||
		report.SubjectHash != subjectHash ||
		report.ReportHash != reportHash ||
		report.ContentHash != contentHash {
		return nil, fmt.Errorf(
			"stored review report for assignment %q has inconsistent identity: %w",
			assignmentID,
			featuredelivery.ErrConflict,
		)
	}
	return &report, nil
}

func scanReviewAssignment(row rowScanner) (featuredelivery.ReviewAssignment, error) {
	var assignment featuredelivery.ReviewAssignment
	var categoriesJSON []byte
	var started, completed sql.NullTime
	err := row.Scan(
		&assignment.ID, &assignment.RoundID, &assignment.ReviewerID,
		&assignment.Agent.ID, &assignment.Agent.Version, &assignment.DefinitionHash,
		&categoriesJSON, &assignment.Required, &assignment.Status, &assignment.Attempt,
		&assignment.AgentRunID, &assignment.ErrorCode, &assignment.CreatedAt, &started, &completed,
	)
	if err != nil {
		return assignment, err
	}
	if err := json.Unmarshal(categoriesJSON, &assignment.Categories); err != nil {
		return assignment, err
	}
	assignment.StartedAt = nullableTime(started)
	assignment.CompletedAt = nullableTime(completed)
	return assignment, nil
}

// ListReviewFindings projects summaries so evidence stays out of list queries.
func (store *FeatureDeliveryStore) ListReviewFindings(
	ctx context.Context,
	roundID string,
	severity featuredelivery.Severity,
	cursor featuredelivery.FindingCursor,
	limit int,
) ([]featuredelivery.FindingSummary, error) {
	limit = boundedLimit(limit, 20, maxReviewFindingPage)
	query := `SELECT id,report_id,round_id,category,severity,claim,impact,recommendation,
		confidence,fingerprint,location_json,content_hash,created_at
		FROM review_findings WHERE round_id=?`
	args := []any{roundID}
	if severity != "" {
		query += ` AND severity=?`
		args = append(args, severity)
	}
	if cursor.ID != "" {
		query += ` AND id>?`
		args = append(args, cursor.ID)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list review findings for round %q: %w", roundID, err)
	}
	defer rows.Close()
	findings := make([]featuredelivery.FindingSummary, 0, limit)
	for rows.Next() {
		finding, err := scanFindingSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("scan review finding: %w", err)
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review findings: %w", err)
	}
	return findings, nil
}

// GetReviewFinding loads one finding and its bounded evidence set.
func (store *FeatureDeliveryStore) GetReviewFinding(
	ctx context.Context,
	id string,
) (*featuredelivery.FindingDetail, error) {
	finding, err := scanFindingSummary(store.db.QueryRowContext(ctx,
		`SELECT id,report_id,round_id,category,severity,claim,impact,recommendation,
			confidence,fingerprint,location_json,content_hash,created_at
		 FROM review_findings WHERE id=? LIMIT 1`,
		id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review finding %q: %w", id, err)
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT kind,ref_value,source_hash,summary
		 FROM review_finding_evidence WHERE finding_id=? ORDER BY sequence LIMIT ?`,
		id, maxReviewEvidence,
	)
	if err != nil {
		return nil, fmt.Errorf("list evidence for review finding %q: %w", id, err)
	}
	defer rows.Close()
	evidence := make([]featuredelivery.FindingEvidence, 0)
	for rows.Next() {
		var item featuredelivery.FindingEvidence
		if err := rows.Scan(&item.Kind, &item.Ref, &item.Hash, &item.Summary); err != nil {
			return nil, fmt.Errorf("scan evidence for review finding %q: %w", id, err)
		}
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evidence for review finding %q: %w", id, err)
	}
	return &featuredelivery.FindingDetail{FindingSummary: finding, Evidence: evidence}, nil
}

func scanFindingSummary(row rowScanner) (featuredelivery.FindingSummary, error) {
	var finding featuredelivery.FindingSummary
	var locationJSON []byte
	err := row.Scan(
		&finding.ID, &finding.ReportID, &finding.RoundID, &finding.Category, &finding.Severity,
		&finding.Claim, &finding.Impact, &finding.Recommendation, &finding.Confidence,
		&finding.Fingerprint, &locationJSON, &finding.ContentHash, &finding.CreatedAt,
	)
	if err != nil {
		return finding, err
	}
	if len(locationJSON) > 0 {
		var location featuredelivery.FindingLocation
		if err := json.Unmarshal(locationJSON, &location); err != nil {
			return finding, err
		}
		finding.Location = &location
	}
	return finding, nil
}

func (store *FeatureDeliveryStore) TransitionReviewRound(
	ctx context.Context,
	id string,
	from, to featuredelivery.ReviewRoundStatus,
	at time.Time,
) error {
	if !featuredelivery.CanTransitionReviewRound(from, to) {
		return featuredelivery.ErrConflict
	}
	query := `UPDATE review_rounds SET status=?`
	args := []any{to}
	if to == featuredelivery.RoundCompleted || to == featuredelivery.RoundFailed || to == featuredelivery.RoundCancelled {
		query += `,completed_at=?`
		args = append(args, at)
	}
	query += ` WHERE id=? AND status=?`
	args = append(args, id, from)
	if to == featuredelivery.RoundEvaluating {
		query += ` AND NOT EXISTS (
			SELECT 1 FROM review_assignments
			WHERE round_id=review_rounds.id AND status IN ('queued','running') LIMIT 1
		)`
	}
	result, err := store.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("transition review round %q from %s to %s: %w", id, from, to, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read review round transition result: %w", err)
	}
	if affected != 1 {
		return featuredelivery.ErrConflict
	}
	return nil
}

func (store *FeatureDeliveryStore) TransitionReviewAssignment(
	ctx context.Context,
	id string,
	from, to featuredelivery.ReviewAssignmentStatus,
	agentRunID, errorCode string,
	at time.Time,
) error {
	if !featuredelivery.CanTransitionReviewAssignment(from, to) {
		return featuredelivery.ErrConflict
	}
	query := `UPDATE review_assignments a
		JOIN review_rounds r ON r.id=a.round_id
		SET a.status=?,a.agent_run_id=?,a.error_code=?`
	args := []any{to, agentRunID, errorCode}
	if to == featuredelivery.AssignmentRunning {
		query += `,a.started_at=?`
		args = append(args, at)
	} else {
		query += `,a.completed_at=?`
		args = append(args, at)
	}
	query += ` WHERE a.id=? AND a.status=? AND r.status='running'`
	args = append(args, id, from)
	result, err := store.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("transition review assignment %q from %s to %s: %w", id, from, to, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read review assignment transition result: %w", err)
	}
	if affected != 1 {
		return featuredelivery.ErrConflict
	}
	return nil
}

func (store *FeatureDeliveryStore) RequestReviewRoundCancel(
	ctx context.Context,
	id string,
	at time.Time,
) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin review round cancellation: %w", err)
	}
	defer tx.Rollback()
	var status featuredelivery.ReviewRoundStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`,
		id,
	).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return false, featuredelivery.ErrNotFound
	} else if err != nil {
		return false, fmt.Errorf("lock review round cancellation %q: %w", id, err)
	}
	switch status {
	case featuredelivery.RoundCancelled:
		return false, nil
	case featuredelivery.RoundCreated, featuredelivery.RoundRunning, featuredelivery.RoundEvaluating:
	default:
		return false, featuredelivery.ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE review_assignments
		 SET status='cancelled',error_code='review_cancelled',completed_at=?
		 WHERE round_id=? AND status IN ('queued','running')`,
		at, id,
	); err != nil {
		return false, fmt.Errorf("cancel assignments for review round %q: %w", id, err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE review_rounds SET status='cancelled',completed_at=?
		 WHERE id=? AND status=?`,
		at, id, status,
	)
	if err != nil {
		return false, fmt.Errorf("cancel review round %q: %w", id, err)
	}
	if err := requireSingleAffected(result); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit review round cancellation %q: %w", id, err)
	}
	return true, nil
}

func (store *FeatureDeliveryStore) AppendReviewEvent(
	ctx context.Context,
	event featuredelivery.ReviewEvent,
) (*featuredelivery.ReviewEvent, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin review event append: %w", err)
	}
	defer tx.Rollback()
	var roundID string
	var status featuredelivery.ReviewRoundStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT id,status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`,
		event.RoundID,
	).Scan(&roundID, &status); errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lock review round %q for event append: %w", event.RoundID, err)
	}
	if !featuredelivery.CanAppendReviewEvent(event.Kind, status) {
		return nil, featuredelivery.ErrConflict
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM review_round_events WHERE round_id=?`,
		event.RoundID,
	).Scan(&event.Seq); err != nil {
		return nil, fmt.Errorf("allocate review event sequence for round %q: %w", event.RoundID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_round_events(round_id,seq,kind,summary,detail_json,created_at)
		 VALUES(?,?,?,?,?,?)`,
		event.RoundID, event.Seq, event.Kind, event.Summary, nullableJSON(event.Detail), event.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("append review event for round %q: %w", event.RoundID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit review event for round %q: %w", event.RoundID, err)
	}
	return &event, nil
}

func (store *FeatureDeliveryStore) ListReviewEvents(
	ctx context.Context,
	roundID string,
	afterSeq int64,
	limit int,
) ([]featuredelivery.ReviewEvent, error) {
	limit = boundedLimit(limit, 100, maxReviewEventPage)
	rows, err := store.db.QueryContext(ctx,
		`SELECT round_id,seq,kind,summary,detail_json,created_at
		 FROM review_round_events WHERE round_id=? AND seq>? ORDER BY seq LIMIT ?`,
		roundID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list review events for round %q: %w", roundID, err)
	}
	defer rows.Close()
	events := make([]featuredelivery.ReviewEvent, 0, limit)
	for rows.Next() {
		var event featuredelivery.ReviewEvent
		var detail []byte
		if err := rows.Scan(
			&event.RoundID, &event.Seq, &event.Kind, &event.Summary, &detail, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan review event: %w", err)
		}
		event.Detail = append([]byte(nil), detail...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review events: %w", err)
	}
	return events, nil
}

func (store *FeatureDeliveryStore) CompleteReviewAssignment(ctx context.Context, report featuredelivery.ReviewReport) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review report completion: %w", err)
	}
	defer tx.Rollback()
	var roundID, reviewerID, subjectHash, roundStatus, existingHash string
	var status featuredelivery.ReviewAssignmentStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT a.round_id,a.reviewer_id,a.status,r.subject_hash,r.status,
		        COALESCE(report.content_hash,'')
		 FROM review_assignments a
		 JOIN review_rounds r ON r.id=a.round_id
		 LEFT JOIN review_reports report ON report.assignment_id=a.id
		 WHERE a.id=? LIMIT 1 FOR UPDATE`,
		report.AssignmentID,
	).Scan(
		&roundID,
		&reviewerID,
		&status,
		&subjectHash,
		&roundStatus,
		&existingHash,
	); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrConflict
	} else if err != nil {
		return fmt.Errorf("lock review assignment %q: %w", report.AssignmentID, err)
	}
	if report.RoundID != roundID || report.ReviewerID != reviewerID ||
		report.SubjectHash != subjectHash {
		return featuredelivery.ErrConflict
	}
	if status == featuredelivery.AssignmentSucceeded && existingHash == report.ContentHash {
		return nil
	}
	if status != featuredelivery.AssignmentRunning ||
		roundStatus != string(featuredelivery.RoundRunning) ||
		existingHash != "" {
		return featuredelivery.ErrConflict
	}
	if err := insertReviewReport(ctx, tx, report); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE review_assignments SET status='succeeded',completed_at=?
		 WHERE id=? AND status='running'`,
		report.CompletedAt, report.AssignmentID,
	)
	if err != nil {
		return fmt.Errorf("complete review assignment %q: %w", report.AssignmentID, err)
	}
	if err := requireSingleAffected(result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review report %q: %w", report.ID, err)
	}
	return nil
}

func insertReviewReport(
	ctx context.Context,
	tx *sql.Tx,
	report featuredelivery.ReviewReport,
) error {
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal review report %q: %w", report.ID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_reports(
			id,round_id,assignment_id,reviewer_id,subject_hash,
			report_json,report_hash,content_hash,completed_at
		 ) VALUES(?,?,?,?,?,?,?,?,?)`,
		report.ID, report.RoundID, report.AssignmentID, report.ReviewerID,
		report.SubjectHash, reportJSON, report.ReportHash,
		report.ContentHash, report.CompletedAt,
	); err != nil {
		if duplicateKey(err) {
			return featuredelivery.ErrConflict
		}
		return fmt.Errorf("insert review report %q: %w", report.ID, err)
	}
	for _, finding := range report.Findings {
		var location any
		if finding.Location != nil {
			raw, err := json.Marshal(finding.Location)
			if err != nil {
				return fmt.Errorf("marshal finding location: %w", err)
			}
			location = raw
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO review_findings(
				id,report_id,round_id,category,severity,claim,impact,recommendation,
				confidence,fingerprint,location_json,content_hash,created_at
			 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			finding.ID, report.ID, report.RoundID, finding.Category, finding.Severity,
			finding.Claim, finding.Impact, finding.Recommendation, finding.Confidence,
			finding.Fingerprint, location, finding.ContentHash, report.CompletedAt,
		); err != nil {
			return fmt.Errorf("insert review finding %q: %w", finding.ID, err)
		}
		for index, evidence := range finding.Evidence {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO review_finding_evidence(
					finding_id,sequence,kind,ref_value,source_hash,summary
				 ) VALUES(?,?,?,?,?,?)`,
				finding.ID, index+1, evidence.Kind, evidence.Ref, evidence.Hash, evidence.Summary,
			); err != nil {
				return fmt.Errorf("insert evidence for finding %q: %w", finding.ID, err)
			}
		}
	}
	return nil
}

func (store *FeatureDeliveryStore) SaveReviewAdjudications(
	ctx context.Context,
	adjudications []featuredelivery.ReviewAdjudication,
) error {
	if len(adjudications) == 0 {
		return nil
	}
	if len(adjudications) > maxReviewAdjudicationPage {
		return featuredelivery.ErrInvalid
	}
	prepared := make([]featuredelivery.ReviewAdjudication, 0, len(adjudications))
	seen := make(map[string]struct{}, len(adjudications))
	for _, adjudication := range adjudications {
		item, err := featuredelivery.PrepareReviewAdjudication(adjudication)
		if err != nil {
			return err
		}
		key := item.RoundID + "\x00" + item.Fingerprint
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf(
				"duplicate review adjudication %q: %w",
				item.Fingerprint,
				featuredelivery.ErrInvalid,
			)
		}
		seen[key] = struct{}{}
		prepared = append(prepared, item)
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review adjudication publication: %w", err)
	}
	defer tx.Rollback()
	for _, adjudication := range prepared {
		raw, err := json.Marshal(adjudication)
		if err != nil {
			return fmt.Errorf("marshal review adjudication %q: %w", adjudication.ID, err)
		}
		result, err := tx.ExecContext(ctx,
			`INSERT INTO review_adjudications(
				id,round_id,subject_hash,policy_hash,fingerprint,agent_id,agent_version,
				definition_hash,decision,error_code,adjudication_json,content_hash,created_at
			 )
			 SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?
			 FROM review_rounds
			 WHERE id=? AND subject_hash=? AND policy_hash=? AND status='evaluating'
			 LIMIT 1`,
			adjudication.ID, adjudication.RoundID, adjudication.SubjectHash,
			adjudication.PolicyHash, adjudication.Fingerprint, adjudication.Agent.ID,
			adjudication.Agent.Version, adjudication.DefinitionHash, adjudication.Decision,
			adjudication.ErrorCode, raw, adjudication.ContentHash, adjudication.CreatedAt,
			adjudication.RoundID, adjudication.SubjectHash, adjudication.PolicyHash,
		)
		if err == nil {
			if err := requireSingleAffected(result); err != nil {
				return fmt.Errorf("review adjudication round mismatch: %w", err)
			}
			continue
		}
		if !duplicateKey(err) {
			return fmt.Errorf("save review adjudication %q: %w", adjudication.ID, err)
		}
		var existingHash string
		readErr := tx.QueryRowContext(ctx,
			`SELECT content_hash FROM review_adjudications
			 WHERE round_id=? AND fingerprint=? LIMIT 1`,
			adjudication.RoundID,
			adjudication.Fingerprint,
		).Scan(&existingHash)
		if readErr != nil {
			return fmt.Errorf("read existing review adjudication %q: %w", adjudication.ID, readErr)
		}
		if existingHash != adjudication.ContentHash {
			return featuredelivery.ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review adjudication publication: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) ListReviewAdjudications(
	ctx context.Context,
	roundID string,
	cursor featuredelivery.ReviewAdjudicationCursor,
	limit int,
) ([]featuredelivery.ReviewAdjudication, error) {
	limit = boundedLimit(limit, 20, maxReviewAdjudicationPage)
	query := `SELECT adjudication_json FROM review_adjudications
		 WHERE round_id=?`
	args := []any{roundID}
	if cursor.Fingerprint != "" {
		query += ` AND (fingerprint>? OR (fingerprint=? AND id>?))`
		args = append(args, cursor.Fingerprint, cursor.Fingerprint, cursor.ID)
	}
	query += ` ORDER BY fingerprint,id LIMIT ?`
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list review adjudications: %w", err)
	}
	defer rows.Close()
	items := make([]featuredelivery.ReviewAdjudication, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan review adjudication: %w", err)
		}
		var adjudication featuredelivery.ReviewAdjudication
		if err := json.Unmarshal(raw, &adjudication); err != nil {
			return nil, fmt.Errorf("decode review adjudication: %w", err)
		}
		prepared, err := featuredelivery.PrepareReviewAdjudication(adjudication)
		if err != nil {
			return nil, fmt.Errorf(
				"validate stored review adjudication %q: %w",
				adjudication.ID,
				err,
			)
		}
		items = append(items, prepared)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review adjudications: %w", err)
	}
	return items, nil
}

// LoadFullReviewEvaluation intentionally loads the bounded policy panel and its reports for one Gate calculation.
func (store *FeatureDeliveryStore) LoadFullReviewEvaluation(ctx context.Context, roundID string) (featuredelivery.ReviewEvaluation, error) {
	round, err := store.GetReviewRound(ctx, roundID)
	if err != nil {
		return featuredelivery.ReviewEvaluation{}, err
	}
	if round.Status != featuredelivery.RoundEvaluating {
		return featuredelivery.ReviewEvaluation{}, featuredelivery.ErrConflict
	}
	policy, err := store.GetReviewPolicy(ctx, round.PolicyID, round.PolicyVersion)
	if err != nil {
		return featuredelivery.ReviewEvaluation{}, err
	}
	assignments, err := store.listLatestReviewAssignments(ctx, roundID)
	if err != nil {
		return featuredelivery.ReviewEvaluation{}, err
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT report.report_json
		 FROM review_reports report
		 JOIN review_assignments assignment ON assignment.id=report.assignment_id
		 JOIN (
			SELECT reviewer_id,MAX(attempt) AS attempt
			FROM review_assignments
			WHERE round_id=? AND status IN ('succeeded','reused')
			GROUP BY reviewer_id
		 ) latest ON latest.reviewer_id=assignment.reviewer_id
			AND latest.attempt=assignment.attempt
		 WHERE assignment.round_id=?
		 ORDER BY assignment.reviewer_id
		 LIMIT ?`,
		roundID, roundID, maxReviewAssignmentPage,
	)
	if err != nil {
		return featuredelivery.ReviewEvaluation{}, fmt.Errorf("list review reports: %w", err)
	}
	defer rows.Close()
	reports := make([]featuredelivery.ReviewReport, 0, len(assignments))
	findingIDs := make([]string, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return featuredelivery.ReviewEvaluation{}, fmt.Errorf("scan review report: %w", err)
		}
		var report featuredelivery.ReviewReport
		if err := json.Unmarshal(raw, &report); err != nil {
			return featuredelivery.ReviewEvaluation{}, fmt.Errorf("decode review report: %w", err)
		}
		reports = append(reports, report)
		for _, finding := range report.Findings {
			findingIDs = append(findingIDs, finding.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return featuredelivery.ReviewEvaluation{}, fmt.Errorf("iterate review reports: %w", err)
	}
	resolutions, err := store.ListFindingResolutionsByIDs(ctx, findingIDs, round.Subject.ContentHash)
	if err != nil {
		return featuredelivery.ReviewEvaluation{}, err
	}
	adjudications, err := store.ListReviewAdjudications(
		ctx,
		roundID,
		featuredelivery.ReviewAdjudicationCursor{},
		maxReviewAdjudicationPage,
	)
	if err != nil {
		return featuredelivery.ReviewEvaluation{}, err
	}
	return featuredelivery.ReviewEvaluation{
		Round: *round, Policy: *policy, Assignments: assignments,
		Reports: reports, Adjudications: adjudications, Resolutions: resolutions,
	}, nil
}

func (store *FeatureDeliveryStore) listLatestReviewAssignments(
	ctx context.Context,
	roundID string,
) ([]featuredelivery.ReviewAssignment, error) {
	rows, err := store.db.QueryContext(ctx,
		`SELECT assignment.id,assignment.round_id,assignment.reviewer_id,
		        assignment.agent_id,assignment.agent_version,assignment.definition_hash,
		        assignment.categories_json,assignment.required_review,assignment.status,
		        assignment.attempt,assignment.agent_run_id,assignment.error_code,
		        assignment.created_at,assignment.started_at,assignment.completed_at
		 FROM review_assignments assignment
		 JOIN (
			SELECT reviewer_id,MAX(attempt) AS attempt
			FROM review_assignments
			WHERE round_id=?
			GROUP BY reviewer_id
		 ) latest ON latest.reviewer_id=assignment.reviewer_id
			AND latest.attempt=assignment.attempt
		 WHERE assignment.round_id=?
		 ORDER BY assignment.reviewer_id
		 LIMIT ?`,
		roundID, roundID, maxReviewAssignmentPage,
	)
	if err != nil {
		return nil, fmt.Errorf("list latest review assignments for round %q: %w", roundID, err)
	}
	defer rows.Close()
	assignments := make([]featuredelivery.ReviewAssignment, 0, maxReviewAssignmentPage)
	for rows.Next() {
		assignment, err := scanReviewAssignment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan latest review assignment: %w", err)
		}
		assignments = append(assignments, assignment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest review assignments: %w", err)
	}
	return assignments, nil
}

func (store *FeatureDeliveryStore) CompleteReviewRound(
	ctx context.Context,
	result featuredelivery.ReviewGateResult,
	completedAt time.Time,
) error {
	if !isStoredGateDecision(result.Decision) {
		return featuredelivery.ErrInvalid
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal review gate result: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review gate completion: %w", err)
	}
	defer tx.Rollback()
	var subjectHash, policyHash, existingHash string
	var status featuredelivery.ReviewRoundStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT round.subject_hash,round.policy_hash,round.status,
		        COALESCE(gate.content_hash,'')
		 FROM review_rounds round
		 LEFT JOIN review_gate_results gate ON gate.round_id=round.id
		 WHERE round.id=? LIMIT 1 FOR UPDATE`,
		result.RoundID,
	).Scan(
		&subjectHash,
		&policyHash,
		&status,
		&existingHash,
	); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock review round %q: %w", result.RoundID, err)
	}
	if subjectHash != result.SubjectHash || policyHash != result.PolicyHash {
		return featuredelivery.ErrConflict
	}
	if status == featuredelivery.RoundCompleted && existingHash == result.ContentHash {
		return nil
	}
	if status != featuredelivery.RoundEvaluating || existingHash != "" {
		return featuredelivery.ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_gate_results(
			id,round_id,subject_hash,decision,result_json,policy_hash,content_hash,created_at
		 ) VALUES(?,?,?,?,?,?,?,?)`,
		result.ID, result.RoundID, result.SubjectHash, result.Decision,
		resultJSON, result.PolicyHash, result.ContentHash, result.CreatedAt,
	); err != nil {
		if duplicateKey(err) {
			return featuredelivery.ErrConflict
		}
		return fmt.Errorf("insert review gate result %q: %w", result.ID, err)
	}
	update, err := tx.ExecContext(ctx,
		`UPDATE review_rounds SET status='completed',completed_at=?
		 WHERE id=? AND status='evaluating'`,
		completedAt, result.RoundID,
	)
	if err != nil {
		return fmt.Errorf("complete review round %q: %w", result.RoundID, err)
	}
	if err := requireSingleAffected(update); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review gate result %q: %w", result.ID, err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetReviewGateResult(ctx context.Context, id string) (*featuredelivery.ReviewGateResult, error) {
	var raw []byte
	err := store.db.QueryRowContext(ctx,
		`SELECT result_json FROM review_gate_results WHERE id=? LIMIT 1`,
		id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review gate result %q: %w", id, err)
	}
	var result featuredelivery.ReviewGateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode review gate result %q: %w", id, err)
	}
	return &result, nil
}

// GetReviewGateResultByRound resolves the single immutable Gate for a round.
func (store *FeatureDeliveryStore) GetReviewGateResultByRound(
	ctx context.Context,
	roundID string,
) (*featuredelivery.ReviewGateResult, error) {
	var raw []byte
	err := store.db.QueryRowContext(ctx,
		`SELECT result_json FROM review_gate_results WHERE round_id=? LIMIT 1`,
		roundID,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review gate result for round %q: %w", roundID, err)
	}
	var result featuredelivery.ReviewGateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode review gate result for round %q: %w", roundID, err)
	}
	return &result, nil
}

func (store *FeatureDeliveryStore) CreateFindingResolution(ctx context.Context, resolution featuredelivery.FindingResolution) error {
	if resolution.ID == "" || resolution.FindingID == "" || resolution.SubjectHash == "" ||
		resolution.Rationale == "" || resolution.ActorID <= 0 || resolution.CreatedAt.IsZero() ||
		!isResolutionKind(resolution.Resolution) {
		return featuredelivery.ErrInvalid
	}
	result, err := store.db.ExecContext(ctx,
		`INSERT INTO finding_resolutions(
			id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at
		 ) SELECT ?,?,?,?,?,?,?,?,? FROM review_findings f
		   JOIN review_rounds r ON r.id=f.round_id
		  WHERE f.id=? AND r.subject_hash=? LIMIT 1`,
		resolution.ID, resolution.FindingID, resolution.Resolution, resolution.SubjectHash,
		resolution.ReplacementHash, resolution.Rationale, resolution.ActorID,
		resolution.ExpiresAt, resolution.CreatedAt, resolution.FindingID, resolution.SubjectHash,
	)
	if err != nil {
		if duplicateKey(err) {
			return featuredelivery.ErrConflict
		}
		return fmt.Errorf("create finding resolution %q: %w", resolution.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read finding resolution %q result: %w", resolution.ID, err)
	}
	if affected != 1 {
		return featuredelivery.ErrNotFound
	}
	return nil
}

func (store *FeatureDeliveryStore) ListFindingResolutions(
	ctx context.Context,
	findingID, subjectHash string,
	cursor featuredelivery.FindingResolutionCursor,
	limit int,
) ([]featuredelivery.FindingResolution, error) {
	if findingID == "" || subjectHash == "" {
		return nil, featuredelivery.ErrInvalid
	}
	limit = boundedLimit(limit, 20, maxReviewResolutionPage)
	query := `SELECT id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at
		FROM finding_resolutions WHERE finding_id=? AND subject_hash=?`
	args := []any{findingID, subjectHash}
	if !cursor.CreatedAt.IsZero() {
		query += ` AND (created_at<? OR (created_at=? AND id<?))`
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list finding resolutions for %q: %w", findingID, err)
	}
	defer rows.Close()
	resolutions := make([]featuredelivery.FindingResolution, 0, limit)
	for rows.Next() {
		resolution, err := scanFindingResolution(rows)
		if err != nil {
			return nil, fmt.Errorf("scan finding resolution: %w", err)
		}
		resolutions = append(resolutions, resolution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate finding resolutions for %q: %w", findingID, err)
	}
	return resolutions, nil
}

func (store *FeatureDeliveryStore) ListFindingResolutionsByIDs(
	ctx context.Context,
	findingIDs []string,
	subjectHash string,
) ([]featuredelivery.FindingResolution, error) {
	if len(findingIDs) == 0 {
		return nil, nil
	}
	if len(findingIDs) > maxReviewAssignmentPage*100 {
		return nil, featuredelivery.ErrInvalid
	}
	args := make([]any, 0, len(findingIDs)+2)
	args = append(args, subjectHash)
	for _, id := range findingIDs {
		args = append(args, id)
	}
	args = append(args, len(findingIDs))
	query := `SELECT id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at
		FROM (
			SELECT id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at,
			       ROW_NUMBER() OVER(PARTITION BY finding_id ORDER BY created_at DESC,id DESC) AS resolution_rank
			FROM finding_resolutions WHERE subject_hash=? AND finding_id IN (` +
		strings.TrimRight(strings.Repeat("?,", len(findingIDs)), ",") +
		`)
		) ranked WHERE resolution_rank=1 ORDER BY finding_id LIMIT ?`
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list finding resolutions: %w", err)
	}
	defer rows.Close()
	resolutions := make([]featuredelivery.FindingResolution, 0)
	for rows.Next() {
		resolution, err := scanFindingResolution(rows)
		if err != nil {
			return nil, fmt.Errorf("scan finding resolution: %w", err)
		}
		resolutions = append(resolutions, resolution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate finding resolutions: %w", err)
	}
	return resolutions, nil
}

func scanFindingResolution(row rowScanner) (featuredelivery.FindingResolution, error) {
	var resolution featuredelivery.FindingResolution
	var expires sql.NullTime
	err := row.Scan(
		&resolution.ID, &resolution.FindingID, &resolution.Resolution,
		&resolution.SubjectHash, &resolution.ReplacementHash, &resolution.Rationale,
		&resolution.ActorID, &expires, &resolution.CreatedAt,
	)
	if err != nil {
		return resolution, err
	}
	resolution.ExpiresAt = nullableTime(expires)
	return resolution, nil
}

func requireSingleAffected(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if affected != 1 {
		return featuredelivery.ErrConflict
	}
	return nil
}

func isStoredGateDecision(decision featuredelivery.GateDecision) bool {
	switch decision {
	case featuredelivery.GatePass, featuredelivery.GateRevise, featuredelivery.GateHumanRequired,
		featuredelivery.GateIncomplete, featuredelivery.GateFailed:
		return true
	default:
		return false
	}
}

func isResolutionKind(kind featuredelivery.FindingResolutionKind) bool {
	switch kind {
	case featuredelivery.ResolutionFixed, featuredelivery.ResolutionWaived,
		featuredelivery.ResolutionInvalidated, featuredelivery.ResolutionSuperseded:
		return true
	default:
		return false
	}
}

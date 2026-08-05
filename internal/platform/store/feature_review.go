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

const maxReviewAssignmentPage = 16

func (store *FeatureDeliveryStore) SaveReviewPolicy(ctx context.Context, policy featuredelivery.ReviewPolicy) error {
	prepared, err := featuredelivery.PrepareReviewPolicy(policy)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(prepared)
	if err != nil {
		return fmt.Errorf("marshal review policy %q: %w", prepared.ID, err)
	}
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO review_policies(id,version,subject_kind,definition_json,content_hash,created_at)
		 VALUES(?,?,?,?,?,?)`,
		prepared.ID, prepared.Version, prepared.SubjectKind, raw, prepared.ContentHash, prepared.CreatedAt,
	)
	if err == nil {
		return nil
	}
	if !duplicateKey(err) {
		return fmt.Errorf("save review policy %q version %d: %w", prepared.ID, prepared.Version, err)
	}
	var existingHash string
	readErr := store.db.QueryRowContext(ctx,
		`SELECT content_hash FROM review_policies WHERE id=? AND version=? LIMIT 1`,
		prepared.ID, prepared.Version,
	).Scan(&existingHash)
	if readErr != nil {
		return fmt.Errorf("read existing review policy %q version %d: %w", prepared.ID, prepared.Version, readErr)
	}
	if existingHash != prepared.ContentHash {
		return featuredelivery.ErrConflict
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
	if len(assignments) < 2 || len(assignments) > maxReviewAssignmentPage {
		return featuredelivery.ErrInvalid
	}
	subjectJSON, err := json.Marshal(round.Subject)
	if err != nil {
		return fmt.Errorf("marshal review subject: %w", err)
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
			id,subject_kind,subject_id,subject_version,subject_hash,subject_json,
			policy_id,policy_version,policy_hash,status,created_by,created_at,completed_at
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		round.ID, round.Subject.Kind, round.Subject.ID, round.Subject.Version,
		round.Subject.ContentHash, subjectJSON, round.PolicyID, round.PolicyVersion,
		round.PolicyHash, round.Status, round.CreatedBy, round.CreatedAt, round.CompletedAt,
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review round %q: %w", round.ID, err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetReviewRound(ctx context.Context, id string) (*featuredelivery.ReviewRound, error) {
	var round featuredelivery.ReviewRound
	var subjectJSON []byte
	var completed sql.NullTime
	err := store.db.QueryRowContext(ctx,
		`SELECT id,subject_json,policy_id,policy_version,policy_hash,status,created_by,created_at,completed_at
		 FROM review_rounds WHERE id=? LIMIT 1`,
		id,
	).Scan(
		&round.ID, &subjectJSON, &round.PolicyID, &round.PolicyVersion, &round.PolicyHash,
		&round.Status, &round.CreatedBy, &round.CreatedAt, &completed,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review round %q: %w", id, err)
	}
	if err := json.Unmarshal(subjectJSON, &round.Subject); err != nil {
		return nil, fmt.Errorf("decode review round %q subject: %w", id, err)
	}
	round.CompletedAt = nullableTime(completed)
	return &round, nil
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

func (store *FeatureDeliveryStore) CompleteReviewAssignment(ctx context.Context, report featuredelivery.ReviewReport) error {
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal review report %q: %w", report.ID, err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review report completion: %w", err)
	}
	defer tx.Rollback()
	var roundID, reviewerID, subjectHash string
	var status featuredelivery.ReviewAssignmentStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT a.round_id,a.reviewer_id,a.status,r.subject_hash
		 FROM review_assignments a JOIN review_rounds r ON r.id=a.round_id
		 WHERE a.id=? AND r.status='running' LIMIT 1 FOR UPDATE`,
		report.AssignmentID,
	).Scan(&roundID, &reviewerID, &status, &subjectHash); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrConflict
	} else if err != nil {
		return fmt.Errorf("lock review assignment %q: %w", report.AssignmentID, err)
	}
	if status != featuredelivery.AssignmentRunning || report.RoundID != roundID ||
		report.ReviewerID != reviewerID || report.SubjectHash != subjectHash {
		return featuredelivery.ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_reports(
			id,round_id,assignment_id,reviewer_id,subject_hash,report_json,content_hash,completed_at
		 ) VALUES(?,?,?,?,?,?,?,?)`,
		report.ID, report.RoundID, report.AssignmentID, report.ReviewerID,
		report.SubjectHash, reportJSON, report.ContentHash, report.CompletedAt,
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
	assignments, err := store.ListReviewAssignments(ctx, roundID, featuredelivery.ReviewAssignmentCursor{}, maxReviewAssignmentPage)
	if err != nil {
		return featuredelivery.ReviewEvaluation{}, err
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT report_json FROM review_reports
		 WHERE round_id=? ORDER BY assignment_id LIMIT ?`,
		roundID, maxReviewAssignmentPage,
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
	return featuredelivery.ReviewEvaluation{
		Round: *round, Policy: *policy, Assignments: assignments,
		Reports: reports, Resolutions: resolutions,
	}, nil
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
	var subjectHash, policyHash string
	var status featuredelivery.ReviewRoundStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT subject_hash,policy_hash,status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`,
		result.RoundID,
	).Scan(&subjectHash, &policyHash, &status); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock review round %q: %w", result.RoundID, err)
	}
	if status != featuredelivery.RoundEvaluating || subjectHash != result.SubjectHash || policyHash != result.PolicyHash {
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

func (store *FeatureDeliveryStore) CreateFindingResolution(ctx context.Context, resolution featuredelivery.FindingResolution) error {
	if resolution.ID == "" || resolution.FindingID == "" || resolution.SubjectHash == "" ||
		resolution.Rationale == "" || !isResolutionKind(resolution.Resolution) {
		return featuredelivery.ErrInvalid
	}
	_, err := store.db.ExecContext(ctx,
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
	return nil
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
	args = append(args, len(findingIDs)*4)
	query := `SELECT id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at
		FROM finding_resolutions WHERE subject_hash=? AND finding_id IN (` +
		strings.TrimRight(strings.Repeat("?,", len(findingIDs)), ",") +
		`) ORDER BY finding_id,created_at DESC,id DESC LIMIT ?`
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list finding resolutions: %w", err)
	}
	defer rows.Close()
	resolutions := make([]featuredelivery.FindingResolution, 0)
	for rows.Next() {
		var resolution featuredelivery.FindingResolution
		var expires sql.NullTime
		if err := rows.Scan(
			&resolution.ID, &resolution.FindingID, &resolution.Resolution,
			&resolution.SubjectHash, &resolution.ReplacementHash, &resolution.Rationale,
			&resolution.ActorID, &expires, &resolution.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan finding resolution: %w", err)
		}
		resolution.ExpiresAt = nullableTime(expires)
		resolutions = append(resolutions, resolution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate finding resolutions: %w", err)
	}
	return resolutions, nil
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

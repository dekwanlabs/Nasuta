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
	mysqlDriver "github.com/go-sql-driver/mysql"
)

const (
	maxFeaturePage  = 100
	maxArtifactPage = 500
	maxRunPage      = 100
	maxEventPage    = 500
)

// FeatureDeliveryStore persists the feature delivery workflow in MySQL.
type FeatureDeliveryStore struct {
	db *sql.DB
}

func NewFeatureDeliveryStore(db *sql.DB) *FeatureDeliveryStore {
	if db == nil {
		return nil
	}
	return &FeatureDeliveryStore{db: db}
}

func (store *FeatureDeliveryStore) CreateFeature(ctx context.Context, feature featuredelivery.FeatureRequest, artifact featuredelivery.Artifact) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin feature creation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_requests(id,title,created_by,created_at,updated_at) VALUES(?,?,?,?,?)`,
		feature.ID, feature.Title, feature.CreatedBy, feature.CreatedAt, feature.UpdatedAt,
	); err != nil {
		return fmt.Errorf("insert feature %q: %w", feature.ID, err)
	}
	if err := insertArtifact(ctx, tx, artifact); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit feature %q: %w", feature.ID, err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetFeature(ctx context.Context, id string) (*featuredelivery.FeatureRequest, error) {
	var feature featuredelivery.FeatureRequest
	var archived sql.NullTime
	err := store.db.QueryRowContext(ctx,
		`SELECT id,title,created_by,archived_at,created_at,updated_at
		 FROM feature_requests WHERE id=? LIMIT 1`, id,
	).Scan(&feature.ID, &feature.Title, &feature.CreatedBy, &archived, &feature.CreatedAt, &feature.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get feature %q: %w", id, err)
	}
	feature.ArchivedAt = nullableTime(archived)
	return &feature, nil
}

func (store *FeatureDeliveryStore) ListFeatures(ctx context.Context, ownerID int64, admin bool, cursor featuredelivery.FeatureCursor, limit int) ([]featuredelivery.FeatureRequest, error) {
	limit = boundedLimit(limit, 20, maxFeaturePage)
	query := `SELECT id,title,created_by,archived_at,created_at,updated_at FROM feature_requests`
	args := make([]any, 0, 4)
	conditions := make([]string, 0, 2)
	if !admin {
		conditions = append(conditions, "created_by=?")
		args = append(args, ownerID)
	}
	if !cursor.UpdatedAt.IsZero() {
		conditions = append(conditions, "(updated_at < ? OR (updated_at = ? AND id < ?))")
		args = append(args, cursor.UpdatedAt, cursor.UpdatedAt, cursor.ID)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY updated_at DESC,id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	defer rows.Close()
	features := make([]featuredelivery.FeatureRequest, 0, limit)
	for rows.Next() {
		var feature featuredelivery.FeatureRequest
		var archived sql.NullTime
		if err := rows.Scan(&feature.ID, &feature.Title, &feature.CreatedBy, &archived, &feature.CreatedAt, &feature.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan feature: %w", err)
		}
		feature.ArchivedAt = nullableTime(archived)
		features = append(features, feature)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate features: %w", err)
	}
	return features, nil
}

func (store *FeatureDeliveryStore) ArchiveFeature(ctx context.Context, id string, actorID int64, admin bool) error {
	query := `UPDATE feature_requests SET archived_at=COALESCE(archived_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=?`
	args := []any{id}
	if !admin {
		query += " AND created_by=?"
		args = append(args, actorID)
	}
	result, err := store.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("archive feature %q: %w", id, err)
	}
	return requireAffected(result)
}

func (store *FeatureDeliveryStore) TouchFeature(ctx context.Context, id string) error {
	result, err := store.db.ExecContext(ctx, `UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("touch feature %q: %w", id, err)
	}
	return requireAffected(result)
}

func (store *FeatureDeliveryStore) CreateArtifact(ctx context.Context, artifact featuredelivery.Artifact) (*featuredelivery.Artifact, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin artifact creation: %w", err)
	}
	defer tx.Rollback()
	if err := lockFeature(ctx, tx, artifact.RequestID); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0)+1 FROM feature_artifacts WHERE request_id=? AND kind=?`,
		artifact.RequestID, artifact.Kind,
	).Scan(&artifact.Version); err != nil {
		return nil, fmt.Errorf("allocate artifact version: %w", err)
	}
	if err := insertArtifact(ctx, tx, artifact); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, artifact.RequestID); err != nil {
		return nil, fmt.Errorf("touch feature for artifact: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit artifact %q: %w", artifact.ID, err)
	}
	return &artifact, nil
}

func insertArtifact(ctx context.Context, tx *sql.Tx, artifact featuredelivery.Artifact) error {
	evidenceJSON, err := json.Marshal(artifact.Evidence)
	if err != nil {
		return fmt.Errorf("marshal artifact evidence: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO feature_artifacts(
			id,request_id,kind,version,parent_artifact_id,origin,document_json,
			rendered_markdown,evidence_json,content_hash,created_by,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		artifact.ID, artifact.RequestID, artifact.Kind, artifact.Version, artifact.ParentArtifactID,
		artifact.Origin, []byte(artifact.DocumentJSON), artifact.RenderedMarkdown, evidenceJSON,
		artifact.ContentHash, artifact.CreatedBy, artifact.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert artifact %q: %w", artifact.ID, err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetArtifact(ctx context.Context, id string) (*featuredelivery.Artifact, error) {
	artifact, err := scanArtifact(store.db.QueryRowContext(ctx, artifactSelect+` WHERE a.id=? LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact %q: %w", id, err)
	}
	return &artifact, nil
}

func (store *FeatureDeliveryStore) ListArtifacts(ctx context.Context, requestID string, limit int) ([]featuredelivery.Artifact, error) {
	limit = boundedLimit(limit, 100, maxArtifactPage)
	rows, err := store.db.QueryContext(ctx,
		artifactSelect+` WHERE a.request_id=? ORDER BY a.kind,a.version DESC LIMIT ?`,
		requestID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list artifacts for feature %q: %w", requestID, err)
	}
	defer rows.Close()
	artifacts := make([]featuredelivery.Artifact, 0, limit)
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts: %w", err)
	}
	return artifacts, nil
}

const artifactSelect = `SELECT
	a.id,a.request_id,a.kind,a.version,a.parent_artifact_id,a.origin,a.document_json,
	a.rendered_markdown,a.evidence_json,a.content_hash,a.created_by,a.created_at,
	r.artifact_id,r.decision,r.comment,r.reviewer,r.created_at
	FROM feature_artifacts a
	LEFT JOIN feature_artifact_reviews r ON r.artifact_id=a.id`

func scanArtifact(row rowScanner) (featuredelivery.Artifact, error) {
	var artifact featuredelivery.Artifact
	var documentJSON, evidenceJSON []byte
	var reviewID, decision, comment sql.NullString
	var reviewer sql.NullInt64
	var reviewedAt sql.NullTime
	err := row.Scan(
		&artifact.ID, &artifact.RequestID, &artifact.Kind, &artifact.Version,
		&artifact.ParentArtifactID, &artifact.Origin, &documentJSON,
		&artifact.RenderedMarkdown, &evidenceJSON, &artifact.ContentHash,
		&artifact.CreatedBy, &artifact.CreatedAt,
		&reviewID, &decision, &comment, &reviewer, &reviewedAt,
	)
	if err != nil {
		return artifact, err
	}
	artifact.DocumentJSON = append([]byte(nil), documentJSON...)
	if err := json.Unmarshal(evidenceJSON, &artifact.Evidence); err != nil {
		return artifact, fmt.Errorf("decode evidence for artifact %q: %w", artifact.ID, err)
	}
	if reviewID.Valid {
		artifact.Review = &featuredelivery.ArtifactReview{
			ArtifactID: reviewID.String,
			Decision:   featuredelivery.ReviewDecision(decision.String),
			Comment:    comment.String,
			Reviewer:   reviewer.Int64,
			CreatedAt:  reviewedAt.Time,
		}
	}
	return artifact, nil
}

func (store *FeatureDeliveryStore) ReviewArtifact(ctx context.Context, review featuredelivery.ArtifactReview) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin artifact review: %w", err)
	}
	defer tx.Rollback()
	var requestID string
	if err := tx.QueryRowContext(ctx,
		`SELECT request_id FROM feature_artifacts WHERE id=? LIMIT 1 FOR UPDATE`,
		review.ArtifactID,
	).Scan(&requestID); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock reviewed artifact %q: %w", review.ArtifactID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_artifact_reviews(artifact_id,decision,comment,reviewer,created_at) VALUES(?,?,?,?,?)`,
		review.ArtifactID, review.Decision, review.Comment, review.Reviewer, review.CreatedAt,
	); err != nil {
		if duplicateKey(err) {
			return featuredelivery.ErrConflict
		}
		return fmt.Errorf("review artifact %q: %w", review.ArtifactID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, requestID); err != nil {
		return fmt.Errorf("touch feature after review: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit artifact review: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) CreateGenerationRun(ctx context.Context, run featuredelivery.GenerationRun) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin generation run: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_generation_runs(
			id,request_id,artifact_kind,parent_artifact_id,status,provider,model,
			requested_by,input_tokens,output_tokens,error_summary,started_at,ended_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.RequestID, run.ArtifactKind, run.ParentArtifactID, run.Status,
		run.Provider, run.Model, run.RequestedBy, run.InputTokens, run.OutputTokens,
		run.ErrorSummary, run.StartedAt, run.EndedAt,
	); err != nil {
		return fmt.Errorf("insert generation run %q: %w", run.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, run.RequestID); err != nil {
		return fmt.Errorf("touch feature after generation start: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit generation run: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) FinishGenerationRun(ctx context.Context, id, status string, inputTokens, outputTokens int64, errorSummary string) error {
	result, err := store.db.ExecContext(ctx,
		`UPDATE feature_generation_runs SET status=?,input_tokens=?,output_tokens=?,error_summary=?,ended_at=CURRENT_TIMESTAMP
		 WHERE id=? AND status='running'`,
		status, inputTokens, outputTokens, errorSummary, id,
	)
	if err != nil {
		return fmt.Errorf("finish generation run %q: %w", id, err)
	}
	return requireAffected(result)
}

func (store *FeatureDeliveryStore) InterruptGenerationRuns(ctx context.Context) error {
	_, err := store.db.ExecContext(ctx,
		`UPDATE feature_generation_runs SET status='interrupted',error_summary='service restarted during generation',ended_at=CURRENT_TIMESTAMP
		 WHERE status='running'`)
	if err != nil {
		return fmt.Errorf("interrupt generation runs: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetOwnerIdentity(ctx context.Context, userID int64) (featuredelivery.OwnerIdentity, error) {
	var identity featuredelivery.OwnerIdentity
	err := store.db.QueryRowContext(ctx,
		`SELECT id,name,email FROM users WHERE id=? LIMIT 1`, userID,
	).Scan(&identity.UserID, &identity.Name, &identity.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return identity, featuredelivery.ErrNotFound
	}
	if err != nil {
		return identity, fmt.Errorf("get owner identity %d: %w", userID, err)
	}
	return identity, nil
}

func (store *FeatureDeliveryStore) GetUserWorkspace(ctx context.Context, userID int64) (*featuredelivery.UserWorkspace, error) {
	workspace, err := scanUserWorkspace(store.db.QueryRowContext(ctx,
		`SELECT user_id,username_key,username_snapshot,created_at
		 FROM feature_user_workspaces WHERE user_id=? LIMIT 1`, userID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user workspace %d: %w", userID, err)
	}
	return &workspace, nil
}

func (store *FeatureDeliveryStore) CreateUserWorkspace(ctx context.Context, workspace featuredelivery.UserWorkspace) error {
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO feature_user_workspaces(user_id,username_key,username_snapshot,created_at) VALUES(?,?,?,?)`,
		workspace.UserID, workspace.UsernameKey, workspace.UsernameSnapshot, workspace.CreatedAt,
	)
	if err != nil {
		if duplicateKey(err) {
			return featuredelivery.ErrConflict
		}
		return fmt.Errorf("create user workspace %d: %w", workspace.UserID, err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetUserWorkspaceByKey(ctx context.Context, key string) (*featuredelivery.UserWorkspace, error) {
	workspace, err := scanUserWorkspace(store.db.QueryRowContext(ctx,
		`SELECT user_id,username_key,username_snapshot,created_at
		 FROM feature_user_workspaces WHERE username_key=? LIMIT 1`, key,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user workspace key %q: %w", key, err)
	}
	return &workspace, nil
}

func scanUserWorkspace(row rowScanner) (featuredelivery.UserWorkspace, error) {
	var workspace featuredelivery.UserWorkspace
	err := row.Scan(&workspace.UserID, &workspace.UsernameKey, &workspace.UsernameSnapshot, &workspace.CreatedAt)
	return workspace, err
}

func (store *FeatureDeliveryStore) CreateImplementation(ctx context.Context, run featuredelivery.ImplementationRun) (*featuredelivery.ImplementationRun, bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin implementation creation: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO feature_implementation_runs(
			id,request_id,client_request_id,request_hash,design_artifact_id,plan_artifact_id,
			parent_run_id,repo,base_ref,base_commit,workspace_user_id,workspace_username,
			provider,model,provider_version,network_enabled,status,worker_id,lease_expires_at,
			cancel_requested_at,provider_session_id,exit_code,error_summary,requested_by,
			started_at,ended_at,retain_until,worktree_cleaned_at,cleanup_error,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.RequestID, run.ClientRequestID, run.RequestHash, run.DesignArtifactID,
		run.PlanArtifactID, run.ParentRunID, run.Repo, run.BaseRef, run.BaseCommit,
		run.WorkspaceUserID, run.WorkspaceUsername, run.Provider, run.Model,
		run.ProviderVersion, run.NetworkEnabled, run.Status, run.WorkerID,
		run.LeaseExpiresAt, run.CancelRequestedAt, run.ProviderSessionID, run.ExitCode,
		run.ErrorSummary, run.RequestedBy, run.StartedAt, run.EndedAt, run.RetainUntil,
		run.WorktreeCleanedAt, run.CleanupError, run.CreatedAt,
	)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, run.RequestID); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("commit implementation creation: %w", err)
		}
		return &run, true, nil
	}
	if !duplicateKey(err) {
		return nil, false, fmt.Errorf("create implementation run %q: %w", run.ID, err)
	}
	if err := tx.Rollback(); err != nil {
		return nil, false, fmt.Errorf("rollback duplicate implementation creation: %w", err)
	}
	existing, getErr := store.getImplementationByClientRequest(ctx, run.RequestedBy, run.ClientRequestID)
	if getErr != nil {
		return nil, false, getErr
	}
	if existing.RequestHash != run.RequestHash {
		return nil, false, featuredelivery.ErrConflict
	}
	return existing, false, nil
}

func (store *FeatureDeliveryStore) getImplementationByClientRequest(ctx context.Context, requestedBy int64, clientRequestID string) (*featuredelivery.ImplementationRun, error) {
	run, err := scanImplementation(store.db.QueryRowContext(ctx,
		implementationSelect+` WHERE r.requested_by=? AND r.client_request_id=? LIMIT 1`,
		requestedBy, clientRequestID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get implementation by idempotency key: %w", err)
	}
	return &run, nil
}

func (store *FeatureDeliveryStore) GetImplementation(ctx context.Context, id string) (*featuredelivery.ImplementationRun, error) {
	run, err := scanImplementation(store.db.QueryRowContext(ctx, implementationSelect+` WHERE r.id=? LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get implementation %q: %w", id, err)
	}
	return &run, nil
}

func (store *FeatureDeliveryStore) ListImplementations(ctx context.Context, requestID string, cursor featuredelivery.RunCursor, limit int) ([]featuredelivery.ImplementationRun, error) {
	limit = boundedLimit(limit, 20, maxRunPage)
	query := implementationSelect + ` WHERE r.request_id=?`
	args := []any{requestID}
	if !cursor.CreatedAt.IsZero() {
		query += ` AND (r.created_at < ? OR (r.created_at = ? AND r.id < ?))`
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	query += ` ORDER BY r.created_at DESC,r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list implementations: %w", err)
	}
	defer rows.Close()
	runs := make([]featuredelivery.ImplementationRun, 0, limit)
	for rows.Next() {
		run, err := scanImplementation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan implementation: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate implementations: %w", err)
	}
	return runs, nil
}

const implementationSelect = `SELECT
	r.id,r.request_id,r.client_request_id,r.request_hash,r.design_artifact_id,r.plan_artifact_id,
	r.parent_run_id,r.repo,r.base_ref,r.base_commit,r.workspace_user_id,r.workspace_username,
	r.provider,r.model,r.provider_version,r.network_enabled,r.status,r.worker_id,r.lease_expires_at,
	r.cancel_requested_at,r.provider_session_id,r.exit_code,r.error_summary,r.requested_by,
	r.started_at,r.ended_at,r.retain_until,r.worktree_cleaned_at,r.cleanup_error,r.created_at,
	c.run_id,c.worktree_head,c.patch_rel_path,c.patch_sha256,c.patch_bytes,c.files_changed,
	c.additions,c.deletions,c.files_json,c.validation_results_json,c.provider_summary,c.created_at,
	v.run_id,v.decision,v.comment,v.reviewer,v.created_at
	FROM feature_implementation_runs r
	LEFT JOIN feature_change_sets c ON c.run_id=r.id
	LEFT JOIN feature_change_reviews v ON v.run_id=r.id`

func scanImplementation(row rowScanner) (featuredelivery.ImplementationRun, error) {
	var run featuredelivery.ImplementationRun
	var lease, cancel, started, ended, retain, cleaned sql.NullTime
	var exitCode sql.NullInt64
	var changeRunID, worktreeHead, patchPath, patchHash, filesJSON, validationsJSON, providerSummary sql.NullString
	var patchBytes sql.NullInt64
	var filesChanged, additions, deletions sql.NullInt64
	var changeCreated sql.NullTime
	var reviewRunID, reviewDecision, reviewComment sql.NullString
	var reviewer sql.NullInt64
	var reviewCreated sql.NullTime
	err := row.Scan(
		&run.ID, &run.RequestID, &run.ClientRequestID, &run.RequestHash,
		&run.DesignArtifactID, &run.PlanArtifactID, &run.ParentRunID, &run.Repo,
		&run.BaseRef, &run.BaseCommit, &run.WorkspaceUserID, &run.WorkspaceUsername,
		&run.Provider, &run.Model, &run.ProviderVersion, &run.NetworkEnabled,
		&run.Status, &run.WorkerID, &lease, &cancel, &run.ProviderSessionID,
		&exitCode, &run.ErrorSummary, &run.RequestedBy, &started, &ended, &retain,
		&cleaned, &run.CleanupError, &run.CreatedAt,
		&changeRunID, &worktreeHead, &patchPath, &patchHash, &patchBytes,
		&filesChanged, &additions, &deletions, &filesJSON, &validationsJSON,
		&providerSummary, &changeCreated,
		&reviewRunID, &reviewDecision, &reviewComment, &reviewer, &reviewCreated,
	)
	if err != nil {
		return run, err
	}
	run.LeaseExpiresAt = nullableTime(lease)
	run.CancelRequestedAt = nullableTime(cancel)
	run.StartedAt = nullableTime(started)
	run.EndedAt = nullableTime(ended)
	run.RetainUntil = nullableTime(retain)
	run.WorktreeCleanedAt = nullableTime(cleaned)
	if exitCode.Valid {
		value := int(exitCode.Int64)
		run.ExitCode = &value
	}
	if changeRunID.Valid {
		change := &featuredelivery.ChangeSet{
			RunID:           changeRunID.String,
			WorktreeHead:    worktreeHead.String,
			PatchRelPath:    patchPath.String,
			PatchSHA256:     patchHash.String,
			PatchBytes:      patchBytes.Int64,
			FilesChanged:    int(filesChanged.Int64),
			Additions:       int(additions.Int64),
			Deletions:       int(deletions.Int64),
			ProviderSummary: providerSummary.String,
			CreatedAt:       changeCreated.Time,
		}
		if err := json.Unmarshal([]byte(filesJSON.String), &change.Files); err != nil {
			return run, fmt.Errorf("decode changed files for run %q: %w", run.ID, err)
		}
		if err := json.Unmarshal([]byte(validationsJSON.String), &change.ValidationResults); err != nil {
			return run, fmt.Errorf("decode validations for run %q: %w", run.ID, err)
		}
		run.ChangeSet = change
	}
	if reviewRunID.Valid {
		run.Review = &featuredelivery.ChangeReview{
			RunID:     reviewRunID.String,
			Decision:  featuredelivery.ReviewDecision(reviewDecision.String),
			Comment:   reviewComment.String,
			Reviewer:  reviewer.Int64,
			CreatedAt: reviewCreated.Time,
		}
	}
	return run, nil
}

func (store *FeatureDeliveryStore) ClaimNextImplementation(ctx context.Context, workerID string, leaseExpiresAt time.Time) (*featuredelivery.ImplementationRun, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin implementation claim: %w", err)
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM feature_implementation_runs
		 WHERE status='queued' ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, featuredelivery.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select implementation claim: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE feature_implementation_runs
		 SET status='preparing',worker_id=?,lease_expires_at=?,started_at=COALESCE(started_at,CURRENT_TIMESTAMP)
		 WHERE id=? AND status='queued'`,
		workerID, leaseExpiresAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("claim implementation %q: %w", id, err)
	}
	if err := requireAffected(result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit implementation claim: %w", err)
	}
	return store.GetImplementation(ctx, id)
}

func (store *FeatureDeliveryStore) TransitionImplementation(ctx context.Context, id, workerID string, from, to featuredelivery.RunStatus, update featuredelivery.RunUpdate) error {
	query := `UPDATE feature_implementation_runs SET status=?,provider_version=?,provider_session_id=?,exit_code=?,error_summary=?`
	args := []any{to, update.ProviderVersion, update.ProviderSessionID, update.ExitCode, update.ErrorSummary}
	if featuredelivery.IsTerminalRun(to) {
		query += `,ended_at=CURRENT_TIMESTAMP,retain_until=?,lease_expires_at=NULL`
		args = append(args, update.RetainUntil)
	}
	query += ` WHERE id=? AND status=?`
	args = append(args, id, from)
	if workerID != "" {
		query += ` AND worker_id=?`
		args = append(args, workerID)
	}
	result, err := store.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("transition implementation %q from %s to %s: %w", id, from, to, err)
	}
	if err := requireAffected(result); err != nil {
		return featuredelivery.ErrConflict
	}
	return nil
}

func (store *FeatureDeliveryStore) RenewImplementationLease(ctx context.Context, id, workerID string, expiresAt time.Time) (bool, bool, error) {
	result, err := store.db.ExecContext(ctx,
		`UPDATE feature_implementation_runs SET lease_expires_at=?
		 WHERE id=? AND worker_id=? AND status IN ('preparing','running','validating')`,
		expiresAt, id, workerID,
	)
	if err != nil {
		return false, false, fmt.Errorf("renew implementation lease %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, false, fmt.Errorf("read lease result: %w", err)
	}
	if affected == 0 {
		return false, false, nil
	}
	var cancelRequested bool
	if err := store.db.QueryRowContext(ctx,
		`SELECT cancel_requested_at IS NOT NULL FROM feature_implementation_runs WHERE id=? LIMIT 1`,
		id,
	).Scan(&cancelRequested); err != nil {
		return false, false, fmt.Errorf("read cancellation intent %q: %w", id, err)
	}
	return true, cancelRequested, nil
}

func (store *FeatureDeliveryStore) RequestCancel(ctx context.Context, id string) (featuredelivery.RunStatus, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin cancellation: %w", err)
	}
	defer tx.Rollback()
	var status featuredelivery.RunStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM feature_implementation_runs WHERE id=? LIMIT 1 FOR UPDATE`, id,
	).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return "", featuredelivery.ErrNotFound
	} else if err != nil {
		return "", fmt.Errorf("lock cancellation run %q: %w", id, err)
	}
	switch status {
	case featuredelivery.RunQueued:
		if _, err := tx.ExecContext(ctx,
			`UPDATE feature_implementation_runs
			 SET status='cancelled',cancel_requested_at=CURRENT_TIMESTAMP,ended_at=CURRENT_TIMESTAMP
			 WHERE id=? AND status='queued'`, id,
		); err != nil {
			return "", fmt.Errorf("cancel queued run %q: %w", id, err)
		}
		status = featuredelivery.RunCancelled
	case featuredelivery.RunPreparing, featuredelivery.RunRunning, featuredelivery.RunValidating:
		if _, err := tx.ExecContext(ctx,
			`UPDATE feature_implementation_runs SET cancel_requested_at=COALESCE(cancel_requested_at,CURRENT_TIMESTAMP)
			 WHERE id=?`, id,
		); err != nil {
			return "", fmt.Errorf("request cancellation %q: %w", id, err)
		}
	default:
		return status, featuredelivery.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit cancellation %q: %w", id, err)
	}
	return status, nil
}

func (store *FeatureDeliveryStore) InterruptActiveImplementations(ctx context.Context, now, retainUntil time.Time) ([]string, error) {
	rows, err := store.db.QueryContext(ctx,
		`SELECT id FROM feature_implementation_runs
		 WHERE status IN ('preparing','running','validating')
		   AND (lease_expires_at IS NULL OR lease_expires_at<=?)
		 ORDER BY created_at,id LIMIT 100`,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("list active implementations: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0, 100)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired implementation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired implementations: %w", err)
	}
	interrupted := ids[:0]
	for _, id := range ids {
		result, err := store.db.ExecContext(ctx,
			`UPDATE feature_implementation_runs
			 SET status='interrupted',error_summary='worker lease expired',
			     ended_at=?,retain_until=?,lease_expires_at=NULL
			 WHERE id=? AND status IN ('preparing','running','validating')
			   AND (lease_expires_at IS NULL OR lease_expires_at<=?)`,
			now, retainUntil, id, now,
		)
		if err != nil {
			return nil, fmt.Errorf("interrupt expired implementation %q: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read interruption result %q: %w", id, err)
		}
		if affected == 1 {
			interrupted = append(interrupted, id)
		}
	}
	return interrupted, nil
}

func (store *FeatureDeliveryStore) AppendRunEvent(ctx context.Context, event featuredelivery.RunEvent) (*featuredelivery.RunEvent, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin run event: %w", err)
	}
	defer tx.Rollback()
	if err := lockImplementation(ctx, tx, event.RunID); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM feature_run_events WHERE run_id=?`,
		event.RunID,
	).Scan(&event.Seq); err != nil {
		return nil, fmt.Errorf("allocate event sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_run_events(run_id,seq,kind,summary,detail_json,created_at) VALUES(?,?,?,?,?,?)`,
		event.RunID, event.Seq, event.Kind, event.Summary, nullableJSON(event.Detail), event.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert event for run %q: %w", event.RunID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit run event: %w", err)
	}
	return &event, nil
}

func (store *FeatureDeliveryStore) ListRunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]featuredelivery.RunEvent, error) {
	limit = boundedLimit(limit, 100, maxEventPage)
	rows, err := store.db.QueryContext(ctx,
		`SELECT run_id,seq,kind,summary,detail_json,created_at
		 FROM feature_run_events WHERE run_id=? AND seq>? ORDER BY seq LIMIT ?`,
		runID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list events for run %q: %w", runID, err)
	}
	defer rows.Close()
	events := make([]featuredelivery.RunEvent, 0, limit)
	for rows.Next() {
		var event featuredelivery.RunEvent
		var detail []byte
		if err := rows.Scan(&event.RunID, &event.Seq, &event.Kind, &event.Summary, &detail, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan run event: %w", err)
		}
		event.Detail = append([]byte(nil), detail...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run events: %w", err)
	}
	return events, nil
}

func (store *FeatureDeliveryStore) SaveChangeSetAndFinish(ctx context.Context, change featuredelivery.ChangeSet, terminalStatus featuredelivery.RunStatus, errorSummary string, retainUntil time.Time) error {
	if terminalStatus != featuredelivery.RunSucceeded && terminalStatus != featuredelivery.RunFailed {
		return fmt.Errorf("change set terminal status must be succeeded or failed")
	}
	filesJSON, err := json.Marshal(change.Files)
	if err != nil {
		return fmt.Errorf("marshal changed files: %w", err)
	}
	validationsJSON, err := json.Marshal(change.ValidationResults)
	if err != nil {
		return fmt.Errorf("marshal validation results: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin change set save: %w", err)
	}
	defer tx.Rollback()
	if err := lockImplementation(ctx, tx, change.RunID); err != nil {
		return err
	}
	var currentStatus featuredelivery.RunStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM feature_implementation_runs WHERE id=? LIMIT 1`, change.RunID,
	).Scan(&currentStatus); err != nil {
		return fmt.Errorf("read run status for change set: %w", err)
	}
	if currentStatus != featuredelivery.RunValidating {
		return featuredelivery.ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_change_sets(
			run_id,worktree_head,patch_rel_path,patch_sha256,patch_bytes,files_changed,
			additions,deletions,files_json,validation_results_json,provider_summary,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		change.RunID, change.WorktreeHead, change.PatchRelPath, change.PatchSHA256,
		change.PatchBytes, change.FilesChanged, change.Additions, change.Deletions,
		filesJSON, validationsJSON, change.ProviderSummary, change.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert change set for run %q: %w", change.RunID, err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE feature_implementation_runs
			 SET status=?,error_summary=?,ended_at=CURRENT_TIMESTAMP,retain_until=?,lease_expires_at=NULL
			 WHERE id=? AND status='validating'`,
		terminalStatus, errorSummary, retainUntil, change.RunID,
	)
	if err != nil {
		return fmt.Errorf("complete implementation %q: %w", change.RunID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read completed implementation rows affected: %w", err)
	}
	if affected != 1 {
		return featuredelivery.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit change set: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) GetChangeSet(ctx context.Context, runID string) (*featuredelivery.ChangeSet, error) {
	run, err := store.GetImplementation(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.ChangeSet == nil {
		return nil, featuredelivery.ErrNotFound
	}
	return run.ChangeSet, nil
}

func (store *FeatureDeliveryStore) ReviewChangeSet(ctx context.Context, review featuredelivery.ChangeReview) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin change review: %w", err)
	}
	defer tx.Rollback()
	var requestID string
	var status featuredelivery.RunStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT request_id,status FROM feature_implementation_runs WHERE id=? LIMIT 1 FOR UPDATE`,
		review.RunID,
	).Scan(&requestID, &status); errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock reviewed change set %q: %w", review.RunID, err)
	}
	if status != featuredelivery.RunSucceeded {
		return featuredelivery.ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feature_change_reviews(run_id,decision,comment,reviewer,created_at) VALUES(?,?,?,?,?)`,
		review.RunID, review.Decision, review.Comment, review.Reviewer, review.CreatedAt,
	); err != nil {
		if duplicateKey(err) {
			return featuredelivery.ErrConflict
		}
		return fmt.Errorf("review change set %q: %w", review.RunID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, requestID); err != nil {
		return fmt.Errorf("touch feature after change review: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit change review: %w", err)
	}
	return nil
}

func (store *FeatureDeliveryStore) ListExpiredWorktrees(ctx context.Context, now time.Time, limit int) ([]featuredelivery.ImplementationRun, error) {
	limit = boundedLimit(limit, 20, maxRunPage)
	rows, err := store.db.QueryContext(ctx,
		implementationSelect+`
		 WHERE r.status IN ('succeeded','failed','cancelled','interrupted')
		   AND r.retain_until IS NOT NULL AND r.retain_until<=?
		   AND r.worktree_cleaned_at IS NULL
		 ORDER BY r.retain_until,r.id LIMIT ?`,
		now, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list expired worktrees: %w", err)
	}
	defer rows.Close()
	runs := make([]featuredelivery.ImplementationRun, 0, limit)
	for rows.Next() {
		run, err := scanImplementation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expired worktree: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired worktrees: %w", err)
	}
	return runs, nil
}

func (store *FeatureDeliveryStore) MarkWorktreeCleaned(ctx context.Context, runID, cleanupError string) error {
	query := `UPDATE feature_implementation_runs SET cleanup_error=?`
	args := []any{cleanupError}
	if cleanupError == "" {
		query += `,worktree_cleaned_at=CURRENT_TIMESTAMP`
	}
	query += ` WHERE id=? AND status IN ('succeeded','failed','cancelled','interrupted')`
	args = append(args, runID)
	result, err := store.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("mark worktree cleaned %q: %w", runID, err)
	}
	return requireAffected(result)
}

func lockFeature(ctx context.Context, tx *sql.Tx, id string) error {
	var locked string
	err := tx.QueryRowContext(ctx, `SELECT id FROM feature_requests WHERE id=? LIMIT 1 FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock feature %q: %w", id, err)
	}
	return nil
}

func lockImplementation(ctx context.Context, tx *sql.Tx, id string) error {
	var locked string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM feature_implementation_runs WHERE id=? LIMIT 1 FOR UPDATE`, id,
	).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return featuredelivery.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock implementation %q: %w", id, err)
	}
	return nil
}

func boundedLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func nullableTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return []byte(value)
}

func requireAffected(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if affected == 0 {
		return featuredelivery.ErrNotFound
	}
	return nil
}

func duplicateKey(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

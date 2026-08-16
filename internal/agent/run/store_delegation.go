package run

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

const (
	DelegationReportArtifactKind       = "delegation_report"
	DelegationVerificationArtifactKind = "delegation_verification"
)

// GetDelegationReport resolves one model-visible report reference inside its
// parent-owned delegation batch and verifies the persisted artifact projection.
func (rs *Store) GetDelegationReport(
	ctx context.Context,
	parentRunID,
	delegationID,
	reportRef string,
) (DelegationArtifact, error) {
	if strings.TrimSpace(parentRunID) == "" ||
		strings.TrimSpace(delegationID) == "" ||
		strings.TrimSpace(reportRef) == "" {
		return DelegationArtifact{}, fmt.Errorf("invalid delegation report lookup")
	}
	artifactID := delegationReportArtifactID(reportRef)
	row := rs.db.QueryRowContext(
		ctx,
		`SELECT t.child_run_id,
			a.artifact_id,a.run_id,a.kind,a.schema_id,a.schema_version,a.content_hash,a.content
		 FROM agent_delegation_tasks t
		 JOIN agent_run_artifacts a ON a.artifact_id=t.report_artifact_id
		 WHERE t.parent_run_id=? AND t.delegation_id=? AND
			t.report_artifact_id=? AND t.admitted=TRUE AND
			t.settled_usage_json IS NOT NULL`,
		parentRunID,
		delegationID,
		artifactID,
	)
	var (
		childRunID string
		artifact   DelegationArtifact
	)
	if err := row.Scan(
		&childRunID,
		&artifact.ID,
		&artifact.RunID,
		&artifact.Kind,
		&artifact.Schema.ID,
		&artifact.Schema.Version,
		&artifact.ContentHash,
		&artifact.Content,
	); err != nil {
		return DelegationArtifact{}, err
	}
	if artifact.ID != artifactID ||
		artifact.RunID != childRunID ||
		artifact.Kind != DelegationReportArtifactKind ||
		artifact.Schema.ID != "delegation.report" ||
		artifact.Schema.Version <= 0 {
		return DelegationArtifact{}, ErrDelegationTaskConflict
	}
	sum := sha256.Sum256(artifact.Content)
	if fmt.Sprintf("%x", sum[:]) != artifact.ContentHash {
		return DelegationArtifact{}, ErrDelegationTaskConflict
	}
	var report agentapi.DelegationReport
	if err := json.Unmarshal(artifact.Content, &report); err != nil {
		return DelegationArtifact{}, fmt.Errorf("decode delegation report: %w", err)
	}
	if report.ReportID != reportRef || report.RunID != childRunID {
		return DelegationArtifact{}, ErrDelegationTaskConflict
	}
	artifact.Content = append([]byte(nil), artifact.Content...)
	return artifact, nil
}

// GetDelegationTask returns the persisted admission and optional report artifact.
func (rs *Store) GetDelegationTask(
	ctx context.Context,
	parentRunID,
	delegationID string,
	taskIndex int,
) (DelegationTaskRecord, *DelegationArtifact, error) {
	if strings.TrimSpace(parentRunID) == "" ||
		strings.TrimSpace(delegationID) == "" ||
		taskIndex < 0 {
		return DelegationTaskRecord{}, nil, fmt.Errorf("invalid delegation task lookup")
	}
	row := rs.db.QueryRowContext(
		ctx,
		`SELECT
			t.parent_run_id,t.delegation_id,t.task_index,t.child_run_id,
			t.capability_id,t.capability_version,t.capability_content_hash,t.objective_hash,
			t.admitted,t.rejection_code,t.reservation_json,t.settled_usage_json,t.report_artifact_id,
			a.artifact_id,a.run_id,a.kind,a.schema_id,a.schema_version,a.content_hash,a.content
		 FROM agent_delegation_tasks t
		 LEFT JOIN agent_run_artifacts a ON a.artifact_id=t.report_artifact_id
		 WHERE t.parent_run_id=? AND t.delegation_id=? AND t.task_index=?`,
		parentRunID,
		delegationID,
		taskIndex,
	)
	var (
		task           DelegationTaskRecord
		reservationRaw []byte
		settledRaw     []byte
		artifactID     sql.NullString
		artifactRunID  sql.NullString
		artifactKind   sql.NullString
		schemaID       sql.NullString
		schemaVersion  sql.NullInt64
		contentHash    sql.NullString
		content        []byte
	)
	if err := row.Scan(
		&task.ParentRunID,
		&task.DelegationID,
		&task.TaskIndex,
		&task.ChildRunID,
		&task.Capability.ID,
		&task.Capability.Version,
		&task.CapabilityHash,
		&task.ObjectiveHash,
		&task.Admitted,
		&task.RejectionCode,
		&reservationRaw,
		&settledRaw,
		&task.ReportArtifactID,
		&artifactID,
		&artifactRunID,
		&artifactKind,
		&schemaID,
		&schemaVersion,
		&contentHash,
		&content,
	); err != nil {
		return DelegationTaskRecord{}, nil, err
	}
	if task.Admitted {
		if err := json.Unmarshal(reservationRaw, &task.Reservation); err != nil {
			return DelegationTaskRecord{}, nil,
				fmt.Errorf("decode delegation reservation: %w", err)
		}
	}
	if len(settledRaw) > 0 {
		var usage agentapi.Usage
		if err := json.Unmarshal(settledRaw, &usage); err != nil {
			return DelegationTaskRecord{}, nil,
				fmt.Errorf("decode delegation settlement: %w", err)
		}
		task.SettledUsage = &usage
	}
	if !artifactID.Valid {
		task.Existing = true
		return task, nil, nil
	}
	task.Existing = true
	return task, &DelegationArtifact{
		ID: artifactID.String, RunID: artifactRunID.String,
		Kind: artifactKind.String,
		Schema: agentapi.SchemaRef{
			ID: schemaID.String, Version: schemaVersion.Int64,
		},
		ContentHash: contentHash.String,
		Content:     append([]byte(nil), content...),
	}, nil
}

// ReserveDelegationBatch atomically admits an idempotent batch against the parent budget.
func (rs *Store) ReserveDelegationBatch(
	ctx context.Context,
	admission DelegationAdmission,
) ([]DelegationTaskRecord, error) {
	if err := validateDelegationAdmission(admission); err != nil {
		return nil, err
	}
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		parentStatus Status
		parentTokens int64
		parentCost   int64
		limitsRaw    []byte
	)
	if err := tx.QueryRowContext(
		ctx,
		`SELECT status,total_tokens,cost_micros,run_limits_json
		 FROM agent_runs WHERE id=? FOR UPDATE`,
		admission.ParentRunID,
	).Scan(&parentStatus, &parentTokens, &parentCost, &limitsRaw); err != nil {
		return nil, err
	}
	if parentStatus != StatusRunning && parentStatus != StatusPaused {
		return nil, ErrNotActive
	}
	var parentLimits agentapi.RunLimits
	if len(limitsRaw) > 0 {
		if err := json.Unmarshal(limitsRaw, &parentLimits); err != nil {
			return nil, fmt.Errorf("decode parent run limits: %w", err)
		}
	}

	persisted, err := loadDelegationTasksForUpdate(ctx, tx, admission.ParentRunID)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]DelegationTaskRecord, len(persisted))
	admittedChildren := 0
	var settledTokens, settledCost, outstandingTokens, outstandingCost int64
	for _, task := range persisted {
		byKey[delegationTaskKey(task.DelegationID, task.TaskIndex)] = task
		if !task.Admitted {
			continue
		}
		admittedChildren++
		if task.SettledUsage != nil {
			settledTokens += task.SettledUsage.TotalTokens
			settledCost += task.SettledUsage.CostMicros
			continue
		}
		outstandingTokens += task.Reservation.ReservedTokens
		outstandingCost += task.Reservation.ReservedCostMicros
	}

	results := make([]DelegationTaskRecord, len(admission.Reservations))
	newReservations := make([]DelegationReservation, 0, len(admission.Reservations))
	seen := make(map[int]struct{}, len(admission.Reservations))
	var newTokens, newCost int64
	for index, reservation := range admission.Reservations {
		if _, duplicate := seen[reservation.TaskIndex]; duplicate {
			return nil, fmt.Errorf("duplicate delegation task index %d", reservation.TaskIndex)
		}
		seen[reservation.TaskIndex] = struct{}{}
		key := delegationTaskKey(admission.DelegationID, reservation.TaskIndex)
		if existing, ok := byKey[key]; ok {
			if err := sameDelegationReservation(existing, reservation); err != nil {
				return nil, err
			}
			existing.Existing = true
			results[index] = existing
			byKey[key] = existing
			continue
		}
		newReservations = append(newReservations, reservation)
		newTokens += reservation.ReservedTokens
		newCost += reservation.ReservedCostMicros
	}
	if admission.MaxChildren > 0 &&
		admittedChildren+len(newReservations) > admission.MaxChildren {
		return nil, ErrDelegationChildLimit
	}

	tokenCeiling := minPositiveInt64(admission.MaxTotalTokens, parentLimits.MaxTotalTokens)
	if tokenCeiling > 0 &&
		parentTokens+settledTokens+outstandingTokens+newTokens+admission.ParentAnswerReserve > tokenCeiling {
		return nil, ErrDelegationBudgetInsufficient
	}
	costCeiling := minPositiveInt64(admission.MaxTotalCostMicros, parentLimits.MaxCostMicros)
	if costCeiling > 0 &&
		parentCost+settledCost+outstandingCost+newCost > costCeiling {
		return nil, ErrDelegationBudgetInsufficient
	}

	createdAt := store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano))
	for _, reservation := range newReservations {
		reservationRaw, err := json.Marshal(reservation)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO agent_delegation_tasks(
				parent_run_id,delegation_id,task_index,child_run_id,
				capability_id,capability_version,capability_content_hash,objective_hash,
				admitted,rejection_code,reservation_json,created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			admission.ParentRunID,
			admission.DelegationID,
			reservation.TaskIndex,
			reservation.ChildRunID,
			reservation.Capability.ID,
			reservation.Capability.Version,
			reservation.CapabilityHash,
			reservation.ObjectiveHash,
			true,
			"",
			reservationRaw,
			createdAt,
		); err != nil {
			return nil, err
		}
		record := recordFromReservation(reservation)
		byKey[delegationTaskKey(admission.DelegationID, reservation.TaskIndex)] = record
	}
	for index, reservation := range admission.Reservations {
		results[index] = byKey[delegationTaskKey(admission.DelegationID, reservation.TaskIndex)]
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

// RejectDelegationTask records a stable rejection without consuming a reservation.
func (rs *Store) RejectDelegationTask(
	ctx context.Context,
	rejection DelegationRejection,
) (DelegationTaskRecord, error) {
	if strings.TrimSpace(rejection.ParentRunID) == "" ||
		strings.TrimSpace(rejection.DelegationID) == "" ||
		rejection.TaskIndex < 0 ||
		strings.TrimSpace(rejection.Code) == "" {
		return DelegationTaskRecord{}, fmt.Errorf("invalid delegation rejection")
	}
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return DelegationTaskRecord{}, err
	}
	defer tx.Rollback()

	existing, err := loadDelegationTaskForUpdate(
		ctx,
		tx,
		rejection.ParentRunID,
		rejection.DelegationID,
		rejection.TaskIndex,
	)
	if err == nil {
		if existing.Admitted ||
			existing.Capability.ID != rejection.Capability.ID ||
			existing.Capability.Version != rejection.Capability.Version ||
			existing.CapabilityHash != rejection.CapabilityHash ||
			existing.ObjectiveHash != rejection.ObjectiveHash ||
			existing.RejectionCode != rejection.Code {
			return DelegationTaskRecord{}, ErrDelegationTaskConflict
		}
		if err := tx.Commit(); err != nil {
			return DelegationTaskRecord{}, err
		}
		existing.Existing = true
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DelegationTaskRecord{}, err
	}
	emptyReservation := []byte(`{}`)
	now := store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO agent_delegation_tasks(
			parent_run_id,delegation_id,task_index,child_run_id,
			capability_id,capability_version,capability_content_hash,objective_hash,
			admitted,rejection_code,reservation_json,created_at,settled_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rejection.ParentRunID,
		rejection.DelegationID,
		rejection.TaskIndex,
		"",
		rejection.Capability.ID,
		rejection.Capability.Version,
		rejection.CapabilityHash,
		rejection.ObjectiveHash,
		false,
		rejection.Code,
		emptyReservation,
		now,
		now,
	); err != nil {
		return DelegationTaskRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return DelegationTaskRecord{}, err
	}
	return DelegationTaskRecord{
		ParentRunID:    rejection.ParentRunID,
		DelegationID:   rejection.DelegationID,
		TaskIndex:      rejection.TaskIndex,
		Capability:     rejection.Capability,
		CapabilityHash: rejection.CapabilityHash,
		ObjectiveHash:  rejection.ObjectiveHash,
		RejectionCode:  rejection.Code,
	}, nil
}

// LinkDelegationChild fills the stable child identity before runtime execution.
func (rs *Store) LinkDelegationChild(
	ctx context.Context,
	parentRunID,
	delegationID string,
	taskIndex int,
	childRunID string,
) error {
	if parentRunID == "" || delegationID == "" || taskIndex < 0 || childRunID == "" {
		return fmt.Errorf("invalid delegation child link")
	}
	result, err := rs.db.ExecContext(
		ctx,
		`UPDATE agent_delegation_tasks
		 SET child_run_id=?
		 WHERE parent_run_id=? AND delegation_id=? AND task_index=?
		   AND admitted=TRUE AND child_run_id IN ('',?)`,
		childRunID,
		parentRunID,
		delegationID,
		taskIndex,
		childRunID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrDelegationTaskConflict
	}
	return nil
}

// SettleDelegationTask persists a report artifact before releasing the reservation.
func (rs *Store) SettleDelegationTask(
	ctx context.Context,
	settlement DelegationSettlement,
) (DelegationTaskRecord, error) {
	if settlement.ParentRunID == "" || settlement.DelegationID == "" ||
		settlement.TaskIndex < 0 || settlement.ChildRunID == "" {
		return DelegationTaskRecord{}, fmt.Errorf("invalid delegation settlement")
	}
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return DelegationTaskRecord{}, err
	}
	defer tx.Rollback()

	task, err := loadDelegationTaskForUpdate(
		ctx,
		tx,
		settlement.ParentRunID,
		settlement.DelegationID,
		settlement.TaskIndex,
	)
	if err != nil {
		return DelegationTaskRecord{}, err
	}
	if !task.Admitted {
		return DelegationTaskRecord{}, ErrDelegationNotAdmitted
	}
	if task.ChildRunID != settlement.ChildRunID {
		return DelegationTaskRecord{}, ErrDelegationTaskConflict
	}
	if task.SettledUsage != nil {
		evidenceArtifactID, err := existingArtifactID(
			ctx,
			tx,
			settlement.ChildRunID,
			EvidenceLedgerArtifactKind,
		)
		if err != nil {
			return DelegationTaskRecord{}, err
		}
		if *task.SettledUsage != settlement.Usage ||
			artifactID(settlement.Artifact) != task.ReportArtifactID ||
			artifactID(settlement.EvidenceArtifact) != evidenceArtifactID {
			return DelegationTaskRecord{}, ErrDelegationTaskConflict
		}
		if err := tx.Commit(); err != nil {
			return DelegationTaskRecord{}, err
		}
		return task, nil
	}
	if task.Reservation.ReservedTokens > 0 &&
		settlement.Usage.TotalTokens > task.Reservation.ReservedTokens ||
		task.Reservation.ReservedCostMicros > 0 &&
			settlement.Usage.CostMicros > task.Reservation.ReservedCostMicros {
		return DelegationTaskRecord{}, ErrDelegationAccounting
	}

	reportArtifactID := ""
	if settlement.Artifact != nil {
		if err := validateDelegationArtifact(*settlement.Artifact, settlement.ChildRunID); err != nil {
			return DelegationTaskRecord{}, err
		}
		reportArtifactID = settlement.Artifact.ID
		if err := insertRunArtifact(ctx, tx, *settlement.Artifact); err != nil {
			return DelegationTaskRecord{}, err
		}
	}
	if settlement.EvidenceArtifact != nil {
		if _, err := decodeEvidenceLedger(*settlement.EvidenceArtifact); err != nil {
			return DelegationTaskRecord{}, err
		}
		if settlement.EvidenceArtifact.RunID != settlement.ChildRunID {
			return DelegationTaskRecord{}, ErrEvidenceLedgerConflict
		}
		if err := insertRunArtifact(
			ctx,
			tx,
			*settlement.EvidenceArtifact,
		); err != nil {
			return DelegationTaskRecord{}, err
		}
	}
	usageRaw, err := json.Marshal(settlement.Usage)
	if err != nil {
		return DelegationTaskRecord{}, err
	}
	settledAt := store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano))
	result, err := tx.ExecContext(
		ctx,
		`UPDATE agent_delegation_tasks
		 SET settled_usage_json=?,report_artifact_id=?,settled_at=?
		 WHERE parent_run_id=? AND delegation_id=? AND task_index=?
		   AND settled_usage_json IS NULL`,
		usageRaw,
		reportArtifactID,
		settledAt,
		settlement.ParentRunID,
		settlement.DelegationID,
		settlement.TaskIndex,
	)
	if err != nil {
		return DelegationTaskRecord{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return DelegationTaskRecord{}, err
	}
	if affected != 1 {
		return DelegationTaskRecord{}, ErrDelegationTaskConflict
	}
	if err := tx.Commit(); err != nil {
		return DelegationTaskRecord{}, err
	}
	task.SettledUsage = &settlement.Usage
	task.ReportArtifactID = reportArtifactID
	return task, nil
}

func loadDelegationTasksForUpdate(
	ctx context.Context,
	tx *sql.Tx,
	parentRunID string,
) ([]DelegationTaskRecord, error) {
	rows, err := tx.QueryContext(
		ctx,
		`SELECT parent_run_id,delegation_id,task_index,child_run_id,
			capability_id,capability_version,capability_content_hash,objective_hash,
			admitted,rejection_code,reservation_json,settled_usage_json,report_artifact_id
		 FROM agent_delegation_tasks WHERE parent_run_id=? FOR UPDATE`,
		parentRunID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []DelegationTaskRecord
	for rows.Next() {
		task, err := scanDelegationTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func loadDelegationTaskForUpdate(
	ctx context.Context,
	tx *sql.Tx,
	parentRunID,
	delegationID string,
	taskIndex int,
) (DelegationTaskRecord, error) {
	return scanDelegationTask(tx.QueryRowContext(
		ctx,
		`SELECT parent_run_id,delegation_id,task_index,child_run_id,
			capability_id,capability_version,capability_content_hash,objective_hash,
			admitted,rejection_code,reservation_json,settled_usage_json,report_artifact_id
		 FROM agent_delegation_tasks
		 WHERE parent_run_id=? AND delegation_id=? AND task_index=? FOR UPDATE`,
		parentRunID,
		delegationID,
		taskIndex,
	))
}

func scanDelegationTask(row rowScanner) (DelegationTaskRecord, error) {
	var (
		task           DelegationTaskRecord
		reservationRaw []byte
		settledRaw     []byte
	)
	if err := row.Scan(
		&task.ParentRunID,
		&task.DelegationID,
		&task.TaskIndex,
		&task.ChildRunID,
		&task.Capability.ID,
		&task.Capability.Version,
		&task.CapabilityHash,
		&task.ObjectiveHash,
		&task.Admitted,
		&task.RejectionCode,
		&reservationRaw,
		&settledRaw,
		&task.ReportArtifactID,
	); err != nil {
		return task, err
	}
	if task.Admitted {
		if err := json.Unmarshal(reservationRaw, &task.Reservation); err != nil {
			return task, fmt.Errorf("decode delegation reservation: %w", err)
		}
	}
	if len(settledRaw) > 0 {
		var usage agentapi.Usage
		if err := json.Unmarshal(settledRaw, &usage); err != nil {
			return task, fmt.Errorf("decode delegation settlement: %w", err)
		}
		task.SettledUsage = &usage
	}
	return task, nil
}

func validateDelegationAdmission(admission DelegationAdmission) error {
	if strings.TrimSpace(admission.ParentRunID) == "" ||
		strings.TrimSpace(admission.DelegationID) == "" ||
		admission.MaxChildren < 0 ||
		admission.MaxTotalTokens < 0 ||
		admission.MaxTotalCostMicros < 0 ||
		admission.ParentAnswerReserve < 0 ||
		len(admission.Reservations) == 0 {
		return fmt.Errorf("invalid delegation admission")
	}
	for index, reservation := range admission.Reservations {
		if reservation.ParentRunID != admission.ParentRunID ||
			reservation.DelegationID != admission.DelegationID ||
			reservation.TaskIndex < 0 ||
			reservation.ChildRunID == "" ||
			reservation.Capability.ID == "" ||
			reservation.Capability.Version <= 0 ||
			reservation.CapabilityHash == "" ||
			reservation.ObjectiveHash == "" ||
			reservation.ReservedTokens < 0 ||
			reservation.ReservedCostMicros < 0 {
			return fmt.Errorf("invalid delegation reservation %d", index)
		}
		if reservation.Limits.MaxTotalTokens > 0 &&
			reservation.ReservedTokens != reservation.Limits.MaxTotalTokens {
			return fmt.Errorf("delegation reservation %d token grant mismatch", index)
		}
		if reservation.Limits.MaxCostMicros > 0 &&
			reservation.ReservedCostMicros != reservation.Limits.MaxCostMicros {
			return fmt.Errorf("delegation reservation %d cost grant mismatch", index)
		}
	}
	return nil
}

func validateDelegationArtifact(artifact DelegationArtifact, childRunID string) error {
	if artifact.ID == "" ||
		artifact.RunID != childRunID ||
		artifact.Schema.ID == "" ||
		artifact.Schema.Version <= 0 ||
		artifact.ContentHash == "" ||
		len(artifact.Content) == 0 {
		return fmt.Errorf("invalid delegation artifact")
	}
	switch artifact.Kind {
	case DelegationReportArtifactKind:
		if artifact.Schema.ID != "delegation.report" {
			return fmt.Errorf("invalid delegation report artifact schema")
		}
	case DelegationVerificationArtifactKind:
		if artifact.Schema.ID != "delegation.verification.artifact" {
			return fmt.Errorf("invalid delegation verification artifact schema")
		}
	default:
		return fmt.Errorf("invalid delegation artifact kind %q", artifact.Kind)
	}
	sum := sha256.Sum256(artifact.Content)
	if fmt.Sprintf("%x", sum[:]) != artifact.ContentHash {
		return fmt.Errorf("invalid delegation artifact content hash")
	}
	return nil
}

func sameDelegationReservation(
	existing DelegationTaskRecord,
	requested DelegationReservation,
) error {
	if !existing.Admitted ||
		existing.ChildRunID != requested.ChildRunID ||
		existing.Capability != requested.Capability ||
		existing.CapabilityHash != requested.CapabilityHash ||
		existing.ObjectiveHash != requested.ObjectiveHash ||
		existing.Reservation.ReservedTokens != requested.ReservedTokens ||
		existing.Reservation.ReservedCostMicros != requested.ReservedCostMicros {
		return ErrDelegationTaskConflict
	}
	existingLimits, _ := json.Marshal(existing.Reservation.Limits)
	requestedLimits, _ := json.Marshal(requested.Limits)
	if string(existingLimits) != string(requestedLimits) {
		return ErrDelegationTaskConflict
	}
	return nil
}

func recordFromReservation(reservation DelegationReservation) DelegationTaskRecord {
	return DelegationTaskRecord{
		ParentRunID:    reservation.ParentRunID,
		DelegationID:   reservation.DelegationID,
		TaskIndex:      reservation.TaskIndex,
		ChildRunID:     reservation.ChildRunID,
		Capability:     reservation.Capability,
		CapabilityHash: reservation.CapabilityHash,
		ObjectiveHash:  reservation.ObjectiveHash,
		Admitted:       true,
		Reservation:    reservation,
	}
}

func delegationTaskKey(delegationID string, taskIndex int) string {
	return fmt.Sprintf("%s\x00%d", delegationID, taskIndex)
}

func minPositiveInt64(left, right int64) int64 {
	switch {
	case left <= 0:
		return right
	case right <= 0:
		return left
	case left < right:
		return left
	default:
		return right
	}
}

func artifactID(artifact *DelegationArtifact) string {
	if artifact == nil {
		return ""
	}
	return artifact.ID
}

func delegationReportArtifactID(reportRef string) string {
	sum := sha256.Sum256([]byte(reportRef))
	return "artifact_" + fmt.Sprintf("%x", sum[:12])
}

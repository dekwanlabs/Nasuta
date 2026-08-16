package run

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/tool"
)

// PutEvidenceLedger persists the authoritative evidence identities owned by one
// Run. Repeated writes must contain the exact same ledger.
func (rs *Store) PutEvidenceLedger(
	ctx context.Context,
	runID string,
	units []tool.EvidenceUnit,
) (RunArtifact, error) {
	artifact, err := NewEvidenceLedgerArtifact(runID, units)
	if err != nil {
		return RunArtifact{}, err
	}
	if _, err := rs.db.ExecContext(
		ctx,
		`INSERT INTO agent_run_artifacts(
			artifact_id,run_id,kind,schema_id,schema_version,content_hash,content,created_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON DUPLICATE KEY UPDATE artifact_id=artifact_id`,
		artifact.ID,
		artifact.RunID,
		artifact.Kind,
		artifact.Schema.ID,
		artifact.Schema.Version,
		artifact.ContentHash,
		artifact.Content,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return RunArtifact{}, err
	}
	persisted, err := rs.getRunArtifact(ctx, runID, EvidenceLedgerArtifactKind)
	if err != nil {
		return RunArtifact{}, err
	}
	if !sameRunArtifact(persisted, artifact) {
		return RunArtifact{}, ErrEvidenceLedgerConflict
	}
	return persisted, nil
}

// GetDelegationEvidence loads child evidence only through its admitted, settled
// parent delegation relationship.
func (rs *Store) GetDelegationEvidence(
	ctx context.Context,
	parentRunID,
	delegationID string,
	taskIndex int,
) ([]tool.EvidenceUnit, error) {
	if strings.TrimSpace(parentRunID) == "" ||
		strings.TrimSpace(delegationID) == "" ||
		taskIndex < 0 {
		return nil, fmt.Errorf("invalid delegation evidence lookup")
	}
	artifact, err := scanRunArtifact(rs.db.QueryRowContext(
		ctx,
		`SELECT a.artifact_id,a.run_id,a.kind,a.schema_id,a.schema_version,
			a.content_hash,a.content
		 FROM agent_delegation_tasks t
		 JOIN agent_run_artifacts a
		   ON a.run_id=t.child_run_id AND a.kind=?
		 WHERE t.parent_run_id=? AND t.delegation_id=? AND t.task_index=?
		   AND t.admitted=TRUE AND t.settled_usage_json IS NOT NULL`,
		EvidenceLedgerArtifactKind,
		parentRunID,
		delegationID,
		taskIndex,
	))
	if err != nil {
		return nil, err
	}
	return decodeEvidenceLedger(artifact)
}

// ResolveWorkflowEscalationEvidence resolves requested refs from the parent
// ledger and, when provided, settled children in the same delegation batch.
func (rs *Store) ResolveWorkflowEscalationEvidence(
	ctx context.Context,
	parentRunID,
	delegationID string,
	refs []string,
) ([]EvidenceReference, error) {
	if strings.TrimSpace(parentRunID) == "" {
		return nil, fmt.Errorf("invalid workflow evidence parent")
	}
	if len(refs) == 0 {
		return nil, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(delegationID) == "" {
		rows, err = rs.db.QueryContext(
			ctx,
			`SELECT artifact_id,run_id,kind,schema_id,schema_version,
				content_hash,content
			 FROM agent_run_artifacts
			 WHERE run_id=? AND kind=?
			 ORDER BY run_id`,
			parentRunID,
			EvidenceLedgerArtifactKind,
		)
	} else {
		rows, err = rs.db.QueryContext(
			ctx,
			`SELECT DISTINCT a.artifact_id,a.run_id,a.kind,a.schema_id,
				a.schema_version,a.content_hash,a.content
			 FROM agent_run_artifacts a
			 WHERE a.kind=? AND (
				a.run_id=? OR EXISTS (
					SELECT 1 FROM agent_delegation_tasks t
					WHERE t.parent_run_id=? AND t.delegation_id=?
					  AND t.child_run_id=a.run_id
					  AND t.admitted=TRUE
					  AND t.settled_usage_json IS NOT NULL
				)
			 )
			 ORDER BY CASE WHEN a.run_id=? THEN 0 ELSE 1 END,a.run_id`,
			EvidenceLedgerArtifactKind,
			parentRunID,
			parentRunID,
			delegationID,
			parentRunID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type resolvedAlias struct {
		unit      tool.EvidenceUnit
		ambiguous bool
	}
	aliases := make(map[string]resolvedAlias)
	for rows.Next() {
		artifact, err := scanRunArtifact(rows)
		if err != nil {
			return nil, err
		}
		units, err := decodeEvidenceLedger(artifact)
		if err != nil {
			return nil, err
		}
		for _, unit := range units {
			for _, alias := range evidenceUnitAliases(unit) {
				existing, ok := aliases[alias]
				if !ok {
					aliases[alias] = resolvedAlias{unit: cloneEvidenceUnit(unit)}
					continue
				}
				if !reflect.DeepEqual(existing.unit, unit) {
					existing.ambiguous = true
					aliases[alias] = existing
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	resolved := make([]EvidenceReference, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		candidate, ok := aliases[ref]
		if !ok {
			return nil, fmt.Errorf(
				"evidence ref %q does not belong to the parent handoff",
				ref,
			)
		}
		if candidate.ambiguous {
			return nil, fmt.Errorf(
				"evidence ref %q is ambiguous in the parent handoff",
				ref,
			)
		}
		resolved = append(resolved, EvidenceReference{
			Ref: ref, Unit: cloneEvidenceUnit(candidate.unit),
		})
	}
	return resolved, nil
}

// NewEvidenceLedgerArtifact builds the canonical ledger persisted for one Run.
func NewEvidenceLedgerArtifact(
	runID string,
	units []tool.EvidenceUnit,
) (RunArtifact, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return RunArtifact{}, fmt.Errorf("evidence ledger run id is required")
	}
	cloned := make([]tool.EvidenceUnit, len(units))
	for index, unit := range units {
		if strings.TrimSpace(unit.SourceKind) == "" ||
			strings.TrimSpace(unit.Target) == "" ||
			strings.TrimSpace(unit.ContentHash) == "" {
			return RunArtifact{}, fmt.Errorf(
				"evidence ledger unit %d has incomplete identity",
				index,
			)
		}
		cloned[index] = cloneEvidenceUnit(unit)
	}
	raw, err := json.Marshal(cloned)
	if err != nil {
		return RunArtifact{}, err
	}
	sum := sha256.Sum256(raw)
	return RunArtifact{
		ID: evidenceLedgerArtifactID(runID), RunID: runID,
		Kind: EvidenceLedgerArtifactKind, Schema: EvidenceLedgerArtifactSchema,
		ContentHash: hex.EncodeToString(sum[:]), Content: raw,
	}, nil
}

func (rs *Store) getRunArtifact(
	ctx context.Context,
	runID,
	kind string,
) (RunArtifact, error) {
	return scanRunArtifact(rs.db.QueryRowContext(
		ctx,
		`SELECT artifact_id,run_id,kind,schema_id,schema_version,
			content_hash,content
		 FROM agent_run_artifacts
		 WHERE run_id=? AND kind=?`,
		runID,
		kind,
	))
}

func scanRunArtifact(scanner rowScanner) (RunArtifact, error) {
	var artifact RunArtifact
	if err := scanner.Scan(
		&artifact.ID,
		&artifact.RunID,
		&artifact.Kind,
		&artifact.Schema.ID,
		&artifact.Schema.Version,
		&artifact.ContentHash,
		&artifact.Content,
	); err != nil {
		return RunArtifact{}, err
	}
	artifact.Content = append([]byte(nil), artifact.Content...)
	return artifact, nil
}

func decodeEvidenceLedger(
	artifact RunArtifact,
) ([]tool.EvidenceUnit, error) {
	if artifact.ID != evidenceLedgerArtifactID(artifact.RunID) ||
		artifact.Kind != EvidenceLedgerArtifactKind ||
		artifact.Schema != EvidenceLedgerArtifactSchema ||
		strings.TrimSpace(artifact.ContentHash) == "" ||
		len(artifact.Content) == 0 {
		return nil, ErrEvidenceLedgerConflict
	}
	sum := sha256.Sum256(artifact.Content)
	if hex.EncodeToString(sum[:]) != artifact.ContentHash {
		return nil, ErrEvidenceLedgerConflict
	}
	var units []tool.EvidenceUnit
	if err := json.Unmarshal(artifact.Content, &units); err != nil {
		return nil, fmt.Errorf("decode evidence ledger: %w", err)
	}
	for index, unit := range units {
		if strings.TrimSpace(unit.SourceKind) == "" ||
			strings.TrimSpace(unit.Target) == "" ||
			strings.TrimSpace(unit.ContentHash) == "" {
			return nil, fmt.Errorf(
				"decode evidence ledger: unit %d has incomplete identity",
				index,
			)
		}
		units[index] = cloneEvidenceUnit(unit)
	}
	return units, nil
}

func sameRunArtifact(left, right RunArtifact) bool {
	return left.ID == right.ID &&
		left.RunID == right.RunID &&
		left.Kind == right.Kind &&
		left.Schema == right.Schema &&
		left.ContentHash == right.ContentHash &&
		string(left.Content) == string(right.Content)
}

func evidenceUnitAliases(unit tool.EvidenceUnit) []string {
	aliases := make([]string, 0, 4)
	aliases = append(aliases, EvidenceReferenceID(unit))
	if contentHash := strings.TrimSpace(unit.ContentHash); contentHash != "" {
		aliases = append(aliases, contentHash)
	}
	if target := strings.TrimSpace(unit.Target); target != "" {
		aliases = append(aliases, target)
		if source := strings.TrimSpace(unit.SourceKind); source != "" {
			aliases = append(aliases, source+":"+target)
		}
	}
	return aliases
}

func cloneEvidenceUnit(unit tool.EvidenceUnit) tool.EvidenceUnit {
	unit.Sections = append([]string(nil), unit.Sections...)
	unit.Facets = append([]string(nil), unit.Facets...)
	return unit
}

func existingArtifactID(
	ctx context.Context,
	tx *sql.Tx,
	runID,
	kind string,
) (string, error) {
	var artifactID string
	err := tx.QueryRowContext(
		ctx,
		`SELECT artifact_id FROM agent_run_artifacts
		 WHERE run_id=? AND kind=?`,
		runID,
		kind,
	).Scan(&artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return artifactID, err
}

func insertRunArtifact(
	ctx context.Context,
	tx *sql.Tx,
	artifact RunArtifact,
) error {
	_, err := tx.ExecContext(
		ctx,
		`INSERT INTO agent_run_artifacts(
			artifact_id,run_id,kind,schema_id,schema_version,content_hash,content,created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		artifact.ID,
		artifact.RunID,
		artifact.Kind,
		artifact.Schema.ID,
		artifact.Schema.Version,
		artifact.ContentHash,
		artifact.Content,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	)
	return err
}

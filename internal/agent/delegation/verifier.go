package delegation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/tool"
)

const SemanticVerifierCapabilityID = "evidence.semantic.verify"

const (
	ErrorVerifierUnavailable      = "semantic_verifier_unavailable"
	ErrorVerificationInput        = "verification_input_invalid"
	ErrorVerificationOutput       = "verification_output_invalid"
	ErrorVerificationPersistence  = "verification_persistence_failed"
	maxVerificationClaims         = 10
	maxVerificationConflicts      = 20
	maxVerificationEvidenceRefs   = 100
	maxVerificationStatementBytes = 2000
)

type verificationOutput struct {
	Summary       string                                   `json:"summary"`
	Verdicts      []agentapi.DelegationVerificationVerdict `json:"verdicts"`
	Uncertainties []string                                 `json:"uncertainties"`
}

type preparedVerification struct {
	index          int
	request        agentapi.DelegationVerificationRequest
	capability     agentapi.Capability
	definition     agentapi.Definition
	objectiveHash  string
	childRunID     string
	verificationID string
	artifactID     string
	limits         agentapi.RunLimits
	permissions    agentapi.PermissionPolicy
	context        []agentapi.ContextBlock
	budget         agentapi.RunBudgetTaskReservation
}

func (executor *Executor) attachVerification(
	ctx context.Context,
	parent ParentContext,
	result *agentapi.DelegationBatchResult,
	evidence map[string]tool.EvidenceUnit,
	observations []agentapi.EvidenceObservation,
	validationErr error,
) {
	if result == nil || validationErr != nil ||
		!result.Validation.RequiresVerification {
		return
	}
	verification := executor.executeVerificationWithObservations(
		ctx,
		parent,
		result.DelegationID,
		result.Results,
		result.Validation,
		evidence,
		observations,
	)
	result.Verification = &verification
}

func (executor *Executor) executeVerification(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	reports []agentapi.DelegationReport,
	validation agentapi.DelegationValidation,
	evidence map[string]tool.EvidenceUnit,
) agentapi.DelegationVerification {
	return executor.executeVerificationWithObservations(
		ctx, parent, delegationID, reports, validation, evidence, nil,
	)
}

func (executor *Executor) executeVerificationWithObservations(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	reports []agentapi.DelegationReport,
	validation agentapi.DelegationValidation,
	evidence map[string]tool.EvidenceUnit,
	observations []agentapi.EvidenceObservation,
) agentapi.DelegationVerification {
	task, code, err := executor.prepareVerificationWithObservations(
		parent,
		delegationID,
		len(reports),
		reports,
		validation,
		evidence,
		observations,
	)
	if err != nil {
		return executor.rejectVerification(
			ctx,
			parent,
			delegationID,
			task,
			code,
			err,
		)
	}

	if record, artifact, loadErr := executor.persistence.GetDelegationTask(
		ctx,
		parent.RunID,
		delegationID,
		task.index,
	); loadErr == nil {
		if !record.Admitted {
			return rejectedVerification(
				task,
				record.RejectionCode,
				errors.New("semantic verifier admission was rejected"),
			)
		}
		if record.SettledUsage != nil {
			return executor.replayVerification(record, artifact, task)
		}
	} else if !errors.Is(loadErr, sql.ErrNoRows) {
		// ReserveDelegationBatch remains the authoritative idempotent admission
		// path; tolerate stores that do not distinguish a missing lookup.
	}

	rootGate := agentapi.RunBudgetTaskGateFromContext(ctx)
	if rootGate != nil {
		task.budget, err = rootGate.ReserveTask(budgetGrant(
			task.limits, executor.policy.MaxChildInputTokens, executor.policy.MaxChildOutputTokens,
		))
		if err != nil {
			return executor.rejectVerification(
				ctx, parent, delegationID, task, ErrorBudgetInsufficient, err,
			)
		}
	}

	reservation := agentrun.DelegationReservation{
		ParentRunID: parent.RunID, DelegationID: delegationID,
		TaskIndex: task.index, ChildRunID: task.childRunID,
		Capability: agentapi.CapabilityRef{
			ID: task.capability.ID, Version: task.capability.Version,
		},
		CapabilityHash:     task.capability.ContentHash,
		ObjectiveHash:      task.objectiveHash,
		Limits:             task.limits,
		ReservedTokens:     task.limits.MaxTotalTokens,
		ReservedCostMicros: task.limits.MaxCostMicros,
	}
	records, err := executor.persistence.ReserveDelegationBatch(
		ctx,
		agentrun.DelegationAdmission{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			MaxChildren:         executor.policy.MaxChildren + 1,
			MaxTotalTokens:      executor.policy.MaxTotalTokens,
			MaxTotalCostMicros:  executor.policy.MaxTotalCostMicros,
			ParentAnswerReserve: executor.policy.ParentAnswerReserve,
			Reservations:        []agentrun.DelegationReservation{reservation},
		},
	)
	if err != nil {
		if task.budget != nil {
			_ = task.budget.Release()
		}
		code := ErrorBudgetInsufficient
		if errors.Is(err, agentrun.ErrDelegationChildLimit) {
			code = ErrorChildLimitExceeded
		} else if !errors.Is(err, agentrun.ErrDelegationBudgetInsufficient) {
			code = ErrorVerifierUnavailable
		}
		return executor.rejectVerification(
			ctx,
			parent,
			delegationID,
			task,
			code,
			err,
		)
	}
	if len(records) != 1 {
		if task.budget != nil {
			_ = task.budget.Release()
		}
		return failedVerification(
			task,
			ErrorVerifierUnavailable,
			fmt.Errorf("semantic verifier admission returned %d records", len(records)),
		)
	}
	return executor.runVerification(
		ctx,
		parent,
		delegationID,
		task,
		records[0],
	)
}

func (executor *Executor) prepareVerification(
	parent ParentContext,
	delegationID string,
	index int,
	reports []agentapi.DelegationReport,
	validation agentapi.DelegationValidation,
	evidence map[string]tool.EvidenceUnit,
) (preparedVerification, string, error) {
	return executor.prepareVerificationWithObservations(
		parent, delegationID, index, reports, validation, evidence, nil,
	)
}

func (executor *Executor) prepareVerificationWithObservations(
	parent ParentContext,
	delegationID string,
	index int,
	reports []agentapi.DelegationReport,
	validation agentapi.DelegationValidation,
	evidence map[string]tool.EvidenceUnit,
	observations []agentapi.EvidenceObservation,
) (preparedVerification, string, error) {
	request := buildVerificationRequest(
		parent.QuestionSummary,
		reports,
		validation,
		evidence,
		parent.Context,
		observations,
	)
	task := preparedVerification{
		index: index, request: request, objectiveHash: hashJSON(request),
	}
	capability, err := executor.capabilities.Resolve(agentapi.CapabilityRef{
		ID: executor.verifierCapability,
	})
	if err != nil {
		return task, ErrorVerifierUnavailable, err
	}
	task.capability = capability
	if !capability.Enabled ||
		capability.Role != agentapi.RoleVerifier ||
		capability.SideEffects != agentapi.SideEffectNone ||
		len(capability.ToolIDs) != 0 {
		return task, ErrorVerifierUnavailable, fmt.Errorf(
			"capability %q is not an enabled tool-free verifier",
			capability.ID,
		)
	}
	definition, err := executor.definitions.Resolve(capability.Agent)
	if err != nil {
		return task, ErrorVerifierUnavailable, err
	}
	task.definition = definition
	task.permissions = intersectPermissions(
		parent.Permissions,
		agentapi.PermissionPolicy{Scopes: capability.PermissionScope},
	)
	if len(task.permissions.Scopes) == 0 {
		return task, ErrorCapabilityNotAllowed, fmt.Errorf(
			"parent has no permission accepted by verifier %q",
			capability.ID,
		)
	}
	task.context = selectContext(
		parent,
		request.EvidenceRefs,
		nil,
		executor.policy.MaxChildInputTokens,
	)
	raw, err := json.Marshal(request)
	if err != nil {
		return task, ErrorVerificationInput, err
	}
	if estimateTokens(raw, task.context) > executor.policy.MaxChildInputTokens {
		return task, ErrorChildInputLimit, fmt.Errorf(
			"semantic verifier input exceeds token limit",
		)
	}
	task.childRunID = stableID(
		"run_verify",
		parent.RunID,
		delegationID,
		capability.ContentHash,
		task.objectiveHash,
	)
	task.verificationID = stableID("verification", task.childRunID)
	task.artifactID = stableID("artifact", task.verificationID)
	task.limits, err = executor.childLimits(parent, definition)
	if err != nil {
		return task, ErrorParentTimeInsufficient, err
	}
	return task, "", nil
}

func buildVerificationRequest(
	question string,
	reports []agentapi.DelegationReport,
	validation agentapi.DelegationValidation,
	evidence map[string]tool.EvidenceUnit,
	contextIndex map[string]agentapi.ContextBlock,
	observations []agentapi.EvidenceObservation,
) agentapi.DelegationVerificationRequest {
	type claimCandidate struct {
		claim      agentapi.DelegationVerificationClaim
		conflicted bool
	}
	conflicted := make(map[string]struct{})
	for _, conflict := range validation.Conflicts {
		for _, claimID := range conflict.ClaimIDs {
			conflicted[claimID] = struct{}{}
		}
	}
	candidates := make([]claimCandidate, 0)
	for _, report := range reports {
		for _, finding := range report.Findings {
			id := report.ReportID + "/" + finding.ID
			_, hasConflict := conflicted[id]
			candidates = append(candidates, claimCandidate{
				claim: agentapi.DelegationVerificationClaim{
					ID: id,
					Statement: truncateText(
						finding.Statement,
						maxVerificationStatementBytes,
					),
					Critical:  finding.Critical,
					Citations: canonicalStrings(finding.Citations),
				},
				conflicted: hasConflict,
			})
		}
	}
	claims := make([]agentapi.DelegationVerificationClaim, 0, min(
		len(candidates),
		maxVerificationClaims,
	))
	seen := make(map[string]struct{}, len(candidates))
	appendClaims := func(selectClaim func(claimCandidate) bool) {
		for _, candidate := range candidates {
			if len(claims) >= maxVerificationClaims ||
				!selectClaim(candidate) {
				continue
			}
			if _, duplicate := seen[candidate.claim.ID]; duplicate {
				continue
			}
			seen[candidate.claim.ID] = struct{}{}
			claims = append(claims, candidate.claim)
		}
	}
	appendClaims(func(candidate claimCandidate) bool { return candidate.conflicted })
	appendClaims(func(candidate claimCandidate) bool { return candidate.claim.Critical })
	appendClaims(func(claimCandidate) bool { return true })

	selected := make(map[string]struct{}, len(claims))
	var evidenceRefs []string
	for _, claim := range claims {
		selected[claim.ID] = struct{}{}
		for _, reference := range claim.Citations {
			if _, ok := evidence[reference]; ok {
				evidenceRefs = append(evidenceRefs, reference)
			}
		}
	}
	evidenceRefs = canonicalStrings(evidenceRefs)
	if len(evidenceRefs) > maxVerificationEvidenceRefs {
		evidenceRefs = evidenceRefs[:maxVerificationEvidenceRefs]
	}
	conflicts := make([]agentapi.DelegationValidationConflict, 0, min(
		len(validation.Conflicts),
		maxVerificationConflicts,
	))
	for _, conflict := range validation.Conflicts {
		include := true
		for _, claimID := range conflict.ClaimIDs {
			if _, ok := selected[claimID]; !ok {
				include = false
				break
			}
		}
		if !include {
			continue
		}
		conflict.ClaimIDs = append([]string(nil), conflict.ClaimIDs...)
		conflicts = append(conflicts, conflict)
		if len(conflicts) >= maxVerificationConflicts {
			break
		}
	}
	return agentapi.DelegationVerificationRequest{
		Question:         truncateText(question, 4000),
		DecisionQuestion: "Which claims are supported, contradicted, distinct, or unresolved by the cited evidence?",
		Claims:           claims,
		Conflicts:        conflicts,
		EvidenceRefs:     evidenceRefs,
		EvidenceLookup:   buildEvidenceLookup(evidenceRefs, evidence, contextIndex, observations),
		Reasons:          canonicalStrings(validation.VerificationReasons),
	}
}

func (executor *Executor) runVerification(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedVerification,
	record agentrun.DelegationTaskRecord,
) agentapi.DelegationVerification {
	if task.budget != nil {
		defer func() { _ = task.budget.Release() }()
	}
	if record.Existing && record.SettledUsage != nil {
		_, artifact, err := executor.persistence.GetDelegationTask(
			ctx,
			parent.RunID,
			delegationID,
			task.index,
		)
		if err != nil {
			return failedVerification(task, ErrorVerificationPersistence, err)
		}
		return executor.replayVerification(record, artifact, task)
	}
	if record.Existing {
		verification := failedVerification(
			task,
			ErrorInterrupted,
			errors.New("semantic verifier admission was recovered without a durable result"),
		)
		executor.persistVerification(
			context.WithoutCancel(ctx),
			parent,
			delegationID,
			task,
			verification,
			agentapi.Usage{},
		)
		executor.emitVerificationTerminal(parent, delegationID, task, verification, time.Time{})
		return verification
	}
	if err := ctx.Err(); err != nil {
		verification := cancelledVerification(
			task,
			ErrorParentCancelled,
			err,
		)
		executor.persistVerification(
			context.WithoutCancel(ctx),
			parent,
			delegationID,
			task,
			verification,
			agentapi.Usage{},
		)
		executor.emitVerificationTerminal(parent, delegationID, task, verification, time.Time{})
		return verification
	}
	slot := executor.capabilitySlot(task.capability)
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		verification := cancelledVerification(
			task,
			ErrorParentCancelled,
			ctx.Err(),
		)
		executor.persistVerification(
			context.WithoutCancel(ctx),
			parent,
			delegationID,
			task,
			verification,
			agentapi.Usage{},
		)
		executor.emitVerificationTerminal(parent, delegationID, task, verification, time.Time{})
		return verification
	}
	if err := executor.persistence.LinkDelegationChild(
		ctx,
		parent.RunID,
		delegationID,
		task.index,
		task.childRunID,
	); err != nil {
		verification := failedVerification(task, ErrorChildExecution, err)
		executor.persistVerification(
			context.WithoutCancel(ctx),
			parent,
			delegationID,
			task,
			verification,
			agentapi.Usage{},
		)
		executor.emitVerificationTerminal(parent, delegationID, task, verification, time.Time{})
		return verification
	}

	started := time.Now()
	executor.emitVerification(
		agentrun.EventDelegationVerificationStarted,
		parent,
		delegationID,
		task,
		"running",
		"",
		0,
		agentapi.Usage{},
	)
	stopToolProjection := func() {}
	if projector, ok := executor.runtime.(toolEventProjector); ok {
		stopToolProjection = projector.ProjectToolEvents(
			task.childRunID, parent.RunID, "", task.childRunID,
		)
	}
	defer stopToolProjection()
	runCtx, cancel := context.WithDeadline(ctx, task.limits.Deadline)
	if task.budget != nil {
		runCtx = agentapi.WithRunBudgetGate(runCtx, task.budget)
	}
	result, runErr := executor.runtime.Run(
		runCtx,
		executor.verificationRunRequest(parent, delegationID, task),
	)
	childErr := runCtx.Err()
	cancel()
	if runErr != nil {
		result = agentapi.RunResult{
			RunID:  task.childRunID,
			Status: agentapi.RunFailed,
			Error: &agentapi.RunError{
				Code: ErrorChildExecution, Message: runErr.Error(),
			},
		}
	}
	if childErr != nil {
		code := ErrorChildTimeout
		if ctx.Err() != nil {
			code = ErrorParentCancelled
		}
		result.Status = agentapi.RunCancelled
		result.Error = &agentapi.RunError{Code: code, Message: childErr.Error()}
	}
	if result.Usage.InputTokens > executor.policy.MaxChildInputTokens {
		result.Status = agentapi.RunFailed
		result.Error = &agentapi.RunError{
			Code: ErrorChildInputLimit, Message: "verifier input token limit exceeded",
		}
	}
	if result.Usage.OutputTokens > executor.policy.MaxChildOutputTokens {
		result.Status = agentapi.RunFailed
		result.Error = &agentapi.RunError{
			Code: ErrorChildOutputLimit, Message: "verifier output token limit exceeded",
		}
	}
	verification := projectVerification(result, task)
	if err := validateVerificationOutput(task.request, verification); err != nil &&
		verification.Status == agentapi.DelegationCompleted {
		verification = failedVerification(task, ErrorVerificationOutput, err)
		verification.Usage = publicDelegationUsage(result)
	}
	if err := executor.persistVerification(
		context.WithoutCancel(ctx),
		parent,
		delegationID,
		task,
		verification,
		result.Usage,
	); err != nil {
		code := ErrorVerificationPersistence
		if errors.Is(err, agentrun.ErrDelegationAccounting) {
			code = ErrorBudgetAccountingViolation
		}
		verification = failedVerification(task, code, err)
		verification.Usage = publicDelegationUsage(result)
	}
	executor.emitVerificationTerminal(parent, delegationID, task, verification, started)
	return verification
}

func (executor *Executor) verificationRunRequest(
	parent ParentContext,
	delegationID string,
	task preparedVerification,
) agentapi.RunRequest {
	input, _ := json.Marshal(task.request)
	return agentapi.RunRequest{
		RunID: task.childRunID,
		Agent: agentapi.DefinitionRef{
			ID: task.definition.ID, Version: task.definition.Version,
		},
		DefinitionHash: task.definition.ContentHash,
		Input:          input,
		Context:        task.context,
		Permissions:    task.permissions,
		ToolScope: agentapi.ToolScope{
			RestrictVisible: true,
			VisibleToolIDs:  []string{},
		},
		Policy: agentapi.RunPolicy{
			EvidenceRequired: len(task.request.EvidenceRefs) > 0,
			EvidenceSeeded:   len(task.context) > 0,
			MaxToolCalls:     0,
		},
		Limits: task.limits,
		Delegation: agentapi.RunDelegation{
			DelegationID: delegationID, Depth: parent.Depth + 1,
			Capability: agentapi.CapabilityRef{
				ID: task.capability.ID, Version: task.capability.Version,
			},
			CapabilityContentHash:      task.capability.ContentHash,
			CapabilityRegistryRevision: executor.capabilities.Revision(),
		},
		Actor: parent.Actor,
		Correlation: agentapi.Correlation{
			SessionID: parent.Correlation.SessionID, ParentRunID: parent.RunID,
		},
	}
}

func projectVerification(
	result agentapi.RunResult,
	task preparedVerification,
) agentapi.DelegationVerification {
	verification := agentapi.DelegationVerification{
		RunID: result.RunID, VerificationID: task.verificationID,
		Status: agentapi.DelegationFailed,
		Usage:  publicDelegationUsage(result),
		Error:  cloneRunError(result.Error),
	}
	if result.Status != agentapi.RunSucceeded {
		verification.Status = ProjectStatus(StatusFacts{
			Admitted: true, Settled: true, RunStatus: result.Status,
			ErrorCode:    runErrorCode(result.Error),
			Completeness: agentapi.DelegationIncomplete,
		})
		return verification
	}
	var output verificationOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		failed := failedVerification(task, ErrorVerificationOutput, err)
		failed.Usage = publicDelegationUsage(result)
		return failed
	}
	verification.Status = agentapi.DelegationCompleted
	verification.Summary = strings.TrimSpace(output.Summary)
	verification.Verdicts = append(
		[]agentapi.DelegationVerificationVerdict(nil),
		output.Verdicts...,
	)
	verification.Uncertainties = canonicalStrings(output.Uncertainties)
	completeVerificationCoverage(&verification, task.request.Claims)
	return verification
}

// completeVerificationCoverage makes the verifier result total over the claims
// submitted by the server. A verifier may group several claims into one verdict,
// but it must not silently omit a claim: omitted claims are deterministic
// unresolved outcomes. This keeps partial provider output safe for the parent.
func completeVerificationCoverage(
	verification *agentapi.DelegationVerification,
	claims []agentapi.DelegationVerificationClaim,
) {
	if verification == nil || len(claims) == 0 {
		return
	}
	covered := make(map[string]struct{}, len(claims))
	for _, verdict := range verification.Verdicts {
		for _, claimID := range verdict.ClaimIDs {
			claimID = strings.TrimSpace(claimID)
			if claimID != "" {
				covered[claimID] = struct{}{}
			}
		}
	}
	missing := make([]string, 0)
	for _, claim := range claims {
		claimID := strings.TrimSpace(claim.ID)
		if claimID == "" {
			continue
		}
		if _, ok := covered[claimID]; ok {
			continue
		}
		missing = append(missing, claimID)
	}
	if len(missing) == 0 {
		return
	}
	for _, claimID := range missing {
		if len(verification.Verdicts) >= maxVerificationClaims {
			break
		}
		verification.Verdicts = append(verification.Verdicts, agentapi.DelegationVerificationVerdict{
			ClaimIDs:  []string{claimID},
			Decision:  "unresolved",
			Rationale: "The verifier returned no claim-level decision for this claim; it remains unresolved.",
		})
	}
	verification.Uncertainties = canonicalStrings(append(
		verification.Uncertainties,
		"The semantic verifier did not cover every submitted claim; omitted claims remain unresolved.",
	))
}

func validateVerificationOutput(
	request agentapi.DelegationVerificationRequest,
	verification agentapi.DelegationVerification,
) error {
	if strings.TrimSpace(verification.Summary) == "" {
		return fmt.Errorf("semantic verifier summary is required")
	}
	claimIDs := make(map[string]struct{}, len(request.Claims))
	for _, claim := range request.Claims {
		claimIDs[claim.ID] = struct{}{}
	}
	evidenceRefs := make(map[string]struct{}, len(request.EvidenceRefs))
	for _, reference := range request.EvidenceRefs {
		evidenceRefs[reference] = struct{}{}
	}
	if len(request.Claims) > 0 && len(verification.Verdicts) == 0 {
		return fmt.Errorf("semantic verifier returned no verdicts for submitted claims")
	}
	if len(verification.Verdicts) > maxVerificationClaims {
		return fmt.Errorf("semantic verifier returned too many verdicts")
	}
	for index, verdict := range verification.Verdicts {
		if len(verdict.ClaimIDs) == 0 ||
			strings.TrimSpace(verdict.Rationale) == "" {
			return fmt.Errorf("semantic verifier verdict %d is incomplete", index)
		}
		switch verdict.Decision {
		case "supported", "contradicted", "distinct", "unresolved":
		default:
			return fmt.Errorf(
				"semantic verifier verdict %d has invalid decision %q",
				index,
				verdict.Decision,
			)
		}
		for _, claimID := range verdict.ClaimIDs {
			if _, ok := claimIDs[claimID]; !ok {
				return fmt.Errorf(
					"semantic verifier verdict %d references unknown claim %q",
					index,
					claimID,
				)
			}
		}
		for _, reference := range verdict.EvidenceRefs {
			if _, ok := evidenceRefs[reference]; !ok {
				return fmt.Errorf(
					"semantic verifier verdict %d references unauthorized evidence %q",
					index,
					reference,
				)
			}
		}
	}
	return nil
}

func (executor *Executor) persistVerification(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedVerification,
	verification agentapi.DelegationVerification,
	usage agentapi.Usage,
) error {
	raw, err := json.Marshal(verification)
	if err != nil {
		return err
	}
	_, err = executor.persistence.SettleDelegationTask(
		ctx,
		agentrun.DelegationSettlement{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			TaskIndex: task.index, ChildRunID: task.childRunID,
			Usage: usage,
			Artifact: &agentrun.DelegationArtifact{
				ID: task.artifactID, RunID: task.childRunID,
				Kind: agentrun.DelegationVerificationArtifactKind,
				Schema: agentapi.SchemaRef{
					ID: "delegation.verification.artifact", Version: 1,
				},
				ContentHash: hashBytes(raw), Content: raw,
			},
		},
	)
	return err
}

func (executor *Executor) replayVerification(
	record agentrun.DelegationTaskRecord,
	artifact *agentrun.DelegationArtifact,
	task preparedVerification,
) agentapi.DelegationVerification {
	if record.SettledUsage == nil || artifact == nil {
		return failedVerification(
			task,
			ErrorVerificationPersistence,
			errors.New("settled semantic verification is unavailable"),
		)
	}
	if artifact.ID != task.artifactID ||
		artifact.RunID != task.childRunID ||
		artifact.Kind != agentrun.DelegationVerificationArtifactKind ||
		artifact.Schema.ID != "delegation.verification.artifact" ||
		artifact.Schema.Version != 1 ||
		hashBytes(artifact.Content) != artifact.ContentHash {
		return failedVerification(
			task,
			ErrorVerificationPersistence,
			errors.New("semantic verification artifact identity mismatch"),
		)
	}
	var verification agentapi.DelegationVerification
	if err := json.Unmarshal(artifact.Content, &verification); err != nil {
		return failedVerification(task, ErrorVerificationPersistence, err)
	}
	if verification.RunID != task.childRunID ||
		verification.VerificationID != task.verificationID {
		return failedVerification(
			task,
			ErrorVerificationPersistence,
			errors.New("semantic verification result identity mismatch"),
		)
	}
	return verification
}

func (executor *Executor) rejectVerification(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedVerification,
	code string,
	rejection error,
) agentapi.DelegationVerification {
	ref := agentapi.CapabilityRef{ID: executor.verifierCapability}
	hash := ""
	if task.capability.ID != "" {
		ref = agentapi.CapabilityRef{
			ID: task.capability.ID, Version: task.capability.Version,
		}
		hash = task.capability.ContentHash
	}
	if task.objectiveHash == "" {
		task.objectiveHash = hashJSON(task.request)
	}
	if _, err := executor.persistence.RejectDelegationTask(
		context.WithoutCancel(ctx),
		agentrun.DelegationRejection{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			TaskIndex: task.index, Capability: ref, CapabilityHash: hash,
			ObjectiveHash: task.objectiveHash, Code: code,
		},
	); err != nil {
		code = ErrorVerificationPersistence
		rejection = fmt.Errorf("persist semantic verifier rejection: %w", err)
	}
	verification := rejectedVerification(task, code, rejection)
	executor.emitVerification(
		agentrun.EventDelegationVerificationRejected,
		parent,
		delegationID,
		task,
		string(verification.Status),
		code,
		0,
		agentapi.Usage{},
	)
	return verification
}

func failedVerification(
	task preparedVerification,
	code string,
	err error,
) agentapi.DelegationVerification {
	return agentapi.DelegationVerification{
		RunID: task.childRunID, VerificationID: task.verificationID,
		Status: agentapi.DelegationFailed,
		Error:  &agentapi.RunError{Code: code, Message: err.Error()},
	}
}

func cancelledVerification(
	task preparedVerification,
	code string,
	err error,
) agentapi.DelegationVerification {
	return agentapi.DelegationVerification{
		RunID: task.childRunID, VerificationID: task.verificationID,
		Status: agentapi.DelegationCancelled,
		Error:  &agentapi.RunError{Code: code, Message: err.Error()},
	}
}

func rejectedVerification(
	task preparedVerification,
	code string,
	err error,
) agentapi.DelegationVerification {
	return agentapi.DelegationVerification{
		RunID: task.childRunID, VerificationID: task.verificationID,
		Status: agentapi.DelegationRejected,
		Error:  &agentapi.RunError{Code: code, Message: err.Error()},
	}
}

func (executor *Executor) emitVerificationTerminal(
	parent ParentContext,
	delegationID string,
	task preparedVerification,
	verification agentapi.DelegationVerification,
	started time.Time,
) {
	eventType := agentrun.EventDelegationVerificationFailed
	if verification.Status == agentapi.DelegationCompleted ||
		verification.Status == agentapi.DelegationPartial {
		eventType = agentrun.EventDelegationVerificationDone
	}
	duration := int64(0)
	if !started.IsZero() {
		duration = time.Since(started).Milliseconds()
	}
	executor.emitVerification(
		eventType,
		parent,
		delegationID,
		task,
		string(verification.Status),
		runErrorCode(verification.Error),
		duration,
		agentapi.Usage{
			InputTokens:     verification.Usage.InputTokens,
			OutputTokens:    verification.Usage.OutputTokens,
			ReasoningTokens: verification.Usage.ReasoningTokens,
			TotalTokens:     verification.Usage.TotalTokens,
			CostMicros:      verification.Usage.CostMicros,
		},
	)
}

func (executor *Executor) emitVerification(
	eventType agentrun.EventType,
	parent ParentContext,
	delegationID string,
	task preparedVerification,
	status string,
	errorCode string,
	durationMS int64,
	usage agentapi.Usage,
) {
	if executor.events == nil {
		return
	}
	agentID, agentName := executor.projectAgent(task.capability.ID)
	executor.events.EmitEvent(eventType, agentrun.ExecutionEvent{
		RunID: parent.RunID, ParentRunID: parent.RunID,
		ChildRunID: task.childRunID, DelegationID: delegationID,
		Capability: task.capability.ID, AgentID: agentID, AgentName: agentName,
		Status: status, ErrorCode: errorCode, DurationMS: durationMS, Usage: usage,
		VerificationID: task.verificationID,
		VerificationReasons: append(
			[]string(nil),
			task.request.Reasons...,
		),
	})
}

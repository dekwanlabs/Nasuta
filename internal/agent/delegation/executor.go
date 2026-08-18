package delegation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	ErrorUnknownCapability      = "unknown_capability"
	ErrorCapabilityDisabled     = "capability_disabled"
	ErrorCapabilityNotAllowed   = "capability_not_allowed"
	ErrorCapabilityNotReadOnly  = "capability_not_read_only"
	ErrorCapabilityRole         = "capability_not_investigator"
	ErrorInvalidObjective       = "invalid_objective"
	ErrorInvalidFacet           = "invalid_focus_facet"
	ErrorUnauthorizedEvidence   = "unauthorized_evidence_ref"
	ErrorDuplicateTask          = "duplicate_task"
	ErrorDepthExceeded          = "delegation_depth_exceeded"
	ErrorChildLimitExceeded     = "child_limit_exceeded"
	ErrorBudgetInsufficient     = "delegation_budget_insufficient"
	ErrorParentTimeInsufficient = "parent_time_budget_insufficient"
	ErrorChildExecution         = "child_execution_failed"
	ErrorChildInputLimit        = "child_input_limit_exceeded"
	ErrorChildOutputLimit       = "child_output_limit_exceeded"
)

const (
	maxObjectiveBytes = 2000
	maxEvidenceRefs   = 20
)

type Persistence interface {
	ReserveDelegationBatch(
		context.Context,
		agentrun.DelegationAdmission,
	) ([]agentrun.DelegationTaskRecord, error)
	RejectDelegationTask(
		context.Context,
		agentrun.DelegationRejection,
	) (agentrun.DelegationTaskRecord, error)
	LinkDelegationChild(context.Context, string, string, int, string) error
	SettleDelegationTask(
		context.Context,
		agentrun.DelegationSettlement,
	) (agentrun.DelegationTaskRecord, error)
	GetDelegationTask(
		context.Context,
		string,
		string,
		int,
	) (agentrun.DelegationTaskRecord, *agentrun.DelegationArtifact, error)
	GetDelegationEvidence(
		context.Context,
		string,
		string,
		int,
	) ([]tool.EvidenceUnit, error)
}

type EventEmitter interface {
	EmitEvent(agentrun.EventType, agentrun.ExecutionEvent)
}

type ExecutorConfig struct {
	Capabilities       *agentapi.CapabilityRegistry
	Definitions        agentapi.DefinitionResolver
	Runtime            agentapi.Runtime
	Persistence        Persistence
	Validator          *Validator
	Policy             agentapi.DelegationPolicy
	Allowlist          []string
	VerifierCapability string
	Events             EventEmitter
}

type Executor struct {
	capabilities       *agentapi.CapabilityRegistry
	definitions        agentapi.DefinitionResolver
	runtime            agentapi.Runtime
	persistence        Persistence
	validator          *Validator
	policy             agentapi.DelegationPolicy
	allowlist          map[string]struct{}
	verifierCapability string
	events             EventEmitter

	mu              sync.Mutex
	capabilitySlots map[string]chan struct{}
	flights         map[string]*taskFlight
}

type taskFlight struct {
	done     chan struct{}
	report   agentapi.DelegationReport
	evidence []tool.EvidenceUnit
}

type preparedTask struct {
	index         int
	request       agentapi.DelegationTask
	capability    agentapi.Capability
	definition    agentapi.Definition
	objectiveHash string
	childRunID    string
	reportID      string
	artifactID    string
	limits        agentapi.RunLimits
	permissions   agentapi.PermissionPolicy
	context       []agentapi.ContextBlock
}

type taskOutcome struct {
	report   agentapi.DelegationReport
	evidence []tool.EvidenceUnit
}

func NewExecutor(config ExecutorConfig) (*Executor, error) {
	if config.Capabilities == nil || config.Definitions == nil ||
		config.Runtime == nil || config.Persistence == nil {
		return nil, fmt.Errorf("delegation executor dependencies are required")
	}
	policy, err := normalizePolicy(config.Policy)
	if err != nil {
		return nil, err
	}
	allowlist := make(map[string]struct{}, len(config.Allowlist))
	for _, id := range config.Allowlist {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("delegation capability allowlist contains an empty id")
		}
		allowlist[id] = struct{}{}
	}
	validator := config.Validator
	if validator == nil {
		validator = NewValidator(nil, ValidationLimits{})
	}
	verifierCapability := strings.TrimSpace(config.VerifierCapability)
	if verifierCapability == "" {
		verifierCapability = SemanticVerifierCapabilityID
	}
	return &Executor{
		capabilities:       config.Capabilities,
		definitions:        config.Definitions,
		runtime:            config.Runtime,
		persistence:        config.Persistence,
		validator:          validator,
		policy:             policy,
		allowlist:          allowlist,
		verifierCapability: verifierCapability,
		events:             config.Events,
		capabilitySlots:    make(map[string]chan struct{}),
		flights:            make(map[string]*taskFlight),
	}, nil
}

func (executor *Executor) Policy() agentapi.DelegationPolicy {
	return executor.policy
}

func (executor *Executor) Capabilities() []agentapi.Capability {
	candidates := executor.capabilities.DelegatableInvestigators()
	if len(executor.allowlist) == 0 {
		return candidates
	}
	filtered := make([]agentapi.Capability, 0, len(candidates))
	for _, capability := range candidates {
		if _, ok := executor.allowlist[capability.ID]; ok {
			filtered = append(filtered, capability)
		}
	}
	return filtered
}

func (executor *Executor) Execute(
	ctx context.Context,
	tasks []agentapi.DelegationTask,
) (agentapi.DelegationBatchResult, []tool.EvidenceUnit, error) {
	parent, ok := ParentContextFrom(ctx)
	if !ok || strings.TrimSpace(parent.RunID) == "" {
		return agentapi.DelegationBatchResult{}, nil,
			fmt.Errorf("delegation parent context is required")
	}
	invocationID, ok := tool.InvocationIDFromContext(ctx)
	if !ok {
		return agentapi.DelegationBatchResult{}, nil,
			fmt.Errorf("delegation invocation id is required")
	}
	if len(tasks) == 0 {
		return agentapi.DelegationBatchResult{}, nil,
			fmt.Errorf("at least one delegation task is required")
	}
	delegationID := stableID("del", parent.RunID, invocationID)
	result := agentapi.DelegationBatchResult{
		DelegationID: delegationID,
		Results:      make([]agentapi.DelegationReport, len(tasks)),
	}
	prepared := make([]preparedTask, 0, len(tasks))
	seenTasks := make(map[string]struct{}, len(tasks))
	for index, task := range tasks {
		executor.emit(agentrun.EventDelegationCreated, parent, delegationID, "", task, "created", "", "", 0, agentapi.Usage{})
		candidate, code, err := executor.prepareTask(
			parent, delegationID, index, task, seenTasks,
		)
		if err != nil {
			report, persistErr := executor.reject(
				ctx, parent, delegationID, index, task,
				candidate.capability, candidate.objectiveHash, code, err,
			)
			if persistErr != nil {
				return agentapi.DelegationBatchResult{}, nil, persistErr
			}
			result.Results[index] = report
			continue
		}
		prepared = append(prepared, candidate)
	}
	if len(prepared) == 0 {
		validation, err := executor.validator.Validate(
			ctx,
			result.Results,
			cloneEvidenceIndex(parent.Evidence),
			parent.HighRisk,
		)
		result.Validation = validation
		executor.emitValidation(parent, delegationID, validation, err)
		executor.attachVerification(
			ctx,
			parent,
			&result,
			cloneEvidenceIndex(parent.Evidence),
			err,
		)
		return result, nil, err
	}

	reservations := make([]agentrun.DelegationReservation, len(prepared))
	for index, task := range prepared {
		reservations[index] = agentrun.DelegationReservation{
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
	}
	records, err := executor.persistence.ReserveDelegationBatch(
		ctx,
		agentrun.DelegationAdmission{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			MaxChildren:         executor.policy.MaxChildren,
			MaxTotalTokens:      executor.policy.MaxTotalTokens,
			MaxTotalCostMicros:  executor.policy.MaxTotalCostMicros,
			ParentAnswerReserve: executor.policy.ParentAnswerReserve,
			Reservations:        reservations,
		},
	)
	if err != nil {
		code := ErrorBudgetInsufficient
		if errors.Is(err, agentrun.ErrDelegationChildLimit) {
			code = ErrorChildLimitExceeded
		} else if !errors.Is(err, agentrun.ErrDelegationBudgetInsufficient) {
			return agentapi.DelegationBatchResult{}, nil,
				fmt.Errorf("reserve delegation batch: %w", err)
		}
		for _, task := range prepared {
			report, rejectErr := executor.reject(
				ctx, parent, delegationID, task.index, task.request,
				task.capability, task.objectiveHash, code, err,
			)
			if rejectErr != nil {
				return agentapi.DelegationBatchResult{}, nil, rejectErr
			}
			result.Results[task.index] = report
		}
		validation, validateErr := executor.validator.Validate(
			ctx,
			result.Results,
			cloneEvidenceIndex(parent.Evidence),
			parent.HighRisk,
		)
		result.Validation = validation
		executor.emitValidation(parent, delegationID, validation, validateErr)
		executor.attachVerification(
			ctx,
			parent,
			&result,
			cloneEvidenceIndex(parent.Evidence),
			validateErr,
		)
		return result, nil, validateErr
	}

	recordByIndex := make(map[int]agentrun.DelegationTaskRecord, len(records))
	for _, record := range records {
		recordByIndex[record.TaskIndex] = record
	}
	outcomes := executor.runTasks(ctx, parent, delegationID, prepared, recordByIndex)
	evidenceLedger := cloneEvidenceIndex(parent.Evidence)
	var returnedEvidence []tool.EvidenceUnit
	for _, outcome := range outcomes {
		result.Results[outcome.reportIndex()] = outcome.report
		evidenceLedger = AddEvidenceUnits(evidenceLedger, outcome.evidence)
		returnedEvidence = append(returnedEvidence, cloneEvidenceUnits(outcome.evidence)...)
	}
	validation, err := executor.validator.Validate(
		ctx,
		result.Results,
		evidenceLedger,
		parent.HighRisk,
	)
	result.Validation = validation
	executor.emitValidation(parent, delegationID, validation, err)
	executor.attachVerification(
		ctx,
		parent,
		&result,
		evidenceLedger,
		err,
	)
	return result, returnedEvidence, err
}

func (executor *Executor) prepareTask(
	parent ParentContext,
	delegationID string,
	index int,
	request agentapi.DelegationTask,
	seen map[string]struct{},
) (preparedTask, string, error) {
	request.Capability = strings.TrimSpace(request.Capability)
	request.Objective = strings.TrimSpace(request.Objective)
	request.FocusFacets = canonicalStrings(request.FocusFacets)
	request.EvidenceRefs = canonicalStrings(request.EvidenceRefs)
	objectiveHash := hashJSON(request)
	candidate := preparedTask{
		index: index, request: request, objectiveHash: objectiveHash,
	}
	if parent.Depth+1 > executor.policy.MaxDepth {
		return candidate, ErrorDepthExceeded, fmt.Errorf("delegation depth exceeds %d", executor.policy.MaxDepth)
	}
	if index >= executor.policy.MaxChildren {
		return candidate, ErrorChildLimitExceeded, fmt.Errorf("delegation child limit is %d", executor.policy.MaxChildren)
	}
	if request.Objective == "" || len(request.Objective) > maxObjectiveBytes {
		return candidate, ErrorInvalidObjective, fmt.Errorf("delegation objective must be between 1 and %d bytes", maxObjectiveBytes)
	}
	if len(request.EvidenceRefs) > maxEvidenceRefs {
		return candidate, ErrorUnauthorizedEvidence, fmt.Errorf("too many evidence references")
	}
	key := hashJSON(struct {
		Capability   string
		Objective    string
		FocusFacets  []string
		EvidenceRefs []string
	}{
		Capability: request.Capability, Objective: request.Objective,
		FocusFacets: request.FocusFacets, EvidenceRefs: request.EvidenceRefs,
	})
	if _, duplicate := seen[key]; duplicate {
		return candidate, ErrorDuplicateTask, fmt.Errorf("duplicate delegation task")
	}
	seen[key] = struct{}{}

	capability, err := executor.capabilities.Resolve(
		agentapi.CapabilityRef{ID: request.Capability},
	)
	if err != nil {
		return candidate, ErrorUnknownCapability, err
	}
	candidate.capability = capability
	if !capability.Enabled {
		return candidate, ErrorCapabilityDisabled, fmt.Errorf("capability %q is disabled", capability.ID)
	}
	if capability.Role != agentapi.RoleInvestigator {
		return candidate, ErrorCapabilityRole, fmt.Errorf("capability %q is not an investigator", capability.ID)
	}
	if capability.SideEffects != agentapi.SideEffectNone {
		return candidate, ErrorCapabilityNotReadOnly, fmt.Errorf("capability %q is not read-only", capability.ID)
	}
	if len(executor.allowlist) > 0 {
		if _, allowed := executor.allowlist[capability.ID]; !allowed {
			return candidate, ErrorCapabilityNotAllowed, fmt.Errorf("capability %q is not enabled for delegation", capability.ID)
		}
	}
	allowedFacets := make(map[string]struct{}, len(capability.InputFacets))
	for _, facet := range capability.InputFacets {
		allowedFacets[facet] = struct{}{}
	}
	for _, facet := range request.FocusFacets {
		if _, allowed := allowedFacets[facet]; !allowed {
			return candidate, ErrorInvalidFacet, fmt.Errorf(
				"focus facet %q is outside capability %q", facet, capability.ID,
			)
		}
	}
	for _, reference := range request.EvidenceRefs {
		if _, allowed := parent.Evidence[reference]; !allowed {
			if _, allowed = parent.Context[reference]; !allowed {
				return candidate, ErrorUnauthorizedEvidence, fmt.Errorf(
					"evidence reference %q is not in the parent ledger", reference,
				)
			}
		}
	}
	definition, err := executor.definitions.Resolve(capability.Agent)
	if err != nil {
		return candidate, ErrorUnknownCapability, err
	}
	candidate.definition = definition
	candidate.permissions = intersectPermissions(
		parent.Permissions,
		agentapi.PermissionPolicy{Scopes: capability.PermissionScope},
	)
	if len(candidate.permissions.Scopes) == 0 {
		return candidate, ErrorCapabilityNotAllowed,
			fmt.Errorf("parent has no permission accepted by capability %q", capability.ID)
	}
	candidate.context = selectContext(
		parent, request.EvidenceRefs, request.FocusFacets,
		executor.policy.MaxChildInputTokens,
	)
	input, err := childInput(parent, delegationID, candidate)
	if err != nil {
		return candidate, ErrorChildInputLimit, err
	}
	if estimateTokens(input, candidate.context) > executor.policy.MaxChildInputTokens {
		return candidate, ErrorChildInputLimit, fmt.Errorf("delegation child input exceeds token limit")
	}
	candidate.childRunID = stableID(
		"run_child", parent.RunID, delegationID, fmt.Sprintf("%d", index),
		capability.ContentHash, objectiveHash,
	)
	candidate.reportID = stableID("report", candidate.childRunID)
	candidate.artifactID = stableID("artifact", candidate.reportID)
	limits, err := executor.childLimits(parent, definition)
	if err != nil {
		return candidate, ErrorParentTimeInsufficient, err
	}
	candidate.limits = limits
	return candidate, "", nil
}

func (executor *Executor) runTasks(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	tasks []preparedTask,
	records map[int]agentrun.DelegationTaskRecord,
) []indexedOutcome {
	outcomes := make([]indexedOutcome, len(tasks))
	jobs := make(chan int)
	workers := min(executor.policy.MaxConcurrent, len(tasks))
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wg.Done()
			for taskOffset := range jobs {
				task := tasks[taskOffset]
				outcome := executor.runTask(
					ctx, parent, delegationID, task, records[task.index],
				)
				outcomes[taskOffset] = indexedOutcome{
					index: task.index, taskOutcome: outcome,
				}
			}
		}()
	}
	for index := range tasks {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	sort.Slice(outcomes, func(i, j int) bool {
		return outcomes[i].index < outcomes[j].index
	})
	return outcomes
}

type indexedOutcome struct {
	index int
	taskOutcome
}

func (outcome indexedOutcome) reportIndex() int { return outcome.index }

func (executor *Executor) runTask(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	record agentrun.DelegationTaskRecord,
) taskOutcome {
	key := fmt.Sprintf("%s\x00%s\x00%d", parent.RunID, delegationID, task.index)
	executor.mu.Lock()
	if existing := executor.flights[key]; existing != nil {
		executor.mu.Unlock()
		select {
		case <-ctx.Done():
			return taskOutcome{report: cancelledReport(task, ErrorParentCancelled, ctx.Err())}
		case <-existing.done:
			return taskOutcome{
				report:   existing.report,
				evidence: cloneEvidenceUnits(existing.evidence),
			}
		}
	}
	flight := &taskFlight{done: make(chan struct{})}
	executor.flights[key] = flight
	executor.mu.Unlock()
	outcome := executor.runTaskOwned(ctx, parent, delegationID, task, record)
	executor.mu.Lock()
	flight.report = outcome.report
	flight.evidence = cloneEvidenceUnits(outcome.evidence)
	close(flight.done)
	delete(executor.flights, key)
	executor.mu.Unlock()
	return outcome
}

func (executor *Executor) runTaskOwned(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	record agentrun.DelegationTaskRecord,
) taskOutcome {
	if record.Existing {
		if record.SettledUsage != nil {
			return executor.replayTask(ctx, parent, delegationID, task)
		}
		report := interruptedReport(task)
		_, _ = executor.persistence.SettleDelegationTask(
			context.WithoutCancel(ctx),
			agentrun.DelegationSettlement{
				ParentRunID: parent.RunID, DelegationID: delegationID,
				TaskIndex: task.index, ChildRunID: task.childRunID,
			},
		)
		executor.emitTerminal(parent, delegationID, task, report, time.Time{})
		return taskOutcome{report: report}
	}
	if err := ctx.Err(); err != nil {
		report := cancelledReport(task, ErrorParentCancelled, err)
		_, _ = executor.persistence.SettleDelegationTask(
			context.WithoutCancel(ctx),
			agentrun.DelegationSettlement{
				ParentRunID: parent.RunID, DelegationID: delegationID,
				TaskIndex: task.index, ChildRunID: task.childRunID,
			},
		)
		executor.emitTerminal(parent, delegationID, task, report, time.Time{})
		return taskOutcome{report: report}
	}
	slot := executor.capabilitySlot(task.capability)
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		report := cancelledReport(task, ErrorParentCancelled, ctx.Err())
		_, _ = executor.persistence.SettleDelegationTask(
			context.WithoutCancel(ctx),
			agentrun.DelegationSettlement{
				ParentRunID: parent.RunID, DelegationID: delegationID,
				TaskIndex: task.index, ChildRunID: task.childRunID,
			},
		)
		executor.emitTerminal(parent, delegationID, task, report, time.Time{})
		return taskOutcome{report: report}
	}
	if err := executor.persistence.LinkDelegationChild(
		ctx, parent.RunID, delegationID, task.index, task.childRunID,
	); err != nil {
		report := failedReport(task, ErrorChildExecution, err)
		_, _ = executor.persistence.SettleDelegationTask(
			context.WithoutCancel(ctx),
			agentrun.DelegationSettlement{
				ParentRunID: parent.RunID, DelegationID: delegationID,
				TaskIndex: task.index, ChildRunID: task.childRunID,
			},
		)
		executor.emitTerminal(parent, delegationID, task, report, time.Time{})
		return taskOutcome{report: report}
	}

	started := time.Now()
	executor.emit(
		agentrun.EventDelegationStarted, parent, delegationID,
		task.childRunID, task.request, "running", "", "", 0, agentapi.Usage{},
	)
	runCtx, cancel := context.WithDeadline(ctx, task.limits.Deadline)
	result, runErr := executor.runtime.Run(runCtx, executor.runRequest(parent, delegationID, task))
	childErr := runCtx.Err()
	cancel()
	if runErr != nil {
		result = agentapi.RunResult{
			RunID: task.childRunID, Status: agentapi.RunFailed,
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
			Code: ErrorChildInputLimit, Message: "child input token limit exceeded",
		}
	}
	if result.Usage.OutputTokens > executor.policy.MaxChildOutputTokens {
		result.Status = agentapi.RunFailed
		result.Error = &agentapi.RunError{
			Code: ErrorChildOutputLimit, Message: "child output token limit exceeded",
		}
	}
	report, err := projectReport(result, task.capability.ID, task.reportID)
	if err != nil {
		report = failedReport(task, ErrorChildExecution, err)
		report.Usage = publicDelegationUsage(result)
	}
	report = boundReport(report, executor.policy.MaxReportTokens)
	raw, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		report = failedReport(task, ErrorReportPersistenceFailed, marshalErr)
		executor.emitTerminal(parent, delegationID, task, report, started)
		return taskOutcome{report: report}
	}
	artifact := &agentrun.DelegationArtifact{
		ID: task.artifactID, RunID: task.childRunID,
		Kind:        agentrun.DelegationReportArtifactKind,
		Schema:      agentapi.SchemaRef{ID: "delegation.report", Version: 1},
		ContentHash: hashBytes(raw), Content: raw,
	}
	var evidenceArtifact *agentrun.RunArtifact
	if len(result.EvidenceUnits) > 0 {
		persisted, evidenceErr := agentrun.NewEvidenceLedgerArtifact(
			task.childRunID,
			result.EvidenceUnits,
		)
		if evidenceErr != nil {
			report = failedReport(task, ErrorReportPersistenceFailed, evidenceErr)
			report.Usage = publicDelegationUsage(result)
			_, _ = executor.persistence.SettleDelegationTask(
				context.WithoutCancel(ctx),
				agentrun.DelegationSettlement{
					ParentRunID: parent.RunID, DelegationID: delegationID,
					TaskIndex: task.index, ChildRunID: task.childRunID,
					Usage: result.Usage,
				},
			)
			executor.emitTerminal(parent, delegationID, task, report, started)
			return taskOutcome{report: report}
		}
		evidenceArtifact = &persisted
	}
	_, settleErr := executor.persistence.SettleDelegationTask(
		context.WithoutCancel(ctx),
		agentrun.DelegationSettlement{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			TaskIndex: task.index, ChildRunID: task.childRunID,
			Usage: result.Usage, Artifact: artifact,
			EvidenceArtifact: evidenceArtifact,
		},
	)
	if settleErr != nil {
		code := ErrorReportPersistenceFailed
		if errors.Is(settleErr, agentrun.ErrDelegationAccounting) {
			code = ErrorBudgetAccountingViolation
		}
		report = failedReport(task, code, settleErr)
		report.Usage = publicDelegationUsage(result)
		executor.emitTerminal(parent, delegationID, task, report, started)
		return taskOutcome{report: report}
	}
	executor.emitTerminal(parent, delegationID, task, report, started)
	return taskOutcome{
		report: report, evidence: cloneEvidenceUnits(result.EvidenceUnits),
	}
}

func (executor *Executor) replayTask(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
) taskOutcome {
	record, artifact, err := executor.persistence.GetDelegationTask(
		ctx, parent.RunID, delegationID, task.index,
	)
	if err != nil || record.SettledUsage == nil || artifact == nil {
		if err == nil {
			err = fmt.Errorf("settled delegation report is unavailable")
		}
		return taskOutcome{report: failedReport(task, ErrorReportPersistenceFailed, err)}
	}
	report, err := decodePersistedReport(*artifact, task)
	if err != nil {
		return taskOutcome{report: failedReport(task, ErrorReportPersistenceFailed, err)}
	}
	evidence, err := executor.persistence.GetDelegationEvidence(
		ctx,
		parent.RunID,
		delegationID,
		task.index,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return taskOutcome{report: failedReport(
			task,
			ErrorReportPersistenceFailed,
			fmt.Errorf("load delegation evidence: %w", err),
		)}
	}
	return taskOutcome{
		report: report, evidence: cloneEvidenceUnits(evidence),
	}
}

func decodePersistedReport(
	artifact agentrun.DelegationArtifact,
	task preparedTask,
) (agentapi.DelegationReport, error) {
	if artifact.ID != task.artifactID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact ID mismatch",
		)
	}
	if artifact.RunID != task.childRunID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact run ID mismatch",
		)
	}
	if artifact.Kind != agentrun.DelegationReportArtifactKind {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact kind mismatch",
		)
	}
	if artifact.Schema.ID != "delegation.report" ||
		artifact.Schema.Version != 1 {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact schema mismatch",
		)
	}
	if hashBytes(artifact.Content) != artifact.ContentHash {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact hash mismatch",
		)
	}
	var report agentapi.DelegationReport
	if err := json.Unmarshal(artifact.Content, &report); err != nil {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"decode delegation report artifact: %w",
			err,
		)
	}
	if report.RunID != task.childRunID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report run ID mismatch",
		)
	}
	if report.ReportID != task.reportID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report ID mismatch",
		)
	}
	if report.Capability != task.capability.ID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report capability mismatch",
		)
	}
	return report, nil
}

func (executor *Executor) reject(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	index int,
	task agentapi.DelegationTask,
	capability agentapi.Capability,
	objectiveHash,
	code string,
	rejection error,
) (agentapi.DelegationReport, error) {
	ref := agentapi.CapabilityRef{ID: strings.TrimSpace(task.Capability)}
	hash := ""
	if capability.ID != "" {
		ref = agentapi.CapabilityRef{ID: capability.ID, Version: capability.Version}
		hash = capability.ContentHash
	}
	if objectiveHash == "" {
		objectiveHash = hashJSON(task)
	}
	if _, err := executor.persistence.RejectDelegationTask(
		context.WithoutCancel(ctx),
		agentrun.DelegationRejection{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			TaskIndex: index, Capability: ref, CapabilityHash: hash,
			ObjectiveHash: objectiveHash, Code: code,
		},
	); err != nil {
		return agentapi.DelegationReport{}, fmt.Errorf("persist delegation rejection: %w", err)
	}
	report := agentapi.DelegationReport{
		Capability:   strings.TrimSpace(task.Capability),
		Status:       agentapi.DelegationRejected,
		Completeness: agentapi.DelegationIncomplete,
		Error: &agentapi.RunError{
			Code: code, Message: rejection.Error(),
		},
	}
	executor.emit(
		agentrun.EventDelegationRejected, parent, delegationID, "",
		task, string(report.Status), code, "", 0, agentapi.Usage{},
	)
	return report, nil
}

func (executor *Executor) runRequest(
	parent ParentContext,
	delegationID string,
	task preparedTask,
) agentapi.RunRequest {
	input, _ := childInput(parent, delegationID, task)
	tools := make([]string, 0, len(task.capability.ToolIDs))
	for _, id := range task.capability.ToolIDs {
		if id != string(DelegateToolID) {
			tools = append(tools, id)
		}
	}
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
			RestrictVisible: true, VisibleToolIDs: tools,
		},
		Policy: agentapi.RunPolicy{
			EvidenceRequired: true, EvidenceSeeded: len(task.context) > 0,
			MaxToolCalls: executor.policy.MaxChildToolCalls,
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
			SessionID:   parent.Correlation.SessionID,
			ParentRunID: parent.RunID,
		},
	}
}

func childInput(
	parent ParentContext,
	delegationID string,
	task preparedTask,
) (json.RawMessage, error) {
	payload := struct {
		Capability            string   `json:"capability"`
		Objective             string   `json:"objective"`
		ParentQuestionSummary string   `json:"parent_question_summary"`
		FocusFacets           []string `json:"focus_facets"`
		EvidenceRefs          []string `json:"evidence_refs"`
		DelegationID          string   `json:"delegation_id"`
		ParentRunID           string   `json:"parent_run_id"`
		TaskIndex             int      `json:"task_index"`
	}{
		Capability: task.capability.ID, Objective: task.request.Objective,
		ParentQuestionSummary: investigation.BoundedSummary(parent.QuestionSummary),
		FocusFacets:           append([]string(nil), task.request.FocusFacets...),
		EvidenceRefs:          append([]string(nil), task.request.EvidenceRefs...),
		DelegationID:          delegationID, ParentRunID: parent.RunID, TaskIndex: task.index,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (executor *Executor) childLimits(
	parent ParentContext,
	definition agentapi.Definition,
) (agentapi.RunLimits, error) {
	now := time.Now().UTC()
	timeout := executor.policy.ChildTimeout
	if timeout >= definition.Budget.Timeout {
		timeout = definition.Budget.Timeout - time.Millisecond
	}
	deadline := now.Add(timeout)
	if !parent.Limits.Deadline.IsZero() && parent.Limits.Deadline.Before(deadline) {
		deadline = parent.Limits.Deadline
	}
	if !deadline.After(now) {
		return agentapi.RunLimits{}, fmt.Errorf("parent has no remaining execution time")
	}
	maxSteps := min(definition.Budget.MaxSteps, executor.policy.MaxChildTurns)
	tokens := executor.policy.MaxChildInputTokens + executor.policy.MaxChildOutputTokens
	cost := estimatedCostMicros(
		executor.policy.MaxChildInputTokens,
		executor.policy.MaxChildOutputTokens,
		definition.Model.InputPriceMicrosPerMillionTokens,
		definition.Model.OutputPriceMicrosPerMillionTokens,
	)
	return agentapi.RunLimits{
		Deadline: deadline, MaxSteps: maxSteps,
		MaxToolCalls:   executor.policy.MaxChildToolCalls,
		MaxTotalTokens: tokens, MaxCostMicros: cost,
	}, nil
}

func (executor *Executor) capabilitySlot(
	capability agentapi.Capability,
) chan struct{} {
	key := fmt.Sprintf("%s@%d", capability.ID, capability.Version)
	executor.mu.Lock()
	defer executor.mu.Unlock()
	slot := executor.capabilitySlots[key]
	if slot == nil {
		slot = make(chan struct{}, capability.MaxConcurrency)
		executor.capabilitySlots[key] = slot
	}
	return slot
}

func (executor *Executor) emitTerminal(
	parent ParentContext,
	delegationID string,
	task preparedTask,
	report agentapi.DelegationReport,
	started time.Time,
) {
	eventType := agentrun.EventDelegationFailed
	switch report.Status {
	case agentapi.DelegationCompleted, agentapi.DelegationPartial:
		eventType = agentrun.EventDelegationDone
	case agentapi.DelegationCancelled:
		eventType = agentrun.EventDelegationCancelled
	}
	duration := int64(0)
	if !started.IsZero() {
		duration = time.Since(started).Milliseconds()
	}
	raw, _ := json.Marshal(report)
	executor.emitWithDetails(
		eventType, parent, delegationID, task.childRunID, task.request,
		string(report.Status), runErrorCode(report.Error), report.ReportID,
		duration,
		agentapi.Usage{
			InputTokens:     report.Usage.InputTokens,
			OutputTokens:    report.Usage.OutputTokens,
			ReasoningTokens: report.Usage.ReasoningTokens,
			TotalTokens:     report.Usage.TotalTokens,
			CostMicros:      report.Usage.CostMicros,
		},
		agentrun.ExecutionEvent{
			ToolCalls:    report.Usage.ToolCalls,
			ReportBytes:  len(raw),
			Completeness: string(report.Completeness),
		},
	)
}

func (executor *Executor) emit(
	eventType agentrun.EventType,
	parent ParentContext,
	delegationID,
	childRunID string,
	task agentapi.DelegationTask,
	status,
	errorCode,
	reportID string,
	durationMS int64,
	usage agentapi.Usage,
) {
	executor.emitWithDetails(
		eventType,
		parent,
		delegationID,
		childRunID,
		task,
		status,
		errorCode,
		reportID,
		durationMS,
		usage,
		agentrun.ExecutionEvent{},
	)
}

func (executor *Executor) emitWithDetails(
	eventType agentrun.EventType,
	parent ParentContext,
	delegationID,
	childRunID string,
	task agentapi.DelegationTask,
	status,
	errorCode,
	reportID string,
	durationMS int64,
	usage agentapi.Usage,
	details agentrun.ExecutionEvent,
) {
	if executor.events == nil {
		return
	}
	details.RunID = parent.RunID
	details.ParentRunID = parent.RunID
	details.ChildRunID = childRunID
	details.DelegationID = delegationID
	details.Capability = strings.TrimSpace(task.Capability)
	details.ObjectiveSummary = truncateText(task.Objective, 240)
	details.Status = status
	details.ErrorCode = errorCode
	details.ReportID = reportID
	details.DurationMS = durationMS
	details.Usage = usage
	executor.events.EmitEvent(eventType, details)
}

func (executor *Executor) emitValidation(
	parent ParentContext,
	delegationID string,
	validation agentapi.DelegationValidation,
	err error,
) {
	if executor.events == nil {
		return
	}
	status := "completed"
	errorCode := ""
	if err != nil {
		status = "failed"
		errorCode = "validation_failed"
	}
	executor.events.EmitEvent(
		agentrun.EventDelegationValidated,
		agentrun.ExecutionEvent{
			RunID: parent.RunID, ParentRunID: parent.RunID,
			DelegationID:            delegationID,
			Status:                  status,
			ErrorCode:               errorCode,
			CitationCoverage:        validation.CitationCoverage,
			StructuredClaimCoverage: validation.StructuredClaimCoverage,
			ConflictCount:           len(validation.Conflicts),
			RequiresVerification:    validation.RequiresVerification,
			VerificationReasons: append(
				[]string(nil),
				validation.VerificationReasons...,
			),
		},
	)
}

func normalizePolicy(
	policy agentapi.DelegationPolicy,
) (agentapi.DelegationPolicy, error) {
	if policy.MaxDepth <= 0 || policy.MaxDepth > 1 {
		return policy, fmt.Errorf("delegation max depth must be 1")
	}
	if policy.MaxChildren <= 0 || policy.MaxConcurrent <= 0 ||
		policy.MaxConcurrent > policy.MaxChildren ||
		policy.MaxChildTurns <= 0 || policy.MaxChildToolCalls <= 0 ||
		policy.MaxChildInputTokens <= 0 || policy.MaxChildOutputTokens <= 0 ||
		policy.MaxReportTokens <= 0 || policy.MaxTotalTokens <= 0 ||
		policy.MaxTotalCostMicros < 0 || policy.ParentAnswerReserve < 0 ||
		policy.ChildTimeout <= 0 {
		return policy, fmt.Errorf("delegation policy limits are invalid")
	}
	if policy.MaxReportTokens < minimumBoundedReportTokens() {
		return policy, fmt.Errorf(
			"delegation max report tokens must be at least %d",
			minimumBoundedReportTokens(),
		)
	}
	return policy, nil
}

func selectContext(
	parent ParentContext,
	references,
	facets []string,
	maxTokens int64,
) []agentapi.ContextBlock {
	if len(references) == 0 || len(parent.Context) == 0 {
		return nil
	}
	facetSet := make(map[string]struct{}, len(facets))
	for _, facet := range facets {
		facetSet[facet] = struct{}{}
	}
	seen := make(map[string]struct{}, len(references))
	var blocks []agentapi.ContextBlock
	remainingBytes := int(maxTokens * 4 / 2)
	for _, reference := range references {
		block, ok := parent.Context[reference]
		if !ok {
			continue
		}
		key := block.ContentHash + "\x00" + block.Source + "\x00" + block.Title
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		filtered := block
		filtered.Evidence = make([]tool.EvidenceUnit, 0, len(block.Evidence))
		for _, unit := range block.Evidence {
			if evidenceMatchesFacets(unit, facetSet) {
				filtered.Evidence = append(filtered.Evidence, cloneEvidenceUnit(unit))
			}
		}
		if remainingBytes <= 0 {
			break
		}
		filtered.Content = truncateText(filtered.Content, remainingBytes)
		filtered.ContentHash = hashBytes([]byte(filtered.Content))
		remainingBytes -= len(filtered.Content)
		blocks = append(blocks, filtered)
	}
	return blocks
}

func evidenceMatchesFacets(
	unit tool.EvidenceUnit,
	facets map[string]struct{},
) bool {
	if len(facets) == 0 || len(unit.Facets) == 0 {
		return true
	}
	for _, facet := range unit.Facets {
		if _, ok := facets[facet]; ok {
			return true
		}
	}
	return false
}

func intersectPermissions(
	left,
	right agentapi.PermissionPolicy,
) agentapi.PermissionPolicy {
	allowed := make(map[string]struct{}, len(right.Scopes))
	for _, scope := range right.Scopes {
		allowed[scope] = struct{}{}
	}
	var scopes []string
	for _, scope := range left.Scopes {
		if _, ok := allowed[scope]; ok {
			scopes = append(scopes, scope)
		}
	}
	sort.Strings(scopes)
	return agentapi.PermissionPolicy{Scopes: scopes}
}

func estimatedCostMicros(
	inputTokens,
	outputTokens,
	inputPrice,
	outputPrice int64,
) int64 {
	inputCost := tokenCost(inputTokens, inputPrice)
	outputCost := tokenCost(outputTokens, outputPrice)
	if inputCost > math.MaxInt64-outputCost {
		return math.MaxInt64
	}
	return inputCost + outputCost
}

func tokenCost(tokens, price int64) int64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	if tokens > math.MaxInt64/price {
		return math.MaxInt64
	}
	product := tokens * price
	cost := product / 1_000_000
	if product%1_000_000 != 0 {
		cost++
	}
	return cost
}

func estimateTokens(
	input json.RawMessage,
	context []agentapi.ContextBlock,
) int64 {
	bytes := len(input)
	for _, block := range context {
		bytes += len(block.Content)
	}
	return int64((bytes + 3) / 4)
}

func failedReport(
	task preparedTask,
	code string,
	err error,
) agentapi.DelegationReport {
	return agentapi.DelegationReport{
		RunID: task.childRunID, ReportID: task.reportID,
		Capability:   task.request.Capability,
		Status:       agentapi.DelegationFailed,
		Completeness: agentapi.DelegationIncomplete,
		Error:        &agentapi.RunError{Code: code, Message: err.Error()},
	}
}

func cancelledReport(
	task preparedTask,
	code string,
	err error,
) agentapi.DelegationReport {
	return agentapi.DelegationReport{
		RunID: task.childRunID, ReportID: task.reportID,
		Capability:   task.request.Capability,
		Status:       agentapi.DelegationCancelled,
		Completeness: agentapi.DelegationIncomplete,
		Error:        &agentapi.RunError{Code: code, Message: err.Error()},
	}
}

func interruptedReport(task preparedTask) agentapi.DelegationReport {
	return agentapi.DelegationReport{
		RunID: task.childRunID, ReportID: task.reportID,
		Capability:   task.request.Capability,
		Status:       agentapi.DelegationInterrupted,
		Completeness: agentapi.DelegationIncomplete,
		Error: &agentapi.RunError{
			Code:    ErrorInterrupted,
			Message: "delegation admission was recovered without a durable report",
		},
	}
}

func canonicalStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func appendUniqueStrings(target []string, values ...string) []string {
	seen := make(map[string]struct{}, len(target)+len(values))
	out := make([]string, 0, len(target)+len(values))
	for _, value := range append(append([]string(nil), target...), values...) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func stableID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

func hashJSON(value any) string {
	raw, _ := json.Marshal(value)
	return hashBytes(raw)
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func truncateText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]tool.EvidenceUnit, len(units))
	for index, unit := range units {
		out[index] = cloneEvidenceUnit(unit)
	}
	return out
}

package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

const escalationReceiptStarting agentapi.WorkflowEscalationStatus = "starting"

type WorkflowEscalationError struct {
	Code   string
	Detail string
	Cause  error
}

func (err *WorkflowEscalationError) Error() string {
	if err == nil {
		return ""
	}
	if err.Detail != "" {
		return err.Code + ": " + err.Detail
	}
	return err.Code
}

func (err *WorkflowEscalationError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func WorkflowEscalationErrorCode(err error) string {
	var escalationErr *WorkflowEscalationError
	if errors.As(err, &escalationErr) {
		return escalationErr.Code
	}
	return ""
}

type WorkflowEscalationParentLoader interface {
	LoadWorkflowEscalationParent(
		context.Context,
		string,
	) (agentapi.WorkflowEscalationParent, error)
}

type ResolvedWorkflowEscalationEvidence struct {
	Ref  string
	Unit tool.EvidenceUnit
}

type WorkflowEscalationHandoff struct {
	Evidence []ResolvedWorkflowEscalationEvidence
	Reports  []agentapi.WorkflowEscalationReport
}

type WorkflowEscalationHandoffResolver interface {
	ResolveWorkflowEscalationHandoff(
		context.Context,
		agentapi.WorkflowEscalationParent,
		agentapi.WorkflowEscalationRequest,
	) (WorkflowEscalationHandoff, error)
}

type WorkflowEscalationStarter interface {
	Start(context.Context, StartRequest) (*RunRecord, error)
	GetRun(context.Context, string, int64, bool) (*RunRecord, error)
}

type WorkflowEscalationRecord struct {
	ParentRunID    string
	RequestID      string
	RequestHash    string
	WorkflowRunID  string
	BindingID      string
	BindingVersion int64
	Status         agentapi.WorkflowEscalationStatus
	ErrorCode      string
}

type WorkflowEscalationReceiptStore interface {
	LoadWorkflowEscalation(
		context.Context,
		string,
		string,
	) (WorkflowEscalationRecord, error)
	ReserveWorkflowEscalation(
		context.Context,
		WorkflowEscalationRecord,
	) (WorkflowEscalationRecord, bool, error)
	FinishWorkflowEscalation(
		context.Context,
		WorkflowEscalationRecord,
	) error
}

type WorkflowEscalatorConfig struct {
	MaxFocusFacets      int
	MaxEvidenceRefs     int
	MaxReportRefs       int
	MaxEvidenceUnits    int
	MaxReports          int
	MaxReportBytes      int
	MaxTotalReportBytes int
	MaxInputBytes       int
}

func DefaultWorkflowEscalatorConfig() WorkflowEscalatorConfig {
	return WorkflowEscalatorConfig{
		MaxFocusFacets:      32,
		MaxEvidenceRefs:     32,
		MaxReportRefs:       16,
		MaxEvidenceUnits:    64,
		MaxReports:          16,
		MaxReportBytes:      64 << 10,
		MaxTotalReportBytes: 256 << 10,
		MaxInputBytes:       1 << 20,
	}
}

type ServerWorkflowEscalator struct {
	bindings *WorkflowBindingRegistry
	starter  WorkflowEscalationStarter
	receipts WorkflowEscalationReceiptStore
	parents  WorkflowEscalationParentLoader
	handoffs WorkflowEscalationHandoffResolver
	schemas  *agentapi.SchemaRegistry
	config   WorkflowEscalatorConfig

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewWorkflowEscalator(
	bindings *WorkflowBindingRegistry,
	starter WorkflowEscalationStarter,
	receipts WorkflowEscalationReceiptStore,
	parents WorkflowEscalationParentLoader,
	handoffs WorkflowEscalationHandoffResolver,
	schemas *agentapi.SchemaRegistry,
	config WorkflowEscalatorConfig,
) (*ServerWorkflowEscalator, error) {
	if bindings == nil || starter == nil || receipts == nil || parents == nil ||
		schemas == nil {
		return nil, fmt.Errorf("workflow escalator dependencies are required")
	}
	defaults := DefaultWorkflowEscalatorConfig()
	if config.MaxFocusFacets <= 0 {
		config.MaxFocusFacets = defaults.MaxFocusFacets
	}
	if config.MaxEvidenceRefs <= 0 {
		config.MaxEvidenceRefs = defaults.MaxEvidenceRefs
	}
	if config.MaxReportRefs <= 0 {
		config.MaxReportRefs = defaults.MaxReportRefs
	}
	if config.MaxEvidenceUnits <= 0 {
		config.MaxEvidenceUnits = defaults.MaxEvidenceUnits
	}
	if config.MaxReports <= 0 {
		config.MaxReports = defaults.MaxReports
	}
	if config.MaxReportBytes <= 0 {
		config.MaxReportBytes = defaults.MaxReportBytes
	}
	if config.MaxTotalReportBytes <= 0 {
		config.MaxTotalReportBytes = defaults.MaxTotalReportBytes
	}
	if config.MaxInputBytes <= 0 {
		config.MaxInputBytes = defaults.MaxInputBytes
	}
	return &ServerWorkflowEscalator{
		bindings: bindings,
		starter:  starter,
		receipts: receipts,
		parents:  parents,
		handoffs: handoffs,
		schemas:  schemas,
		config:   config,
		locks:    make(map[string]*sync.Mutex),
	}, nil
}

// SupportsWorkflowEscalation reports whether an exact Capability snapshot has
// a registered Workflow binding before the parent Run is escalated.
func (escalator *ServerWorkflowEscalator) SupportsWorkflowEscalation(
	ref agentapi.CapabilityRef,
	contentHash string,
) bool {
	if escalator == nil || escalator.bindings == nil {
		return false
	}
	_, _, _, err := escalator.bindings.Resolve(ref, contentHash)
	return err == nil
}

func StableWorkflowEscalationRunID(parentRunID, requestID string) string {
	sum := sha256.Sum256([]byte(
		"workflow_escalation\x00" + parentRunID + "\x00" + requestID,
	))
	return "workflow_" + hex.EncodeToString(sum[:24])
}

func (escalator *ServerWorkflowEscalator) Escalate(
	ctx context.Context,
	request agentapi.WorkflowEscalationRequest,
) (agentapi.WorkflowEscalationReceipt, error) {
	if escalator == nil || escalator.bindings == nil || escalator.starter == nil ||
		escalator.receipts == nil || escalator.parents == nil ||
		escalator.schemas == nil {
		return rejectedEscalationReceipt(request, agentapi.WorkflowUnavailable),
			escalationError(agentapi.WorkflowUnavailable, "workflow escalator is unavailable", nil)
	}
	normalized, requestHash, err := escalator.normalizeRequest(request)
	if err != nil {
		return rejectedEscalationReceipt(request, agentapi.WorkflowInvalidHandoff),
			escalationError(agentapi.WorkflowInvalidHandoff, err.Error(), err)
	}
	lock := escalator.requestLock(normalized.ParentRunID, normalized.RequestID)
	lock.Lock()
	defer lock.Unlock()

	persisted, err := escalator.receipts.LoadWorkflowEscalation(
		ctx,
		normalized.ParentRunID,
		normalized.RequestID,
	)
	if err == nil {
		return escalator.resumeReceipt(ctx, normalized, requestHash, persisted)
	}
	if !errors.Is(err, ErrNotFound) {
		return rejectedEscalationReceipt(normalized, agentapi.WorkflowUnavailable),
			escalationError(agentapi.WorkflowUnavailable, "load escalation receipt", err)
	}

	parent, err := escalator.parents.LoadWorkflowEscalationParent(
		ctx,
		normalized.ParentRunID,
	)
	if err != nil {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			WorkflowEscalationRecord{},
			agentapi.WorkflowStartConflict,
			"load active parent",
			err,
		)
	}
	if parent.RunID != normalized.ParentRunID || parent.Actor.UserID <= 0 {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			WorkflowEscalationRecord{},
			agentapi.WorkflowInvalidHandoff,
			"parent identity is invalid",
			nil,
		)
	}
	registration, capability, definition, err := escalator.bindings.Resolve(
		normalized.Capability,
		normalized.CapabilityHash,
	)
	if err != nil {
		code := agentapi.WorkflowUnavailable
		detail := "resolve workflow binding"
		if errors.Is(err, ErrWorkflowBindingNotFound) {
			detail = agentapi.WorkflowBindingNotFound
		}
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			WorkflowEscalationRecord{},
			code,
			detail,
			err,
		)
	}
	record := WorkflowEscalationRecord{
		ParentRunID: normalized.ParentRunID,
		RequestID:   normalized.RequestID,
		RequestHash: requestHash,
		WorkflowRunID: StableWorkflowEscalationRunID(
			normalized.ParentRunID,
			normalized.RequestID,
		),
		BindingID:      registration.Binding.ID,
		BindingVersion: registration.Binding.Version,
		Status:         escalationReceiptStarting,
	}
	if !bindingAllowsReason(registration.Binding, normalized.Reason) {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			record,
			agentapi.WorkflowReasonNotAllowed,
			fmt.Sprintf("reason %q is not allowed by binding", normalized.Reason),
			nil,
		)
	}
	effectivePermissions, err := escalationPermissions(
		parent,
		capability,
		registration.Binding,
		definition,
	)
	if err != nil {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			record,
			agentapi.WorkflowPermissionDenied,
			"workflow permissions exceed the escalation ceiling",
			err,
		)
	}
	if err := ensureEscalationBudget(parent.Remaining, definition.Budget); err != nil {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			record,
			agentapi.WorkflowBudgetInsufficient,
			"workflow budget exceeds the remaining escalation budget",
			err,
		)
	}
	handoff, err := escalator.resolveHandoff(ctx, parent, normalized)
	if err != nil {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			record,
			agentapi.WorkflowInvalidHandoff,
			"resolve bounded escalation handoff",
			err,
		)
	}
	buildResult, err := registration.Builder.BuildWorkflowEscalation(
		ctx,
		agentapi.WorkflowEscalationBuildRequest{
			Request: normalized, Parent: cloneEscalationParent(parent),
			Capability: cloneCapabilityForBinding(capability),
			Binding:    cloneWorkflowBinding(registration.Binding),
			Evidence:   cloneEscalationEvidence(handoff.Evidence),
			Reports:    cloneEscalationReports(handoff.Reports),
		},
	)
	if err != nil {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			record,
			agentapi.WorkflowInvalidHandoff,
			"build workflow input",
			err,
		)
	}
	if err := escalator.validateBuildResult(
		registration.Binding,
		definition,
		handoff,
		buildResult,
	); err != nil {
		return escalator.reject(
			ctx,
			normalized,
			requestHash,
			record,
			agentapi.WorkflowInvalidHandoff,
			"validate workflow input",
			err,
		)
	}
	reserved, created, err := escalator.receipts.ReserveWorkflowEscalation(
		ctx,
		record,
	)
	if err != nil {
		return rejectedEscalationReceipt(normalized, agentapi.WorkflowStartConflict),
			escalationError(agentapi.WorkflowStartConflict, "reserve escalation receipt", err)
	}
	if !created {
		if reserved.RequestHash != requestHash {
			return receiptFromRecord(reserved, agentapi.EscalationRejected),
				escalationError(
					agentapi.WorkflowStartConflict,
					"request id is already bound to different escalation content",
					nil,
				)
		}
		if reserved.Status != escalationReceiptStarting {
			return escalator.receiptForExisting(reserved)
		}
		record = reserved
	}
	started, startErr := escalator.starter.Start(ctx, StartRequest{
		RunID:       record.WorkflowRunID,
		ParentRunID: normalized.ParentRunID,
		Workflow: DefinitionRef{
			ID:      registration.Binding.Workflow.ID,
			Version: registration.Binding.Workflow.Version,
		},
		Input:               append(json.RawMessage(nil), buildResult.Input...),
		SeedEvidence:        cloneToolEvidence(buildResult.SeedEvidence),
		Actor:               parent.Actor,
		ActorPermissions:    effectivePermissions,
		Scenario:            registration.Binding.Scenario,
		ScenarioPermissions: effectivePermissions,
	})
	if startErr != nil {
		if errors.Is(startErr, ErrConflict) {
			existing, lookupErr := escalator.starter.GetRun(
				ctx,
				record.WorkflowRunID,
				parent.Actor.UserID,
				false,
			)
			if lookupErr == nil && sameEscalatedWorkflow(
				existing,
				record,
				registration.Binding,
				parent,
			) {
				record.Status = agentapi.EscalationAccepted
				record.ErrorCode = ""
				if err := escalator.receipts.FinishWorkflowEscalation(ctx, record); err != nil {
					return receiptFromRecord(record, agentapi.EscalationAlreadyStarted),
						escalationError(
							agentapi.WorkflowStartConflict,
							"persist recovered escalation receipt",
							err,
						)
				}
				return receiptFromRecord(record, agentapi.EscalationAlreadyStarted), nil
			}
		}
		code := classifyWorkflowStartError(startErr)
		record.Status = agentapi.EscalationRejected
		record.ErrorCode = code
		finishErr := escalator.receipts.FinishWorkflowEscalation(ctx, record)
		if finishErr != nil {
			startErr = errors.Join(startErr, finishErr)
		}
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(code, "start durable workflow", startErr)
	}
	if started == nil || !sameEscalatedWorkflow(
		started,
		record,
		registration.Binding,
		parent,
	) {
		record.Status = agentapi.EscalationRejected
		record.ErrorCode = agentapi.WorkflowStartConflict
		finishErr := escalator.receipts.FinishWorkflowEscalation(ctx, record)
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(
				agentapi.WorkflowStartConflict,
				"workflow start returned a conflicting run",
				finishErr,
			)
	}
	record.Status = agentapi.EscalationAccepted
	record.ErrorCode = ""
	if err := escalator.receipts.FinishWorkflowEscalation(ctx, record); err != nil {
		return receiptFromRecord(record, agentapi.EscalationAccepted),
			escalationError(agentapi.WorkflowStartFailed, "persist accepted escalation receipt", err)
	}
	return receiptFromRecord(record, agentapi.EscalationAccepted), nil
}

func (escalator *ServerWorkflowEscalator) resumeReceipt(
	ctx context.Context,
	request agentapi.WorkflowEscalationRequest,
	requestHash string,
	record WorkflowEscalationRecord,
) (agentapi.WorkflowEscalationReceipt, error) {
	if record.RequestHash != requestHash {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(
				agentapi.WorkflowStartConflict,
				"request id is already bound to different escalation content",
				nil,
			)
	}
	if record.Status != escalationReceiptStarting {
		return escalator.receiptForExisting(record)
	}
	parent, err := escalator.parents.LoadWorkflowEscalationParent(
		ctx,
		request.ParentRunID,
	)
	if err != nil {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(agentapi.WorkflowStartConflict, "reload escalation parent", err)
	}
	registration, capability, definition, err := escalator.bindings.Resolve(
		request.Capability,
		request.CapabilityHash,
	)
	if err != nil ||
		registration.Binding.ID != record.BindingID ||
		registration.Binding.Version != record.BindingVersion {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(
				agentapi.WorkflowStartConflict,
				"reserved workflow binding is unavailable",
				err,
			)
	}
	effectivePermissions, err := escalationPermissions(
		parent,
		capability,
		registration.Binding,
		definition,
	)
	if err != nil {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(agentapi.WorkflowPermissionDenied, "reload escalation permissions", err)
	}
	handoff, err := escalator.resolveHandoff(ctx, parent, request)
	if err != nil {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(agentapi.WorkflowInvalidHandoff, "reload escalation handoff", err)
	}
	buildResult, err := registration.Builder.BuildWorkflowEscalation(
		ctx,
		agentapi.WorkflowEscalationBuildRequest{
			Request: request, Parent: cloneEscalationParent(parent),
			Capability: cloneCapabilityForBinding(capability),
			Binding:    cloneWorkflowBinding(registration.Binding),
			Evidence:   cloneEscalationEvidence(handoff.Evidence),
			Reports:    cloneEscalationReports(handoff.Reports),
		},
	)
	if err != nil {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(agentapi.WorkflowInvalidHandoff, "rebuild workflow input", err)
	}
	if err := escalator.validateBuildResult(
		registration.Binding,
		definition,
		handoff,
		buildResult,
	); err != nil {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(agentapi.WorkflowInvalidHandoff, "revalidate workflow input", err)
	}
	started, startErr := escalator.starter.Start(ctx, StartRequest{
		RunID:       record.WorkflowRunID,
		ParentRunID: request.ParentRunID,
		Workflow: DefinitionRef{
			ID:      registration.Binding.Workflow.ID,
			Version: registration.Binding.Workflow.Version,
		},
		Input:               append(json.RawMessage(nil), buildResult.Input...),
		SeedEvidence:        cloneToolEvidence(buildResult.SeedEvidence),
		Actor:               parent.Actor,
		ActorPermissions:    effectivePermissions,
		Scenario:            registration.Binding.Scenario,
		ScenarioPermissions: effectivePermissions,
	})
	if startErr != nil {
		if errors.Is(startErr, ErrConflict) {
			started, err = escalator.starter.GetRun(
				ctx,
				record.WorkflowRunID,
				parent.Actor.UserID,
				false,
			)
			if err == nil && sameEscalatedWorkflow(
				started,
				record,
				registration.Binding,
				parent,
			) {
				record.Status = agentapi.EscalationAccepted
				record.ErrorCode = ""
				if err := escalator.receipts.FinishWorkflowEscalation(ctx, record); err != nil {
					return receiptFromRecord(record, agentapi.EscalationAlreadyStarted),
						escalationError(agentapi.WorkflowStartConflict, "finish recovered receipt", err)
				}
				return receiptFromRecord(record, agentapi.EscalationAlreadyStarted), nil
			}
		}
		code := classifyWorkflowStartError(startErr)
		record.Status = agentapi.EscalationRejected
		record.ErrorCode = code
		_ = escalator.receipts.FinishWorkflowEscalation(ctx, record)
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(code, "resume durable workflow start", startErr)
	}
	if !sameEscalatedWorkflow(started, record, registration.Binding, parent) {
		return receiptFromRecord(record, agentapi.EscalationRejected),
			escalationError(
				agentapi.WorkflowStartConflict,
				"resumed workflow start returned a conflicting run",
				nil,
			)
	}
	record.Status = agentapi.EscalationAccepted
	record.ErrorCode = ""
	if err := escalator.receipts.FinishWorkflowEscalation(ctx, record); err != nil {
		return receiptFromRecord(record, agentapi.EscalationAccepted),
			escalationError(agentapi.WorkflowStartFailed, "finish resumed receipt", err)
	}
	return receiptFromRecord(record, agentapi.EscalationAccepted), nil
}

func (escalator *ServerWorkflowEscalator) receiptForExisting(
	record WorkflowEscalationRecord,
) (agentapi.WorkflowEscalationReceipt, error) {
	if record.Status == agentapi.EscalationAccepted {
		return receiptFromRecord(record, agentapi.EscalationAlreadyStarted), nil
	}
	code := record.ErrorCode
	if code == "" {
		code = agentapi.WorkflowStartConflict
	}
	return receiptFromRecord(record, agentapi.EscalationRejected),
		escalationError(code, "escalation request was previously rejected", nil)
}

func (escalator *ServerWorkflowEscalator) reject(
	ctx context.Context,
	request agentapi.WorkflowEscalationRequest,
	requestHash string,
	record WorkflowEscalationRecord,
	code,
	detail string,
	cause error,
) (agentapi.WorkflowEscalationReceipt, error) {
	if record.ParentRunID == "" {
		record.ParentRunID = request.ParentRunID
		record.RequestID = request.RequestID
		record.RequestHash = requestHash
		record.WorkflowRunID = StableWorkflowEscalationRunID(
			request.ParentRunID,
			request.RequestID,
		)
		record.Status = escalationReceiptStarting
	}
	reserved, created, reserveErr := escalator.receipts.ReserveWorkflowEscalation(
		ctx,
		record,
	)
	if reserveErr != nil {
		return rejectedEscalationReceipt(request, code),
			escalationError(code, detail, errors.Join(cause, reserveErr))
	}
	if !created {
		if reserved.RequestHash != requestHash {
			return receiptFromRecord(reserved, agentapi.EscalationRejected),
				escalationError(
					agentapi.WorkflowStartConflict,
					"request id is already bound to different escalation content",
					nil,
				)
		}
		if reserved.Status != escalationReceiptStarting {
			return escalator.receiptForExisting(reserved)
		}
		record = reserved
	}
	record.Status = agentapi.EscalationRejected
	record.ErrorCode = code
	if finishErr := escalator.receipts.FinishWorkflowEscalation(ctx, record); finishErr != nil {
		cause = errors.Join(cause, finishErr)
	}
	return receiptFromRecord(record, agentapi.EscalationRejected),
		escalationError(code, detail, cause)
}

func (escalator *ServerWorkflowEscalator) normalizeRequest(
	request agentapi.WorkflowEscalationRequest,
) (agentapi.WorkflowEscalationRequest, string, error) {
	normalized := request
	normalized.RequestID = strings.TrimSpace(normalized.RequestID)
	normalized.ParentRunID = strings.TrimSpace(normalized.ParentRunID)
	normalized.DelegationID = strings.TrimSpace(normalized.DelegationID)
	normalized.Capability.ID = strings.TrimSpace(normalized.Capability.ID)
	normalized.CapabilityHash = strings.ToLower(strings.TrimSpace(
		normalized.CapabilityHash,
	))
	normalized.Objective = strings.TrimSpace(normalized.Objective)
	if !validOpaqueIdentifier(normalized.RequestID, 128) {
		return agentapi.WorkflowEscalationRequest{}, "", fmt.Errorf(
			"request id is invalid",
		)
	}
	if !canonicalID.MatchString(normalized.ParentRunID) {
		return agentapi.WorkflowEscalationRequest{}, "", fmt.Errorf(
			"parent run id is invalid",
		)
	}
	if normalized.DelegationID != "" &&
		!validOpaqueIdentifier(normalized.DelegationID, 64) {
		return agentapi.WorkflowEscalationRequest{}, "", fmt.Errorf(
			"delegation id is invalid",
		)
	}
	if !canonicalID.MatchString(normalized.Capability.ID) ||
		normalized.Capability.Version <= 0 ||
		!validContentHash(normalized.CapabilityHash) {
		return agentapi.WorkflowEscalationRequest{}, "", fmt.Errorf(
			"exact capability id, version, and content hash are required",
		)
	}
	if normalized.Objective == "" || len(normalized.Objective) > 8192 {
		return agentapi.WorkflowEscalationRequest{}, "", fmt.Errorf(
			"objective is required and must not exceed 8192 bytes",
		)
	}
	if _, err := canonicalEscalationReasons(
		[]agentapi.WorkflowEscalationReason{normalized.Reason},
	); err != nil {
		return agentapi.WorkflowEscalationRequest{}, "", err
	}
	var err error
	normalized.FocusFacets, err = canonicalEscalationValues(
		normalized.FocusFacets,
		escalator.config.MaxFocusFacets,
		128,
		"focus facet",
	)
	if err != nil {
		return agentapi.WorkflowEscalationRequest{}, "", err
	}
	normalized.EvidenceRefs, err = canonicalEscalationValues(
		normalized.EvidenceRefs,
		escalator.config.MaxEvidenceRefs,
		256,
		"evidence ref",
	)
	if err != nil {
		return agentapi.WorkflowEscalationRequest{}, "", err
	}
	normalized.ReportRefs, err = canonicalEscalationValues(
		normalized.ReportRefs,
		escalator.config.MaxReportRefs,
		256,
		"report ref",
	)
	if err != nil {
		return agentapi.WorkflowEscalationRequest{}, "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return agentapi.WorkflowEscalationRequest{}, "", err
	}
	sum := sha256.Sum256(raw)
	return normalized, hex.EncodeToString(sum[:]), nil
}

func (escalator *ServerWorkflowEscalator) resolveHandoff(
	ctx context.Context,
	parent agentapi.WorkflowEscalationParent,
	request agentapi.WorkflowEscalationRequest,
) (WorkflowEscalationHandoff, error) {
	if len(request.EvidenceRefs) == 0 && len(request.ReportRefs) == 0 {
		return WorkflowEscalationHandoff{}, nil
	}
	if escalator.handoffs == nil {
		return WorkflowEscalationHandoff{}, fmt.Errorf(
			"referenced escalation handoff is unavailable",
		)
	}
	handoff, err := escalator.handoffs.ResolveWorkflowEscalationHandoff(
		ctx,
		cloneEscalationParent(parent),
		request,
	)
	if err != nil {
		return WorkflowEscalationHandoff{}, err
	}
	return escalator.validateResolvedHandoff(request, handoff)
}

func (escalator *ServerWorkflowEscalator) validateResolvedHandoff(
	request agentapi.WorkflowEscalationRequest,
	handoff WorkflowEscalationHandoff,
) (WorkflowEscalationHandoff, error) {
	if len(handoff.Evidence) > escalator.config.MaxEvidenceUnits ||
		len(handoff.Reports) > escalator.config.MaxReports {
		return WorkflowEscalationHandoff{}, fmt.Errorf(
			"resolved escalation handoff exceeds item limits",
		)
	}
	evidenceByRef := make(
		map[string]ResolvedWorkflowEscalationEvidence,
		len(handoff.Evidence),
	)
	for _, resolved := range handoff.Evidence {
		resolved.Ref = strings.TrimSpace(resolved.Ref)
		if resolved.Ref == "" || resolved.Unit.ContentHash == "" {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"resolved evidence identity is incomplete",
			)
		}
		if _, duplicate := evidenceByRef[resolved.Ref]; duplicate {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"resolved evidence ref %q is duplicated",
				resolved.Ref,
			)
		}
		evidenceByRef[resolved.Ref] = resolved
	}
	orderedEvidence := make(
		[]ResolvedWorkflowEscalationEvidence,
		0,
		len(request.EvidenceRefs),
	)
	for _, ref := range request.EvidenceRefs {
		resolved, ok := evidenceByRef[ref]
		if !ok {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"evidence ref %q was not resolved",
				ref,
			)
		}
		orderedEvidence = append(orderedEvidence, resolved)
		delete(evidenceByRef, ref)
	}
	if len(evidenceByRef) != 0 {
		return WorkflowEscalationHandoff{}, fmt.Errorf(
			"resolver returned unrequested evidence",
		)
	}

	reportsByRef := make(
		map[string]agentapi.WorkflowEscalationReport,
		len(handoff.Reports),
	)
	totalReportBytes := 0
	for _, report := range handoff.Reports {
		report.Ref = strings.TrimSpace(report.Ref)
		if report.Ref == "" || report.RunID == "" ||
			report.Schema.ID == "" || report.Schema.Version <= 0 ||
			!validContentHash(report.ContentHash) ||
			len(report.Payload) == 0 || !json.Valid(report.Payload) {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"resolved report %q is invalid",
				report.Ref,
			)
		}
		if len(report.Payload) > escalator.config.MaxReportBytes {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"resolved report %q exceeds the byte limit",
				report.Ref,
			)
		}
		totalReportBytes += len(report.Payload)
		if totalReportBytes > escalator.config.MaxTotalReportBytes {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"resolved reports exceed the aggregate byte limit",
			)
		}
		sum := sha256.Sum256(report.Payload)
		if hex.EncodeToString(sum[:]) != report.ContentHash {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"resolved report %q content hash mismatch",
				report.Ref,
			)
		}
		if _, duplicate := reportsByRef[report.Ref]; duplicate {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"resolved report ref %q is duplicated",
				report.Ref,
			)
		}
		reportsByRef[report.Ref] = cloneEscalationReport(report)
	}
	orderedReports := make(
		[]agentapi.WorkflowEscalationReport,
		0,
		len(request.ReportRefs),
	)
	for _, ref := range request.ReportRefs {
		report, ok := reportsByRef[ref]
		if !ok {
			return WorkflowEscalationHandoff{}, fmt.Errorf(
				"report ref %q was not resolved",
				ref,
			)
		}
		orderedReports = append(orderedReports, report)
		delete(reportsByRef, ref)
	}
	if len(reportsByRef) != 0 {
		return WorkflowEscalationHandoff{}, fmt.Errorf(
			"resolver returned unrequested reports",
		)
	}
	return WorkflowEscalationHandoff{
		Evidence: orderedEvidence,
		Reports:  orderedReports,
	}, nil
}

func (escalator *ServerWorkflowEscalator) validateBuildResult(
	binding agentapi.WorkflowBinding,
	definition Definition,
	handoff WorkflowEscalationHandoff,
	result agentapi.WorkflowEscalationBuildResult,
) error {
	if len(result.Input) == 0 || !json.Valid(result.Input) {
		return fmt.Errorf("workflow input is not valid JSON")
	}
	maxInputBytes := escalator.config.MaxInputBytes
	if definition.Budget.MaxHandoffBytes > 0 &&
		(int64(maxInputBytes) > definition.Budget.MaxHandoffBytes ||
			maxInputBytes <= 0) {
		maxInputBytes = int(definition.Budget.MaxHandoffBytes)
	}
	if len(result.Input) > maxInputBytes {
		return fmt.Errorf("workflow input exceeds %d bytes", maxInputBytes)
	}
	if binding.InputSchema != definition.InputSchema {
		return fmt.Errorf("binding input schema no longer matches the workflow")
	}
	if err := escalator.schemas.Validate(binding.InputSchema, result.Input); err != nil {
		return fmt.Errorf("workflow input schema: %w", err)
	}
	allowedEvidence := make(map[string]tool.EvidenceUnit, len(handoff.Evidence))
	for _, resolved := range handoff.Evidence {
		allowedEvidence[resolved.Unit.ContentHash] = resolved.Unit
	}
	seen := make(map[string]struct{}, len(result.SeedEvidence))
	for _, unit := range result.SeedEvidence {
		if unit.ContentHash == "" {
			return fmt.Errorf("seed evidence content hash is required")
		}
		authoritative, ok := allowedEvidence[unit.ContentHash]
		if !ok || !sameEvidenceUnit(authoritative, unit) {
			return fmt.Errorf(
				"seed evidence %q was not resolved from the parent handoff",
				unit.ContentHash,
			)
		}
		if _, duplicate := seen[unit.ContentHash]; duplicate {
			return fmt.Errorf("seed evidence %q is duplicated", unit.ContentHash)
		}
		seen[unit.ContentHash] = struct{}{}
	}
	return nil
}

func escalationPermissions(
	parent agentapi.WorkflowEscalationParent,
	capability agentapi.Capability,
	binding agentapi.WorkflowBinding,
	definition Definition,
) (agentapi.PermissionPolicy, error) {
	if err := scope.Validate(parent.Permissions.Scopes); err != nil {
		return agentapi.PermissionPolicy{}, err
	}
	if err := scope.Validate(capability.PermissionScope); err != nil {
		return agentapi.PermissionPolicy{}, err
	}
	if err := scope.Validate(binding.ScenarioPermissions.Scopes); err != nil {
		return agentapi.PermissionPolicy{}, err
	}
	effective := IntersectPermissions(
		parent.Permissions,
		agentapi.PermissionPolicy{
			Scopes: append([]string(nil), capability.PermissionScope...),
		},
		binding.ScenarioPermissions,
	)
	if err := scope.EnsureSubset(
		definition.Permissions.Scopes,
		effective.Scopes,
	); err != nil {
		return agentapi.PermissionPolicy{}, err
	}
	return effective, nil
}

func ensureEscalationBudget(
	remaining agentapi.WorkflowEscalationBudget,
	budget Budget,
) error {
	if remaining.MaxTotalTokens < 0 {
		return fmt.Errorf("workflow token budget is exhausted")
	}
	if remaining.MaxTotalTokens > 0 &&
		budget.MaxTotalTokens > remaining.MaxTotalTokens {
		return fmt.Errorf(
			"workflow token budget %d exceeds remaining %d",
			budget.MaxTotalTokens,
			remaining.MaxTotalTokens,
		)
	}
	if remaining.MaxCostMicros < 0 {
		return fmt.Errorf("workflow cost budget is exhausted")
	}
	if remaining.MaxCostMicros > 0 &&
		budget.MaxCostMicros > remaining.MaxCostMicros {
		return fmt.Errorf(
			"workflow cost budget %d exceeds remaining %d",
			budget.MaxCostMicros,
			remaining.MaxCostMicros,
		)
	}
	if !remaining.Deadline.IsZero() &&
		time.Now().UTC().Add(budget.Timeout).After(remaining.Deadline) {
		return fmt.Errorf("workflow timeout exceeds the remaining deadline")
	}
	return nil
}

func bindingAllowsReason(
	binding agentapi.WorkflowBinding,
	reason agentapi.WorkflowEscalationReason,
) bool {
	for _, allowed := range binding.AllowedReasons {
		if allowed == reason {
			return true
		}
	}
	return false
}

func sameEscalatedWorkflow(
	run *RunRecord,
	record WorkflowEscalationRecord,
	binding agentapi.WorkflowBinding,
	parent agentapi.WorkflowEscalationParent,
) bool {
	return run != nil &&
		run.ID == record.WorkflowRunID &&
		run.ParentRunID == record.ParentRunID &&
		run.WorkflowID == binding.Workflow.ID &&
		run.WorkflowVersion == binding.Workflow.Version &&
		run.WorkflowHash == binding.Workflow.ContentHash &&
		run.ActorUserID == parent.Actor.UserID &&
		run.ActorTenantID == parent.Actor.TenantID
}

func classifyWorkflowStartError(err error) string {
	switch {
	case errors.Is(err, ErrForbidden):
		return agentapi.WorkflowPermissionDenied
	case errors.Is(err, ErrInvalid):
		return agentapi.WorkflowInvalidHandoff
	case errors.Is(err, ErrConflict):
		return agentapi.WorkflowStartConflict
	case errors.Is(err, ErrUnavailable):
		return agentapi.WorkflowUnavailable
	default:
		return agentapi.WorkflowStartFailed
	}
}

func receiptFromRecord(
	record WorkflowEscalationRecord,
	status agentapi.WorkflowEscalationStatus,
) agentapi.WorkflowEscalationReceipt {
	return agentapi.WorkflowEscalationReceipt{
		RequestID:      record.RequestID,
		WorkflowRunID:  record.WorkflowRunID,
		BindingID:      record.BindingID,
		BindingVersion: record.BindingVersion,
		Status:         status,
		ErrorCode:      record.ErrorCode,
	}
}

func rejectedEscalationReceipt(
	request agentapi.WorkflowEscalationRequest,
	code string,
) agentapi.WorkflowEscalationReceipt {
	return agentapi.WorkflowEscalationReceipt{
		RequestID: strings.TrimSpace(request.RequestID),
		WorkflowRunID: StableWorkflowEscalationRunID(
			strings.TrimSpace(request.ParentRunID),
			strings.TrimSpace(request.RequestID),
		),
		Status:    agentapi.EscalationRejected,
		ErrorCode: code,
	}
}

func escalationError(code, detail string, cause error) error {
	return &WorkflowEscalationError{
		Code: code, Detail: detail, Cause: cause,
	}
}

func canonicalEscalationValues(
	values []string,
	maxItems,
	maxBytes int,
	label string,
) ([]string, error) {
	if len(values) > maxItems {
		return nil, fmt.Errorf("%s count exceeds %d", label, maxItems)
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !validOpaqueIdentifier(value, maxBytes) {
			return nil, fmt.Errorf("%s %q is invalid", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func validOpaqueIdentifier(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func cloneEscalationParent(
	parent agentapi.WorkflowEscalationParent,
) agentapi.WorkflowEscalationParent {
	parent.Permissions.Scopes = append([]string(nil), parent.Permissions.Scopes...)
	return parent
}

func cloneEscalationEvidence(
	evidence []ResolvedWorkflowEscalationEvidence,
) []tool.EvidenceUnit {
	out := make([]tool.EvidenceUnit, len(evidence))
	for index, resolved := range evidence {
		out[index] = cloneToolEvidenceUnit(resolved.Unit)
	}
	return out
}

func cloneEscalationReports(
	reports []agentapi.WorkflowEscalationReport,
) []agentapi.WorkflowEscalationReport {
	out := make([]agentapi.WorkflowEscalationReport, len(reports))
	for index, report := range reports {
		out[index] = cloneEscalationReport(report)
	}
	return out
}

func cloneEscalationReport(
	report agentapi.WorkflowEscalationReport,
) agentapi.WorkflowEscalationReport {
	report.Payload = append(json.RawMessage(nil), report.Payload...)
	return report
}

func cloneToolEvidence(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	out := make([]tool.EvidenceUnit, len(units))
	for index, unit := range units {
		out[index] = cloneToolEvidenceUnit(unit)
	}
	return out
}

func cloneToolEvidenceUnit(unit tool.EvidenceUnit) tool.EvidenceUnit {
	unit.Sections = append([]string(nil), unit.Sections...)
	unit.Facets = append([]string(nil), unit.Facets...)
	return unit
}

func sameEvidenceUnit(left, right tool.EvidenceUnit) bool {
	leftRaw, _ := json.Marshal(left)
	rightRaw, _ := json.Marshal(right)
	return string(leftRaw) == string(rightRaw)
}

func (escalator *ServerWorkflowEscalator) requestLock(
	parentRunID,
	requestID string,
) *sync.Mutex {
	key := parentRunID + "\x00" + requestID
	escalator.locksMu.Lock()
	defer escalator.locksMu.Unlock()
	lock := escalator.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		escalator.locks[key] = lock
	}
	return lock
}

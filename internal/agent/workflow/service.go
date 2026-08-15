package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	persistenceTimeout              = 5 * time.Second
	nodeRetryableErrorCode          = "node_retryable"
	nodeRestartedErrorCode          = "workflow_restarted"
	nodeRestartedRetryableErrorCode = "workflow_restarted_retryable"
)

type ExecuteRequest struct {
	RunID               string
	ParentRunID         string
	Round               int
	BaseDepth           int
	Workflow            DefinitionRef
	Input               json.RawMessage
	SeedEvidence        []tool.EvidenceUnit
	Actor               agentapi.Actor
	ActorPermissions    agentapi.PermissionPolicy
	Scenario            string
	ScenarioPermissions agentapi.PermissionPolicy
}

type StartRequest struct {
	RunID               string
	ParentRunID         string
	Round               int
	BaseDepth           int
	Workflow            DefinitionRef
	Input               json.RawMessage
	SeedEvidence        []tool.EvidenceUnit
	Actor               agentapi.Actor
	ActorPermissions    agentapi.PermissionPolicy
	Scenario            string
	ScenarioPermissions agentapi.PermissionPolicy
	Admin               bool
}

type ApprovalRequest struct {
	WorkflowRunID string
	NodeID        string
	Decision      ApprovalDecision
	Approver      agentapi.Actor
	Admin         bool
	Comment       string
}

type ApprovalResult struct {
	Approval Approval
	Applied  bool
	Status   RunStatus
	Result   *Result
}

type ResumeResult struct {
	RunID   string
	Applied bool
	Status  RunStatus
	Result  *Result
}

type RecoveryReport struct {
	Scanned      int
	Resumed      int
	Succeeded    int
	WaitingHuman int
	Failed       int
	Cancelled    int
	TimedOut     int
	Errors       int
}

// RecoveryObserver reconciles one resumed Run with its owning domain.
type RecoveryObserver func(context.Context, string, ResumeResult, error) error

type persistence interface {
	StartRun(context.Context, RunRecord, Handoff) error
	StartNode(context.Context, NodeRunRecord) error
	SucceedNode(context.Context, string, string, int, string, Handoff, *GateDecision, Usage, time.Time) error
	FailNode(context.Context, string, string, int, string, RunStatus, string, Usage, time.Time) error
	FinishRun(context.Context, string, RunStatus, string, StopReason, *Handoff, time.Time) error
	LoadFullRunState(context.Context, string) (*RunState, error)
	LoadTerminalResult(context.Context, string) (TerminalResult, error)
	GetRun(context.Context, string) (*RunRecord, error)
	ListNodeRuns(context.Context, string, NodeRunCursor, int) ([]NodeRunRecord, error)
	ListEvents(context.Context, string, int64, int) ([]Event, error)
	ListHandoffs(context.Context, string, HandoffCursor, int) ([]Handoff, error)
	ListActiveRuns(context.Context, time.Time, ActiveRunCursor, int) ([]ActiveRunRef, error)
	DecideApproval(context.Context, Approval, *Handoff) (ApprovalTransition, error)
	CancelRun(context.Context, string, time.Time) (CancelTransition, error)
	SubscribeEvents(string) (<-chan Event, func())
}

type resumeCall struct {
	done   chan struct{}
	result ResumeResult
	err    error
}

type activeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// RunEventReader scopes repeated event reads to one authorized Run.
type RunEventReader struct {
	store persistence
	runID string
}

func (reader *RunEventReader) List(
	ctx context.Context,
	afterSeq int64,
	limit int,
) ([]Event, error) {
	if reader == nil || reader.store == nil {
		return nil, ErrUnavailable
	}
	return reader.store.ListEvents(ctx, reader.runID, afterSeq, limit)
}

// Service fixes a Catalog snapshot and closes each execution transition in Store.
type Service struct {
	catalog *Catalog
	store   persistence

	mu           sync.RWMutex
	orchestrator *Orchestrator

	resumeMu sync.Mutex
	resumes  map[string]*resumeCall

	activeMu sync.Mutex
	active   map[string]*activeRun
	activeWG sync.WaitGroup
	closed   bool
}

func NewService(
	catalog *Catalog,
	store persistence,
	orchestrator *Orchestrator,
) (*Service, error) {
	if catalog == nil {
		return nil, fmt.Errorf("workflow service: catalog is required")
	}
	if store == nil {
		return nil, fmt.Errorf("workflow service: store is required")
	}
	return &Service{
		catalog: catalog, store: store, orchestrator: orchestrator,
		resumes: make(map[string]*resumeCall),
		active:  make(map[string]*activeRun),
	}, nil
}

// SetOrchestrator atomically replaces the execution capability for future Runs.
func (service *Service) SetOrchestrator(orchestrator *Orchestrator) {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.orchestrator = orchestrator
	service.mu.Unlock()
}

// Available reports whether new workflow runs can execute.
func (service *Service) Available() bool {
	_, err := service.executionCapability()
	return err == nil
}

func (service *Service) executionCapability() (*Orchestrator, error) {
	if service == nil || service.catalog == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	service.activeMu.Lock()
	closed := service.closed
	service.activeMu.Unlock()
	if closed {
		return nil, ErrUnavailable
	}
	service.mu.RLock()
	orchestrator := service.orchestrator
	service.mu.RUnlock()
	if orchestrator == nil {
		return nil, ErrUnavailable
	}
	return orchestrator, nil
}

func validateRunID(runID string) error {
	if !canonicalID.MatchString(runID) {
		return fmt.Errorf("workflow run id %q is not canonical: %w", runID, ErrInvalid)
	}
	return nil
}

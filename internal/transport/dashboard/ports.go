package dashboard

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

// QASessionStorePort is the session-boundary Dashboard needs for context
// loading, listing, feedback, creation, and delete.
type QASessionStorePort interface {
	List(userID int64) ([]memory.SessionRecord, error)
	Create(memory.SessionRecord) error
	Delete(id string, userID int64) (bool, error)
	GetContextSnapshot(id string, userID int64, metadataLimit, dialogueLimit int) (*memory.SessionRecord, error)
	ListMessagesBefore(id string, userID int64, beforeSeq, limit int) (*memory.MessagePage, error)
	ListTurnsBefore(id string, userID int64, beforeSeq, limit int) (*memory.MessagePage, error)
	SetMessageFeedback(sessionID string, userID int64, seq int, runID, feedback string) (bool, error)
}

// QARunStorePort is the run-query/control boundary Dashboard needs. Run
// control still delegates to the hub after resolving the control record.
type QARunStorePort interface {
	ListPage(userID int64, sessionID string, status run.Status, page, pageSize int) (*domain.Page[run.Record], error)
	GetForUser(id string, userID int64) (*run.Detail, error)
	GetToolArtifact(userID int64, sessionID, artifactID string, offset int64, limit int) (*run.ToolResultArtifactChunk, error)
	GetControlForUser(id string, userID int64) (run.ControlRecord, error)
	UsageSummary(ctx context.Context, userID int64, sessionID, runID string) (run.UsageSummary, error)
	EvidenceByIDs(userID int64, sessionID string, runIDs []string) (map[string]run.EvidenceMetrics, error)
	DeleteBySession(id string, userID int64) error
}

// QAMemoryStorePort is the memory-management boundary Dashboard needs.
type QAMemoryStorePort interface {
	List(ctx context.Context, userID int64, options memory.ListOptions) (memory.MemoryPage, error)
	Delete(ctx context.Context, userID int64, id string) (bool, error)
	Clear(ctx context.Context, userID int64) (int, error)
	DeleteBySession(ctx context.Context, userID int64, sessionID string) (int, error)
}

// QARuntimeStatusPort is the read-only status/control boundary used by
// APIQARuntimeStatus and control routing. Compaction status lives on the
// application port because it is owned by the QA service.
type QARuntimeStatusPort interface {
	ContextUsage(runID string) (run.ContextUsageEvent, bool)
	Send(runID string, signal run.ControlSignal)
	Resume(runID string) error
}

// QAApplicationPort starts one QA run and returns an already-bound event
// stream so early prepare/retrieval events are never dropped. The adapter owns
// the returned stream's lifetime and must call Close when it stops consuming.
type QAApplicationPort interface {
	Start(ctx context.Context, request QAStartRequest) (QAStartedRun, error)
	CompactionStatus(runID string) run.SessionStatusEvent
	ContextUsage(runID string) (run.ContextUsageEvent, bool)
}

// QAStartRequest is the transport-neutral input contract for a QA run.
type QAStartRequest struct {
	Question        string
	Conversation    execution.ConversationContext
	UserID          int64
	RolePrompt      string
	TraceEnabled    bool
	EvidencePlan    *domain.EvidencePlan
	WriteAuthorized bool
	WriteRequested  bool
}

// QAStartedRun is the result of a successful admission. Events carries the
// durable real-time stream bound before preparation; Close must always be
// called by the adapter to release the subscription.
type QAStartedRun struct {
	RunID   string
	Context *retrieval.RetrievedContext
	Events  <-chan run.SSEEvent
	Close   func()
}

package investigation

// ProgressKind is the bounded set of lifecycle transitions exposed to transport
// adapters. It intentionally mirrors RunStatus rather than SSE event names.
type ProgressKind string

const (
	ProgressWorkflowStarted   ProgressKind = "workflow_started"
	ProgressTaskStarted       ProgressKind = "task_started"
	ProgressTaskCompleted     ProgressKind = "task_completed"
	ProgressWorkflowCompleted ProgressKind = "workflow_completed"
)

// ProgressEvent is a transport-neutral observation of one  run transition.
type ProgressEvent struct {
	Kind     ProgressKind
	RunID    string
	NodeID   string
	Executor ExecutorType
	ToolID   string
	Status   string
	Reason   string
}

// ProgressObserver receives lifecycle events without importing the transport
// event vocabulary into the investigation package.
type ProgressObserver func(ProgressEvent)

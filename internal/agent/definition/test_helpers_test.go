package definition

import (
	"context"
	"database/sql"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

type Registry = tool.Registry
type RunStore = agentrun.RunStore
type StepRecord = agentrun.StepRecord
type RunOutcome = agentrun.RunOutcome
type EvidenceMetrics = agentrun.EvidenceMetrics
type SSEEvent = agentrun.SSEEvent
type RunTerminal = agentrun.RunTerminal
type RunResult = agentexecution.RunResult

const (
	ToolKindRead  = tool.KindRead
	ToolKindWrite = tool.KindWrite

	RunKindAgent    = agentrun.RunKindAgent
	RunKindQAParent = agentrun.RunKindQAParent

	RunStatusRunning = agentrun.RunStatusRunning
	RunStatusDone    = agentrun.RunStatusDone
	RunStatusFailed  = agentrun.RunStatusFailed
	RunStatusPaused  = agentrun.RunStatusPaused

	EvidenceComplete    = agentrun.EvidenceComplete
	EvidenceUnavailable = agentrun.EvidenceUnavailable

	EventRunFinished = agentrun.EventRunFinished
)

func bindRunStore(db *sql.DB) *RunStore {
	return agentrun.BindStore(db)
}

func testRegistry(t *testing.T, tools ...tool.Tool) *Registry {
	t.Helper()
	registry := tool.NewRegistry()
	if err := registry.RegisterAll(tools); err != nil {
		t.Fatal(err)
	}
	return registry
}

func testAgentTool(
	id tool.ToolID,
	kind tool.Kind,
	run func(context.Context, tool.Arguments) (string, error),
) tool.Tool {
	return tool.Tool{
		ID:          id,
		Description: "test tool",
		Kind:        kind,
		InputSchema: tool.JSONSchema{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: tool.HandlerFunc(func(ctx context.Context, arguments tool.Arguments) (tool.Result, error) {
			content, err := run(ctx, arguments)
			return tool.Result{Content: content}, err
		}),
	}
}

func noopTool(context.Context, tool.Arguments) (string, error) {
	return "ok", nil
}

func publicResultMessages(messages []agentapi.Message) []llm.Message {
	converted := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		converted = append(converted, internalMessage(message))
	}
	return converted
}

func waitForTerminal(t *testing.T, events chan SSEEvent) *RunTerminal {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if terminal := agentrun.TerminalFromEvent(event); terminal != nil {
				return terminal
			}
		case <-timer.C:
			t.Fatal("run did not emit terminal event")
		}
	}
}

var NewToolExecutor = agentexecution.NewToolExecutor

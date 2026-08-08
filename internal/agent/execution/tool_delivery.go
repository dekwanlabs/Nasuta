package execution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

var ErrToolResultDelivery = errors.New("tool result could not be delivered without loss")

type toolDeliveryError struct {
	Error           string         `json:"error"`
	Tool            string         `json:"tool"`
	ResultBytes     int            `json:"result_bytes"`
	AvailableTokens int            `json:"available_tokens,omitempty"`
	RequiredTokens  int            `json:"required_tokens,omitempty"`
	MissingLiterals int            `json:"missing_literals,omitempty"`
	ArtifactID      string         `json:"artifact_id,omitempty"`
	Retry           map[string]any `json:"retry"`
}

func (agent *Agent) prepareToolDelivery(runID string, messages []llm.Message, tools []llm.ToolDef, call llm.ToolCall, execution ToolExecution) ToolExecution {
	if execution.Failed {
		return execution
	}
	missing := missingRequiredLiterals(execution.PromptContent, execution.AnswerContract)
	if len(missing) > 0 {
		return failedToolDelivery(call.Function.Name, execution, toolDeliveryError{
			Error:           "answer_contract_missing_from_prompt",
			Tool:            call.Function.Name,
			ResultBytes:     len(execution.AuthoritativeContent),
			MissingLiterals: len(missing),
			Retry:           map[string]any{"action": "restore_authoritative_content_or_retry_with_pagination"},
		})
	}
	available, required, err := agent.toolDeliveryBudget(messages, tools, call, execution)
	if err != nil {
		log.WarnfCtx(
			log.WithTraceID(context.Background(), runID),
			"[agent] tool %s result budget calculation failed: %v",
			call.Function.Name,
			err,
		)
		return failedToolDelivery(call.Function.Name, execution, toolDeliveryError{
			Error:       "tool_result_budget_calculation_failed",
			Tool:        call.Function.Name,
			ResultBytes: len(execution.AuthoritativeContent),
			Retry:       map[string]any{"action": "retry_with_pagination_or_narrower_query"},
		})
	}
	if available >= 0 && required > available {
		execution.ArtifactID = toolResultArtifactID(runID, call.ID)
		return failedToolDelivery(call.Function.Name, execution, toolDeliveryError{
			Error:           "tool_result_exceeds_context_budget",
			Tool:            call.Function.Name,
			ResultBytes:     len(execution.AuthoritativeContent),
			AvailableTokens: available,
			RequiredTokens:  required,
			ArtifactID:      execution.ArtifactID,
			Retry:           map[string]any{"action": "retry_with_pagination_or_narrower_query"},
		})
	}
	return execution
}

func (agent *Agent) toolDeliveryBudget(messages []llm.Message, tools []llm.ToolDef, call llm.ToolCall, execution ToolExecution) (int, int, error) {
	if agent.cfg.ContextWindow <= 0 {
		return -1, 0, nil
	}
	inputTokens := estimateMessagesTokens(messages)
	if len(tools) > 0 {
		encoded, err := json.Marshal(tools)
		if err != nil {
			return 0, 0, fmt.Errorf("encode tool definitions: %w", err)
		}
		inputTokens += tooloutput.EstimateTokens(string(encoded))
	}
	outputReserve := max(agent.cfg.AnswerMaxTokens, agent.cfg.ConclusionMaxTokens)
	safety := max(agent.cfg.ContextWindow/20, 1024)
	available := agent.cfg.ContextWindow - inputTokens - outputReserve - safety
	candidate := []llm.Message{toolMessage(call.ID, call.Function.Name, execution.PromptContent)}
	if contractMessage, ok := answerContractMessage(execution.AnswerContract); ok {
		candidate = append(candidate, contractMessage)
	}
	return max(0, available), estimateMessagesTokens(candidate), nil
}

func failedToolDelivery(name string, execution ToolExecution, failure toolDeliveryError) ToolExecution {
	encoded, err := json.Marshal(failure)
	if err != nil {
		encoded = []byte(fmt.Sprintf(`{"error":%q,"tool":%q}`, ErrToolResultDelivery.Error(), name))
	}
	execution.PromptContent = string(encoded)
	execution.Notices = nil
	execution.Evidence = false
	execution.Failed = true
	execution.DeliveryError = failure.Error
	return execution
}

func missingRequiredLiterals(content string, contract tool.AnswerContract) []string {
	checker := &exactAnswerContract{}
	checker.Add(contract)
	return checker.Missing(content)
}

func toolResultTraceID(runID, toolCallID string) string {
	return "trc_" + platform.UUIDFromString("tool_result\x00"+runID+"\x00"+toolCallID)
}

func toolResultArtifactID(runID, toolCallID string) string {
	return "art_" + platform.UUIDFromString("tool_result_artifact\x00"+runID+"\x00"+toolCallID)
}

func toolContentSHA256(content string) string {
	digest := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", digest)
}

func newToolResultStep(runID string, stepNo int, call llm.ToolCall, execution ToolExecution) StepRecord {
	return StepRecord{
		StepNo:              stepNo,
		Kind:                StepKindToolResult,
		TraceID:             toolResultTraceID(runID, call.ID),
		ArtifactID:          execution.ArtifactID,
		ToolCallID:          call.ID,
		Tool:                call.Function.Name,
		Args:                call.Function.Arguments,
		Failed:              execution.Failed,
		DeliveryError:       execution.DeliveryError,
		Content:             execution.AuthoritativeContent,
		PromptContent:       execution.PromptContent,
		AuthoritativeSHA256: toolContentSHA256(execution.AuthoritativeContent),
		PromptSHA256:        toolContentSHA256(execution.PromptContent),
		SizeBytes:           int64(len(execution.AuthoritativeContent)),
		Coverage:            execution.Coverage,
		AnswerContract:      execution.AnswerContract,
		DurationMs:          execution.DurationMs,
	}
}

func toolResultPreview(content string) string {
	return runeSafeTruncate(content, 1200)
}

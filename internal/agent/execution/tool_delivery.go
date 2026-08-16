package execution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
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

func (agent *Agent) prepareDelivery(
	runID string,
	messages []llm.Message,
	pendingNotices []string,
	tools []llm.ToolDef,
	call llm.ToolCall,
	currentContract *exactAnswerContract,
	execution ToolExecution,
) ToolExecution {
	if execution.Failed {
		return execution
	}
	missing := missingRequiredLiterals(execution.PromptContent, execution.AnswerContract)
	if len(missing) > 0 {
		return failedDelivery(call.Function.Name, execution, toolDeliveryError{
			Error:           "answer_contract_missing_from_prompt",
			Tool:            call.Function.Name,
			ResultBytes:     len(execution.AuthoritativeContent),
			MissingLiterals: len(missing),
			Retry:           map[string]any{"action": "restore_authoritative_content_or_retry_with_pagination"},
		})
	}
	available, required, err := agent.deliveryBudget(messages, pendingNotices, tools, call, currentContract, execution)
	if err != nil {
		log.WarnfCtx(
			log.WithTraceID(context.Background(), runID),
			"[agent] tool %s result budget calculation failed: %v",
			call.Function.Name,
			err,
		)
		return failedDelivery(call.Function.Name, execution, toolDeliveryError{
			Error:       "tool_result_budget_calculation_failed",
			Tool:        call.Function.Name,
			ResultBytes: len(execution.AuthoritativeContent),
			Retry:       map[string]any{"action": "retry_with_pagination_or_narrower_query"},
		})
	}
	if available >= 0 && required > available {
		execution.ArtifactID = toolResultArtifactID(runID, call.ID)
		return failedDelivery(call.Function.Name, execution, toolDeliveryError{
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

func (agent *Agent) deliveryBudget(
	messages []llm.Message,
	pendingNotices []string,
	tools []llm.ToolDef,
	call llm.ToolCall,
	currentContract *exactAnswerContract,
	execution ToolExecution,
) (int, int, error) {
	if agent.cfg.ContextWindow <= 0 {
		return -1, 0, nil
	}
	current := deliveryContextMessages(messages, pendingNotices, currentContract)
	currentInputTokens, err := estimateInputTokens(current, tools)
	if err != nil {
		return 0, 0, err
	}
	outputReserve := agent.outputReserve()
	safety := contextSafetyTokens(agent.cfg.ContextWindow)
	available := agent.cfg.ContextWindow - currentInputTokens - outputReserve - safety
	candidate := deliveryMessages(messages, pendingNotices, call, currentContract, execution)
	candidateInputTokens, err := estimateInputTokens(candidate, tools)
	if err != nil {
		return 0, 0, err
	}
	return max(0, available), max(0, candidateInputTokens-currentInputTokens), nil
}

func deliveryMessages(
	messages []llm.Message,
	pendingNotices []string,
	call llm.ToolCall,
	currentContract *exactAnswerContract,
	execution ToolExecution,
) []llm.Message {
	candidate := append(append([]llm.Message(nil), messages...), toolMessage(call.ID, call.Function.Name, execution.PromptContent))
	return appendDeliveryContext(candidate, pendingNotices, currentContract, execution)
}

func deliveryContextMessages(
	messages []llm.Message,
	pendingNotices []string,
	currentContract *exactAnswerContract,
) []llm.Message {
	return appendDeliveryContext(
		append([]llm.Message(nil), messages...), pendingNotices, currentContract, ToolExecution{},
	)
}

func appendDeliveryContext(
	candidate []llm.Message,
	pendingNotices []string,
	currentContract *exactAnswerContract,
	execution ToolExecution,
) []llm.Message {
	for _, notice := range pendingNotices {
		candidate = append(candidate, deliveryNoticeMessage(notice))
	}
	for _, notice := range execution.Notices {
		candidate = append(candidate, deliveryNoticeMessage(notice))
	}
	if contractMessage, ok := combinedContractMessage(currentContract, execution.AnswerContract); ok {
		candidate = append(withoutContractMessages(candidate), contractMessage)
	}
	return candidate
}

func deliveryNoticeMessage(notice string) llm.Message {
	return llm.Message{
		Role: "system",
		Content: prompts.MustRender(prompts.AgentQAToolDeliveryNotice, struct {
			Notice string
		}{Notice: notice}),
	}
}

func combinedContractMessage(
	current *exactAnswerContract,
	addition tool.AnswerContract,
) (llm.Message, bool) {
	combined := &exactAnswerContract{}
	if current != nil {
		combined.Add(current.snapshot())
	}
	combined.Add(addition)
	if !combined.Active() {
		return llm.Message{}, false
	}
	return contractMessage(combined.snapshot())
}

func failedDelivery(name string, execution ToolExecution, failure toolDeliveryError) ToolExecution {
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

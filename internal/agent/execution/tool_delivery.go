package execution

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dekwanlabs/nasuta/internal/agent/run"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

var ErrToolResultDelivery = errors.New("tool result could not be delivered without loss")

type toolDeliveryError struct {
	Error           string         `json:"error"`
	Tool            string         `json:"tool"`
	ResultBytes     int            `json:"result_bytes"`
	MissingLiterals int            `json:"missing_literals,omitempty"`
	Retry           map[string]any `json:"retry"`
}

func (agent *Agent) prepareDelivery(call llm.ToolCall, execution ToolExecution) ToolExecution {
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
	return execution
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

func newToolResultStep(runID string, stepNo int, call llm.ToolCall, execution ToolExecution) run.StepRecord {
	return run.StepRecord{
		StepNo:              stepNo,
		Kind:                run.StepKindToolResult,
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

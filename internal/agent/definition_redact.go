package agent

import (
	"context"
	"encoding/json"
	"errors"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/platform"
)

type redactingDefinitionObserver struct {
	next Observer
}

func (observer redactingDefinitionObserver) OnStep(
	ctx context.Context,
	runID string,
	step StepRecord,
) error {
	return observer.next.OnStep(ctx, runID, redactDefinitionStep(step))
}

func (observer redactingDefinitionObserver) OnToken(
	ctx context.Context,
	runID, token string,
) {
	observer.next.OnToken(ctx, runID, platform.RedactSensitiveText(token))
}

func (observer redactingDefinitionObserver) OnReasoning(
	ctx context.Context,
	runID, token string,
) {
	observer.next.OnReasoning(ctx, runID, platform.RedactSensitiveText(token))
}

func redactDefinitionRequest(request agentapi.RunRequest) agentapi.RunRequest {
	if !request.Policy.RedactSensitive {
		return request
	}
	request.Input = redactRawMessage(request.Input)
	request.Messages = redactPublicMessages(request.Messages)
	request.Context = redactContextBlocks(request.Context)
	return request
}

func redactDefinitionStart(start agentapi.RunStart) agentapi.RunStart {
	if !start.Policy.RedactSensitive {
		return start
	}
	start.Input = redactRawMessage(start.Input)
	return start
}

func redactDefinitionResult(result agentapi.RunResult) agentapi.RunResult {
	result.Output = redactRawMessage(result.Output)
	result.Text = platform.RedactSensitiveText(result.Text)
	result.References = redactPublicReferences(result.References)
	result.Messages = redactPublicMessages(result.Messages)
	if result.Error != nil {
		copied := *result.Error
		copied.Message = platform.RedactSensitiveText(copied.Message)
		result.Error = &copied
	}
	return result
}

func redactDefinitionOutcome(outcome RunOutcome) RunOutcome {
	outcome.Answer = platform.RedactSensitiveText(outcome.Answer)
	outcome.SessionMessages = redactLLMMessages(outcome.SessionMessages)
	outcome.References = redactPublicReferences(outcome.References)
	if outcome.Err != nil {
		outcome.Err = errors.New(platform.RedactSensitiveText(outcome.Err.Error()))
	}
	return outcome
}

func redactDefinitionStep(step StepRecord) StepRecord {
	content := platform.RedactSensitiveText(step.Content)
	if content != step.Content {
		step.Content = content
		step.SizeBytes = int64(len(content))
		if step.AuthoritativeSHA256 != "" {
			step.AuthoritativeSHA256 = hashString(content)
		}
	}
	prompt := platform.RedactSensitiveText(step.PromptContent)
	if prompt != step.PromptContent {
		step.PromptContent = prompt
		if step.PromptSHA256 != "" {
			step.PromptSHA256 = hashString(prompt)
		}
	}
	step.Args = platform.RedactSensitiveText(step.Args)
	step.ResultPreview = platform.RedactSensitiveText(step.ResultPreview)
	step.DeliveryError = platform.RedactSensitiveText(step.DeliveryError)
	step.AnswerContract.RequiredLiterals = append(
		[]string(nil),
		step.AnswerContract.RequiredLiterals...,
	)
	for index := range step.AnswerContract.RequiredLiterals {
		step.AnswerContract.RequiredLiterals[index] = platform.RedactSensitiveText(
			step.AnswerContract.RequiredLiterals[index],
		)
	}
	return step
}

func redactRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(platform.RedactSensitiveText(string(raw)))
}

func redactContextBlocks(blocks []agentapi.ContextBlock) []agentapi.ContextBlock {
	redacted := make([]agentapi.ContextBlock, len(blocks))
	for index, block := range blocks {
		block.Source = platform.RedactSensitiveText(block.Source)
		block.Title = platform.RedactSensitiveText(block.Title)
		block.Content = platform.RedactSensitiveText(block.Content)
		block.ContentHash = hashString(block.Content)
		block.References = redactPublicReferences(block.References)
		redacted[index] = block
	}
	return redacted
}

func redactPublicReferences(references []agentapi.Reference) []agentapi.Reference {
	redacted := make([]agentapi.Reference, len(references))
	for index, reference := range references {
		reference.Label = platform.RedactSensitiveText(reference.Label)
		reference.Target = platform.RedactSensitiveText(reference.Target)
		redacted[index] = reference
	}
	return redacted
}

func redactPublicMessages(messages []agentapi.Message) []agentapi.Message {
	redacted := make([]agentapi.Message, len(messages))
	for index, message := range messages {
		message.Content = platform.RedactSensitiveText(message.Content)
		message.ToolCalls = append([]agentapi.ToolCall(nil), message.ToolCalls...)
		for callIndex := range message.ToolCalls {
			message.ToolCalls[callIndex].Function.Arguments = platform.RedactSensitiveText(
				message.ToolCalls[callIndex].Function.Arguments,
			)
		}
		redacted[index] = message
	}
	return redacted
}

func redactLLMMessages(messages []llm.Message) []llm.Message {
	redacted := make([]llm.Message, len(messages))
	for index, message := range messages {
		message.Content = platform.RedactSensitiveText(message.Content)
		message.ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
		for callIndex := range message.ToolCalls {
			message.ToolCalls[callIndex].Function.Arguments = platform.RedactSensitiveText(
				message.ToolCalls[callIndex].Function.Arguments,
			)
		}
		redacted[index] = message
	}
	return redacted
}

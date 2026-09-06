// Package messages projects internal chat messages onto the public agent API
// shape. The internal execution engine works in llm.Message; the public
// runtime contract is agent.Message. Keeping this projection in one place
// avoids duplicating the field-by-field copy across call sites.
package messages

import (
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

// Public converts internal chat messages into the public agent API shape.
// An empty input yields a nil slice so callers observe the same "absent"
// value the public contract expects.
func Public(in []llm.Message) []agentapi.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentapi.Message, 0, len(in))
	for _, message := range in {
		compiled := agentapi.Message{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			Name:       message.Name,
		}
		if len(message.ToolCalls) > 0 {
			compiled.ToolCalls = make([]agentapi.ToolCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				compiled.ToolCalls = append(compiled.ToolCalls, agentapi.ToolCall{
					ID:   call.ID,
					Type: call.Type,
					Function: agentapi.ToolFunction{
						Name:      call.Function.Name,
						Arguments: call.Function.Arguments,
					},
				})
			}
		}
		out = append(out, compiled)
	}
	return out
}

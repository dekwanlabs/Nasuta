package execution

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

// enforceFlowContract keeps the flow output contract small and cheap: when a
// server-owned FlowIR is available, the model answer is rebuilt around the one
// deterministic diagram rendered from that FlowIR. There is no post-hoc format
// validation, no extra LLM repair round-trip, and no prose/diagram-rearranging
// fallback. The model's own answer is used as-is when no FlowIR exists.
func (agent *Agent) enforceFlowContract(ctx context.Context, messages []llm.Message, initial *llm.ChatStreamResult, contract agentapi.RunOutputContract, maxTokens int, stream *StreamPipe) *llm.ChatStreamResult {
	if initial == nil || contract.Kind != "flow" || !contract.RequireMermaid {
		return initial
	}
	if flow := flowIRFromContext(ctx); flow != nil {
		initial.Content = canonicalFlowAnswer(initial.Content, flow)
	}
	return initial
}

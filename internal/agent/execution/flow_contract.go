package execution

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

// enforceFlowContract installs the server-owned FlowIR diagrams whenever they
// are present in context. Flows only exist after a delegation produced them, so
// their presence — not the query-intent classifier — is the authoritative
// signal that the answer should carry structured flow diagrams. There is no
// post-hoc format validation or LLM repair round-trip; the model's own answer
// is used as-is when no FlowIR exists.
func (agent *Agent) enforceFlowContract(ctx context.Context, _ []llm.Message, initial *llm.ChatStreamResult, _ agentapi.RunOutputContract, _ int, _ *StreamPipe) *llm.ChatStreamResult {
	if initial == nil {
		return initial
	}
	if flows := flowsFromContext(ctx); len(flows) > 0 {
		initial.Content = canonicalFlowAnswer(initial.Content, flows)
	}
	return initial
}

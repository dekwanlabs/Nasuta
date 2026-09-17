package execution

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

// enforceFlowContract installs the server-owned FlowIR diagrams whenever they
// are present in context. A flow reaches that context only after the server
// canonicalized it, whether it came from a delegated child's report or from the
// model's own answer fence, so presence — not the query-intent classifier — is
// the authoritative signal that the answer should carry structured diagrams.
// There is no post-hoc format validation or LLM repair round-trip; the model's
// own answer is used as-is when no FlowIR exists.
func (agent *Agent) enforceFlowContract(ctx context.Context, _ []llm.Message, initial *llm.ChatStreamResult, _ agentapi.RunOutputContract, _ int, _ *StreamPipe) *llm.ChatStreamResult {
	if initial == nil {
		return initial
	}
	if flows := flowsFromContext(ctx); len(flows) > 0 {
		initial.Content = canonicalFlowAnswer(initial.Content, flows)
	}
	return initial
}

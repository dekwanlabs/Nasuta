package execution

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

// enforceFlowContract is the single authority for what a user-visible answer
// does with flow diagrams. It takes whatever fences the model wrote out of the
// answer, canonicalizes the ones that survive validation, and installs the
// authoritative diagram blocks.
//
// A flow reaches the renderer either as a delegated child's report flow, which
// the caller puts in context, or as a fence the model authored itself, which is
// read from the answer here. Routing both through this one function is what
// makes an answer's diagrams independent of which agent produced them and of
// how the answer was reached — a normal answer turn and a forced conclusion
// behave identically. There is no post-hoc format validation or LLM repair
// round-trip; an answer with no flow is used as-is.
func (agent *Agent) enforceFlowContract(ctx context.Context, _ []llm.Message, initial *llm.ChatStreamResult, _ agentapi.RunOutputContract, _ int, _ *StreamPipe) *llm.ChatStreamResult {
	if initial == nil {
		return initial
	}
	view := flowRenderViewFrom(ctx)
	prose, authored := canonicalizeAuthoredFences(ctx, initial.Content, view.observed)
	initial.Content = canonicalFlowAnswer(prose, mergeFlowSources(ctx, view.flows, authored))
	return initial
}

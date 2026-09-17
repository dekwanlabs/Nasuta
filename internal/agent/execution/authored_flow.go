package execution

import (
	"context"
	"encoding/json"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/delegation"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

// authoredFlowFence marks a flow the model wrote into its own answer. The
// answerer has no structured output slot — its answer is free prose — so a
// fenced block is the only channel a single-agent run can submit a FlowIR
// through.
const authoredFlowFence = "flowir"

// canonicalizeAuthoredFences takes the flow fences the model wrote out of its
// own answer and returns the answer without them plus the flows that survived
// validation.
//
// The fence is a submission channel, not output: a valid one is replaced by the
// canonical block the renderer installs, and an invalid one is dropped so raw
// model JSON never reaches the user. This is the single-agent counterpart to a
// delegated child's structured report flow, so both sources run through the same
// canonicalization and the same subject merge. A fence may carry the number of
// its section, which gives the renderer the same language-stable anchor a
// delegated flow gets from its task index.
func canonicalizeAuthoredFences(ctx context.Context, content string, observed []tool.EvidenceUnit) (string, []*agentapi.FlowIR) {
	prose, bodies := stripAuthoredFlowBlocks(content)
	if len(bodies) == 0 {
		return content, nil
	}
	evidenceIndex := delegation.AddEvidenceUnits(nil, observed)
	adopted := make([]agentapi.FlowIR, 0, len(bodies))
	for _, body := range bodies {
		flow, err := decodeAuthoredFlow(body)
		if err != nil {
			log.WarnfCtx(ctx, "dropped authored flow block: %v", err)
			continue
		}
		canonical, err := delegation.CanonicalFlowIR(flow, evidenceIndex)
		if err != nil {
			log.WarnfCtx(ctx, "dropped authored flow %q: %v", flow.Subject, err)
			continue
		}
		adopted = append(adopted, *canonical)
	}
	if len(adopted) == 0 {
		return prose, nil
	}
	return prose, toFlowPtrs(adopted)
}

// mergeFlowSources folds the flows the model authored into the server-owned
// ones so the renderer sees one set and one diagram per subject.
func mergeFlowSources(ctx context.Context, server []*agentapi.FlowIR, authored []*agentapi.FlowIR) []*agentapi.FlowIR {
	if len(authored) == 0 {
		return server
	}
	merged, err := delegation.MergeFlowIRsBySubject(
		append(flattenExecutionFlows(server), flattenExecutionFlows(authored)...),
	)
	if err != nil {
		log.WarnfCtx(ctx, "authored flow merge failed: %v", err)
		return server
	}
	return merged
}

func toFlowPtrs(flows []agentapi.FlowIR) []*agentapi.FlowIR {
	out := make([]*agentapi.FlowIR, 0, len(flows))
	for index := range flows {
		out = append(out, &flows[index])
	}
	return out
}

// authoredFlowWire is the payload of one model-authored fence: a FlowIR plus
// the number of the section the diagram belongs to.
//
// The order is declared here rather than on agentapi.FlowIR because that
// type's Order is server-derived from a child's task index and is deliberately
// excluded from JSON. Keeping it out of FlowIR means the fence the server
// renders stays byte-identical for delegated answers, and a model can never
// overwrite a task index the parent assigned.
type authoredFlowWire struct {
	agentapi.FlowIR
	Order int `json:"order,omitempty"`
}

// decodeAuthoredFlow rejects unknown fields so a model that invents extra keys
// is told its block is malformed instead of having the keys silently ignored.
func decodeAuthoredFlow(body string) (*agentapi.FlowIR, error) {
	var wire authoredFlowWire
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, err
	}
	flow := wire.FlowIR
	// A missing or non-positive order means no section was claimed. Leaving it 0
	// keeps placement on subject-text matching, which is better than anchoring
	// the diagram to a section the model never named.
	if wire.Order > 0 {
		flow.Order = wire.Order
	}
	return &flow, nil
}

// stripAuthoredFlowBlocks removes every completed flowir fence from the answer
// and returns the raw block bodies. Other fenced blocks (mermaid, svg, code) and
// an unterminated fence are left in place: the model chose a different diagram
// form, or the answer was cut off mid-block, and neither justifies swallowing
// the surrounding prose.
func stripAuthoredFlowBlocks(content string) (string, []string) {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	var bodies []string
	for index := 0; index < len(lines); {
		if info := fenceInfo(lines[index]); info != authoredFlowFence {
			kept = append(kept, lines[index])
			index++
			continue
		}
		body, next, closed := fencedBody(lines, index+1)
		if !closed {
			kept = append(kept, lines[index])
			index++
			continue
		}
		bodies = append(bodies, body)
		index = next
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), bodies
}

// fenceInfo returns the language of a fence opening line, or "" when the line
// does not open a fence.
func fenceInfo(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "```") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
}

// fencedBody collects the lines up to the closing fence, returning the body and
// the index to resume at. An unterminated fence reports closed=false so the
// caller drops it rather than emitting a partial block.
func fencedBody(lines []string, start int) (string, int, bool) {
	for index := start; index < len(lines); index++ {
		if strings.HasPrefix(strings.TrimSpace(lines[index]), "```") {
			return strings.TrimSpace(strings.Join(lines[start:index], "\n")), index + 1, true
		}
	}
	return "", len(lines), false
}

func flattenExecutionFlows(flows []*agentapi.FlowIR) []agentapi.FlowIR {
	if len(flows) == 0 {
		return nil
	}
	out := make([]agentapi.FlowIR, 0, len(flows))
	for _, flow := range flows {
		if flow != nil {
			out = append(out, *flow)
		}
	}
	return out
}

// observedEvidenceUnits is the evidence this run actually saw. Authored flow
// references are reduced against it so a model cannot cite a handle it never
// received.
func (state *compiledLoop) observedEvidenceUnits() []tool.EvidenceUnit {
	if state == nil || state.evidenceLedger == nil {
		return nil
	}
	units, _ := state.evidenceLedger.snapshot()
	return units
}

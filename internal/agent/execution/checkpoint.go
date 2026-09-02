package execution

import (
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

// LogicalLoopCheckpoint is the durable replay boundary emitted after every
// completed logical turn. A checkpoint never claims an in-flight provider call
// is complete; the last persisted message state is replayed on recovery.
type LogicalLoopCheckpoint struct {
	StepNo int
	Phase  string
	State  []byte
}

// LogicalLoopState is versioned state sufficient to rebuild a parent loop
// without re-running completed tool turns. Provider/model handles are not
// serialized; the recovering process supplies the immutable tool snapshot.
type LogicalLoopState struct {
	Version             int                           `json:"version"`
	Request             *agentapi.RunRequest          `json:"request,omitempty"`
	Input               Input                         `json:"input"`
	Messages            []llm.Message                 `json:"messages"`
	AnswerContract      tool.AnswerContract           `json:"answer_contract"`
	EvaluatedAdoptions  []agentapi.DelegationAdoption `json:"evaluated_adoptions,omitempty"`
	StepNo              int                           `json:"step_no"`
	StepSeq             int                           `json:"step_seq,omitempty"`
	Answer              string                        `json:"answer,omitempty"`
	References          []tool.Reference              `json:"references,omitempty"`
	Flow                *agentapi.FlowIR              `json:"flow,omitempty"`
	DelegatedFlows      []agentapi.FlowIR             `json:"delegated_flows,omitempty"`
	DelegationAdoptions []agentapi.DelegationAdoption `json:"delegation_adoptions,omitempty"`
	Answered            bool                          `json:"answered"`
	ToolBudgetExhausted bool                          `json:"tool_budget_exhausted"`
	EvidenceUnits       []tool.EvidenceUnit           `json:"evidence_units,omitempty"`
	EvidenceConflicts   []evidence.Conflict           `json:"evidence_conflicts,omitempty"`
}

func MarshalLogicalLoopState(state LogicalLoopState) ([]byte, error) {
	if state.Version == 0 {
		state.Version = 1
	}
	if len(state.Messages) == 0 {
		return nil, fmt.Errorf("logical loop checkpoint messages are required")
	}
	return json.Marshal(state)
}

func UnmarshalLogicalLoopState(raw []byte) (LogicalLoopState, error) {
	var state LogicalLoopState
	if len(raw) == 0 {
		return state, fmt.Errorf("logical loop checkpoint is empty")
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.Version != 1 {
		return state, fmt.Errorf("unsupported logical loop checkpoint version %d", state.Version)
	}
	if len(state.Messages) == 0 {
		return state, fmt.Errorf("logical loop checkpoint messages are required")
	}
	return state, nil
}

func cloneExecutionFlows(flows []agentapi.FlowIR) []agentapi.FlowIR {
	if len(flows) == 0 {
		return nil
	}
	out := make([]agentapi.FlowIR, 0, len(flows))
	for index := range flows {
		if flow := cloneExecutionFlow(&flows[index]); flow != nil {
			out = append(out, *flow)
		}
	}
	return out
}

func (agent *Agent) checkpointState(state *compiledLoop, phase string, step int) error {
	if agent == nil || agent.cfg.Checkpoint == nil || state == nil {
		return nil
	}
	units, conflicts := state.evidenceLedger.snapshot()
	answerContract := tool.AnswerContract{}
	if state.answerContract != nil {
		answerContract = state.answerContract.snapshot()
	}
	raw, err := MarshalLogicalLoopState(LogicalLoopState{
		Version: 1, Request: state.input.OriginalRequest, Input: state.input, Messages: append([]llm.Message(nil), state.messages...),
		AnswerContract: answerContract, EvaluatedAdoptions: func() []agentapi.DelegationAdoption {
			if state.answerContract == nil {
				return nil
			}
			return state.answerContract.Adoptions()
		}(),
		StepSeq: state.stepSeq, Answer: state.result.Answer, References: append([]tool.Reference(nil), state.result.References...), Flow: cloneExecutionFlow(state.result.Flow), DelegatedFlows: cloneExecutionFlows(state.delegatedFlows), DelegationAdoptions: cloneDelegationAdoptions(state.result.DelegationAdoptions),
		StepNo: step, Answered: state.answered, ToolBudgetExhausted: state.toolBudgetExhausted,
		EvidenceUnits: units, EvidenceConflicts: conflicts,
	})
	if err != nil {
		return err
	}
	return agent.cfg.Checkpoint(LogicalLoopCheckpoint{StepNo: step, Phase: phase, State: raw})
}

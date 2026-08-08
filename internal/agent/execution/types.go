package execution

import (
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/tool"
)

type Registry = tool.Registry
type ToolPolicy = tool.Policy

type Observer = run.Observer
type Controller = run.Controller
type EvidenceMetrics = run.EvidenceMetrics
type StepRecord = run.StepRecord
type StepKind = run.StepKind
type ControlKind = run.ControlKind

const (
	EvidenceNotRequired = run.EvidenceNotRequired
	EvidenceComplete    = run.EvidenceComplete
	EvidencePartial     = run.EvidencePartial
	EvidenceUnavailable = run.EvidenceUnavailable

	StepKindThink      = run.StepKindThink
	StepKindToolCall   = run.StepKindToolCall
	StepKindToolResult = run.StepKindToolResult
	StepKindAnswer     = run.StepKindAnswer
	StepKindRetrieval  = run.StepKindRetrieval

	CtrlAbort = run.CtrlAbort
	CtrlPause = run.CtrlPause
	CtrlNudge = run.CtrlNudge
)

func NoopObserver() Observer {
	return run.NoopObserver()
}

func ToolPolicyForRun(allowWrite bool) ToolPolicy {
	return ToolPolicy{
		AllowRead:  true,
		AllowWrite: allowWrite,
	}
}

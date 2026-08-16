package delegation

import (
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const (
	ErrorChildTimeout              = "child_timeout"
	ErrorParentCancelled           = "parent_cancelled"
	ErrorClientCancelled           = "client_cancelled"
	ErrorInterrupted               = "interrupted"
	ErrorReportPersistenceFailed   = "report_persistence_failed"
	ErrorBudgetAccountingViolation = "budget_accounting_violation"
)

// StatusFacts are the persisted facts used by every delegation projection.
type StatusFacts struct {
	Admitted       bool
	Settled        bool
	RunStatus      agentapi.RunStatus
	ErrorCode      string
	EvidenceStatus string
	Completeness   agentapi.DelegationCompleteness
}

// ProjectStatus keeps API and event state derived from the same facts.
func ProjectStatus(facts StatusFacts) agentapi.DelegationStatus {
	if !facts.Admitted {
		return agentapi.DelegationRejected
	}
	code := strings.TrimSpace(facts.ErrorCode)
	switch code {
	case ErrorChildTimeout:
		return agentapi.DelegationTimeout
	case ErrorParentCancelled, ErrorClientCancelled, "cancelled":
		return agentapi.DelegationCancelled
	case ErrorInterrupted:
		return agentapi.DelegationInterrupted
	}
	switch facts.RunStatus {
	case agentapi.RunCancelled:
		return agentapi.DelegationCancelled
	case agentapi.RunFailed:
		return agentapi.DelegationFailed
	case agentapi.RunSucceeded:
		if !facts.Settled {
			return agentapi.DelegationInterrupted
		}
		if facts.Completeness == agentapi.DelegationIncomplete ||
			facts.EvidenceStatus == "partial" ||
			facts.EvidenceStatus == "unavailable" {
			return agentapi.DelegationPartial
		}
		return agentapi.DelegationCompleted
	default:
		return agentapi.DelegationInterrupted
	}
}

package agent

import (
	agentsession "github.com/dekwanlabs/nasuta/internal/agent/session"
)

// SessionHistory is the bounded current-session archive capability consumed by QA.
type SessionHistory = agentsession.SessionHistory

// SessionCompactionResult reports whether the monotonic archive boundary advanced.
type SessionCompactionResult = agentsession.SessionCompactionResult

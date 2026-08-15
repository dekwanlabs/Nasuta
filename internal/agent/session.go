package agent

import (
	"github.com/dekwanlabs/nasuta/internal/agent/session"
)

// SessionHistory is the bounded current-session archive capability consumed by QA.
type SessionHistory = session.History

// SessionCompactionResult reports whether the monotonic archive boundary advanced.
type SessionCompactionResult = session.CompactionResult

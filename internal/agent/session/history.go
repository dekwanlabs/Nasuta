package session

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/memory"
)

// HistoryCandidates is the bounded result of the candidate-discovery phase.
// Refs are already relevance-filtered and ordered for materialization.
type HistoryCandidates struct {
	Mode string
	Refs []string
}

// SessionHistory is the bounded current-session archive capability consumed by QA.
type SessionHistory interface {
	PrepareRecords([]memory.TurnContextRecord)
	Recall(context.Context, int64, string, string, string, int) (string, error)
	Find(context.Context, int64, string, string, int, int) (string, error)
}

// CandidateDiscovery is an optional two-stage history API. Keeping it
// separate from SessionHistory preserves compatibility with existing fakes and
// private tool implementations.
type CandidateDiscovery interface {
	Discover(context.Context, int64, string, string) (HistoryCandidates, error)
	Materialize(context.Context, int64, string, HistoryCandidates, int, int, bool) (string, error)
}

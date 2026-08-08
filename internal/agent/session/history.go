package session

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/memory"
)

// SessionHistory is the bounded current-session archive capability consumed by QA.
type SessionHistory interface {
	PrepareRecords([]memory.TurnContextRecord)
	Recall(context.Context, int64, string, string, string, int) (string, error)
	Find(context.Context, int64, string, string, int, int) (string, error)
}

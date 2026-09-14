package probe

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

// Can an EXTERNAL package call Score with a hand-built non-empty doc slice?
func TestExternalCallerCanBuildDocs(t *testing.T) {
	rr := retrieval.NewDashScopeReranker(&config.PlatformSettings{})
	// Attempt 1: name the element type.
	_, _ = rr.Score(context.Background(), "q", []retrieval.CodeDoc{{}})
	_ = rr
}

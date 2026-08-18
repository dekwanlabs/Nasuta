package investigation

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
)

func TestBoundedSummaryPreservesShortTextAndBoundsLongText(t *testing.T) {
	if got := BoundedSummary("  checkout failure  "); got != "checkout failure" {
		t.Fatalf("short summary = %q", got)
	}
	long := strings.Repeat("checkout failure investigation ", 1000)
	got := BoundedSummary(long)
	if got == long || tooloutput.EstimateTokens(got) > MaxTaskSummaryTokens {
		t.Fatalf("long summary uses %d tokens", tooloutput.EstimateTokens(got))
	}
}

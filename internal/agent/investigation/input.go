package investigation

import (
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
)

// MaxTaskSummaryTokens bounds text copied from a parent request into a child task.
const MaxTaskSummaryTokens = 384

// BoundedSummary keeps child task context independent from the parent question size.
func BoundedSummary(value string) string {
	return tooloutput.TruncateContent(
		strings.TrimSpace(value),
		MaxTaskSummaryTokens,
	)
}

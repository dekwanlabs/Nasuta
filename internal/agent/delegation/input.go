package delegation

import (
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
)

// maxParentQuestionSummaryTokens bounds parent context copied into a delegated
// child request. The child must receive enough context to stay on task without
// inheriting an unbounded parent prompt.
const maxParentQuestionSummaryTokens = 384

func boundedSummary(value string) string {
	return tooloutput.TruncateContent(
		strings.TrimSpace(value),
		maxParentQuestionSummaryTokens,
	)
}

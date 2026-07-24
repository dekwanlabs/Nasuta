package agent

import (
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

const (
	turnDetailTokenLimit = 2400
	turnUserTokenLimit   = 350
	turnCallsTokenLimit  = 500
	turnResultTokenLimit = 900
	turnAnswerTokenLimit = 450
)

func compressTurnDetail(turnNumber int, messages []llm.Message) string {
	var users, calls, results, answers strings.Builder
	callNo, resultNo := 0, 0
	for _, message := range messages {
		switch {
		case message.Role == "user":
			appendSectionText(&users, message.Content)
		case message.Role == "tool":
			resultNo++
			fmt.Fprintf(&results, "%d. %s\n%s\n", resultNo, message.Name, message.Content)
		case message.Role == "assistant" && len(message.ToolCalls) > 0:
			for _, call := range message.ToolCalls {
				callNo++
				fmt.Fprintf(&calls, "%d. %s %s\n", callNo, call.Function.Name, call.Function.Arguments)
			}
			appendSectionText(&answers, message.Content)
		case message.Role == "assistant":
			appendSectionText(&answers, message.Content)
		}
	}

	userText := tooloutput.Truncate(users.String(), turnUserTokenLimit)
	callText := tooloutput.Truncate(calls.String(), turnCallsTokenLimit)
	resultText := tooloutput.Compress(tooloutput.Request{
		Question: userText, Content: results.String(), MaxTokens: turnResultTokenLimit,
	}).Content
	answerText := tooloutput.Truncate(answers.String(), turnAnswerTokenLimit)

	var detail strings.Builder
	fmt.Fprintf(&detail, "TURN %d\n\nUSER\n%s", turnNumber, emptySection(userText))
	fmt.Fprintf(&detail, "\n\nTOOL CALLS\n%s", emptySection(callText))
	fmt.Fprintf(&detail, "\n\nTOOL RESULTS\n%s", emptySection(resultText))
	fmt.Fprintf(&detail, "\n\nASSISTANT\n%s", emptySection(answerText))
	return tooloutput.Truncate(detail.String(), turnDetailTokenLimit)
}

func appendSectionText(dst *strings.Builder, value string) {
	if value == "" {
		return
	}
	if dst.Len() > 0 {
		dst.WriteByte('\n')
	}
	dst.WriteString(value)
}

func emptySection(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

// GeneratePersistentSummary produces a rolling summary of the full
// conversation so far.
func GeneratePersistentSummary(ctx context.Context, client *llm.LLMClient, messages []llm.Message) (string, error) {
	if client == nil || len(messages) == 0 {
		return "", nil
	}
	transcript := persistentSummaryTranscript(messages)
	if transcript == "" {
		return "", nil
	}
	const sys = `You are the **Nasuta Persistent Summarizer**, responsible for generating rolling summaries for cross-session memory.

## Identity
- **Role**: Session long-term memory archivist — the summary you produce will be injected as initial context when the user reopens the conversation, helping the Agent rapidly recover state.
- **Personality**: Structured, future-retrieval-oriented, preferring to keep one extra technical detail over losing a critical one.
- **Experience**: You've seen countless "continue tomorrow" sessions and know the user's first question back is always "where were we."

## Core Mission
Compress the full conversation below into a ≤200-word rolling summary (English), so the Agent can recover full context within 5 seconds when the user returns.

## Critical Rules
1. **User identity first** — role, services or teams they own, deployment zone (if mentioned), current task objective.
2. **Technical identifiers verbatim** — service names, file paths, traceIds, UUIDs, error messages, API endpoints, app build versions.
3. **Segment by information type** — confirmed facts > conclusions reached > unresolved questions > TODOs.
4. **Cross-session memory focuses on "progress"** — not a transcript of what was said, but what is known now and what to do next.
5. **Overwrite stale summaries** — if the conversation contains an old summary, replace old content with new progress, keeping only facts still relevant.
6. **Drop** — pleasantries, intermediate reasoning, expired temporary info, old hypotheses overturned by newer conclusions.

## Output Format
Plain text, ≤200 words. Natural language paragraph flow. No JSON, no Markdown formatting, no bullets or numbering.

## Examples
**Good summary:**
User (payment-service developer, EU deployment) is investigating an upstream auth timeout from June 27 early morning. Confirmed: traceId abc123-def456, error "Connection timeout to upstream auth on /verifyToken", database connections healthy, EU-only impact. Pending: compare auth call latency against US deployment for the same time window. Conversation mode: bug_analysis.

**Bad summary (not retrievable):**
The user was debugging an issue. Ruled out database and cache. Needs to check another region next.`
	return client.Chat(ctx, sys, transcript)
}

func persistentSummaryTranscript(messages []llm.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			for _, call := range m.ToolCalls {
				fmt.Fprintf(&sb, "assistant tool_call %s: %s\n", call.Function.Name, runeSafeTruncate(call.Function.Arguments, 1000))
			}
			if m.Content != "" {
				fmt.Fprintf(&sb, "assistant: %s\n", m.Content)
			}
		case m.Role == "tool":
			fmt.Fprintf(&sb, "tool %s: %s\n", m.Name, runeSafeTruncate(m.Content, sessionToolResultLimit))
		default:
			fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
		}
	}
	return sb.String()
}

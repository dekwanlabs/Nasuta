package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"golang.org/x/sync/errgroup"
)

const (
	turnSummaryTokenLimit      = 120
	turnSummaryBatchSize       = 4
	turnSummaryBatchWorkers    = 6
	turnSummaryBatchMaxTokens  = 2048
	summaryToolProjectionRunes = 1200
)

type turnSummaryResponse struct {
	Items []turnSummaryItem `json:"items"`
}

type turnSummaryItem struct {
	Item int    `json:"item"`
	Text string `json:"text"`
}

// GenerateTurnCompactionSummaries creates one short, ref-bound summary per turn.
func GenerateTurnCompactionSummaries(ctx context.Context, client *llm.LLMClient, records []memory.TurnContextRecord) (map[string]string, error) {
	if client == nil || len(records) == 0 {
		return nil, nil
	}
	batchCount := (len(records) + turnSummaryBatchSize - 1) / turnSummaryBatchSize
	batches := make([]map[string]string, batchCount)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(turnSummaryBatchWorkers)
	for batchIndex := range batchCount {
		start := batchIndex * turnSummaryBatchSize
		end := min(start+turnSummaryBatchSize, len(records))
		group.Go(func() error {
			batch := records[start:end]
			summaries, err := generateTurnSummaryBatch(groupCtx, client, batch)
			if err != nil {
				return fmt.Errorf("summarize turn batch %d-%d: %w",
					batch[0].TurnNumber, batch[len(batch)-1].TurnNumber, err)
			}
			batches[batchIndex] = summaries
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(records))
	for _, summaries := range batches {
		for ref, summary := range summaries {
			out[ref] = summary
		}
	}
	return out, nil
}

func generateTurnSummaryBatch(ctx context.Context, client *llm.LLMClient, records []memory.TurnContextRecord) (map[string]string, error) {
	transcript, err := turnSummaryTranscript(records)
	if err != nil {
		return nil, err
	}
	if transcript == "" {
		return nil, nil
	}
	sys := prompts.Text(prompts.AgentQATurnSummary)
	user := "Archived turn details as JSON:\n" + transcript
	var response turnSummaryResponse
	err = client.ChatJSON(ctx, sys, user, &response, llm.CallOptions{
		MaxTokens: turnSummaryBatchMaxTokens,
		Validate: func(parsed any) error {
			value, ok := parsed.(*turnSummaryResponse)
			if !ok {
				return fmt.Errorf("unexpected turn summary response type %T", parsed)
			}
			return validateTurnSummaryItems(value.Items, len(records))
		},
	})
	if err != nil {
		return nil, err
	}
	return mapTurnSummaries(response.Items, records), nil
}

func turnSummaryTranscript(records []memory.TurnContextRecord) (string, error) {
	items := make([]struct {
		Item   int             `json:"item"`
		Turn   int             `json:"turn"`
		Detail json.RawMessage `json:"detail"`
	}, len(records))
	for i, record := range records {
		if !json.Valid(record.DetailJSON) {
			return "", fmt.Errorf("turn %d detail is not valid JSON", record.TurnNumber)
		}
		items[i].Item = i + 1
		items[i].Turn = record.TurnNumber
		items[i].Detail = record.DetailJSON
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("marshal turn summary input: %w", err)
	}
	return string(raw), nil
}

func parseTurnSummaries(raw string, records []memory.TurnContextRecord) (map[string]string, error) {
	var response turnSummaryResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &response); err != nil {
		return nil, fmt.Errorf("parse turn summary JSON: %w", err)
	}
	if err := validateTurnSummaryItems(response.Items, len(records)); err != nil {
		return nil, err
	}
	return mapTurnSummaries(response.Items, records), nil
}

func validateTurnSummaryItems(items []turnSummaryItem, recordCount int) error {
	if len(items) != recordCount {
		return fmt.Errorf("turn summary item count mismatch: got %d want %d", len(items), recordCount)
	}
	seen := make(map[int]struct{}, len(items))
	for _, item := range items {
		if item.Item < 1 || item.Item > recordCount {
			return fmt.Errorf("turn summary returned unknown item %d", item.Item)
		}
		if _, duplicate := seen[item.Item]; duplicate {
			return fmt.Errorf("turn summary returned duplicate item %d", item.Item)
		}
		seen[item.Item] = struct{}{}
		text := strings.TrimSpace(item.Text)
		if text == "" {
			return fmt.Errorf("turn summary returned empty text for item %d", item.Item)
		}
	}
	for item := 1; item <= recordCount; item++ {
		if _, ok := seen[item]; !ok {
			return fmt.Errorf("turn summary missing item %d", item)
		}
	}
	return nil
}

func mapTurnSummaries(items []turnSummaryItem, records []memory.TurnContextRecord) map[string]string {
	out := make(map[string]string, len(items))
	for _, item := range items {
		text := tooloutput.TruncateContent(strings.TrimSpace(item.Text), turnSummaryTokenLimit)
		out[records[item.Item-1].Ref] = text
	}
	return out
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
			fmt.Fprintf(&sb, "tool %s: %s\n", m.Name, runeSafeTruncate(m.Content, summaryToolProjectionRunes))
		default:
			fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
		}
	}
	return sb.String()
}

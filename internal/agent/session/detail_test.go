package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestCompressTurnDetailProducesBoundedStructuredJSON(t *testing.T) {
	detailJSON, err := CompressTurnDetail(7, []llm.Message{
		{Role: "user", Content: "查一下设备订阅"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call-1", Function: llm.ToolFunction{Name: "search_code", Arguments: `{"query":"subscription"}`},
		}}},
		{Role: "tool", Name: "search_code", ToolCallID: "call-1", Content: `{"matches":[{"path":"Client.java"}]}`},
		{Role: "assistant", Content: strings.Repeat("结论", 300)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(detailJSON) {
		t.Fatalf("detail is not JSON: %s", detailJSON)
	}
	if strings.Contains(string(detailJSON), "tool output truncated") {
		t.Fatalf("inline truncation marker leaked into detail JSON: %s", detailJSON)
	}
	var detail archivedTurnDetail
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Version != 1 || detail.Turn != 7 || len(detail.ToolCalls) != 1 || len(detail.ToolResults) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.ToolCalls[0].Name != "search_code" || detail.ToolResults[0].Coverage != "full" {
		t.Fatalf("tool archive = %+v / %+v", detail.ToolCalls[0], detail.ToolResults[0])
	}
	if tooloutput.EstimateTokens(string(detailJSON)) > turnDetailTokenLimit {
		t.Fatalf("detail exceeds token limit: %d", tooloutput.EstimateTokens(string(detailJSON)))
	}
}

func TestCompressTurnDetailHonorsAggregateToolBudgets(t *testing.T) {
	const toolCount = 20
	detailJSON, err := CompressTurnDetail(34, toolHeavyTurnMessages(toolCount))
	if err != nil {
		t.Fatal(err)
	}
	var detail archivedTurnDetail
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.ToolCalls) != toolCount || len(detail.ToolResults) != toolCount ||
		detail.OmittedToolCalls != 0 || detail.OmittedToolResults != 0 {
		t.Fatalf("tool archive counts = %d+%d/%d+%d, want %d/%d",
			len(detail.ToolCalls), detail.OmittedToolCalls,
			len(detail.ToolResults), detail.OmittedToolResults, toolCount, toolCount)
	}
	if tokens := tooloutput.EstimateTokens(string(detailJSON)); tokens > turnDetailTokenLimit {
		t.Fatalf("detail uses %d tokens, want at most %d", tokens, turnDetailTokenLimit)
	}
}

func TestCompressTurnDetailOmitsMiddleToolEventsWhenMetadataExceedsBudget(t *testing.T) {
	const toolCount = 40
	detailJSON, err := CompressTurnDetail(35, toolHeavyTurnMessages(toolCount))
	if err != nil {
		t.Fatal(err)
	}
	var detail archivedTurnDetail
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.OmittedToolCalls == 0 || detail.OmittedToolResults == 0 ||
		len(detail.ToolCalls)+detail.OmittedToolCalls != toolCount ||
		len(detail.ToolResults)+detail.OmittedToolResults != toolCount {
		t.Fatalf("tool archive coverage = %d+%d/%d+%d, want totals %d/%d",
			len(detail.ToolCalls), detail.OmittedToolCalls,
			len(detail.ToolResults), detail.OmittedToolResults, toolCount, toolCount)
	}
	if tokens := tooloutput.EstimateTokens(string(detailJSON)); tokens > turnDetailTokenLimit {
		t.Fatalf("detail uses %d tokens, want at most %d", tokens, turnDetailTokenLimit)
	}
}

func toolHeavyTurnMessages(toolCount int) []llm.Message {
	messages := []llm.Message{{
		Role:    "user",
		Content: "对齐现有实现并给出完整接入方案",
	}}
	for i := range toolCount {
		callID := fmt.Sprintf("call-%d", i)
		messages = append(messages,
			llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: callID,
				Function: llm.ToolFunction{
					Name:      "get_symbol",
					Arguments: fmt.Sprintf(`{"query":"%s"}`, strings.Repeat("implementation detail ", 20)),
				},
			}}},
			llm.Message{
				Role: "tool", Name: "get_symbol", ToolCallID: callID,
				Content: fmt.Sprintf(`{"matches":[{"content":"%s"}]}`, strings.Repeat("service implementation ", 80)),
			},
		)
	}
	messages = append(messages, llm.Message{
		Role:    "assistant",
		Content: strings.Repeat("完整方案与迁移步骤", 1200),
	})
	return messages
}

package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestCompressTurnDetailProducesBoundedStructuredJSON(t *testing.T) {
	detailJSON, err := compressTurnDetail(7, []llm.Message{
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
	const toolCount = 14
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

	detailJSON, err := compressTurnDetail(34, messages)
	if err != nil {
		t.Fatal(err)
	}
	var detail archivedTurnDetail
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.ToolCalls) != toolCount || len(detail.ToolResults) != toolCount {
		t.Fatalf("tool archive counts = %d/%d, want %d/%d",
			len(detail.ToolCalls), len(detail.ToolResults), toolCount, toolCount)
	}
	if tokens := tooloutput.EstimateTokens(string(detailJSON)); tokens > turnDetailTokenLimit {
		t.Fatalf("detail uses %d tokens, want at most %d", tokens, turnDetailTokenLimit)
	}
}

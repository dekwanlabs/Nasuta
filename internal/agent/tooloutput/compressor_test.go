package tooloutput

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/tool"
)

func TestCompressPassesUnderBudgetContentThroughUnchanged(t *testing.T) {
	content := `{"deviceId":"2039236571084886018","name":"雾化风扇"}`
	got := Compress(Request{Content: content, MaxTokens: 100})

	if got.Content != content {
		t.Fatalf("content changed: %q", got.Content)
	}
	if got.Compressed || got.Strategy != strategyPassthrough {
		t.Fatalf("result = %+v, want passthrough", got)
	}
}

func TestCompressJSONPreservesLongIDsAndSelectsRelevantItem(t *testing.T) {
	content := largeDeviceJSON(80, 57)
	got := Compress(Request{
		Question:  "雾化风扇的 deviceId 是什么？",
		Arguments: tool.Arguments{"familyId": "2007830887593005058"},
		Content:   content,
		MaxTokens: 700,
	})

	var envelope struct {
		Nasuta struct {
			Compressed    bool   `json:"compressed"`
			SourceFormat  string `json:"source_format"`
			ItemCoverage  string `json:"item_coverage"`
			FieldCoverage string `json:"field_coverage"`
		} `json:"_nasuta"`
		Chunks []struct {
			Ref     string          `json:"ref"`
			Content json.RawMessage `json:"content"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(got.Content), &envelope); err != nil {
		t.Fatalf("compressed content is not valid JSON: %v\n%s", err, got.Content)
	}
	if !envelope.Nasuta.Compressed || envelope.Nasuta.SourceFormat != "json" {
		t.Fatalf("metadata = %+v", envelope.Nasuta)
	}
	if envelope.Nasuta.ItemCoverage != "partial" || envelope.Nasuta.FieldCoverage != "full" {
		t.Fatalf("coverage = item:%s field:%s", envelope.Nasuta.ItemCoverage, envelope.Nasuta.FieldCoverage)
	}
	joined := got.Content
	for _, want := range []string{
		`"familyId":"2007830887593005058"`,
		`"name":"雾化风扇"`,
		`"deviceId":"2039236571084886018"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("compressed result missing %s: %s", want, joined)
		}
	}
	if strings.Index(joined, `"device-0"`) > strings.Index(joined, `"雾化风扇"`) {
		t.Fatalf("chunks were not restored to source order: %s", joined)
	}
}

func TestCompressPreservesUnquotedLongJSONNumber(t *testing.T) {
	content := `{"records":[` + strings.Repeat(`{"id":2039236571084886018,"payload":"`+strings.Repeat("x", 80)+`"},`, 30) +
		`{"id":2039236571084886019,"payload":"target"}]}`
	got := Compress(Request{Question: "2039236571084886019", Content: content, MaxTokens: 450})

	if !strings.Contains(got.Content, "2039236571084886019") {
		t.Fatalf("long numeric ID was lost: %s", got.Content)
	}
	if strings.Contains(got.Content, "2039236571084886000") {
		t.Fatalf("long numeric ID lost precision: %s", got.Content)
	}
}

func TestCompressJSONLAndTextKeepLineReferences(t *testing.T) {
	t.Run("jsonl", func(t *testing.T) {
		var content strings.Builder
		for i := 1; i <= 80; i++ {
			fmt.Fprintf(&content, `{"seq":%d,"message":"event-%d %s"}`+"\n", i, i, strings.Repeat("x", 20))
		}
		got := Compress(Request{Question: "event-61", Content: content.String(), MaxTokens: 400})
		if !strings.Contains(got.Content, `"source_format":"jsonl"`) ||
			!strings.Contains(got.Content, `"ref":"lines:61-61"`) {
			t.Fatalf("JSONL references missing: %s", got.Content)
		}
	})

	t.Run("text", func(t *testing.T) {
		lines := make([]string, 80)
		for i := range lines {
			lines[i] = fmt.Sprintf("line %d %s", i+1, strings.Repeat("payload ", 12))
		}
		lines[60] = "line 61 kibana timeout exact-target"
		got := Compress(Request{Question: "exact-target", Content: strings.Join(lines, "\n"), MaxTokens: 400})
		if !strings.Contains(got.Content, `"source_format":"text"`) ||
			!strings.Contains(got.Content, `"ref":"lines:`) ||
			!strings.Contains(got.Content, "exact-target") {
			t.Fatalf("text references missing: %s", got.Content)
		}
	})
}

func TestCompressIncludesNoticesWithinBudget(t *testing.T) {
	content := strings.Repeat("line payload\n", 800)
	notice := "Retained chunks are exact excerpts. Omitted chunks are unknown."
	got := Compress(Request{Content: content, Notices: []string{notice}, MaxTokens: 300})

	if EstimateTokens(got.Content) > 300 {
		t.Fatalf("result uses %d tokens, want at most 300", EstimateTokens(got.Content))
	}
	if !strings.Contains(got.Content, notice) {
		t.Fatalf("notice missing: %s", got.Content)
	}
}

func TestTruncateHonorsTinyBudgetsAndUTF8(t *testing.T) {
	input := "开头\n" + strings.Repeat("abc内容", 200) + "\n结尾"
	for _, limit := range []int{-1, 0, 1, 2, 10, 100} {
		got := Truncate(input, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d produced invalid UTF-8: %q", limit, got)
		}
		if tokens := EstimateTokens(got); tokens > max(0, limit) {
			t.Fatalf("limit %d produced %d estimated tokens: %q", limit, tokens, got)
		}
	}
}

func largeDeviceJSON(count, target int) string {
	devices := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("device-%d", i)
		deviceID := fmt.Sprintf("20392365710848%05d", i)
		if i == target {
			name = "雾化风扇"
			deviceID = "2039236571084886018"
		}
		devices = append(devices, map[string]any{
			"name":     name,
			"deviceId": deviceID,
			"image":    "https://resources.example.com/" + strings.Repeat("asset", 12),
			"owner":    true,
		})
	}
	value := []any{map[string]any{
		"familyId":   "2007830887593005058",
		"familyName": "brooke.wang的家",
		"roomDeviceItems": []any{map[string]any{
			"id":       "2007830887605587970",
			"roomName": "默认房间",
			"devices":  devices,
		}},
	}}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

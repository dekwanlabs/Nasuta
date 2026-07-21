package tooloutput

import (
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/tool"
)

const (
	strategyPassthrough = "passthrough"
	strategyExtractive  = "structured-extractive-v1"
	strategyFallback    = "head-tail-fallback"
)

// Request contains only runtime-owned compression inputs.
type Request struct {
	Question  string
	Arguments tool.Arguments
	Content   string
	Notices   []string
	MaxTokens int
}

// Result describes the exact model-side content and its compression metrics.
type Result struct {
	Content         string
	Compressed      bool
	Strategy        string
	SourceFormat    string
	OriginalTokens  int
	RetainedTokens  int
	OriginalChunks  int
	RetainedChunks  int
	OmittedChunks   int
	ChunkCoverage   string
	ItemCoverage    string
	FieldCoverage   string
	FallbackReason  string
	CompressionTime time.Duration
}

// Compress returns unchanged content below budget and a traceable envelope above it.
func Compress(req Request) Result {
	started := time.Now()
	result := compress(req)
	result.CompressionTime = time.Since(started)
	return result
}

func compress(req Request) Result {
	req.Notices = normalizeNotices(req.Notices)
	combined := appendNotices(req.Content, req.Notices)
	originalTokens := EstimateTokens(combined)
	if originalTokens <= req.MaxTokens && req.MaxTokens > 0 {
		return Result{
			Content:        combined,
			Strategy:       strategyPassthrough,
			OriginalTokens: originalTokens,
			RetainedTokens: originalTokens,
			ChunkCoverage:  "full",
			ItemCoverage:   "not_applicable",
			FieldCoverage:  "not_applicable",
		}
	}
	if req.MaxTokens <= 0 {
		return fallbackResult(req, "non-positive token budget")
	}

	targetTokens := chunkTarget(req.MaxTokens)
	if chunks, contexts, ok := buildJSONChunks(req.Content, targetTokens); ok {
		return pack(req, "json", chunks, contexts, originalTokens)
	}
	if chunks, ok := buildJSONLChunks(req.Content); ok {
		return pack(req, "jsonl", chunks, nil, originalTokens)
	}
	chunks := buildTextChunks(req.Content, targetTokens)
	if len(chunks) == 0 {
		return fallbackResult(req, "no compressible chunks")
	}
	return pack(req, "text", chunks, nil, originalTokens)
}

func fallbackResult(req Request, reason string) Result {
	combined := appendNotices(req.Content, req.Notices)
	originalTokens := EstimateTokens(combined)
	content := truncate(combined, req.MaxTokens, originalTokens)
	return Result{
		Content:        content,
		Compressed:     originalTokens > req.MaxTokens,
		Strategy:       strategyFallback,
		SourceFormat:   "text",
		OriginalTokens: originalTokens,
		RetainedTokens: EstimateTokens(content),
		ChunkCoverage:  "partial",
		ItemCoverage:   "not_applicable",
		FieldCoverage:  "not_applicable",
		FallbackReason: reason,
	}
}

func appendNotices(content string, notices []string) string {
	if len(notices) == 0 {
		return content
	}
	var out strings.Builder
	out.Grow(len(content) + len(notices)*32)
	out.WriteString(content)
	for _, notice := range notices {
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(notice)
	}
	return out.String()
}

func normalizeNotices(notices []string) []string {
	out := make([]string, 0, len(notices))
	for _, notice := range notices {
		if notice = strings.TrimSpace(notice); notice != "" {
			out = append(out, notice)
		}
	}
	return out
}

func chunkTarget(maxTokens int) int {
	target := 600
	if maxTokens/3 < target {
		target = maxTokens / 3
	}
	if target < 64 {
		target = 64
	}
	return target
}

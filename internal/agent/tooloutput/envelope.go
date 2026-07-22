package tooloutput

import (
	"encoding/json"
	"sort"
)

type envelope struct {
	Nasuta   envelopeMetadata  `json:"_nasuta"`
	Contexts []envelopeContext `json:"contexts,omitempty"`
	Chunks   []envelopeChunk   `json:"chunks"`
	Notices  []string          `json:"notices,omitempty"`
}

type envelopeMetadata struct {
	Version                 int    `json:"version"`
	Compressed              bool   `json:"compressed"`
	Strategy                string `json:"strategy"`
	SourceFormat            string `json:"source_format"`
	OriginalEstimatedTokens int    `json:"original_estimated_tokens"`
	RetainedEstimatedTokens int    `json:"retained_estimated_tokens"`
	OriginalChunks          int    `json:"original_chunks"`
	RetainedChunks          int    `json:"retained_chunks"`
	OmittedChunks           int    `json:"omitted_chunks"`
	ChunkCoverage           string `json:"chunk_coverage"`
	ItemCoverage            string `json:"item_coverage"`
	FieldCoverage           string `json:"field_coverage"`
}

type envelopeContext struct {
	Ref    string         `json:"ref"`
	Fields map[string]any `json:"fields"`
}

type envelopeChunk struct {
	Ref              string   `json:"ref"`
	Ordinal          int      `json:"ordinal"`
	Kind             string   `json:"kind"`
	ContextRefs      []string `json:"context_refs,omitempty"`
	Content          any      `json:"content"`
	ContentTruncated bool     `json:"content_truncated,omitempty"`
}

func pack(req Request, sourceFormat string, chunks []chunk, contexts []ancestorContext, originalTokens int) Result {
	ranked := rankChunks(chunks, req.Question)
	contextByRef := make(map[string]ancestorContext, len(contexts))
	for _, item := range contexts {
		contextByRef[item.ref] = item
	}

	base := buildEnvelope(sourceFormat, chunks, contexts, nil, req.Notices, originalTokens)
	baseBytes, err := json.Marshal(base)
	if err != nil || EstimateTokens(string(baseBytes)) >= req.MaxTokens {
		return fallbackResult(req, "envelope metadata exceeds token budget")
	}

	target := req.MaxTokens * 97 / 100
	if target <= 0 {
		target = req.MaxTokens
	}
	used := EstimateTokens(string(baseBytes))
	selected := make([]int, 0, len(chunks))
	selectedContexts := make(map[string]struct{}, len(contexts))
	for _, index := range ranked {
		cost := estimateEnvelopeChunk(chunks[index])
		for _, ref := range chunks[index].contextRefs {
			if _, exists := selectedContexts[ref]; exists {
				continue
			}
			if item, ok := contextByRef[ref]; ok {
				cost += estimateEnvelopeContext(item)
			}
		}
		if used+cost > target {
			continue
		}
		selected = append(selected, index)
		used += cost
		for _, ref := range chunks[index].contextRefs {
			selectedContexts[ref] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return packTruncatedChunk(req, sourceFormat, chunks, contexts, ranked, originalTokens)
	}

	for len(selected) > 0 {
		current := buildEnvelope(sourceFormat, chunks, contexts, selected, req.Notices, originalTokens)
		encoded, marshalErr := marshalEnvelope(current)
		if marshalErr == nil && EstimateTokens(encoded) <= req.MaxTokens {
			return resultFromEnvelope(encoded, current, originalTokens)
		}
		selected = selected[:len(selected)-1]
	}
	return packTruncatedChunk(req, sourceFormat, chunks, contexts, ranked, originalTokens)
}

func buildEnvelope(sourceFormat string, chunks []chunk, contexts []ancestorContext, selected []int, notices []string, originalTokens int) envelope {
	selectedSet := make(map[int]struct{}, len(selected))
	requiredContexts := make(map[string]struct{})
	for _, index := range selected {
		selectedSet[index] = struct{}{}
		for _, ref := range chunks[index].contextRefs {
			requiredContexts[ref] = struct{}{}
		}
	}

	envelopeContexts := make([]envelopeContext, 0, len(requiredContexts))
	for _, item := range contexts {
		if _, ok := requiredContexts[item.ref]; !ok {
			continue
		}
		envelopeContexts = append(envelopeContexts, envelopeContext{
			Ref: item.ref, Fields: item.fields,
		})
	}

	sourceOrder := append([]int(nil), selected...)
	sort.SliceStable(sourceOrder, func(i, j int) bool {
		return chunks[sourceOrder[i]].ordinal < chunks[sourceOrder[j]].ordinal
	})
	envelopeChunks := make([]envelopeChunk, 0, len(sourceOrder))
	for _, index := range sourceOrder {
		item := chunks[index]
		envelopeChunks = append(envelopeChunks, envelopeChunk{
			Ref:              item.ref,
			Ordinal:          item.ordinal,
			Kind:             item.kind,
			ContextRefs:      item.contextRefs,
			Content:          item.envelopeContent(),
			ContentTruncated: item.contentTruncated,
		})
	}

	chunkCoverage, itemCoverage, fieldCoverage := coverage(sourceFormat, chunks, selectedSet)
	return envelope{
		Nasuta: envelopeMetadata{
			Version:                 1,
			Compressed:              true,
			Strategy:                strategyExtractive,
			SourceFormat:            sourceFormat,
			OriginalEstimatedTokens: originalTokens,
			OriginalChunks:          len(chunks),
			RetainedChunks:          len(selected),
			OmittedChunks:           len(chunks) - len(selected),
			ChunkCoverage:           chunkCoverage,
			ItemCoverage:            itemCoverage,
			FieldCoverage:           fieldCoverage,
		},
		Contexts: envelopeContexts,
		Chunks:   envelopeChunks,
		Notices:  notices,
	}
}

func coverage(sourceFormat string, chunks []chunk, selected map[int]struct{}) (string, string, string) {
	chunkCoverage := "full"
	if len(selected) < len(chunks) {
		chunkCoverage = "partial"
	}
	if sourceFormat == "text" || sourceFormat == "jsonl" {
		return chunkCoverage, "not_applicable", "not_applicable"
	}

	groupTotals := make(map[string]int)
	groupSelected := make(map[string]int)
	fieldCoverage := "full"
	for index, item := range chunks {
		if item.itemRef != "" {
			groupTotals[item.itemRef]++
		}
		if _, retained := selected[index]; retained {
			if item.itemRef != "" {
				groupSelected[item.itemRef]++
			}
			if item.contentTruncated {
				fieldCoverage = "partial"
			}
			continue
		}
		if item.itemRef == "" {
			fieldCoverage = "partial"
		}
	}

	itemCoverage := "not_applicable"
	if len(groupTotals) > 0 {
		itemCoverage = "full"
		for ref := range groupTotals {
			if groupSelected[ref] == 0 {
				itemCoverage = "partial"
				continue
			}
			if groupSelected[ref] < groupTotals[ref] {
				fieldCoverage = "partial"
			}
		}
	}
	return chunkCoverage, itemCoverage, fieldCoverage
}

func marshalEnvelope(value envelope) (string, error) {
	value.Nasuta.RetainedEstimatedTokens = 0
	first, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	value.Nasuta.RetainedEstimatedTokens = EstimateTokens(string(first))
	second, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	finalTokens := EstimateTokens(string(second))
	if finalTokens != value.Nasuta.RetainedEstimatedTokens {
		value.Nasuta.RetainedEstimatedTokens = finalTokens
		second, err = json.Marshal(value)
		if err != nil {
			return "", err
		}
	}
	return string(second), nil
}

func resultFromEnvelope(content string, value envelope, originalTokens int) Result {
	return Result{
		Content:        content,
		Compressed:     true,
		Strategy:       value.Nasuta.Strategy,
		SourceFormat:   value.Nasuta.SourceFormat,
		OriginalTokens: originalTokens,
		RetainedTokens: EstimateTokens(content),
		OriginalChunks: value.Nasuta.OriginalChunks,
		RetainedChunks: value.Nasuta.RetainedChunks,
		OmittedChunks:  value.Nasuta.OmittedChunks,
		ChunkCoverage:  value.Nasuta.ChunkCoverage,
		ItemCoverage:   value.Nasuta.ItemCoverage,
		FieldCoverage:  value.Nasuta.FieldCoverage,
	}
}

func packTruncatedChunk(req Request, sourceFormat string, chunks []chunk, contexts []ancestorContext, ranked []int, originalTokens int) Result {
	if len(ranked) == 0 {
		return fallbackResult(req, "no ranked chunks")
	}
	index := ranked[0]
	excerptBudget := max(16, req.MaxTokens/3)
	excerpt := Truncate(chunks[index].searchableText(), excerptBudget)
	truncated := chunk{
		ref:              chunks[index].ref,
		kind:             "text",
		text:             excerpt,
		ordinal:          chunks[index].ordinal,
		itemRef:          chunks[index].itemRef,
		contentTruncated: true,
	}
	replacement := append([]chunk(nil), chunks...)
	replacement[index] = truncated
	value := buildEnvelope(sourceFormat, replacement, contexts, []int{index}, req.Notices, originalTokens)
	encoded, err := marshalEnvelope(value)
	if err != nil || EstimateTokens(encoded) > req.MaxTokens {
		return fallbackResult(req, "no complete chunk fits token budget")
	}
	result := resultFromEnvelope(encoded, value, originalTokens)
	return result
}

func estimateEnvelopeChunk(item chunk) int {
	encoded, err := json.Marshal(envelopeChunk{
		Ref:              item.ref,
		Ordinal:          item.ordinal,
		Kind:             item.kind,
		ContextRefs:      item.contextRefs,
		Content:          item.envelopeContent(),
		ContentTruncated: item.contentTruncated,
	})
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return EstimateTokens(string(encoded)) + 2
}

func estimateEnvelopeContext(item ancestorContext) int {
	encoded, err := json.Marshal(envelopeContext{Ref: item.ref, Fields: item.fields})
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return EstimateTokens(string(encoded)) + 2
}

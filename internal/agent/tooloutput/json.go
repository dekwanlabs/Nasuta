package tooloutput

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type jsonChunkBuilder struct {
	target   int
	chunks   []chunk
	contexts []ancestorContext
	ordinal  int
}

func buildJSONChunks(content string, target int) ([]chunk, []ancestorContext, bool) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, nil, false
	}

	builder := jsonChunkBuilder{target: target}
	builder.walk(root, "$", nil, "")
	if len(builder.chunks) == 0 {
		return nil, nil, false
	}
	return builder.chunks, builder.contexts, true
}

func (b *jsonChunkBuilder) walk(value any, ref string, contextRefs []string, itemRef string) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	if EstimateTokens(string(raw)) <= b.target {
		b.addJSONChunk(ref, raw, contextRefs, itemRef)
		return
	}

	switch typed := value.(type) {
	case []any:
		for i, item := range typed {
			childRef := fmt.Sprintf("%s[%d]", ref, i)
			b.walk(item, childRef, contextRefs, childRef)
		}
	case map[string]any:
		b.walkObject(typed, ref, contextRefs, itemRef)
	default:
		b.addJSONChunk(ref, raw, contextRefs, itemRef)
	}
}

func (b *jsonChunkBuilder) walkObject(value map[string]any, ref string, contextRefs []string, itemRef string) {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	contextLimit := max(32, b.target/3)
	contextFields := make(map[string]any)
	contextTokens := 0
	remaining := make([]string, 0, len(keys))
	for _, key := range keys {
		field := value[key]
		if isContainer(field) {
			remaining = append(remaining, key)
			continue
		}
		raw, err := json.Marshal(map[string]any{key: field})
		if err != nil {
			continue
		}
		cost := EstimateTokens(string(raw))
		if contextTokens+cost <= contextLimit {
			contextFields[key] = field
			contextTokens += cost
			continue
		}
		remaining = append(remaining, key)
	}

	childContexts := contextRefs
	if len(contextFields) > 0 {
		b.contexts = append(b.contexts, ancestorContext{
			ref: ref, fields: contextFields,
		})
		childContexts = appendCopy(contextRefs, ref)
	}
	if len(remaining) == 0 {
		b.addJSONChunk(ref, json.RawMessage(`{}`), childContexts, itemRef)
		return
	}
	for _, key := range remaining {
		b.walk(value[key], jsonPathField(ref, key), childContexts, itemRef)
	}
}

func (b *jsonChunkBuilder) addJSONChunk(ref string, raw json.RawMessage, contextRefs []string, itemRef string) {
	copied := append(json.RawMessage(nil), raw...)
	b.chunks = append(b.chunks, chunk{
		ref:         ref,
		kind:        "json",
		raw:         copied,
		contextRefs: append([]string(nil), contextRefs...),
		ordinal:     b.ordinal,
		itemRef:     itemRef,
	})
	b.ordinal++
}

func isContainer(value any) bool {
	switch value.(type) {
	case []any, map[string]any:
		return true
	default:
		return false
	}
}

func appendCopy(values []string, value string) []string {
	out := make([]string, len(values), len(values)+1)
	copy(out, values)
	return append(out, value)
}

func jsonPathField(parent, key string) string {
	if isIdentifier(key) {
		return parent + "." + key
	}
	return parent + "[" + strconv.Quote(key) + "]"
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

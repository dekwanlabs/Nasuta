package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

// ToolExecutor adapts tools.Registry to the agent loop.
type ToolExecutor struct {
	registry *Registry
	runtime  *tool.Executor
}

// ToolExecution separates authoritative evidence from the exact model payload.
type ToolExecution struct {
	AuthoritativeContent string
	PromptContent        string
	Notices              []string
	References           []tool.Reference
	EvidenceUnits        []tool.EvidenceUnit
	Evidence             bool
	Failed               bool
	Coverage             tool.EvidenceCoverage
	AnswerContract       tool.AnswerContract
	DeliveryError        string
	ArtifactID           string
	DurationMs           int
}

// NewToolExecutor wraps a registry with a default per-tool timeout.
func NewToolExecutor(registry *Registry) *ToolExecutor {
	return &ToolExecutor{registry: registry, runtime: tool.NewExecutor(15 * time.Second)}
}

// Snapshot pins definitions and handlers before the model sees any tool.
func (te *ToolExecutor) Snapshot(policy ToolPolicy) tool.Snapshot {
	return te.registry.Snapshot(policy)
}

// Definitions returns model schemas from one immutable snapshot.
func (te *ToolExecutor) Definitions(snapshot tool.Snapshot) []llm.ToolDef {
	return ToolDefinitions(snapshot.Tools())
}

// DefinitionsFor snapshots current tools for one-shot callers.
func (te *ToolExecutor) DefinitionsFor(policy ToolPolicy) []llm.ToolDef {
	return te.Definitions(te.Snapshot(policy))
}

// Execute runs against the same snapshot used to publish model definitions.
func (te *ToolExecutor) Execute(ctx context.Context, snapshot tool.Snapshot, call llm.ToolCall, referenceTypes map[string]tool.ReferenceType, seen map[string]bool) ToolExecution {
	name := call.Function.Name
	args, err := parseArgs(ctx, call.Function.Arguments)
	if err != nil {
		result := fmt.Sprintf("error: %v", err)
		return ToolExecution{AuthoritativeContent: result, PromptContent: result, Failed: true}
	}
	arguments := args

	candidate, ok := snapshot.Get(tool.ToolID(name))
	if !ok {
		result := fmt.Sprintf("error: unknown tool %q", name)
		return ToolExecution{AuthoritativeContent: result, PromptContent: result, Failed: true}
	}
	if mismatch := referenceMismatch(snapshot, candidate, arguments, referenceTypes); mismatch != "" {
		return ToolExecution{AuthoritativeContent: mismatch, PromptContent: mismatch, Failed: true}
	}

	fp := ""
	if seen != nil {
		fp = toolFingerprint(name, args)
		if seen[fp] {
			log.InfofCtx(ctx, "[agent] tool %s deduped (repeat call — returning placeholder)", name)
			result := "(already searched with the same arguments; see previous result above)"
			return ToolExecution{AuthoritativeContent: result, PromptContent: result}
		}
	}

	t0 := time.Now()
	toolResult, err := te.runtime.Execute(ctx, snapshot, tool.ToolID(name), arguments)
	duration := time.Since(t0)
	if err != nil {
		result := fmt.Sprintf("error: %v", err)
		log.InfofCtx(ctx, "[agent] tool %s error after %s: args=%s err=%v", name, duration, platform.TruncateForLog(argSummary(args), 400), err)
		return ToolExecution{AuthoritativeContent: result, PromptContent: result, Failed: true, DurationMs: int(duration / time.Millisecond)}
	}
	result := toolResult.Content
	if seen != nil {
		seen[fp] = true
	}

	log.InfofCtx(ctx, "[agent] tool %s ok in %s (%d chars): args=%s result=%s", name, duration, len(result),
		platform.TruncateForLog(argSummary(args), 600), platform.TruncateForLog(result, 1200))
	execution := ToolExecution{
		AuthoritativeContent: result,
		PromptContent:        result,
		References:           cloneReferences(toolResult.References),
		EvidenceUnits:        cloneEvidenceUnits(toolResult.EvidenceUnits),
		Evidence:             true,
		Coverage:             toolResult.Coverage,
		AnswerContract:       toolResult.AnswerContract,
		DurationMs:           int(duration / time.Millisecond),
	}
	return execution
}

func cloneReferences(refs []tool.Reference) []tool.Reference {
	if len(refs) == 0 {
		return nil
	}
	out := make([]tool.Reference, len(refs))
	copy(out, refs)
	return out
}

// mergeToolReferences dedups references by (type, target) so repeated tool hits
// never inflate the final reference set.
func mergeToolReferences(dst *[]tool.Reference, refs []tool.Reference) {
	if len(refs) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(*dst)+len(refs))
	for _, existing := range *dst {
		seen[string(existing.Type)+"\x00"+existing.Target] = struct{}{}
	}
	for _, ref := range refs {
		key := string(ref.Type) + "\x00" + ref.Target
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		*dst = append(*dst, ref)
	}
}

func referenceTypeIndex(context *retrieval.RetrievedContext) map[string]tool.ReferenceType {
	if context == nil || len(context.References) == 0 {
		return nil
	}
	index := make(map[string]tool.ReferenceType, len(context.References))
	for _, reference := range context.References {
		referenceType := tool.ReferenceType(reference.Type)
		switch referenceType {
		case tool.ReferenceRunbook, tool.ReferenceService, tool.ReferenceSymbol:
			if reference.Target != "" {
				index[reference.Target] = referenceType
			}
		}
	}
	return index
}

func referenceMismatch(snapshot tool.Snapshot, candidate tool.Tool, args tool.Arguments, references map[string]tool.ReferenceType) string {
	if len(references) == 0 || len(candidate.ReferenceInputs) == 0 {
		return ""
	}
	for _, input := range candidate.ReferenceInputs {
		value := args.String(input.Argument)
		if value == "" {
			continue
		}
		for entity, actualType := range references {
			if !containsReferenceToken(value, entity) || acceptsReference(input.Accepts, actualType) {
				continue
			}
			candidates := snapshot.CandidateTools(actualType)
			candidateNames := make([]string, len(candidates))
			for i, id := range candidates {
				candidateNames[i] = string(id)
			}
			content, _ := json.Marshal(map[string]any{
				"code": "entity_type_mismatch", "entity": entity,
				"actualType": actualType, "tool": candidate.ID, "candidateTools": candidateNames,
			})
			return string(content)
		}
	}
	return ""
}

func acceptsReference(accepted []tool.ReferenceType, actual tool.ReferenceType) bool {
	for _, candidate := range accepted {
		if candidate == actual {
			return true
		}
	}
	return false
}

func containsReferenceToken(value, entity string) bool {
	for offset := 0; ; {
		position := strings.Index(value[offset:], entity)
		if position < 0 {
			return false
		}
		start := offset + position
		end := start + len(entity)
		if referenceBoundary(value, start-1) && referenceBoundary(value, end) {
			return true
		}
		offset = end
	}
}

func referenceBoundary(value string, index int) bool {
	if index < 0 || index >= len(value) {
		return true
	}
	b := value[index]
	return !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-')
}

func isWebEvidenceTool(name string) bool {
	return name == "web_search" || name == "web_fetch"
}

func extendWebStepLimit(step, current, configured int, attempted, succeeded bool) int {
	if step != current || !attempted || succeeded || current >= configured {
		return current
	}
	return current + 1
}

func extendEvidenceStepLimit(step, current, configured int, produced, alreadyExtended bool) int {
	if step != current || !produced || alreadyExtended || current >= configured {
		return current
	}
	return current + 1
}

const sessionToolArgumentLimit = 8_000

func canonicalSessionToolCalls(calls []llm.ToolCall) []llm.ToolCall {
	out := make([]llm.ToolCall, len(calls))
	for i, call := range calls {
		out[i] = call
		if len(call.Function.Arguments) > sessionToolArgumentLimit {
			out[i].Function.Arguments = `{"_nasuta_omitted":"arguments exceeded session limit"}`
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args == nil {
			// Preserve malformed model output for audit instead of making it look valid.
			continue
		}
		canonical, err := json.Marshal(args)
		if err != nil {
			continue
		}
		out[i].Function.Arguments = string(canonical)
	}
	return out
}

// ExecuteArguments is the non-LLM entry used by trusted prefetch plans.
func (te *ToolExecutor) ExecuteArguments(ctx context.Context, snapshot tool.Snapshot, id tool.ToolID, args tool.Arguments) (tool.Result, error) {
	return te.runtime.Execute(ctx, snapshot, id, args)
}

// ExecuteWithPolicy snapshots current tools for one-shot callers.
func (te *ToolExecutor) ExecuteWithPolicy(ctx context.Context, policy ToolPolicy, call llm.ToolCall, seen map[string]bool) ToolExecution {
	return te.Execute(ctx, te.Snapshot(policy), call, nil, seen)
}

// toolFingerprint builds a stable dedup key (name + canonical JSON args).
func toolFingerprint(name string, args map[string]any) string {
	canonical, err := json.Marshal(args)
	if err != nil {
		canonical = []byte(fmt.Sprintf("%v", args))
	}
	return name + "|" + string(canonical)
}

func toolMessage(toolCallID, name, content string) llm.Message {
	return llm.Message{
		Role:       "tool",
		ToolCallID: toolCallID,
		Name:       name,
		Content:    content,
	}
}

// parseArgs decodes a tool call's JSON arguments.
func parseArgs(ctx context.Context, arguments string) (tool.Arguments, error) {
	args := tool.Arguments{}
	s := strings.TrimSpace(arguments)
	if s == "" {
		return args, nil
	}
	if err := json.Unmarshal([]byte(s), &args); err != nil {
		log.InfofCtx(ctx, "[agent] malformed tool args %q: %v", arguments, err)
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	if args == nil {
		return nil, fmt.Errorf("invalid tool arguments: expected a JSON object")
	}
	return args, nil
}

// argSummary renders tool args as a compact "k=v,..." log string.
func argSummary(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	var sb strings.Builder
	first := true
	for k, v := range args {
		if !first {
			sb.WriteString(", ")
		}
		first = false
		sb.WriteString(k)
		sb.WriteString("=")
		b, err := json.Marshal(v)
		if err != nil {
			fmt.Fprintf(&sb, "%v", v)
		} else {
			sb.Write(b)
		}
	}
	return sb.String()
}

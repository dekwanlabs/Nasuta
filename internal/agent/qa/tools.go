package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

func routingCandidates(tools []tool.Tool) []retrieval.ToolRouteCandidate {
	candidates := make([]retrieval.ToolRouteCandidate, 0, len(tools))
	for _, candidate := range tools {
		if candidate.Kind != tool.KindRead || candidate.Routing == nil {
			continue
		}
		candidates = append(candidates, retrieval.ToolRouteCandidate{
			ID: string(candidate.ID), Intent: candidate.Routing.Intent,
			Temporal:       candidate.Routing.Temporal,
			EvidenceSource: string(candidate.Routing.EvidenceSource),
		})
	}
	return candidates
}

func scenarioToolIDs(tools []tool.Tool) []string {
	ids := make([]string, 0, len(tools))
	for _, candidate := range tools {
		ids = append(ids, string(candidate.ID))
	}
	return ids
}

func scenarioToolsContain(prepared ScenarioToolSet, id tool.ToolID) bool {
	if prepared == nil {
		return false
	}
	_, ok := prepared.Get(id)
	return ok
}

func (svc *Service) prunedToolIDSet(tools []tool.Tool, routed []string) map[tool.ToolID]struct{} {
	candidates := routingCandidates(tools)
	allowed := baseToolIDSet(tools, candidates)
	for id := range pruneAllowance(routed, candidates) {
		allowed[id] = struct{}{}
	}
	return allowed
}

type filteredScenarioTools struct {
	base  ScenarioToolSet
	tools []tool.Tool
	byID  map[tool.ToolID]tool.Tool
}

func (filtered filteredScenarioTools) Tools() []tool.Tool {
	return append([]tool.Tool(nil), filtered.tools...)
}

func (filtered filteredScenarioTools) Get(id tool.ToolID) (tool.Tool, bool) {
	candidate, ok := filtered.byID[id]
	return candidate, ok
}

func (filtered filteredScenarioTools) Execute(ctx context.Context, id tool.ToolID, arguments tool.Arguments) (tool.Result, error) {
	if _, ok := filtered.byID[id]; !ok {
		return tool.Result{}, fmt.Errorf("tool %q is outside the prepared scenario tools", id)
	}
	return filtered.base.Execute(ctx, id, arguments)
}

func withoutScenarioTool(prepared ScenarioToolSet, excluded tool.ToolID) ScenarioToolSet {
	return withoutScenarioTools(prepared, excluded)
}

func withoutHistoryTools(prepared ScenarioToolSet) ScenarioToolSet {
	return withoutScenarioTools(prepared, "get_turn", "find_turns")
}

func withoutScenarioTools(
	prepared ScenarioToolSet,
	excluded ...tool.ToolID,
) ScenarioToolSet {
	if prepared == nil || len(excluded) == 0 {
		return prepared
	}
	excludedSet := make(map[tool.ToolID]struct{}, len(excluded))
	for _, id := range excluded {
		excludedSet[id] = struct{}{}
	}
	tools := prepared.Tools()
	filtered := filteredScenarioTools{
		base: prepared, tools: make([]tool.Tool, 0, len(tools)),
		byID: make(map[tool.ToolID]tool.Tool, len(tools)),
	}
	for _, candidate := range tools {
		if _, remove := excludedSet[candidate.ID]; remove {
			continue
		}
		filtered.tools = append(filtered.tools, candidate)
		filtered.byID[candidate.ID] = candidate
	}
	return filtered
}

func preferenceInstruction(ids []string, delegationActive bool) string {
	return prompts.MustRender(prompts.AgentPreferredTool, struct {
		ToolIDs          string
		DelegationActive bool
	}{ToolIDs: strings.Join(ids, ", "), DelegationActive: delegationActive})
}

func parentDelegationInstruction(prepared *preparation) string {
	if prepared == nil ||
		prepared.execution.RouteReason != routeReasonParentDynamicDelegation ||
		!scenarioToolsContain(prepared.candidateToolSet, "delegate_investigation") {
		return ""
	}
	return prompts.Text(prompts.AgentQAParentDelegation)
}

func (svc *Service) executePrefetch(
	ctx context.Context,
	runID string,
	prepared ScenarioToolSet,
	plan ToolPlan,
	stepRecorder preparationStepRecorder,
) ([]ContextBlock, error) {
	if len(plan.Prefetch) == 0 {
		return nil, nil
	}
	blocks := make([]ContextBlock, 0, len(plan.Prefetch))
	for index, call := range plan.Prefetch {
		args, err := marshalPrefetchArgs(call)
		if err != nil {
			return nil, err
		}
		callID := prefetchToolCallID(runID, index, call.ToolID)
		startedAt := time.Now()
		if err := recordPrefetchStep(ctx, stepRecorder, run.StepRecord{
			Kind:       run.StepKindToolCall,
			ToolCallID: callID,
			Tool:       string(call.ToolID),
			Args:       string(args),
			CreatedAt:  startedAt,
		}); err != nil {
			return nil, fmt.Errorf("record prefetch tool %q call: %w", call.ToolID, err)
		}
		block, err := executePrefetchCall(
			ctx, runID, prepared, call, args, callID, startedAt, stepRecorder,
		)
		if err != nil {
			return nil, err
		}
		if block != nil {
			blocks = append(blocks, *block)
		}
	}
	return blocks, nil
}

func marshalPrefetchArgs(call PlannedToolCall) ([]byte, error) {
	args, err := json.Marshal(call.Arguments)
	if err != nil {
		return nil, fmt.Errorf("marshal prefetch tool %q arguments: %w", call.ToolID, err)
	}
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	return args, nil
}

func executePrefetchCall(
	ctx context.Context,
	runID string,
	prepared ScenarioToolSet,
	call PlannedToolCall,
	args []byte,
	callID string,
	startedAt time.Time,
	stepRecorder preparationStepRecorder,
) (*ContextBlock, error) {
	candidate, ok := prepared.Get(call.ToolID)
	if !ok {
		failure := fmt.Errorf("tool is unavailable")
		if err := recordPrefetchResult(
			ctx, stepRecorder, runID, callID, call.ToolID, string(args),
			tool.Result{}, failure, "tool_unavailable", startedAt,
		); err != nil {
			return nil, err
		}
		if call.Required {
			return nil, fmt.Errorf("required prefetch tool %q is unavailable", call.ToolID)
		}
		block := unavailableToolBlock(call.ToolID, "tool is unavailable")
		return &block, nil
	}
	if candidate.Kind != tool.KindRead || candidate.Prefetch == nil {
		failure := fmt.Errorf("tool is not eligible for prefetch")
		if err := recordPrefetchResult(
			ctx, stepRecorder, runID, callID, call.ToolID, string(args),
			tool.Result{}, failure, "tool_not_prefetch_eligible", startedAt,
		); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("prefetch tool %q is not eligible", call.ToolID)
	}
	result, err := prepared.Execute(ctx, call.ToolID, call.Arguments)
	if err != nil {
		if recordErr := recordPrefetchResult(
			ctx, stepRecorder, runID, callID, call.ToolID, string(args),
			result, err, "tool_execution_failed", startedAt,
		); recordErr != nil {
			return nil, recordErr
		}
		if call.Required {
			return nil, fmt.Errorf("required prefetch tool %q: %w", call.ToolID, err)
		}
		block := unavailableToolBlock(call.ToolID, err.Error())
		return &block, nil
	}
	if err := recordPrefetchResult(
		ctx, stepRecorder, runID, callID, call.ToolID, string(args),
		result, nil, "", startedAt,
	); err != nil {
		return nil, err
	}
	references := make([]retrieval.Reference, 0, len(result.References))
	for _, ref := range result.References {
		references = append(references, retrieval.Reference{
			Type: string(ref.Type), Label: ref.Label, Target: ref.Target,
		})
	}
	return &ContextBlock{
		Source: string(call.ToolID), Title: candidate.Description,
		Content: result.Content, References: references,
		Evidence: cloneEvidenceUnits(result.EvidenceUnits),
	}, nil
}

func recordPrefetchStep(
	ctx context.Context,
	recorder preparationStepRecorder,
	step run.StepRecord,
) error {
	if recorder == nil {
		return nil
	}
	return recorder.RecordStep(ctx, step)
}

func recordPrefetchResult(
	ctx context.Context,
	recorder preparationStepRecorder,
	runID, callID string,
	toolID tool.ToolID,
	args string,
	result tool.Result,
	executionErr error,
	deliveryError string,
	startedAt time.Time,
) error {
	if recorder == nil {
		return nil
	}
	content := result.Content
	if executionErr != nil {
		content = "error: " + executionErr.Error()
	}
	step := run.StepRecord{
		Kind:                run.StepKindToolResult,
		TraceID:             prefetchTraceID(runID, callID),
		ToolCallID:          callID,
		Tool:                string(toolID),
		Args:                args,
		Content:             content,
		PromptContent:       content,
		AuthoritativeSHA256: hashString(content),
		PromptSHA256:        hashString(content),
		SizeBytes:           int64(len(content)),
		Coverage:            result.Coverage,
		AnswerContract:      result.AnswerContract,
		Failed:              executionErr != nil,
		DeliveryError:       deliveryError,
		DurationMs:          int(time.Since(startedAt) / time.Millisecond),
		CreatedAt:           time.Now(),
	}
	if err := recorder.RecordStep(ctx, step); err != nil {
		return fmt.Errorf("record prefetch tool %q result: %w", toolID, err)
	}
	return nil
}

func prefetchToolCallID(runID string, index int, toolID tool.ToolID) string {
	seed := fmt.Sprintf("prefetch_tool_call\x00%s\x00%d\x00%s", runID, index, toolID)
	return "call_" + platform.UUIDFromString(seed)
}

func prefetchTraceID(runID, callID string) string {
	return "trc_" + platform.UUIDFromString("prefetch_tool_result\x00"+runID+"\x00"+callID)
}

func unavailableToolBlock(id tool.ToolID, reason string) ContextBlock {
	return ContextBlock{
		Source:  string(id),
		Title:   string(id) + " unavailable",
		Content: "The prefetch could not be completed: " + reason,
	}
}

const preloadedContextBudget = 16000

func (svc *Service) contextBudget() int {
	if svc.retriever != nil {
		return svc.retriever.ContextBudget()
	}
	return 48000
}

type preloadedMergeState struct {
	context       *retrieval.RetrievedContext
	blocks        []ContextBlock
	totalBudget   int
	seenContent   map[string]struct{}
	seenRefs      map[string]struct{}
	evidence      *evidence.Ledger
	conflicts     []evidence.Conflict
	preloadedText strings.Builder
	spentRunes    int
}

func mergePreloadedContext(context *retrieval.RetrievedContext, blocks []ContextBlock, totalBudget int) {
	if context == nil || len(blocks) == 0 {
		return
	}
	if totalBudget <= 0 {
		totalBudget = 48000
	}
	state := newPreloadedMergeState(context, blocks, totalBudget)
	state.fillPreloadedBlocks()
	if state.preloadedText.Len() == 0 {
		return
	}
	existingComplete := state.appendExistingContext()
	state.finalize(existingComplete)
}

func newPreloadedMergeState(context *retrieval.RetrievedContext, blocks []ContextBlock, totalBudget int) *preloadedMergeState {
	seenRefs := make(map[string]struct{}, len(context.References)+len(blocks))
	for _, ref := range context.References {
		seenRefs[ref.Type+"\x00"+ref.Target] = struct{}{}
	}
	return &preloadedMergeState{
		context:     context,
		blocks:      blocks,
		totalBudget: totalBudget,
		seenContent: make(map[string]struct{}, len(blocks)),
		seenRefs:    seenRefs,
		evidence:    evidence.New(nil, ""),
	}
}

func (state *preloadedMergeState) fillPreloadedBlocks() {
	preloadedLimit := min(preloadedContextBudget, state.totalBudget)
	budget := preloadedLimit
	for _, block := range state.blocks {
		content := strings.TrimSpace(block.Content)
		if content == "" {
			continue
		}
		if _, duplicate := state.seenContent[content]; duplicate {
			continue
		}
		state.seenContent[content] = struct{}{}
		delivered, truncated, spent := state.appendPreloadedSection(block, content, budget)
		budget = spent
		state.collectPreloadedReferences(block)
		state.conflicts = appendEvidenceConflicts(
			state.conflicts,
			state.evidence.Add(
				deliveredEvidenceUnits(block.Evidence, delivered, truncated),
				"preload",
			),
		)
		if budget == 0 {
			break
		}
	}
	_ = preloadedLimit
}

func (state *preloadedMergeState) appendPreloadedSection(block ContextBlock, content string, budget int) (string, bool, int) {
	title := strings.TrimSpace(block.Title)
	if title == "" {
		title = strings.TrimSpace(block.Source)
	}
	if title == "" {
		title = "Preloaded Context"
	}
	section := "## " + title + "\n" + content + "\n"
	runes := []rune(section)
	truncated := len(runes) > budget
	if len(runes) > budget {
		runes = runes[:budget]
	}
	delivered := string(runes)
	state.preloadedText.WriteString(delivered)
	state.spentRunes += len(runes)
	return delivered, truncated, budget - len(runes)
}

func (state *preloadedMergeState) collectPreloadedReferences(block ContextBlock) {
	for _, ref := range block.References {
		if ref.Target == "" {
			continue
		}
		key := ref.Type + "\x00" + ref.Target
		if _, duplicate := state.seenRefs[key]; duplicate {
			continue
		}
		state.seenRefs[key] = struct{}{}
		state.context.References = append(state.context.References, ref)
	}
}

func (state *preloadedMergeState) appendExistingContext() bool {
	existingComplete := true
	if state.context.Text == "" {
		return existingComplete
	}
	preloadedLimit := min(preloadedContextBudget, state.totalBudget)
	remaining := state.totalBudget - (preloadedLimit - state.consumedPreloadedBudget())
	if remaining > 0 {
		state.preloadedText.WriteString("\n")
		remaining--
	}
	if remaining > 0 {
		existing := []rune(state.context.Text)
		if len(existing) > remaining {
			existing = existing[:remaining]
			existingComplete = false
		}
		state.preloadedText.WriteString(string(existing))
	} else {
		existingComplete = false
	}
	return existingComplete
}

func (state *preloadedMergeState) consumedPreloadedBudget() int {
	return state.spentRunes
}

func (state *preloadedMergeState) finalize(existingComplete bool) {
	state.context.Text = state.preloadedText.String()
	mergedEvidence := evidence.New(nil, "")
	conflicts := appendEvidenceConflicts(nil, state.context.EvidenceConflicts)
	if existingComplete {
		mergedEvidence.Add(state.context.EvidenceUnits, "retrieval")
	}
	mergedEvidence.RememberConflicts(conflicts)
	mergedEvidence.RememberConflicts(state.conflicts)
	conflicts = appendEvidenceConflicts(conflicts, state.conflicts)
	conflicts = appendEvidenceConflicts(
		conflicts,
		mergedEvidence.Add(state.evidence.Units(), "preload"),
	)
	state.context.EvidenceUnits = mergedEvidence.Units()
	state.context.EvidenceConflicts = evidence.CloneConflicts(conflicts)
	state.context.HitCount = len(state.context.References)
}

func appendEvidenceConflicts(
	target []evidence.Conflict,
	incoming []evidence.Conflict,
) []evidence.Conflict {
	if len(incoming) == 0 {
		return target
	}
	seen := make(map[string]struct{}, len(target)+len(incoming))
	for _, conflict := range target {
		seen[evidence.ConflictFingerprint(conflict)] = struct{}{}
	}
	for _, conflict := range incoming {
		fingerprint := evidence.ConflictFingerprint(conflict)
		if _, duplicate := seen[fingerprint]; duplicate {
			continue
		}
		seen[fingerprint] = struct{}{}
		target = append(target, evidence.CloneConflict(conflict))
	}
	return target
}

func deliveredEvidenceUnits(
	units []tool.EvidenceUnit,
	delivered string,
	truncated bool,
) []tool.EvidenceUnit {
	deliveredUnits := evidence.CloneUnits(units)
	tokenCost := tooloutput.EstimateTokens(delivered)
	for index := range deliveredUnits {
		unit := &deliveredUnits[index]
		if truncated {
			unit.Coverage.Complete = false
			unit.Coverage.Partial = true
		}
		unit.TokenCost = tokenCost
	}
	return deliveredUnits
}

func canonicalRetrievalQuery(cleanQuestion, contextTerms string) string {
	q := strings.TrimSpace(cleanQuestion)
	terms := strings.TrimSpace(contextTerms)
	if terms != "" {
		return q + " " + terms
	}
	return q
}

func appendUnavailableWeb(rc *retrieval.RetrievedContext, unavailable bool) {
	if !unavailable || rc == nil {
		return
	}
	if rc.Text != "" {
		rc.Text += "\n\n"
	}
	rc.Text += "## Evidence Availability\n- Web source unavailable: web search is not configured.\n"
}

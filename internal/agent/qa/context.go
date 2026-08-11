package qa

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

const (
	activeHistoryTopK      = 4
	activeHistoryMaxTokens = 32_000
)

var explicitHistoryRefPattern = regexp.MustCompile(`(?i)(?:\b(?:turn|run)[-_: #]?[a-z0-9-]+\b|第[[:space:]]*[0-9]+[[:space:]]*轮)`)
var selectionReferencePattern = regexp.MustCompile(`(?i)^[[:space:]]*(?:#?[0-9]{1,2}|第[[:space:]]*(?:[0-9]{1,2}|[一二三四五六七八九十两]+)[[:space:]]*(?:个|项|条|种|组|份|类|位|套)?|(?:选|选择)[[:space:]]*(?:第[[:space:]]*)?(?:[0-9]{1,2}|[一二三四五六七八九十两]+)[[:space:]]*(?:个|项|条|种|组|份|类|位|套)?)[[:space:]]*[。.!！]?[[:space:]]*$`)

type contextAssembleStats struct {
	Relation            retrieval.HistoryRelation
	RelationOrigin      string
	UpgradeReason       string
	CandidateCount      int
	SelectedCount       int
	FullTurnCount       int
	DetailCount         int
	ReferenceCount      int
	OmittedCount        int
	HistoryBudgetTokens int
	HistoryUsedTokens   int
	SelectedTurnNumbers []int
	SelectedReasons     []string
}

type contextAssembleInput struct {
	Question      string
	UserID        int64
	Conversation  ConversationContext
	Relation      retrieval.HistoryRelation
	Origin        string
	Upgrade       string
	Candidates    *HistoryCandidates
	ContextWindow int
	OutputReserve int
}

type contextAssembleOutput struct {
	Conversation ConversationContext
	Stats        contextAssembleStats
}

var contextAssembleSpec = runtrace.Spec[contextAssembleInput, contextAssembleOutput]{
	Operation: "agent.context_assemble",
	Node:      "context_assemble",
	Output: func(_ contextAssembleInput, output contextAssembleOutput, err error) map[string]any {
		stats := output.Stats
		return map[string]any{
			"topic_affinity": stats.Relation.TopicAffinity, "confidence": stats.Relation.Confidence,
			"relation_origin":        stats.RelationOrigin,
			"needs_prior_entities":   stats.Relation.NeedsPriorEntities,
			"needs_prior_conclusion": stats.Relation.NeedsPriorConclusion,
			"needs_prior_evidence":   stats.Relation.NeedsPriorEvidence,
			"dependency_upgrade":     stats.UpgradeReason, "candidate_turns": stats.CandidateCount,
			"selected_turns": stats.SelectedCount, "full_turns": stats.FullTurnCount,
			"detail_turns": stats.DetailCount, "reference_turns": stats.ReferenceCount,
			"omitted_turns": stats.OmittedCount, "history_budget_tokens": stats.HistoryBudgetTokens,
			"history_used_tokens":   stats.HistoryUsedTokens,
			"selected_turn_numbers": stats.SelectedTurnNumbers, "selected_reasons": stats.SelectedReasons,
		}
	},
	Status: func(output contextAssembleOutput, err error) string {
		if err == nil && output.Stats.RelationOrigin == "deterministic" {
			return "degraded"
		}
		return ""
	},
}

func (svc *QA) assembleContext(ctx context.Context, input contextAssembleInput) (contextAssembleOutput, error) {
	return runtrace.Invoke(ctx, contextAssembleSpec, input, func(ctx context.Context, input contextAssembleInput) (contextAssembleOutput, error) {
		contextWindow, outputReserve := svc.contextLimits(
			input.ContextWindow, input.OutputReserve,
		)
		conversation, stats, err := svc.assembleActiveHistory(
			ctx, input.Question, input.UserID, input.Conversation, input.Relation, input.Origin, input.Upgrade,
			contextWindow, outputReserve,
		)
		output := contextAssembleOutput{Conversation: conversation, Stats: stats}
		if err != nil {
			return output, err
		}
		continuity := ""
		if len(conversation.RecentTurns) > 0 &&
			(input.Relation.NeedsPriorEntities || input.Relation.NeedsPriorConclusion || input.Relation.NeedsPriorEvidence) {
			continuity = conversation.RecentTurns[0].Question
		}
		if svc.history == nil || conversation.CompactedThroughTurn <= 0 || conversation.SessionID == "" {
			return output, nil
		}
		historyBudget := min(int(float64(contextWindow)*0.08), 32768)
		var recalled string
		var recallErr error
		materialized := false
		if input.Candidates != nil && !historyNeedsContinuity(input.Relation) {
			if discovery, ok := svc.history.(CandidateDiscovery); ok {
				recalled, recallErr = discovery.Materialize(
					ctx, input.UserID, conversation.SessionID, *input.Candidates,
					activeHistoryTopK, historyBudget, true,
				)
				materialized = true
			}
		}
		if !materialized {
			recalled, recallErr = svc.history.Recall(
				ctx, input.UserID, conversation.SessionID, input.Question, continuity, historyBudget,
			)
		}
		if recallErr != nil {
			return output, fmt.Errorf("recall current session history: %w", recallErr)
		}
		output.Conversation.RetrievedHistory = recalled
		return output, nil
	})
}

func (svc *QA) contextLimits(contextWindow, outputReserve int) (int, int) {
	if contextWindow <= 0 {
		contextWindow = svc.contextWindow
	}
	if outputReserve <= 0 {
		outputReserve = svc.outputReserve
	}
	return contextWindow, outputReserve
}

type scoredTurn struct {
	metadata memory.TurnMetadata
	score    float64
	reason   string
}

type turnMinHeap []scoredTurn

func (items turnMinHeap) Len() int           { return len(items) }
func (items turnMinHeap) Less(i, j int) bool { return items[i].score < items[j].score }
func (items turnMinHeap) Swap(i, j int)      { items[i], items[j] = items[j], items[i] }
func (items *turnMinHeap) Push(value any)    { *items = append(*items, value.(scoredTurn)) }
func (items *turnMinHeap) Pop() any {
	old := *items
	last := old[len(old)-1]
	*items = old[:len(old)-1]
	return last
}

type historicalTurn struct {
	TurnNumber     int                     `json:"turn"`
	RunID          string                  `json:"run_id,omitempty"`
	Representation string                  `json:"representation"`
	Reason         string                  `json:"reason"`
	Question       string                  `json:"question,omitempty"`
	TopicKey       string                  `json:"topic_key,omitempty"`
	Entities       []string                `json:"entities,omitempty"`
	Evidence       memory.EvidenceManifest `json:"evidence_manifest"`
	EvidenceStatus string                  `json:"evidence_status,omitempty"`
	Forced         bool                    `json:"forced_conclusion,omitempty"`
	Detail         json.RawMessage         `json:"detail,omitempty"`
}

type historicalContextEnvelope struct {
	Label string           `json:"label"`
	Turns []historicalTurn `json:"turns"`
}

func buildHistoryRouteContext(conversation ConversationContext) string {
	if len(conversation.RecentTurns) == 0 && len(conversation.RecentDialogue) == 0 {
		return ""
	}
	payload := struct {
		SessionTitle   string                      `json:"session_title,omitempty"`
		PreviousTurn   *memory.TurnMetadata        `json:"previous_turn,omitempty"`
		RecentDialogue []memory.RecentDialogueTurn `json:"recent_dialogue,omitempty"`
	}{
		SessionTitle:   conversation.SessionTitle,
		RecentDialogue: conversation.RecentDialogue,
	}
	if len(conversation.RecentTurns) > 0 {
		payload.PreviousTurn = &conversation.RecentTurns[0]
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(raw)
}

func resolveHistoryRelation(question string, recent []memory.TurnMetadata, model retrieval.HistoryRelation, modelValid bool) (retrieval.HistoryRelation, string, string) {
	if len(recent) == 0 {
		return retrieval.HistoryRelation{}, "none", ""
	}
	_, currentEntities, currentTerms := memory.CanonicalQuestionMetadata(question)
	latest := recent[0]
	localAffinity, conflict := turnAffinity(currentEntities, currentTerms, latest, 0, len(recent))
	relation := model
	origin := "model"
	upgrade := ""
	if !modelValid {
		relation = retrieval.HistoryRelation{
			TopicAffinity:    localAffinity,
			Confidence:       0.5,
			ExplicitTurnRefs: explicitHistoryRefPattern.FindAllString(question, 4),
		}
		origin = "deterministic"
	}
	pronoun := containsAnyFold(question, []string{
		"这个", "那个", "它", "该", "上述", "前面", "刚才", "继续", "然后", "呢", "this", "that", "it", "previous", "continue",
	})
	evidenceReference := containsAnyFold(question, []string{
		"证据", "结果", "日志", "请求", "响应", "报错", "错误", "trace", "request", "response", "message", "result", "evidence",
	})
	if selectionReferencePattern.MatchString(question) {
		relation.NeedsPriorConclusion = true
		relation.NeedsPriorEntities = true
		upgrade = "selection_reference"
	}
	if pronoun && !conflict && !relation.NeedsPriorEntities {
		relation.NeedsPriorEntities = true
		upgrade = "unresolved_reference"
	}
	if pronoun && evidenceReference && !conflict && latest.EvidenceManifest.Status != "none" {
		relation.NeedsPriorEvidence = true
		relation.NeedsPriorConclusion = true
		relation.NeedsPriorEntities = true
		upgrade = "reference_requires_evidence"
	}
	if relation.NeedsPriorEvidence {
		relation.NeedsPriorConclusion = true
		relation.NeedsPriorEntities = true
	} else if relation.NeedsPriorConclusion {
		relation.NeedsPriorEntities = true
	}
	if !modelValid || relation.TopicAffinity == 0 {
		relation.TopicAffinity = localAffinity
	}
	if conflict && len(relation.ExplicitTurnRefs) == 0 && !pronoun {
		relation.TopicAffinity *= 0.25
	}
	return relation, origin, upgrade
}

func (svc *QA) assembleActiveHistory(
	ctx context.Context,
	question string,
	userID int64,
	conversation ConversationContext,
	relation retrieval.HistoryRelation,
	origin string,
	upgrade string,
	contextWindow int,
	outputReserve int,
) (ConversationContext, contextAssembleStats, error) {
	stats := contextAssembleStats{
		Relation: relation, RelationOrigin: origin, UpgradeReason: upgrade,
		CandidateCount: len(conversation.RecentTurns),
	}
	if len(conversation.RecentTurns) == 0 {
		return conversation, stats, nil
	}
	selected := selectActiveTurns(question, conversation.RecentTurns, relation)
	stats.SelectedCount = len(selected)
	if len(selected) == 0 {
		conversation.Recent = nil
		return conversation, stats, nil
	}
	latestTurn := conversation.RecentTurns[0].TurnNumber
	latestHasAnswer := recentDialogueHasAssistant(conversation.RecentDialogue, latestTurn)
	turnNumbers := make([]int, 0, len(selected))
	for _, item := range selected {
		metadata := item.metadata
		fullPreferred := explicitTurnSelected(metadata, relation.ExplicitTurnRefs) ||
			metadata.TurnNumber == latestTurn && relation.NeedsPriorEvidence
		needsDetail := fullPreferred || metadata.TurnNumber == latestTurn &&
			relation.NeedsPriorConclusion && !latestHasAnswer
		if needsDetail {
			turnNumbers = append(turnNumbers, metadata.TurnNumber)
		}
	}
	turnByNumber := make(map[int][]llm.Message, len(turnNumbers))
	if len(turnNumbers) > 0 {
		if svc.sessions == nil || conversation.SessionID == "" {
			return ConversationContext{}, stats, fmt.Errorf("active history detail selected without a session store")
		}
		turns, err := svc.sessions.LoadTurns(conversation.SessionID, userID, turnNumbers)
		if err != nil {
			return ConversationContext{}, stats, fmt.Errorf("load selected active history: %w", err)
		}
		for _, turn := range turns {
			turnByNumber[turn.TurnNumber] = turn.Messages
		}
	}
	_ = ctx
	budget := activeHistoryMaxTokens
	if contextWindow > 0 {
		safety := max(contextWindow/20, 1024)
		budget = min(activeHistoryMaxTokens, max(0, contextWindow-outputReserve-safety)/4)
	}
	stats.HistoryBudgetTokens = budget
	historical := make([]historicalTurn, 0, len(selected))
	conversation.Recent = nil
	for _, item := range selected {
		metadata := item.metadata
		messages := turnByNumber[metadata.TurnNumber]
		remaining := budget - stats.HistoryUsedTokens
		fullPreferred := explicitTurnSelected(metadata, relation.ExplicitTurnRefs) ||
			metadata.TurnNumber == latestTurn && relation.NeedsPriorEvidence
		if fullPreferred {
			atomic := replayableTailMessages(messages, 0)
			cost := estimateMessagesTokens(atomic)
			if len(atomic) == len(messages) && cost <= remaining {
				conversation.Recent = append(conversation.Recent, atomic...)
				stats.HistoryUsedTokens += cost
				stats.FullTurnCount++
				stats.SelectedTurnNumbers = append(stats.SelectedTurnNumbers, metadata.TurnNumber)
				stats.SelectedReasons = append(stats.SelectedReasons, item.reason+":full")
				continue
			}
		}
		needsDetail := fullPreferred || metadata.TurnNumber == latestTurn &&
			relation.NeedsPriorConclusion && !latestHasAnswer
		if needsDetail {
			detail, detailErr := compressTurnDetail(metadata.TurnNumber, messages)
			if detailErr == nil {
				cost := tooloutput.EstimateTokens(string(detail))
				if cost <= remaining {
					historical = append(historical, historicalTurn{
						TurnNumber: metadata.TurnNumber, RunID: metadata.RunID, Representation: "detail",
						Reason: item.reason, Evidence: metadata.EvidenceManifest,
						EvidenceStatus: metadata.EvidenceStatus, Forced: metadata.ForcedConclusion, Detail: detail,
					})
					stats.HistoryUsedTokens += cost
					stats.DetailCount++
					stats.SelectedTurnNumbers = append(stats.SelectedTurnNumbers, metadata.TurnNumber)
					stats.SelectedReasons = append(stats.SelectedReasons, item.reason+":detail")
					continue
				}
			}
		}
		reference := historicalTurn{
			TurnNumber: metadata.TurnNumber, RunID: metadata.RunID, Representation: "reference",
			Reason: item.reason, Question: metadata.Question, TopicKey: metadata.TopicKey,
			Entities: metadata.Entities, Evidence: metadata.EvidenceManifest,
			EvidenceStatus: metadata.EvidenceStatus, Forced: metadata.ForcedConclusion,
		}
		raw, marshalErr := json.Marshal(reference)
		cost := tooloutput.EstimateTokens(string(raw))
		if marshalErr == nil && cost <= remaining {
			historical = append(historical, reference)
			stats.HistoryUsedTokens += cost
			stats.ReferenceCount++
			stats.SelectedTurnNumbers = append(stats.SelectedTurnNumbers, metadata.TurnNumber)
			stats.SelectedReasons = append(stats.SelectedReasons, item.reason+":reference")
			continue
		}
		stats.OmittedCount++
	}
	if len(historical) > 0 {
		raw, err := json.Marshal(historicalContextEnvelope{Label: "HISTORICAL_CONTEXT", Turns: historical})
		if err != nil {
			return ConversationContext{}, stats, fmt.Errorf("encode historical context: %w", err)
		}
		conversation.HistoricalContext = string(raw)
	}
	return conversation, stats, nil
}

func recentDialogueHasAssistant(dialogue []memory.RecentDialogueTurn, turnNumber int) bool {
	for _, turn := range dialogue {
		if turn.TurnNumber == turnNumber {
			return strings.TrimSpace(turn.Assistant) != ""
		}
	}
	return false
}

func selectActiveTurns(question string, candidates []memory.TurnMetadata, relation retrieval.HistoryRelation) []scoredTurn {
	_, currentEntities, currentTerms := memory.CanonicalQuestionMetadata(question)
	mandatory := make(map[int]scoredTurn, len(relation.ExplicitTurnRefs)+1)
	if len(candidates) > 0 && (relation.NeedsPriorEntities || relation.NeedsPriorConclusion || relation.NeedsPriorEvidence) {
		mandatory[candidates[0].TurnNumber] = scoredTurn{metadata: candidates[0], score: 2, reason: "prior_dependency"}
	}
	for _, candidate := range candidates {
		if explicitTurnSelected(candidate, relation.ExplicitTurnRefs) {
			mandatory[candidate.TurnNumber] = scoredTurn{metadata: candidate, score: 3, reason: "explicit_reference"}
		}
	}
	slots := max(0, activeHistoryTopK-len(mandatory))
	top := make(turnMinHeap, 0, slots)
	for index, candidate := range candidates {
		if _, required := mandatory[candidate.TurnNumber]; required {
			continue
		}
		score, conflict := turnAffinity(currentEntities, currentTerms, candidate, index, len(candidates))
		if index == 0 {
			score = (score + relation.TopicAffinity) / 2
		}
		if conflict || score <= 0 || slots == 0 {
			continue
		}
		item := scoredTurn{metadata: candidate, score: score, reason: "continuous_affinity"}
		if top.Len() < slots {
			heap.Push(&top, item)
		} else if score > top[0].score {
			heap.Pop(&top)
			heap.Push(&top, item)
		}
	}
	selected := make([]scoredTurn, 0, len(mandatory)+top.Len())
	for _, item := range mandatory {
		selected = append(selected, item)
	}
	for top.Len() > 0 {
		selected = append(selected, heap.Pop(&top).(scoredTurn))
	}
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].metadata.TurnNumber < selected[j].metadata.TurnNumber
	})
	return selected
}

func turnAffinity(currentEntities, currentTerms []string, candidate memory.TurnMetadata, index, count int) (float64, bool) {
	entityOverlap := overlapRatio(currentEntities, candidate.Entities)
	termOverlap := overlapRatio(currentTerms, candidate.QuestionTerms)
	topicTerms := strings.Fields(candidate.TopicKey)
	topicOverlap := overlapRatio(append([]string(nil), currentEntities...), topicTerms)
	conflict := len(currentEntities) > 0 && len(candidate.Entities) > 0 && entityOverlap == 0
	if entityOverlap == 0 && termOverlap == 0 && topicOverlap == 0 {
		return 0, conflict
	}
	decay := 1.0
	if count > 1 {
		decay = 1 - float64(index)/float64(count)
	}
	score := entityOverlap*0.45 + termOverlap*0.30 + topicOverlap*0.15 + decay*0.10
	if conflict {
		score *= 0.25
	}
	return min(1, score), conflict
}

func overlapRatio(left, right []string) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(left))
	for _, value := range left {
		set[value] = struct{}{}
	}
	matches := 0
	seen := make(map[string]struct{}, len(right))
	for _, value := range right {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		if _, ok := set[value]; ok {
			matches++
		}
	}
	return float64(matches) / float64(max(len(set), len(seen)))
}

func explicitTurnSelected(metadata memory.TurnMetadata, refs []string) bool {
	turnNumber := strconv.Itoa(metadata.TurnNumber)
	for _, ref := range refs {
		normalized := strings.ToLower(strings.TrimSpace(ref))
		if normalized == strings.ToLower(metadata.RunID) || normalized == "turn-"+turnNumber ||
			normalized == "turn:"+turnNumber || normalized == "turn "+turnNumber {
			return true
		}
		if strings.HasPrefix(normalized, "第") && strings.HasSuffix(normalized, "轮") {
			digits := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(normalized, "第"), "轮"))
			if digits == turnNumber {
				return true
			}
		}
	}
	return false
}

func containsAnyFold(value string, terms []string) bool {
	lower := strings.ToLower(value)
	for _, term := range terms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func estimateMessagesTokens(messages []llm.Message) int {
	total := 0
	for _, message := range messages {
		total += tooloutput.EstimateTokens(message.Content) + 4
		for _, call := range message.ToolCalls {
			total += tooloutput.EstimateTokens(call.Function.Name) + tooloutput.EstimateTokens(call.Function.Arguments) + 4
		}
	}
	return total
}

package agent

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
)

const (
	activeHistoryTopK      = 4
	activeHistoryMaxTokens = 32_000
)

var explicitHistoryRefPattern = regexp.MustCompile(`(?i)(?:\b(?:turn|run)[-_: #]?[a-z0-9-]+\b|第[[:space:]]*[0-9]+[[:space:]]*轮)`)

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
	if len(conversation.RecentTurns) == 0 {
		return ""
	}
	latest := conversation.RecentTurns[0]
	payload := struct {
		SessionTitle string              `json:"session_title,omitempty"`
		PreviousTurn memory.TurnMetadata `json:"previous_turn"`
	}{SessionTitle: conversation.SessionTitle, PreviousTurn: latest}
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

func (svc *QA) assembleActiveHistory(ctx context.Context, question string, userID int64, conversation ConversationContext, relation retrieval.HistoryRelation, origin, upgrade string) (ConversationContext, contextAssembleStats, error) {
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
	if svc.sessions == nil || conversation.SessionID == "" {
		return ConversationContext{}, stats, fmt.Errorf("active history selected without a session store")
	}
	turnNumbers := make([]int, len(selected))
	for i := range selected {
		turnNumbers[i] = selected[i].metadata.TurnNumber
	}
	turns, err := svc.sessions.LoadTurns(conversation.SessionID, userID, turnNumbers)
	if err != nil {
		return ConversationContext{}, stats, fmt.Errorf("load selected active history: %w", err)
	}
	_ = ctx
	turnByNumber := make(map[int][]llm.Message, len(turns))
	for _, turn := range turns {
		turnByNumber[turn.TurnNumber] = turn.Messages
	}
	budget := activeHistoryMaxTokens
	if svc.contextWindow > 0 {
		outputReserve := max(svc.agent.cfg.AnswerMaxTokens, svc.agent.cfg.ConclusionMaxTokens)
		safety := max(svc.contextWindow/20, 1024)
		budget = min(activeHistoryMaxTokens, max(0, svc.contextWindow-outputReserve-safety)/4)
	}
	stats.HistoryBudgetTokens = budget
	historical := make([]historicalTurn, 0, len(selected))
	conversation.Recent = nil
	latestTurn := conversation.RecentTurns[0].TurnNumber
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
		needsDetail := fullPreferred || metadata.TurnNumber == latestTurn && relation.NeedsPriorConclusion
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

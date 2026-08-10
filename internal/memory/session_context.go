package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

const (
	RecentTurnMetadataLimit = 24
	RecentDialogueTurnLimit = 3
	maxContextTurnBatch     = 24
	maxQuestionRunes        = 2048
	maxAssistantAnswerRunes = 12 * 1024
	maxMetadataTerms        = 24
	maxManifestItems        = 12
	maxManifestRefs         = 8
	maxReferenceInputBytes  = 16 * 1024
)

// EvidenceManifest is bounded routing metadata, never a tool payload.
type EvidenceManifest struct {
	Status string                 `json:"status"`
	Items  []EvidenceManifestItem `json:"items"`
}

type EvidenceManifestItem struct {
	Tool       string   `json:"tool"`
	Source     string   `json:"source"`
	References []string `json:"references,omitempty"`
	Coverage   string   `json:"coverage"`
	Omitted    int      `json:"omitted,omitempty"`
}

// TurnMetadata is the narrow online candidate representation.
type TurnMetadata struct {
	TurnNumber       int              `json:"turn"`
	RunID            string           `json:"run_id,omitempty"`
	TokenEstimate    int              `json:"token_estimate"`
	Question         string           `json:"question"`
	TopicKey         string           `json:"topic_key,omitempty"`
	Entities         []string         `json:"entities"`
	QuestionTerms    []string         `json:"question_terms"`
	EvidenceManifest EvidenceManifest `json:"evidence_manifest"`
	EvidenceStatus   string           `json:"evidence_status,omitempty"`
	ForcedConclusion bool             `json:"forced_conclusion,omitempty"`
	CreatedAt        string           `json:"created_at"`
}

// TurnMessages holds one selected atomic turn.
type TurnMessages struct {
	TurnNumber int
	Messages   []llm.Message
}

// RecentDialogueTurn is a bounded user/final-assistant exchange without tool payloads.
type RecentDialogueTurn struct {
	TurnNumber int    `json:"turn"`
	User       string `json:"user"`
	Assistant  string `json:"assistant,omitempty"`
}

func buildTurnMetadata(turnNumber int, runID string, messages []llm.Message, createdAt string) TurnMetadata {
	question := ""
	for _, message := range messages {
		if message.Role == "user" {
			question = truncateRunes(message.Content, maxQuestionRunes)
			break
		}
	}
	terms, entities := canonicalQuestionTerms(question)
	topicParts := entities
	if len(topicParts) == 0 {
		topicParts = terms
	}
	topicKey := strings.Join(topicParts[:min(6, len(topicParts))], " ")
	return TurnMetadata{
		TurnNumber: turnNumber, RunID: runID, TokenEstimate: estimateSessionTokens(messages),
		Question: question, TopicKey: topicKey, Entities: entities, QuestionTerms: terms,
		EvidenceManifest: buildEvidenceManifest(messages), CreatedAt: createdAt,
	}
}

// CanonicalQuestionMetadata applies the same lexical boundary to current and persisted questions.
func CanonicalQuestionMetadata(question string) (string, []string, []string) {
	terms, entities := canonicalQuestionTerms(question)
	parts := entities
	if len(parts) == 0 {
		parts = terms
	}
	return strings.Join(parts[:min(6, len(parts))], " "), entities, terms
}

func canonicalQuestionTerms(question string) ([]string, []string) {
	terms := make([]string, 0, maxMetadataTerms)
	entities := make([]string, 0, 8)
	seenTerms := make(map[string]struct{}, maxMetadataTerms)
	seenEntities := make(map[string]struct{}, 8)
	add := func(raw string) {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			return
		}
		if _, exists := seenTerms[value]; !exists && len(terms) < maxMetadataTerms {
			seenTerms[value] = struct{}{}
			terms = append(terms, value)
		}
		if isEntityTerm(value) {
			if _, exists := seenEntities[value]; !exists && len(entities) < maxMetadataTerms {
				seenEntities[value] = struct{}{}
				entities = append(entities, value)
			}
		}
	}

	var token strings.Builder
	var previousHan rune
	flush := func() {
		add(token.String())
		token.Reset()
	}
	for _, r := range question {
		if unicode.In(r, unicode.Han) {
			flush()
			if previousHan != 0 {
				add(string([]rune{previousHan, r}))
			}
			previousHan = r
			continue
		}
		previousHan = 0
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._/:@-", r) {
			token.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return terms, entities
}

func isEntityTerm(value string) bool {
	if len([]rune(value)) < 3 {
		return false
	}
	hasDigit := false
	hasSeparator := false
	for _, r := range value {
		hasDigit = hasDigit || unicode.IsDigit(r)
		hasSeparator = hasSeparator || strings.ContainsRune("._/:@-", r)
	}
	return hasDigit || hasSeparator
}

func buildEvidenceManifest(messages []llm.Message) EvidenceManifest {
	items := make([]EvidenceManifestItem, 0, min(maxManifestItems, len(messages)))
	callNames := make(map[string]string, maxManifestItems)
	callRefs := make(map[string][]string, maxManifestItems)
	omitted := 0
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.ID == "" || call.Function.Name == "" {
				continue
			}
			callNames[call.ID] = call.Function.Name
			callRefs[call.ID] = extractManifestReferences(call.Function.Arguments)
		}
		if message.Role != "tool" {
			continue
		}
		if len(items) >= maxManifestItems {
			omitted++
			continue
		}
		name := message.Name
		if name == "" {
			name = callNames[message.ToolCallID]
		}
		coverage := "full"
		if strings.Contains(message.Content, `"coverage":"partial"`) ||
			strings.Contains(message.Content, `"chunk_coverage":"partial"`) ||
			strings.Contains(message.Content, `"item_coverage":"partial"`) ||
			strings.Contains(message.Content, `"field_coverage":"partial"`) {
			coverage = "partial"
		}
		items = append(items, EvidenceManifestItem{
			Tool: name, Source: name, References: callRefs[message.ToolCallID], Coverage: coverage,
		})
	}
	if len(items) == 0 {
		return EvidenceManifest{Status: "none", Items: []EvidenceManifestItem{}}
	}
	if omitted > 0 {
		items[len(items)-1].Omitted = omitted
	}
	return EvidenceManifest{Status: "available", Items: items}
}

func extractManifestReferences(arguments string) []string {
	if len(arguments) == 0 || len(arguments) > maxReferenceInputBytes || !json.Valid([]byte(arguments)) {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(arguments), &value); err != nil {
		return nil
	}
	refs := make([]string, 0, maxManifestRefs)
	seen := make(map[string]struct{}, maxManifestRefs)
	var walk func(any)
	walk = func(current any) {
		if len(refs) >= maxManifestRefs {
			return
		}
		switch typed := current.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				entry := typed[key]
				if referenceKey(key) {
					if text, ok := entry.(string); ok {
						text = strings.TrimSpace(text)
						if text != "" {
							if _, exists := seen[text]; !exists {
								seen[text] = struct{}{}
								refs = append(refs, truncateRunes(text, 256))
							}
						}
					}
				}
				walk(entry)
			}
		case []any:
			for _, entry := range typed {
				walk(entry)
			}
		}
	}
	walk(value)
	return refs
}

func referenceKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "ref" || key == "url" || key == "source" || key == "traceid" ||
		key == "trace_id" || key == "run_id" || strings.HasSuffix(key, "_id")
}

func truncateRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
}

func encodeMetadataJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// GetContextMetadata reads only the newest uncompacted candidate metadata.
func (ss *SessionStore) GetContextMetadata(id string, userID int64, limit int) (*SessionRecord, error) {
	record, err := ss.getSession(id, userID)
	if err != nil || record == nil {
		return record, err
	}
	if limit <= 0 || limit > RecentTurnMetadataLimit {
		limit = RecentTurnMetadataLimit
	}
	rows, err := ss.db.Query(
		`SELECT t.turn_no,t.run_id,t.token_estimate,t.question_text,t.topic_key,
		        t.entities_json,t.question_terms_json,t.evidence_manifest_json,
		        COALESCE(r.evidence_status,'unavailable'),COALESCE(r.forced_conclusion,0),t.created_at
		 FROM qa_turns t JOIN qa_sessions s ON s.id=t.session_id
		 LEFT JOIN agent_runs r ON r.id=t.run_id AND r.session_id=t.session_id AND r.user_id=s.user_id
		 WHERE t.session_id=? AND s.user_id=? AND t.turn_no>?
		 ORDER BY t.turn_no DESC LIMIT ?`,
		id, userID, record.CompactedThroughTurn, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	record.RecentTurns = make([]TurnMetadata, 0, limit)
	for rows.Next() {
		var turn TurnMetadata
		var entitiesJSON, termsJSON, manifestJSON []byte
		var createdAt sql.NullTime
		if err := rows.Scan(
			&turn.TurnNumber, &turn.RunID, &turn.TokenEstimate, &turn.Question, &turn.TopicKey,
			&entitiesJSON, &termsJSON, &manifestJSON, &turn.EvidenceStatus, &turn.ForcedConclusion, &createdAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(entitiesJSON, &turn.Entities); err != nil {
			return nil, fmt.Errorf("memory/session: decode turn %d entities: %w", turn.TurnNumber, err)
		}
		if err := json.Unmarshal(termsJSON, &turn.QuestionTerms); err != nil {
			return nil, fmt.Errorf("memory/session: decode turn %d terms: %w", turn.TurnNumber, err)
		}
		if err := json.Unmarshal(manifestJSON, &turn.EvidenceManifest); err != nil {
			return nil, fmt.Errorf("memory/session: decode turn %d evidence manifest: %w", turn.TurnNumber, err)
		}
		turn.CreatedAt = store.FormatDatabaseTime(createdAt)
		record.RecentTurns = append(record.RecentTurns, turn)
	}
	return record, rows.Err()
}

// GetContextSnapshot loads routing metadata plus the newest bounded user/assistant exchanges.
func (ss *SessionStore) GetContextSnapshot(id string, userID int64, metadataLimit, dialogueLimit int) (*SessionRecord, error) {
	record, err := ss.GetContextMetadata(id, userID, metadataLimit)
	if err != nil || record == nil || len(record.RecentTurns) == 0 {
		return record, err
	}
	if dialogueLimit <= 0 || dialogueLimit > RecentDialogueTurnLimit {
		dialogueLimit = RecentDialogueTurnLimit
	}
	turnNumbers := make([]int, 0, min(dialogueLimit, len(record.RecentTurns)))
	for _, turn := range record.RecentTurns[:min(dialogueLimit, len(record.RecentTurns))] {
		turnNumbers = append(turnNumbers, turn.TurnNumber)
	}
	turns, err := ss.LoadTurns(id, userID, turnNumbers)
	if err != nil {
		return nil, err
	}
	record.RecentDialogue = recentDialogueFromTurns(turns)
	return record, nil
}

func recentDialogueFromTurns(turns []TurnMessages) []RecentDialogueTurn {
	dialogue := make([]RecentDialogueTurn, 0, len(turns))
	for _, turn := range turns {
		item := RecentDialogueTurn{TurnNumber: turn.TurnNumber}
		for _, message := range turn.Messages {
			content := strings.TrimSpace(message.Content)
			switch {
			case message.Role == "user" && item.User == "" && content != "":
				item.User = truncateRunes(content, maxQuestionRunes)
			case message.Role == "assistant" && content != "":
				item.Assistant = truncateRunes(content, maxAssistantAnswerRunes)
			}
		}
		if item.User != "" || item.Assistant != "" {
			dialogue = append(dialogue, item)
		}
	}
	return dialogue
}

// LoadTurns batch-loads selected atomic turns in chronological order.
func (ss *SessionStore) LoadTurns(id string, userID int64, turnNumbers []int) ([]TurnMessages, error) {
	if len(turnNumbers) == 0 {
		return nil, nil
	}
	if len(turnNumbers) > maxContextTurnBatch {
		return nil, fmt.Errorf("memory/session: selected turn count %d exceeds %d", len(turnNumbers), maxContextTurnBatch)
	}
	seen := make(map[int]struct{}, len(turnNumbers))
	placeholders := make([]string, 0, len(turnNumbers))
	args := make([]any, 0, len(turnNumbers)+2)
	args = append(args, id, userID)
	for _, turnNumber := range turnNumbers {
		if turnNumber <= 0 {
			return nil, fmt.Errorf("memory/session: invalid turn number %d", turnNumber)
		}
		if _, duplicate := seen[turnNumber]; duplicate {
			continue
		}
		seen[turnNumber] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, turnNumber)
	}
	rows, err := ss.db.Query(
		`SELECT m.turn_no,m.role,m.content,COALESCE(m.tool_calls_json,''),m.tool_call_id,m.tool_name
		 FROM qa_messages m JOIN qa_sessions s ON s.id=m.session_id
		 WHERE m.session_id=? AND s.user_id=? AND m.turn_no IN (`+strings.Join(placeholders, ",")+`)
		 ORDER BY m.turn_no,m.seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := make([]TurnMessages, 0, len(placeholders))
	for rows.Next() {
		var turnNumber int
		var message llm.Message
		var toolCalls string
		if err := rows.Scan(&turnNumber, &message.Role, &message.Content, &toolCalls, &message.ToolCallID, &message.Name); err != nil {
			return nil, err
		}
		if err := unmarshalToolCalls(toolCalls, &message); err != nil {
			return nil, err
		}
		if len(turns) == 0 || turns[len(turns)-1].TurnNumber != turnNumber {
			turns = append(turns, TurnMessages{TurnNumber: turnNumber})
		}
		turns[len(turns)-1].Messages = append(turns[len(turns)-1].Messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(turns) != len(placeholders) {
		return nil, fmt.Errorf("memory/session: loaded %d of %d selected turns", len(turns), len(placeholders))
	}
	return turns, nil
}

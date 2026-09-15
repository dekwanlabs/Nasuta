package memory

import (
	"context"
	"encoding/json"
	"fmt"
)

// crossSessionTopicMinOverlap is the term/entity overlap ratio above which a
// historical turn counts as the same topic as the current question. Calibrated
// against real turns: unrelated lookups score below ~0.03, related topics above
// ~0.13, so 0.1 separates a recurring work topic from a one-off lookup without
// requiring identical phrasing.
const crossSessionTopicMinOverlap = 0.1

// crossSessionTopicScanLimit bounds how many recent turns are read when checking
// cross-session topic recurrence. Reads stay bounded at the storage boundary.
const crossSessionTopicScanLimit = 200

// HasCrossSessionTopic reports whether the user asked about the same topic in at
// least one other session, so a current-focus candidate is a recurring work
// topic rather than a one-off lookup. Topic similarity reuses the same canonical
// terms and overlap measure as session-turn affinity.
func (memory *MemoryStore) HasCrossSessionTopic(
	ctx context.Context,
	userID int64,
	excludeSessionID string,
	question string,
) (bool, error) {
	if memory.db == nil || userID <= 0 {
		return false, nil
	}
	terms, entities := canonicalQuestionTerms(question)
	if len(terms) == 0 && len(entities) == 0 {
		return false, nil
	}

	rows, err := memory.db.QueryContext(ctx,
		`SELECT t.question_terms_json, t.entities_json
		 FROM qa_turns t JOIN qa_sessions s ON s.id=t.session_id
		 WHERE s.user_id=? AND t.session_id<>?
		 ORDER BY t.created_at DESC LIMIT ?`,
		userID, excludeSessionID, crossSessionTopicScanLimit,
	)
	if err != nil {
		return false, fmt.Errorf("memory: scan cross-session turns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var termsJSON, entitiesJSON []byte
		if err := rows.Scan(&termsJSON, &entitiesJSON); err != nil {
			return false, fmt.Errorf("memory: scan cross-session turn: %w", err)
		}
		var turnTerms, turnEntities []string
		if err := json.Unmarshal(termsJSON, &turnTerms); err != nil {
			continue
		}
		if err := json.Unmarshal(entitiesJSON, &turnEntities); err != nil {
			continue
		}
		if termOverlapRatio(terms, turnTerms) >= crossSessionTopicMinOverlap ||
			(len(entities) > 0 && len(turnEntities) > 0 &&
				termOverlapRatio(entities, turnEntities) >= crossSessionTopicMinOverlap) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// termOverlapRatio is the Jaccard-style overlap of two term sets: |A∩B| / |A∪B|.
func termOverlapRatio(left, right []string) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(left))
	for _, value := range left {
		set[value] = struct{}{}
	}
	shared := 0
	seenRight := make(map[string]struct{}, len(right))
	for _, value := range right {
		if _, dup := seenRight[value]; dup {
			continue
		}
		seenRight[value] = struct{}{}
		if _, ok := set[value]; ok {
			shared++
		}
	}
	union := len(set) + len(seenRight) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

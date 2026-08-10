package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/semantic"
)

const (
	consolidationRecallLimit       = 8
	consolidationRecallSearchLimit = 24
	consolidationRecallPerFactKey  = 2
	consolidationMinDenseScore     = 0.78
	consolidationDecisionMinScore  = 0.85
)

var (
	// ErrStaleMemoryDecision reports that recalled state changed before consolidation.
	ErrStaleMemoryDecision = errors.New("memory: stale consolidation decision")
	// ErrRejectedMemoryDecision reports a mutation blocked by durable authority.
	ErrRejectedMemoryDecision = errors.New("memory: consolidation decision rejected")
)

// ConsolidationAction describes how a candidate relates to durable state.
type ConsolidationAction string

const (
	ConsolidationAdd     ConsolidationAction = "add"
	ConsolidationRefresh ConsolidationAction = "refresh"
	ConsolidationReplace ConsolidationAction = "replace"
	ConsolidationReject  ConsolidationAction = "reject"
	ConsolidationDiscard ConsolidationAction = "discard"
)

// ConsolidationMatch is one authoritative memory admitted for write-time comparison.
type ConsolidationMatch struct {
	Record       MemoryRecord `json:"record"`
	DenseScore   float32      `json:"dense_score"`
	ExactFactKey bool         `json:"exact_fact_key"`
}

// ConsolidationRecallStats records bounded write-time retrieval decisions.
type ConsolidationRecallStats struct {
	Probes            int
	ExactFactKeys     int
	Candidates        int
	BelowScore        int
	InvalidPayload    int
	MissingRecords    int
	Unauthorized      int
	InvalidStatus     int
	Expired           int
	PerFactKeyDropped int
	Admitted          int
}

// ConsolidationRecallResult contains candidates without mutating usage counters.
type ConsolidationRecallResult struct {
	Matches []ConsolidationMatch
	Stats   ConsolidationRecallStats
}

// MemoryDecision is a validated consolidation command produced from one turn.
type MemoryDecision struct {
	Record             MemoryRecord
	Action             ConsolidationAction
	TargetID           string
	Relation           string
	DecisionConfidence float32
}

// RecallForConsolidation combines exact fact slots with dense-only retrieval.
func (memory *MemoryStore) RecallForConsolidation(
	ctx context.Context,
	userID int64,
	probes []MemoryProbe,
) (ConsolidationRecallResult, error) {
	var result ConsolidationRecallResult
	if !memory.Enabled() {
		return result, fmt.Errorf("memory: semantic recall unavailable")
	}
	if userID <= 0 {
		return result, fmt.Errorf("memory: authenticated user is required")
	}
	if len(probes) == 0 {
		return result, nil
	}
	if len(probes) > 5 {
		probes = probes[:5]
	}
	result.Stats.Probes = len(probes)

	queries := make([]string, 0, len(probes))
	factKeys := make([]string, 0, len(probes))
	seenFactKeys := make(map[string]struct{}, len(probes))
	for _, probe := range probes {
		query := strings.TrimSpace(probe.Query)
		if query != "" {
			queries = append(queries, query)
		}
		if probe.FactKeyHint == "" {
			continue
		}
		if _, exists := seenFactKeys[probe.FactKeyHint]; exists {
			continue
		}
		seenFactKeys[probe.FactKeyHint] = struct{}{}
		factKeys = append(factKeys, probe.FactKeyHint)
	}
	exactRecords, err := memory.loadActiveFactsByKeys(ctx, userID, factKeys)
	if err != nil {
		return result, err
	}
	result.Stats.ExactFactKeys = len(exactRecords)
	if len(queries) == 0 {
		result.Matches = exactConsolidationMatches(exactRecords)
		result.Stats.Admitted = len(result.Matches)
		return result, nil
	}

	vecs, err := memory.embedder.Embed(ctx, queries)
	if err != nil {
		return result, fmt.Errorf("memory: embed consolidation probes: %w", err)
	}
	if len(vecs) != len(queries) {
		return result, fmt.Errorf("memory: expected %d consolidation vectors, got %d", len(queries), len(vecs))
	}

	ids := make([]string, 0, len(queries)*consolidationRecallSearchLimit)
	scores := make(map[string]float32, cap(ids))
	for _, vector := range vecs {
		hits, err := memory.semantic.Search(ctx, semantic.Query{
			DenseVector: vector,
			Filter: semantic.Filter{
				Keywords:   map[string]string{"kind": "memory"},
				AnyInteger: map[string][]int64{"user_id": {userID}},
			},
			Limit: consolidationRecallSearchLimit,
		})
		if err != nil {
			return result, fmt.Errorf("memory: search consolidation candidates: %w", err)
		}
		result.Stats.Candidates += len(hits)
		for _, hit := range hits {
			if hit.ScoreKind != semantic.ScoreDense {
				return result, fmt.Errorf("memory: consolidation recall requires dense scores, got %q", hit.ScoreKind)
			}
			if hit.DenseScore < consolidationMinDenseScore {
				result.Stats.BelowScore++
				continue
			}
			payloadUserID, ok := memoryPayloadUserID(hit.Metadata)
			if !ok || payloadUserID != userID {
				result.Stats.InvalidPayload++
				continue
			}
			id, _ := hit.Metadata["memory_id"].(string)
			if id == "" {
				result.Stats.InvalidPayload++
				continue
			}
			if previous, exists := scores[id]; exists {
				if hit.DenseScore > previous {
					scores[id] = hit.DenseScore
				}
				continue
			}
			ids = append(ids, id)
			scores[id] = hit.DenseScore
		}
	}

	var records []MemoryRecord
	if len(ids) > 0 {
		records, err = memory.loadRecallCandidates(ctx, userID, ids)
		if err != nil {
			return result, err
		}
	}
	byID := make(map[string]MemoryRecord, len(records)+len(exactRecords))
	for _, rec := range records {
		byID[rec.ID] = rec
	}
	for _, rec := range exactRecords {
		byID[rec.ID] = rec
	}

	now := memory.now().UTC()
	matchesByID := make(map[string]ConsolidationMatch, len(byID))
	for _, rec := range exactRecords {
		matchesByID[rec.ID] = ConsolidationMatch{Record: rec, DenseScore: 1, ExactFactKey: true}
	}
	for _, id := range ids {
		rec, ok := byID[id]
		if !ok {
			result.Stats.MissingRecords++
			continue
		}
		if rec.UserID != userID {
			result.Stats.Unauthorized++
			continue
		}
		if rec.Status != StatusActive && rec.Status != StatusSuperseded {
			result.Stats.InvalidStatus++
			continue
		}
		if rec.ExpiresAt != nil && !rec.ExpiresAt.After(now) {
			result.Stats.Expired++
			continue
		}
		if existing, ok := matchesByID[id]; ok && existing.ExactFactKey {
			continue
		}
		matchesByID[id] = ConsolidationMatch{Record: rec, DenseScore: scores[id]}
	}
	matches := make([]ConsolidationMatch, 0, len(matchesByID))
	for _, match := range matchesByID {
		matches = append(matches, match)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].ExactFactKey != matches[j].ExactFactKey {
			return matches[i].ExactFactKey
		}
		if matches[i].DenseScore != matches[j].DenseScore {
			return matches[i].DenseScore > matches[j].DenseScore
		}
		if matches[i].Record.Status != matches[j].Record.Status {
			return matches[i].Record.Status == StatusActive
		}
		return matches[i].Record.UpdatedAt.After(matches[j].Record.UpdatedAt)
	})

	perFactKey := make(map[string]int, min(len(matches), consolidationRecallLimit))
	result.Matches = make([]ConsolidationMatch, 0, min(len(matches), consolidationRecallLimit))
	for _, match := range matches {
		if perFactKey[match.Record.FactKey] >= consolidationRecallPerFactKey {
			result.Stats.PerFactKeyDropped++
			continue
		}
		perFactKey[match.Record.FactKey]++
		result.Matches = append(result.Matches, match)
		if len(result.Matches) == consolidationRecallLimit {
			break
		}
	}
	result.Stats.Admitted = len(result.Matches)
	return result, nil
}

func (memory *MemoryStore) loadActiveFactsByKeys(ctx context.Context, userID int64, factKeys []string) ([]MemoryRecord, error) {
	if len(factKeys) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(factKeys)+2)
	args = append(args, userID)
	for _, factKey := range factKeys {
		args = append(args, factKey)
	}
	args = append(args, len(factKeys))
	rows, err := memory.db.QueryContext(ctx,
		`SELECT `+memorySelectColumns+`
		 FROM qa_memories
		 WHERE user_id=? AND status='active' AND fact_key IN (`+placeholders(len(factKeys))+`)
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: load active consolidation facts: %w", err)
	}
	defer rows.Close()

	records := make([]MemoryRecord, 0, len(factKeys))
	for rows.Next() {
		rec, err := scanMemory(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("memory: scan active consolidation fact: %w", err)
		}
		records = append(records, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate active consolidation facts: %w", err)
	}
	return records, nil
}

func exactConsolidationMatches(records []MemoryRecord) []ConsolidationMatch {
	matches := make([]ConsolidationMatch, len(records))
	for i := range records {
		matches[i] = ConsolidationMatch{Record: records[i], DenseScore: 1, ExactFactKey: true}
	}
	return matches
}

// ApplyDecision revalidates a consolidation action against current durable state.
func (memory *MemoryStore) ApplyDecision(ctx context.Context, decision MemoryDecision) (WriteResult, error) {
	switch decision.Action {
	case ConsolidationAdd:
		return memory.write(ctx, decision.Record, &writeExpectation{requireAbsent: true})
	case ConsolidationReplace:
		if strings.TrimSpace(decision.TargetID) == "" {
			return WriteResult{}, fmt.Errorf("memory: replace target is required")
		}
		return memory.write(ctx, decision.Record, &writeExpectation{
			activeID: decision.TargetID, requireAuthority: true,
		})
	case ConsolidationRefresh:
		return memory.refreshEquivalent(ctx, decision)
	default:
		return WriteResult{}, fmt.Errorf("memory: action %q does not mutate durable state", decision.Action)
	}
}

func (memory *MemoryStore) refreshEquivalent(ctx context.Context, decision MemoryDecision) (WriteResult, error) {
	rec, err := canonicalizeRecord(decision.Record)
	if err != nil {
		return WriteResult{}, err
	}
	if strings.TrimSpace(decision.TargetID) == "" {
		return WriteResult{}, fmt.Errorf("memory: refresh target is required")
	}

	tx, err := memory.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteResult{}, fmt.Errorf("memory: begin equivalent refresh: %w", err)
	}
	defer tx.Rollback()

	active, err := getActiveFact(ctx, tx, rec.UserID, rec.FactKey)
	if err != nil {
		return WriteResult{}, err
	}
	if active == nil || active.ID != decision.TargetID {
		return WriteResult{}, fmt.Errorf("%w for fact %q", ErrStaleMemoryDecision, rec.FactKey)
	}

	sourceType := active.SourceType
	authority := active.Authority
	kind := active.Kind
	sourceSession := active.SourceSession
	confidence := active.Confidence
	expiresAt := active.ExpiresAt
	if rec.Authority >= active.Authority {
		sourceType = rec.SourceType
		authority = rec.Authority
		kind = rec.Kind
		sourceSession = rec.SourceSession
		confidence = max(active.Confidence, rec.Confidence)
		expiresAt = rec.ExpiresAt
	}
	now := memory.now().UTC()
	result, err := tx.ExecContext(ctx,
		`UPDATE qa_memories
		 SET kind=?,source_type=?,authority=?,source_session=?,confidence=?,
		     expires_at=?,updated_at=?,use_count=use_count+1
		 WHERE id=? AND user_id=? AND fact_key=? AND status='active'`,
		kind, sourceType, authority, sourceSession, confidence, expiresAt, now,
		active.ID, rec.UserID, rec.FactKey,
	)
	if err != nil {
		return WriteResult{}, fmt.Errorf("memory: refresh equivalent fact %q: %w", rec.FactKey, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return WriteResult{}, fmt.Errorf("memory: inspect equivalent refresh %q: %w", rec.FactKey, err)
	}
	if affected != 1 {
		return WriteResult{}, fmt.Errorf("%w for fact %q", ErrStaleMemoryDecision, rec.FactKey)
	}

	active.Kind = kind
	active.SourceType = sourceType
	active.Authority = authority
	active.SourceSession = sourceSession
	active.Confidence = confidence
	active.ExpiresAt = expiresAt
	active.UpdatedAt = now
	active.UseCount++
	if err := tx.Commit(); err != nil {
		return WriteResult{}, fmt.Errorf("memory: commit equivalent refresh %q: %w", rec.FactKey, err)
	}

	return WriteResult{
		ID: active.ID, Outcome: WriteRefreshed,
		VectorSynced: memory.syncVector(ctx, *active),
	}, nil
}

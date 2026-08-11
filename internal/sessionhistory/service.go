package sessionhistory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	candidateLimit = 64
	selectedLimit  = 24
	outboxBatch    = 64
	rrfK           = 60.0
)

// RelevancePolicy defines score-kind-aware history admission thresholds.
type RelevancePolicy struct {
	DenseOnlyMin         float32
	DenseLexicalMin      float32
	DenseLexicalCoverage float64
	LexicalOnlyCoverage  float64
	LexicalOnlyMinTerms  int
}

func defaultRelevancePolicy() RelevancePolicy {
	return RelevancePolicy{
		DenseOnlyMin: 0.80, DenseLexicalMin: 0.70, DenseLexicalCoverage: 0.35,
		LexicalOnlyCoverage: 0.70, LexicalOnlyMinTerms: 2,
	}
}

// Service owns current-session history recall and eventual vector indexing.
type Service struct {
	sessions      *memory.SessionStore
	semantic      semantic.Store
	embedder      embed.Embedder
	bm25          *retrieval.BM25Builder
	bm25VocabPath string
	relevance     RelevancePolicy
	syncMu        sync.Mutex
}

// New keeps lexical recall available when the optional dense backend is absent.
func New(sessions *memory.SessionStore, sem semantic.Store, emb embed.Embedder) *Service {
	if sessions == nil {
		return nil
	}
	return &Service{
		sessions: sessions, semantic: sem, embedder: emb,
		relevance: defaultRelevancePolicy(),
	}
}

// WithRelevancePolicy overrides calibrated thresholds for this embedding model.
func (service *Service) WithRelevancePolicy(policy RelevancePolicy) *Service {
	if service == nil {
		return nil
	}
	if policy.DenseOnlyMin > 0 {
		service.relevance.DenseOnlyMin = policy.DenseOnlyMin
	}
	if policy.DenseLexicalMin > 0 {
		service.relevance.DenseLexicalMin = policy.DenseLexicalMin
	}
	if policy.DenseLexicalCoverage > 0 {
		service.relevance.DenseLexicalCoverage = policy.DenseLexicalCoverage
	}
	if policy.LexicalOnlyCoverage > 0 {
		service.relevance.LexicalOnlyCoverage = policy.LexicalOnlyCoverage
	}
	if policy.LexicalOnlyMinTerms > 0 {
		service.relevance.LexicalOnlyMinTerms = policy.LexicalOnlyMinTerms
	}
	return service
}

// EnableBM25 binds sparse coordinates to the dedicated history collection.
func (service *Service) EnableBM25(vocabPath string) error {
	if vocabPath == "" {
		return fmt.Errorf("session history BM25: vocabulary path is required")
	}
	builder, err := retrieval.LoadVocab(vocabPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session history BM25: load vocabulary %q: %w", vocabPath, err)
		}
		builder = retrieval.NewBM25Builder()
	}
	service.bm25 = builder
	service.bm25VocabPath = vocabPath
	return nil
}

// PrepareRecords canonicalizes lexical terms at the archive ingress boundary.
func (service *Service) PrepareRecords(records []memory.TurnContextRecord) {
	for i := range records {
		terms := extractTerms(records[i].SummaryText, 32)
		records[i].Terms = make([]memory.HistoryTerm, 0, len(terms))
		for _, term := range terms {
			records[i].Terms = append(records[i].Terms, memory.HistoryTerm{Value: term.value, Weight: term.weight})
		}
	}
}

// Recall returns token-bounded archived summaries relevant to the current question.
func (service *Service) Recall(ctx context.Context, userID int64, sessionID, query, continuity string, tokenBudget int) (string, error) {
	combined := strings.TrimSpace(strings.TrimSpace(query) + "\n" + strings.TrimSpace(continuity))
	candidates, err := service.Discover(ctx, userID, sessionID, combined)
	if err != nil {
		return "", err
	}
	return service.Materialize(ctx, userID, sessionID, candidates, selectedLimit, tokenBudget, true)
}

// Find performs an explicit current-session history search for the private tool.
func (service *Service) Find(ctx context.Context, userID int64, sessionID, query string, limit, tokenBudget int) (string, error) {
	candidates, err := service.Discover(ctx, userID, sessionID, query)
	if err != nil {
		return "", err
	}
	result, err := service.Materialize(ctx, userID, sessionID, candidates, limit, tokenBudget, true)
	if err != nil || result != "" {
		return result, err
	}
	return `{"version":1,"mode":"no_history_hit","turns":[]}`, nil
}

// Discover performs the remote and lexical candidate phase without loading
// summary text. It is safe to start before query analysis has completed.
func (service *Service) Discover(ctx context.Context, userID int64, sessionID, query string) (session.HistoryCandidates, error) {
	output, err := runtrace.Invoke(ctx, sessionHistoryDiscoverySpec, recallInput{
		UserID: userID, SessionID: sessionID, Query: query, Limit: selectedLimit,
	}, service.discoverHistory)
	if err != nil {
		return session.HistoryCandidates{}, err
	}
	return session.HistoryCandidates{Mode: output.Mode, Refs: output.Refs}, nil
}

// Materialize loads authoritative summaries for discovered candidates and
// applies neighbor expansion and the final token budget.
func (service *Service) Materialize(
	ctx context.Context,
	userID int64,
	sessionID string,
	candidates session.HistoryCandidates,
	limit, tokenBudget int,
	neighbors bool,
) (string, error) {
	result, err := runtrace.Invoke(ctx, sessionHistoryMaterializeSpec, recallInput{
		UserID: userID, SessionID: sessionID, Query: strings.Join(candidates.Refs, ","),
		Limit: limit, TokenBudget: tokenBudget, Neighbors: neighbors,
	}, func(ctx context.Context, input recallInput) (recallOutput, error) {
		return service.materializeHistory(ctx, input, candidates)
	})
	return result.Payload, err
}

type historyCandidate struct {
	ref             string
	denseRank       int
	lexicalRank     int
	denseScore      float32
	lexicalWeight   int
	matchedTerms    int
	lexicalCoverage float64
	rankScore       float64
}

type historyPayload struct {
	Version int                     `json:"version"`
	Mode    string                  `json:"mode"`
	Turns   []memory.HistorySummary `json:"turns"`
}

type recallInput struct {
	UserID      int64
	SessionID   string
	Query       string
	Limit       int
	TokenBudget int
	Neighbors   bool
}

type recallOutput struct {
	Payload            string
	Mode               string
	Refs               []string
	DenseCandidates    int
	LexicalCandidates  int
	FusedCandidates    int
	ScoreFiltered      int
	AcceptedCandidates int
	LoadedRecords      int
	DenseOnlyAccepted  int
	StaleCandidates    int
	SelectedItems      int
	SelectedTokens     int
	NeighborItems      int
	Record             bool
}

var sessionHistoryRecallSpec = runtrace.Spec[recallInput, recallOutput]{
	Operation: "session_history.recall",
	Node:      "session_history_recall",
	Output: func(input recallInput, output recallOutput, _ error) map[string]any {
		return map[string]any{
			"mode": output.Mode, "dense_candidates": output.DenseCandidates, "lexical_candidates": output.LexicalCandidates,
			"fused_candidates": output.FusedCandidates, "score_filtered": output.ScoreFiltered,
			"accepted_candidates": output.AcceptedCandidates, "loaded_records": output.LoadedRecords,
			"dense_only_accepted": output.DenseOnlyAccepted, "stale_candidates": output.StaleCandidates,
			"selected_items": output.SelectedItems, "selected_tokens": output.SelectedTokens,
			"neighbor_items": output.NeighborItems, "query_tokens": tooloutput.EstimateTokens(input.Query),
		}
	},
	Record: func(output recallOutput, err error) bool {
		return err == nil && output.Record
	},
}

var sessionHistoryDiscoverySpec = runtrace.Spec[recallInput, recallOutput]{
	Operation: "session_history.discover",
	Node:      "session_history_discover",
	Output: func(input recallInput, output recallOutput, _ error) map[string]any {
		return map[string]any{
			"mode": output.Mode, "dense_candidates": output.DenseCandidates,
			"lexical_candidates": output.LexicalCandidates, "fused_candidates": output.FusedCandidates,
			"score_filtered": output.ScoreFiltered, "accepted_candidates": output.AcceptedCandidates,
			"query_tokens": tooloutput.EstimateTokens(input.Query),
		}
	},
	Record: func(output recallOutput, err error) bool {
		return err == nil && output.Record
	},
}

var sessionHistoryMaterializeSpec = runtrace.Spec[recallInput, recallOutput]{
	Operation: "session_history.materialize",
	Node:      "session_history_materialize",
	Output: func(input recallInput, output recallOutput, _ error) map[string]any {
		return map[string]any{
			"mode": output.Mode, "accepted_candidates": output.AcceptedCandidates,
			"loaded_records": output.LoadedRecords, "stale_candidates": output.StaleCandidates,
			"selected_items": output.SelectedItems, "selected_tokens": output.SelectedTokens,
			"neighbor_items": output.NeighborItems, "query_tokens": tooloutput.EstimateTokens(input.Query),
		}
	},
	Record: func(output recallOutput, err error) bool {
		return err == nil && output.Record
	},
}

func (service *Service) find(ctx context.Context, userID int64, sessionID, query string, limit, tokenBudget int, neighbors bool) (string, error) {
	result, err := runtrace.Invoke(ctx, sessionHistoryRecallSpec, recallInput{
		UserID: userID, SessionID: sessionID, Query: query, Limit: limit, TokenBudget: tokenBudget, Neighbors: neighbors,
	}, service.findHistory)
	return result.Payload, err
}

func (service *Service) findHistory(ctx context.Context, input recallInput) (recallOutput, error) {
	userID, sessionID, query := input.UserID, input.SessionID, input.Query
	limit, tokenBudget := input.Limit, input.TokenBudget
	if userID <= 0 || sessionID == "" || strings.TrimSpace(query) == "" {
		return recallOutput{}, nil
	}
	if limit <= 0 || limit > selectedLimit {
		limit = selectedLimit
	}
	if tokenBudget <= 0 {
		return recallOutput{}, nil
	}
	discovered, err := service.discoverHistory(ctx, input)
	if err != nil {
		return recallOutput{}, err
	}
	candidates := session.HistoryCandidates{Mode: discovered.Mode, Refs: discovered.Refs}
	return service.materializeHistory(ctx, input, candidates)
}

func (service *Service) discoverHistory(ctx context.Context, input recallInput) (recallOutput, error) {
	userID, sessionID, query := input.UserID, input.SessionID, input.Query
	if userID <= 0 || sessionID == "" || strings.TrimSpace(query) == "" {
		return recallOutput{}, nil
	}
	queryTerms := extractTerms(query, 16)
	canonicalTerms := make([]string, 0, len(queryTerms))
	for _, term := range queryTerms {
		canonicalTerms = append(canonicalTerms, term.value)
	}
	type lexicalResult struct {
		refs []memory.HistoryLexicalCandidate
		err  error
	}
	type denseResult struct {
		hits []semantic.Hit
		mode string
	}
	lexicalCh := make(chan lexicalResult, 1)
	go func() {
		refs, err := service.sessions.FindHistoryRefs(ctx, userID, sessionID, canonicalTerms, candidateLimit)
		lexicalCh <- lexicalResult{refs: refs, err: err}
	}()
	denseCh := make(chan denseResult, 1)
	go func() {
		result := denseResult{mode: "lexical_only_unavailable_dense"}
		if service.semantic == nil || service.embedder == nil || !service.embedder.Enabled() {
			denseCh <- result
			return
		}
		vectors, embedErr := service.embedder.Embed(ctx, []string{query})
		if embedErr != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
			result.mode = "lexical_only_dense_error"
			if embedErr != nil {
				log.ErrorfCtx(ctx, "[qa] session history dense query embedding failed: %v", embedErr)
			} else {
				log.ErrorfCtx(ctx, "[qa] session history dense query returned %d vectors, want one non-empty vector", len(vectors))
			}
			denseCh <- result
			return
		}
		searchQuery := semantic.Query{
			DenseVector: vectors[0],
			Filter: semantic.Filter{
				Keywords:   map[string]string{"kind": "session_turn", "session_id": sessionID},
				AnyInteger: map[string][]int64{"user_id": {userID}},
			},
			Limit: candidateLimit,
		}
		hits, searchErr := service.semantic.Search(ctx, searchQuery)
		if searchErr != nil {
			result.mode = "lexical_only_dense_error"
			log.ErrorfCtx(ctx, "[qa] session history hybrid search failed: %v", searchErr)
			denseCh <- result
			return
		}
		result.mode = "dense_only"
		seen := make(map[string]struct{}, len(hits))
		for _, hit := range hits {
			if hit.ScoreKind != semantic.ScoreDense {
				result.mode = "lexical_only_dense_score_kind_error"
				log.ErrorfCtx(ctx, "[qa] session history dense query returned score kind %q", hit.ScoreKind)
				result.hits = nil
				denseCh <- result
				return
			}
			ref, _ := hit.Metadata["ref"].(string)
			if ref == "" {
				ref = hit.ID
			}
			if ref == "" {
				continue
			}
			if _, exists := seen[ref]; exists {
				continue
			}
			seen[ref] = struct{}{}
			hit.Metadata = map[string]any{"ref": ref}
			result.hits = append(result.hits, hit)
		}
		denseCh <- result
	}()

	lexicalResultValue := <-lexicalCh
	if lexicalResultValue.err != nil {
		return recallOutput{}, lexicalResultValue.err
	}
	lexical := lexicalResultValue.refs
	denseResultValue := <-denseCh
	mode := denseResultValue.mode
	dense := denseResultValue.hits
	if len(dense) > 0 && len(lexical) > 0 && mode == "dense_only" {
		mode = "dense_lexical"
	}
	if mode == "dense_only" && len(dense) == 0 && len(lexical) > 0 {
		mode = "lexical_only"
		pending, pendingErr := service.sessions.HasPendingHistoryUpserts(ctx, userID, sessionID)
		if pendingErr != nil {
			return recallOutput{}, pendingErr
		}
		if pending {
			mode = "lexical_only_pending_dense"
		}
	}

	ranked := mergeHistoryCandidates(dense, lexical, len(queryTerms))
	if len(ranked) == 0 {
		log.InfofCtx(ctx, "[qa] session history recall session=%s mode=no_history_hit dense=%d lexical=%d", sessionID, len(dense), len(lexical))
		return recallOutput{
			Mode: "no_history_hit", DenseCandidates: len(dense), LexicalCandidates: len(lexical), Record: true,
		}, nil
	}
	accepted, filtered, denseOnlyAccepted := service.filterRelevant(ranked)
	if len(accepted) == 0 {
		log.InfofCtx(ctx, "[qa] session history recall session=%s mode=no_relevant_history dense=%d lexical=%d fused=%d filtered=%d",
			sessionID, len(dense), len(lexical), len(ranked), filtered)
		return recallOutput{
			Mode: "no_relevant_history", DenseCandidates: len(dense), LexicalCandidates: len(lexical),
			FusedCandidates: len(ranked), ScoreFiltered: filtered, Record: true,
		}, nil
	}
	refs := make([]string, 0, min(len(accepted), candidateLimit))
	for _, candidate := range accepted {
		refs = append(refs, candidate.ref)
	}
	return recallOutput{
		Mode: acceptedMode(mode, len(accepted)), DenseCandidates: len(dense), LexicalCandidates: len(lexical),
		FusedCandidates: len(ranked), ScoreFiltered: filtered, AcceptedCandidates: len(accepted),
		DenseOnlyAccepted: denseOnlyAccepted, Refs: refs, Record: true,
	}, nil
}

func acceptedMode(mode string, accepted int) string {
	if accepted == 0 {
		return "no_relevant_history"
	}
	return mode
}

func (service *Service) materializeHistory(ctx context.Context, input recallInput, candidates session.HistoryCandidates) (recallOutput, error) {
	userID, sessionID := input.UserID, input.SessionID
	limit, tokenBudget, neighbors := input.Limit, input.TokenBudget, input.Neighbors
	if userID <= 0 || sessionID == "" || len(candidates.Refs) == 0 {
		return recallOutput{Mode: candidates.Mode, Record: true}, nil
	}
	if limit <= 0 || limit > selectedLimit {
		limit = selectedLimit
	}
	if tokenBudget <= 0 {
		return recallOutput{}, nil
	}
	refs := candidates.Refs
	records, err := service.sessions.LoadHistorySummaries(ctx, userID, sessionID, refs)
	if err != nil {
		return recallOutput{}, err
	}
	byRef := make(map[string]memory.HistorySummary, len(records))
	for _, record := range records {
		byRef[record.Ref] = record
	}
	ordered := make([]memory.HistorySummary, 0, len(records))
	for _, ref := range refs {
		if record, ok := byRef[ref]; ok {
			ordered = append(ordered, record)
		}
	}
	neighborItems := 0
	if neighbors && len(ordered) > 0 {
		turns := make([]int, 0, min(len(ordered), limit))
		for _, record := range ordered[:min(len(ordered), limit)] {
			turns = append(turns, record.TurnNumber)
		}
		adjacent, neighborErr := service.sessions.LoadHistoryNeighbors(ctx, userID, sessionID, turns)
		if neighborErr != nil {
			return recallOutput{}, neighborErr
		}
		seen := make(map[string]struct{}, len(ordered)+len(adjacent))
		for _, record := range ordered {
			seen[record.Ref] = struct{}{}
		}
		neighborsByTurn := make(map[int][]memory.HistorySummary, len(adjacent))
		for _, record := range adjacent {
			neighborsByTurn[record.TurnNumber] = append(neighborsByTurn[record.TurnNumber], record)
		}
		interleaved := make([]memory.HistorySummary, 0, len(ordered)+len(adjacent))
		primaryCount := min(len(ordered), limit)
		for _, record := range ordered[:primaryCount] {
			interleaved = append(interleaved, record)
			for _, neighbor := range neighborsByTurn[record.TurnNumber-1] {
				if _, exists := seen[neighbor.Ref]; exists {
					continue
				}
				seen[neighbor.Ref] = struct{}{}
				interleaved = append(interleaved, neighbor)
				neighborItems++
			}
			for _, neighbor := range neighborsByTurn[record.TurnNumber+1] {
				if _, exists := seen[neighbor.Ref]; exists {
					continue
				}
				seen[neighbor.Ref] = struct{}{}
				interleaved = append(interleaved, neighbor)
				neighborItems++
			}
		}
		ordered = append(interleaved, ordered[primaryCount:]...)
		for _, record := range adjacent {
			if _, exists := seen[record.Ref]; !exists {
				seen[record.Ref] = struct{}{}
				ordered = append(ordered, record)
				neighborItems++
			}
		}
	}
	selected, selectedItems, err := selectPayloadWithCount(candidates.Mode, ordered, limit, tokenBudget)
	if err != nil {
		return recallOutput{}, err
	}
	selectedTokens := tooloutput.EstimateTokens(selected)
	log.InfofCtx(ctx, "[qa] session history recall session=%s mode=%s dense=%d lexical=%d fused=%d filtered=%d accepted=%d loaded_records=%d selected_tokens=%d",
		sessionID, candidates.Mode, 0, 0, 0, 0, len(refs), len(records), selectedTokens)
	return recallOutput{
		Payload: selected, Mode: candidates.Mode, AcceptedCandidates: len(refs),
		LoadedRecords: len(records), StaleCandidates: len(refs) - len(records),
		SelectedItems: selectedItems, SelectedTokens: selectedTokens, NeighborItems: neighborItems, Record: true,
	}, nil
}

func mergeHistoryCandidates(dense []semantic.Hit, lexical []memory.HistoryLexicalCandidate, queryTerms int) []historyCandidate {
	result := make([]historyCandidate, 0, len(dense)+len(lexical))
	byRef := make(map[string]int, len(dense)+len(lexical))
	ensure := func(ref string) int {
		if index, ok := byRef[ref]; ok {
			return index
		}
		index := len(result)
		byRef[ref] = index
		result = append(result, historyCandidate{ref: ref})
		return index
	}
	for rank, hit := range dense {
		ref, _ := hit.Metadata["ref"].(string)
		if ref == "" {
			continue
		}
		index := ensure(ref)
		result[index].denseRank = rank + 1
		result[index].denseScore = hit.DenseScore
		result[index].rankScore += 1 / (rrfK + float64(rank+1))
	}
	for rank, lexicalCandidate := range lexical {
		if lexicalCandidate.Ref == "" {
			continue
		}
		index := ensure(lexicalCandidate.Ref)
		result[index].lexicalRank = rank + 1
		result[index].lexicalWeight = lexicalCandidate.Weight
		result[index].matchedTerms = lexicalCandidate.MatchedTerms
		if queryTerms > 0 {
			result[index].lexicalCoverage = min(1, float64(lexicalCandidate.MatchedTerms)/float64(queryTerms))
		}
		result[index].rankScore += 1 / (rrfK + float64(rank+1))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].rankScore != result[j].rankScore {
			return result[i].rankScore > result[j].rankScore
		}
		if result[i].denseScore != result[j].denseScore {
			return result[i].denseScore > result[j].denseScore
		}
		return result[i].ref < result[j].ref
	})
	return result
}

func (service *Service) filterRelevant(candidates []historyCandidate) ([]historyCandidate, int, int) {
	accepted := make([]historyCandidate, 0, len(candidates))
	denseOnlyAccepted := 0
	for _, candidate := range candidates {
		hasDense := candidate.denseRank > 0
		hasLexical := candidate.lexicalRank > 0
		pass := false
		switch {
		case hasDense && hasLexical:
			pass = candidate.denseScore >= service.relevance.DenseLexicalMin &&
				candidate.lexicalCoverage >= service.relevance.DenseLexicalCoverage
		case hasDense:
			pass = candidate.denseScore >= service.relevance.DenseOnlyMin
			if pass {
				denseOnlyAccepted++
			}
		case hasLexical:
			pass = candidate.lexicalCoverage >= service.relevance.LexicalOnlyCoverage &&
				candidate.matchedTerms >= service.relevance.LexicalOnlyMinTerms
		}
		if pass {
			accepted = append(accepted, candidate)
		}
	}
	return accepted, len(candidates) - len(accepted), denseOnlyAccepted
}

func selectPayload(mode string, candidates []memory.HistorySummary, limit, tokenBudget int) (string, error) {
	result, _, err := selectPayloadWithCount(mode, candidates, limit, tokenBudget)
	return result, err
}

func selectPayloadWithCount(mode string, candidates []memory.HistorySummary, limit, tokenBudget int) (string, int, error) {
	payload := historyPayload{Version: 1, Mode: mode, Turns: make([]memory.HistorySummary, 0, min(limit, len(candidates)))}
	for _, candidate := range candidates {
		if len(payload.Turns) == limit {
			break
		}
		payload.Turns = append(payload.Turns, candidate)
		encoded, err := json.Marshal(payload)
		if err != nil {
			return "", 0, err
		}
		if tooloutput.EstimateTokens(string(encoded)) > tokenBudget {
			payload.Turns = payload.Turns[:len(payload.Turns)-1]
		}
	}
	if len(payload.Turns) == 0 {
		return "", 0, nil
	}
	encoded, err := json.Marshal(payload)
	return string(encoded), len(payload.Turns), err
}

type weightedTerm struct {
	value  string
	weight int
	order  int
}

func extractTerms(text string, limit int) []weightedTerm {
	if limit <= 0 {
		return nil
	}
	byValue := make(map[string]weightedTerm, limit*2)
	start := -1
	order := 0
	flush := func(end int) {
		if start < 0 || end <= start {
			start = -1
			return
		}
		raw := strings.TrimSpace(text[start:end])
		value := strings.ToLower(raw)
		start = -1
		if len(value) < 2 || len(value) > 191 || isLowInformation(value) {
			return
		}
		weight := 1
		if strings.ContainsAny(value, "_./:-") || containsDigit(value) || hasMixedCase(raw) {
			weight = 4
		}
		if previous, exists := byValue[value]; !exists || weight > previous.weight {
			byValue[value] = weightedTerm{value: value, weight: weight, order: order}
			order++
		}
	}
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_./:-", r) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(text))
	terms := make([]weightedTerm, 0, len(byValue))
	for _, term := range byValue {
		terms = append(terms, term)
	}
	sort.SliceStable(terms, func(i, j int) bool {
		if terms[i].weight != terms[j].weight {
			return terms[i].weight > terms[j].weight
		}
		return terms[i].order < terms[j].order
	})
	if len(terms) > limit {
		terms = terms[:limit]
	}
	return terms
}

func containsDigit(value string) bool {
	return strings.IndexFunc(value, unicode.IsDigit) >= 0
}

func hasMixedCase(value string) bool {
	return value != strings.ToLower(value) && value != strings.ToUpper(value)
}

func isLowInformation(value string) bool {
	switch value {
	case "the", "and", "for", "with", "this", "that", "from", "into", "what", "when", "where", "how", "now", "need", "can",
		"怎么", "这个", "那个", "现在", "需要", "可以",
		"der", "die", "das", "und", "für", "mit", "dies", "diese", "dieser", "von", "aus", "was", "wann", "wo", "wie", "jetzt", "brauchen", "können",
		"il", "lo", "la", "gli", "le", "per", "con", "questo", "questa", "quello", "quella", "da", "cosa", "quando", "dove", "come", "ora", "serve", "può",
		"el", "los", "las", "para", "este", "esta", "esto", "ese", "esa", "desde", "qué", "cuándo", "dónde", "cómo", "ahora", "necesita", "puede",
		"これ", "それ", "あれ", "この", "その", "あの", "何", "いつ", "どこ", "どう", "今", "現在", "必要", "できる", "できます",
		"이", "그", "저", "이것", "그것", "저것", "무엇", "언제", "어디", "어떻게", "지금", "현재", "필요", "가능":
		return true
	default:
		return false
	}
}

// SyncPending processes one bounded outbox batch.
func (service *Service) SyncPending(ctx context.Context) error {
	if service.semantic == nil || service.embedder == nil || !service.embedder.Enabled() {
		return nil
	}
	service.syncMu.Lock()
	defer service.syncMu.Unlock()
	tasks, err := service.sessions.ListHistoryIndexTasks(ctx, outboxBatch)
	if err != nil || len(tasks) == 0 {
		return err
	}
	if err := service.processTasks(ctx, tasks); err != nil {
		ids := taskIDs(tasks)
		retryAt := time.Now().UTC().Add(retryDelay(tasks))
		if failErr := service.sessions.FailHistoryIndexTasks(ctx, ids, truncateError(err), retryAt); failErr != nil {
			return fmt.Errorf("session history index: %v; record retry: %w", err, failErr)
		}
		return err
	}
	if err := service.sessions.CompleteHistoryIndexTasks(ctx, taskIDs(tasks)); err != nil {
		return fmt.Errorf("session history index: complete outbox: %w", err)
	}
	log.InfofCtx(ctx, "[qa] session history index synced tasks=%d", len(tasks))
	return nil
}

func (service *Service) processTasks(ctx context.Context, tasks []memory.HistoryIndexTask) error {
	deleteIDs := make([]string, 0, len(tasks))
	type scope struct {
		userID    int64
		sessionID string
	}
	upserts := make(map[scope][]string)
	for _, task := range tasks {
		switch task.Operation {
		case "delete":
			deleteIDs = append(deleteIDs, semanticPointID(task.Ref))
		case "upsert":
			key := scope{userID: task.UserID, sessionID: task.SessionID}
			upserts[key] = append(upserts[key], task.Ref)
		default:
			return fmt.Errorf("session history index: unknown operation %q", task.Operation)
		}
	}
	if len(deleteIDs) > 0 {
		if err := service.semantic.Delete(ctx, semantic.DeleteQuery{IDs: deleteIDs}); err != nil {
			return fmt.Errorf("session history index: delete: %w", err)
		}
	}
	for key, refs := range upserts {
		records, err := service.sessions.LoadHistorySummaries(ctx, key.userID, key.sessionID, refs)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			if err := service.semantic.Delete(ctx, semantic.DeleteQuery{IDs: semanticPointIDs(refs)}); err != nil {
				return fmt.Errorf("session history index: remove stale upserts: %w", err)
			}
			continue
		}
		texts := make([]string, len(records))
		for i := range records {
			texts[i] = records[i].Summary
		}
		vectors, err := service.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("session history index: embed summaries: %w", err)
		}
		if len(vectors) != len(records) {
			return fmt.Errorf("session history index: got %d vectors for %d summaries", len(vectors), len(records))
		}
		sparseVectors := make([]*semantic.SparseVector, len(records))
		if service.bm25 != nil {
			for i, record := range records {
				tokens := service.bm25.AddDoc(record.Summary)
				indices, values := retrieval.SparseToSorted(service.bm25.BuildSparse(tokens))
				if len(indices) > 0 {
					sparseVectors[i] = &semantic.SparseVector{Indices: indices, Values: values}
				}
			}
			if err := service.bm25.SaveVocab(service.bm25VocabPath); err != nil {
				return fmt.Errorf("session history index: save BM25 vocabulary %q: %w", service.bm25VocabPath, err)
			}
		}
		points := make([]semantic.Record, len(records))
		for i, record := range records {
			points[i] = semantic.Record{ID: semanticPointID(record.Ref), DenseVector: vectors[i], SparseVector: sparseVectors[i], Metadata: map[string]any{
				"kind": "session_turn", "ref": record.Ref, "user_id": key.userID,
				"session_id": key.sessionID, "turn_number": record.TurnNumber,
			}}
		}
		if err := service.semantic.Upsert(ctx, points); err != nil {
			return fmt.Errorf("session history index: upsert: %w", err)
		}
	}
	return nil
}

func semanticPointID(ref string) string {
	return platform.UUIDFromString("session_history\x00" + ref)
}

func semanticPointIDs(refs []string) []string {
	ids := make([]string, len(refs))
	for i, ref := range refs {
		ids[i] = semanticPointID(ref)
	}
	return ids
}

// Run retries pending vector mutations until the platform context is canceled.
func (service *Service) Run(ctx context.Context) {
	if service.semantic == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := service.SyncPending(ctx); err != nil {
			log.ErrorfCtx(ctx, "[qa] session history index sync failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Close releases the dedicated session-history semantic store.
func (service *Service) Close() error {
	if service.semantic == nil {
		return nil
	}
	return service.semantic.Close()
}

func taskIDs(tasks []memory.HistoryIndexTask) []int64 {
	ids := make([]int64, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
	}
	return ids
}

func retryDelay(tasks []memory.HistoryIndexTask) time.Duration {
	maxAttempts := 0
	for _, task := range tasks {
		maxAttempts = max(maxAttempts, task.Attempts)
	}
	seconds := min(300, 1<<min(maxAttempts, 8))
	return time.Duration(seconds) * time.Second
}

func truncateError(err error) string {
	text := err.Error()
	if len(text) > 1024 {
		return text[:1024]
	}
	return text
}

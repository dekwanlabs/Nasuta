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

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
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

// Service owns current-session history recall and eventual vector indexing.
type Service struct {
	sessions      *memory.SessionStore
	semantic      semantic.Store
	embedder      embed.Embedder
	bm25          *retrieval.BM25Builder
	bm25VocabPath string
	syncMu        sync.Mutex
}

// New keeps lexical recall available when the optional dense backend is absent.
func New(sessions *memory.SessionStore, sem semantic.Store, emb embed.Embedder) *Service {
	if sessions == nil {
		return nil
	}
	return &Service{sessions: sessions, semantic: sem, embedder: emb}
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
	return service.find(ctx, userID, sessionID, combined, selectedLimit, tokenBudget, true)
}

// Find performs an explicit current-session history search for the private tool.
func (service *Service) Find(ctx context.Context, userID int64, sessionID, query string, limit, tokenBudget int) (string, error) {
	result, err := service.find(ctx, userID, sessionID, query, limit, tokenBudget, true)
	if err != nil || result != "" {
		return result, err
	}
	return `{"version":1,"mode":"no_history_hit","turns":[]}`, nil
}

type rankedRef struct {
	ref   string
	score float64
}

type historyPayload struct {
	Version int                     `json:"version"`
	Mode    string                  `json:"mode"`
	Turns   []memory.HistorySummary `json:"turns"`
}

func (service *Service) find(ctx context.Context, userID int64, sessionID, query string, limit, tokenBudget int, neighbors bool) (string, error) {
	started := time.Now()
	if userID <= 0 || sessionID == "" || strings.TrimSpace(query) == "" {
		return "", nil
	}
	if limit <= 0 || limit > selectedLimit {
		limit = selectedLimit
	}
	if tokenBudget <= 0 {
		return "", nil
	}
	queryTerms := extractTerms(query, 16)
	canonicalTerms := make([]string, 0, len(queryTerms))
	for _, term := range queryTerms {
		canonicalTerms = append(canonicalTerms, term.value)
	}
	lexical, err := service.sessions.FindHistoryRefs(ctx, userID, sessionID, canonicalTerms, candidateLimit)
	if err != nil {
		return "", err
	}

	mode := "lexical_only_unavailable_dense"
	dense := []string(nil)
	if service.semantic != nil && service.embedder != nil && service.embedder.Enabled() {
		vectors, embedErr := service.embedder.Embed(ctx, []string{query})
		if embedErr != nil {
			mode = "lexical_only_dense_error"
			log.ErrorfCtx(ctx, "[qa] session history dense query embedding failed: %v", embedErr)
		} else if len(vectors) != 1 {
			mode = "lexical_only_dense_error"
			log.ErrorfCtx(ctx, "[qa] session history dense query returned %d vectors, want 1", len(vectors))
		} else {
			searchQuery := semantic.Query{
				DenseVector: vectors[0],
				Filter: semantic.Filter{
					Keywords:   map[string]string{"kind": "session_turn", "session_id": sessionID},
					AnyInteger: map[string][]int64{"user_id": {userID}},
				},
				Limit: candidateLimit,
			}
			if service.bm25 != nil {
				indices, values := retrieval.SparseToSorted(service.bm25.QuerySparse(query))
				if len(indices) > 0 {
					searchQuery.SparseVector = &semantic.SparseVector{Indices: indices, Values: values}
				}
			}
			hits, searchErr := service.semantic.Search(ctx, searchQuery)
			if searchErr != nil {
				mode = "lexical_only_dense_error"
				log.ErrorfCtx(ctx, "[qa] session history hybrid search failed: %v", searchErr)
			} else {
				mode = "hybrid"
				dense = make([]string, 0, len(hits))
				seen := make(map[string]struct{}, len(hits))
				for _, hit := range hits {
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
					dense = append(dense, ref)
				}
			}
		}
	}
	if mode == "hybrid" && len(dense) == 0 && len(lexical) > 0 {
		pending, pendingErr := service.sessions.HasPendingHistoryUpserts(ctx, userID, sessionID)
		if pendingErr != nil {
			return "", pendingErr
		}
		if pending {
			mode = "lexical_only_pending_dense"
		}
	}

	ranked := fuseRanks(dense, lexical)
	if len(ranked) == 0 {
		log.InfofCtx(ctx, "[qa] session history recall session=%s mode=no_history_hit dense=%d lexical=%d", sessionID, len(dense), len(lexical))
		recordRecallTrace(ctx, started, "no_history_hit", query, len(dense), len(lexical), 0, 0, 0, 0, 0)
		return "", nil
	}
	refs := make([]string, 0, min(len(ranked), candidateLimit))
	for _, candidate := range ranked {
		refs = append(refs, candidate.ref)
	}
	records, err := service.sessions.LoadHistorySummaries(ctx, userID, sessionID, refs)
	if err != nil {
		return "", err
	}
	byRef := make(map[string]memory.HistorySummary, len(records))
	for _, record := range records {
		byRef[record.Ref] = record
	}
	scores := make(map[string]float64, len(ranked))
	for _, candidate := range ranked {
		scores[candidate.ref] = candidate.score
	}
	ordered := make([]memory.HistorySummary, 0, len(records))
	for ref, record := range byRef {
		if _, ok := scores[ref]; ok {
			ordered = append(ordered, record)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := scores[ordered[i].Ref], scores[ordered[j].Ref]
		if left != right {
			return left > right
		}
		return ordered[i].TurnNumber > ordered[j].TurnNumber
	})
	neighborItems := 0
	if neighbors && len(ordered) > 0 {
		turns := make([]int, 0, min(len(ordered), limit))
		for _, record := range ordered[:min(len(ordered), limit)] {
			turns = append(turns, record.TurnNumber)
		}
		adjacent, neighborErr := service.sessions.LoadHistoryNeighbors(ctx, userID, sessionID, turns)
		if neighborErr != nil {
			return "", neighborErr
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
	selected, selectedItems, err := selectPayloadWithCount(mode, ordered, limit, tokenBudget)
	if err == nil {
		log.InfofCtx(ctx, "[qa] session history recall session=%s mode=%s dense=%d lexical=%d fused=%d revalidated=%d selected_tokens=%d",
			sessionID, mode, len(dense), len(lexical), len(ranked), len(records), tooloutput.EstimateTokens(selected))
		recordRecallTrace(ctx, started, mode, query, len(dense), len(lexical), len(ranked),
			len(ranked)-len(records), selectedItems, tooloutput.EstimateTokens(selected), neighborItems)
	}
	return selected, err
}

func recordRecallTrace(ctx context.Context, started time.Time, mode, query string, dense, lexical, fused,
	stale, selectedItems, selectedTokens, neighborItems int) {
	domain.RecordTrace(ctx, domain.EvaluationTrace{
		Node: "session_history_recall", DurationMS: time.Since(started).Milliseconds(),
		Output: map[string]any{
			"mode": mode, "dense_candidates": dense, "lexical_candidates": lexical,
			"fused_candidates": fused, "stale_candidates": stale,
			"selected_items": selectedItems, "selected_tokens": selectedTokens,
			"neighbor_items": neighborItems, "query_tokens": tooloutput.EstimateTokens(query),
		},
	})
}

func fuseRanks(dense, lexical []string) []rankedRef {
	scores := make(map[string]float64, len(dense)+len(lexical))
	for _, ranking := range [][]string{dense, lexical} {
		for i, ref := range ranking {
			scores[ref] += 1 / (rrfK + float64(i+1))
		}
	}
	result := make([]rankedRef, 0, len(scores))
	for ref, score := range scores {
		result = append(result, rankedRef{ref: ref, score: score})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].score != result[j].score {
			return result[i].score > result[j].score
		}
		return result[i].ref < result[j].ref
	})
	return result
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
	case "the", "and", "for", "with", "this", "that", "from", "into", "what", "when", "where", "怎么", "这个", "那个", "现在", "需要", "可以":
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

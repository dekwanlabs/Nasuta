package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/platform/httputil"

	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

type qaAskRequest struct {
	Question     string               `json:"question"`
	History      []llm.Message        `json:"history"`
	SessionID    string               `json:"session_id"`
	SourceMode   string               `json:"source_mode"`
	Trace        bool                 `json:"trace"`
	EvidencePlan *domain.EvidencePlan `json:"-"`
}

type qaRunControlReq struct {
	Action  string `json:"action"`
	Message string `json:"message"`
}

type qaHistoryMessage struct {
	memory.SessionMessage
	Evidence *agent.EvidenceMetrics `json:"evidence,omitempty"`
}

type qaHistoryPage struct {
	Messages      []qaHistoryMessage `json:"messages"`
	NextBeforeSeq int                `json:"next_before_seq"`
	HasMore       bool               `json:"has_more"`
}

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

const (
	qaSSEHeartbeatInterval = 10 * time.Second
	qaCompactionMinTimeout = 2 * time.Minute
)

func (handler *Handler) APIQAAsk(w http.ResponseWriter, r *http.Request) {
	req, err := parseQAAskRequest(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	// QA needs a configured LLM to run the agent loop. Reject before the SSE
	// stream starts (headers unflushed) so the client gets a clear status code
	// rather than a faked retrieval-only answer.
	if handler.qa == nil {
		httputil.WriteServiceUnavailable(w, "QA service not initialized")
		return
	}
	stream, err := newSSEWriter(w)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	stopHeartbeat := stream.startHeartbeat(r.Context(), qaSSEHeartbeatInterval)
	defer stopHeartbeat()
	conversation, err := handler.prepareSessionContext(
		r.Context(), req.Question, req.SessionID, currentUserID(r), req.History, stream.emit,
	)
	if err != nil {
		log.ErrorfCtx(r.Context(), "[qa] session compaction failed for %s: %v", req.SessionID, err)
		stream.emit("error", jsonStr(map[string]string{"message": err.Error()}))
		return
	}
	conversation.SessionID = req.SessionID
	handler.serveAgentSSE(r.Context(), req.Question, conversation, req.SessionID, req.Trace, req.EvidencePlan, stream.emit, r)
}

func (handler *Handler) prepareSessionContext(ctx context.Context, question, sessionID string, userID int64,
	fallback []llm.Message, sseEvent func(string, string)) (agent.ConversationContext, error) {
	if sessionID == "" || handler.qaSessions == nil || handler.qa == nil || handler.platform == nil {
		return handler.loadSessionContext(ctx, sessionID, userID, fallback)
	}
	var latestUsage agent.ContextUsageSnapshot
	if runs := handler.qa.RunStore(); runs != nil {
		var err error
		latestUsage, err = runs.LatestContextUsage(userID, sessionID)
		if err != nil {
			return agent.ConversationContext{}, fmt.Errorf("load latest session context usage: %w", err)
		}
	}
	outputReserve := max(
		handler.platform.LLMMaxTokens,
		max(handler.platform.LLMAnswerMaxTokens, handler.platform.LLMConclusionMaxTokens),
	)
	compactionTimeout := max(qaCompactionMinTimeout, time.Duration(handler.platform.AgentTimeout))
	compactCtx, cancel := context.WithTimeout(ctx, compactionTimeout)
	defer cancel()
	result, err := agent.CompactSessionIfNeeded(
		compactCtx, handler.qa.LLM(), handler.qaSessions, sessionID, userID,
		agent.SessionCompactionUsage{
			ContextWindow:              handler.platform.LLMContextWindow,
			PreviousPeakInputTokens:    latestUsage.PeakInputTokens,
			PreviousPeakReservedTokens: latestUsage.PeakReservedTokens,
			OutputReserveTokens:        outputReserve,
		}, question,
		func(fromTurn, toTurn int) {
			sseEvent("compaction", jsonStr(map[string]string{
				"status": "start",
				"text":   fmt.Sprintf("正在压缩第 %d–%d 轮历史上下文…", fromTurn, toTurn),
			}))
		}, handler.history,
	)
	if err != nil {
		handler.emitSessionRestartRecommendation(ctx, sseEvent, sessionID, result, true)
		return agent.ConversationContext{}, fmt.Errorf("prepare session compaction %q: %w", sessionID, err)
	}
	if result.Applied {
		sseEvent("compaction", jsonStr(map[string]string{
			"status": "done",
			"text":   "历史上下文压缩完成",
		}))
		log.InfofCtx(ctx, "[qa] compacted session %s turns %d-%d refs=%d before answer",
			sessionID, result.FromTurn, result.ToTurn, len(result.References))
	} else if result.Stale {
		log.InfofCtx(ctx, "[qa] ignored stale pre-answer compaction for session %s through turn %d",
			sessionID, result.ToTurn)
	}
	handler.emitSessionRestartRecommendation(ctx, sseEvent, sessionID, result, false)
	return handler.loadSessionContext(ctx, sessionID, userID, fallback)
}

func (handler *Handler) emitSessionRestartRecommendation(ctx context.Context, sseEvent func(string, string),
	sessionID string, result agent.SessionCompactionResult, compactionFailed bool) {
	reason, message, recommend := compactionRestartRecommendation(result, compactionFailed)
	if !recommend {
		return
	}
	projectedTokens := result.ProjectedAfterTokens
	if projectedTokens == 0 {
		projectedTokens = result.ProjectedBeforeTokens
	}
	sseEvent("session_restart_recommended", jsonStr(map[string]any{
		"text":                   message,
		"reason":                 reason,
		"archived_turns":         result.ArchivedTurnCount,
		"restart_turn_threshold": result.RestartTurnThreshold,
		"projected_tokens":       projectedTokens,
		"context_window":         handler.platform.LLMContextWindow,
	}))
	log.WarnfCtx(ctx, "[qa] recommended new session session=%s reason=%s projected=%d window=%d archived_turns=%d restart_turn_threshold=%d",
		sessionID, reason, projectedTokens, handler.platform.LLMContextWindow,
		result.ArchivedTurnCount, result.RestartTurnThreshold)
}

func compactionRestartRecommendation(result agent.SessionCompactionResult, compactionFailed bool) (string, string, bool) {
	switch {
	case compactionFailed:
		return "compaction_failed", "历史上下文压缩失败，当前会话无法安全继续，请开启新对话后重试。", true
	case !result.NewSessionRecommended:
		return "", "", false
	case result.CriticalWaterReached:
		return "context_critical", "当前会话压缩后仍接近上下文上限，建议开启新对话继续，避免回答被截断。", true
	default:
		return "archived_history_limit", "当前会话已积累较多压缩历史，建议开启新对话继续，以保持上下文清晰。", true
	}
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func parseQAAskRequest(r *http.Request) (qaAskRequest, error) {
	var req qaAskRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		return qaAskRequest{}, err
	}
	if strings.TrimSpace(req.Question) == "" {
		return qaAskRequest{}, fmt.Errorf("question is required")
	}
	req.Question = strings.TrimSpace(req.Question)
	req.SourceMode = strings.ToLower(strings.TrimSpace(req.SourceMode))
	if req.SourceMode == "" {
		req.SourceMode = "auto"
	}
	if req.SourceMode != "auto" {
		plan, err := domain.ParseEvidencePlan(req.SourceMode)
		if err != nil {
			return qaAskRequest{}, err
		}
		req.EvidencePlan = &plan
	}
	return req, nil
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}
	return &sseWriter{w: w, flusher: flusher}, nil
}

func (s *sseWriter) emit(event, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	s.flusher.Flush()
}

func (s *sseWriter) startHeartbeat(ctx context.Context, interval time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				fmt.Fprint(s.w, ": keepalive\n\n")
				s.flusher.Flush()
				s.mu.Unlock()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (handler *Handler) loadSessionContext(ctx context.Context, sessionID string, userID int64, fallback []llm.Message) (agent.ConversationContext, error) {
	if sessionID == "" || handler.qaSessions == nil {
		return agent.ConversationContext{SessionID: sessionID, Recent: fallback}, nil
	}
	sess, err := handler.qaSessions.GetContextMetadata(sessionID, userID, memory.RecentTurnMetadataLimit)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] session load error: %v", err)
		return agent.ConversationContext{}, fmt.Errorf("load bounded session metadata %q: %w", sessionID, err)
	}
	if sess == nil {
		return agent.ConversationContext{SessionID: sessionID, Recent: fallback}, nil
	}
	log.InfofCtx(ctx, "[qa] loaded session %s: candidateTurns=%d compactedThrough=%d",
		sessionID, len(sess.RecentTurns), sess.CompactedThroughTurn)
	return agent.ConversationContext{
		SessionID: sessionID, SessionTitle: sess.Title, CompactedThroughTurn: sess.CompactedThroughTurn,
		RecentTurns: sess.RecentTurns,
	}, nil
}

func (handler *Handler) serveAgentSSE(ctx context.Context, question string, conversation agent.ConversationContext, sessionID string, traceEnabled bool, evidencePlan *domain.EvidencePlan, sseEvent func(string, string), r *http.Request) {
	userID := currentUserID(r)
	log.InfofCtx(ctx, "[qa] agent mode: question=%q userID=%d", platform.TruncateForLog(question, 12), userID)

	// Subscribe before AskAgent starts.
	// AskAgent emits phase hints during synchronous preprocessing and retrieval.
	// Subscribing later would drop those early updates.
	runID := agent.NewRunID()
	var channel chan agent.SSEEvent
	hub := handler.qa.Hub()
	if hub != nil {
		channel = hub.Subscribe(runID)
		defer hub.Unsubscribe(runID, channel)
	}
	sseEvent("run_start", jsonStr(map[string]any{"run_id": runID, "mode": "single"}))
	var traceRecorder *qaTraceRecorder
	if traceEnabled && hub != nil {
		traceRecorder = &qaTraceRecorder{started: time.Now(), runID: runID, hub: hub}
		ctx = domain.WithTraceRecorder(ctx, traceRecorder)
	}

	user := auth.UserFromContext(r.Context())
	allowWrite := handler.writeAvailable && user != nil && user.IsAdmin
	type askResponse struct {
		result *agent.AskResult
		err    error
	}
	askDone := make(chan askResponse, 1)
	go func() {
		result, err := handler.qa.AskAgentWithContext(ctx, question, conversation, userID, handler.rolePromptFor(userID), runID, evidencePlan, allowWrite)
		askDone <- askResponse{result: result, err: err}
	}()

	var response askResponse
	responseReceived := false
	var answerText string
	var terminal *agent.RunTerminal
	for !responseReceived {
		if channel == nil {
			select {
			case response = <-askDone:
				responseReceived = true
			case <-r.Context().Done():
				return
			}
			continue
		}
		select {
		case response = <-askDone:
			responseReceived = true
		case ev := <-channel:
			answerText = emitHubEvent(answerText, ev, runID, sseEvent)
			if ev.Terminal != nil {
				terminal = ev.Terminal
			}
		case <-r.Context().Done():
			return
		}
	}
	if traceRecorder != nil {
		for _, event := range traceRecorder.Activate() {
			sseEvent("trace", jsonStr(event))
		}
	}
	if response.err != nil {
		log.ErrorfCtx(ctx, "[qa] agent init error: %v", response.err)
		sseEvent("error", jsonStr(map[string]string{"message": response.err.Error()}))
		return
	}
	result := response.result

	if result.Context != nil && len(result.Context.References) > 0 {
		sseEvent("context", jsonStr(map[string]any{
			"references": result.Context.References,
			"hitCount":   result.Context.HitCount,
		}))
	}

	if terminal == nil {
		answerText, terminal = handler.streamAgentEvents(result, channel, answerText, sseEvent, r)
	}
	if r.Context().Err() != nil {
		return
	}
	if terminal != nil && terminal.Status == agent.RunStatusDone && answerText != "" {
		handler.saveTurnToSession(ctx, runID, sessionID, userID, question, answerText, terminal.SessionMessages)
	}
}

func (handler *Handler) APIQASessions(w http.ResponseWriter, r *http.Request) {
	if !handler.ensureQASessions(w) {
		httputil.WriteServiceUnavailable(w, "qa session store not available")
		return
	}
	list, err := handler.qaSessions.List(currentUserID(r))
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, list)
}

// APIQARuntimeStatus serves the composer endpoint and bounded usage snapshot.
func (handler *Handler) APIQARuntimeStatus(w http.ResponseWriter, r *http.Request) {
	query := httputil.Query(r)
	sessionID := query.Str("session_id")
	runID := query.Str("run_id")
	if err := query.Err(); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}

	var usage agent.RunUsageSummary
	usageAvailable := false
	if runs := handler.runStore(); runs != nil {
		var err error
		usage, err = runs.UsageSummary(r.Context(), currentUserID(r), sessionID, runID)
		if err != nil {
			httputil.WriteErr(w, err)
			return
		}
		usageAvailable = true
	}

	status := "deactive"
	endpointDomain := ""
	model := ""
	roundMaxTokens := 0
	if handler.platform != nil {
		endpointDomain = qaEndpointDomain(handler.platform.LLMBaseURL)
		model = handler.platform.LLMModel
		roundMaxTokens = handler.platform.LLMContextWindow
		if handler.platform.LLMEnabled() {
			status = "active"
		}
	}
	roundCurrentTokens := max(usage.RoundPeakInputTokens, usage.RoundPeakReservedTokens)
	httputil.WriteJSON(w, map[string]any{
		"endpoint_domain":           endpointDomain,
		"endpoint_status":           status,
		"model":                     model,
		"token_usage_available":     usageAvailable,
		"cache_percent":             cachePercent(usage.RoundCachedInputTokens, usage.RoundInputTokens),
		"session_total_tokens":      usage.SessionTotalTokens,
		"round_total_tokens":        usage.RoundTotalTokens,
		"round_current_tokens":      roundCurrentTokens,
		"round_max_tokens":          roundMaxTokens,
		"round_input_tokens":        usage.RoundInputTokens,
		"round_cached_input_tokens": usage.RoundCachedInputTokens,
	})
}

func qaEndpointDomain(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func cachePercent(cachedTokens, inputTokens int64) int {
	if inputTokens <= 0 {
		return 0
	}
	return int((cachedTokens*100 + inputTokens/2) / inputTokens)
}

func (handler *Handler) APIQAMemories(w http.ResponseWriter, r *http.Request) {
	store := handler.memoryStore()
	if store == nil {
		httputil.WriteServiceUnavailable(w, "memory store not available")
		return
	}
	userID := currentUserID(r)
	if userID <= 0 {
		httputil.WriteUnauthorized(w, "authenticated user is required")
		return
	}
	query := httputil.Query(r)
	limit := query.Int("limit", 20)
	options := memory.ListOptions{
		Limit:  limit,
		Cursor: query.Str("cursor"),
		Kind:   memory.MemoryKind(query.Str("kind")),
		Status: memory.MemoryStatus(query.Str("status")),
	}
	if err := query.Err(); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	page, err := store.List(r.Context(), userID, options)
	if err != nil {
		if errors.Is(err, memory.ErrInvalidMemoryQuery) {
			httputil.WriteBadRequest(w, err.Error())
		} else {
			httputil.WriteErr(w, err)
		}
		return
	}
	httputil.WriteJSON(w, page)
}

func (handler *Handler) APIQAMemoryDelete(w http.ResponseWriter, r *http.Request) {
	store := handler.memoryStore()
	if store == nil {
		httputil.WriteServiceUnavailable(w, "memory store not available")
		return
	}
	userID := currentUserID(r)
	if userID <= 0 {
		httputil.WriteUnauthorized(w, "authenticated user is required")
		return
	}
	deleted, err := store.Delete(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if !deleted {
		httputil.WriteErrStatus(w, http.StatusNotFound, fmt.Errorf("memory not found"))
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "deleted"})
}

func (handler *Handler) APIQAMemoriesClear(w http.ResponseWriter, r *http.Request) {
	store := handler.memoryStore()
	if store == nil {
		httputil.WriteServiceUnavailable(w, "memory store not available")
		return
	}
	userID := currentUserID(r)
	if userID <= 0 {
		httputil.WriteUnauthorized(w, "authenticated user is required")
		return
	}
	deleted, err := store.Clear(r.Context(), userID)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"status": "deleted", "deleted": deleted})
}

func (handler *Handler) APIQASessionMessages(w http.ResponseWriter, r *http.Request) {
	if !handler.ensureQASessions(w) {
		httputil.WriteServiceUnavailable(w, "qa session store not available")
		return
	}
	q := httputil.Query(r)
	beforeSeq := q.Int("before_seq", -1)
	limit := q.Int("limit", 20)
	turnLimit := q.Int("turn_limit", 0)
	if err := q.Err(); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if beforeSeq < -1 {
		httputil.WriteBadRequest(w, "before_seq must be -1 or greater")
		return
	}
	if limit <= 0 || limit > 100 {
		httputil.WriteBadRequest(w, "limit must be between 1 and 100")
		return
	}
	if turnLimit < 0 || turnLimit > 100 {
		httputil.WriteBadRequest(w, "turn_limit must be between 1 and 100")
		return
	}
	var page *memory.MessagePage
	var err error
	if turnLimit > 0 {
		page, err = handler.qaSessions.ListTurnsBefore(r.PathValue("id"), currentUserID(r), beforeSeq, turnLimit)
	} else {
		page, err = handler.qaSessions.ListMessagesBefore(r.PathValue("id"), currentUserID(r), beforeSeq, limit)
	}
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	historyPage, err := handler.qaHistoryPage(r.Context(), currentUserID(r), r.PathValue("id"), page)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, historyPage)
}

func (handler *Handler) qaHistoryPage(ctx context.Context, userID int64, sessionID string, page *memory.MessagePage) (*qaHistoryPage, error) {
	result := &qaHistoryPage{
		Messages:      make([]qaHistoryMessage, len(page.Messages)),
		NextBeforeSeq: page.NextBeforeSeq,
		HasMore:       page.HasMore,
	}
	runSet := make(map[string]struct{}, len(page.Messages)/2)
	runIDs := make([]string, 0, len(page.Messages)/2)
	for i, message := range page.Messages {
		result.Messages[i].SessionMessage = message
		if message.RunID == "" {
			continue
		}
		if _, exists := runSet[message.RunID]; exists {
			continue
		}
		runSet[message.RunID] = struct{}{}
		runIDs = append(runIDs, message.RunID)
	}
	runs := handler.runStore()
	if len(runIDs) == 0 || runs == nil {
		return result, nil
	}
	evidenceByRun, err := runs.EvidenceByIDs(userID, sessionID, runIDs)
	if err != nil {
		return nil, fmt.Errorf("load QA history evidence: %w", err)
	}
	for i := range result.Messages {
		metrics, ok := evidenceByRun[result.Messages[i].RunID]
		if ok {
			result.Messages[i].Evidence = &metrics
		}
	}
	return result, nil
}

func (handler *Handler) APIQASessionSave(w http.ResponseWriter, r *http.Request) {
	if !handler.ensureQASessions(w) {
		httputil.WriteServiceUnavailable(w, "qa session store not available")
		return
	}
	var rec memory.SessionRecord
	if err := httputil.DecodeJSON(r, &rec); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if rec.ID == "" {
		httputil.WriteBadRequest(w, "id is required")
		return
	}
	rec.UserID = currentUserID(r)
	if err := handler.qaSessions.Save(rec); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "saved"})
}

func (handler *Handler) APIQASessionDelete(w http.ResponseWriter, r *http.Request) {
	if !handler.ensureQASessions(w) {
		httputil.WriteServiceUnavailable(w, "qa session store not available")
		return
	}
	id := r.PathValue("id")
	userID := currentUserID(r)
	deleted, err := handler.qaSessions.Delete(id, userID)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if !deleted {
		httputil.WriteErrStatus(w, http.StatusNotFound, fmt.Errorf("session not found"))
		return
	}
	if m := handler.memoryStore(); m != nil {
		if _, err := m.DeleteBySession(r.Context(), userID, id); err != nil {
			log.ErrorfCtx(r.Context(), "[qa] delete memories for session %s: %v", id, err)
		}
	}
	if rs := handler.qa.RunStore(); rs != nil {
		if err := rs.DeleteBySession(id, userID); err != nil {
			log.ErrorfCtx(r.Context(), "[qa] delete runs for session %s: %v", id, err)
		}
	}
	httputil.WriteJSON(w, map[string]string{"status": "deleted"})
}

func (handler *Handler) saveTurnToSession(ctx context.Context, runID, sessionID string, userID int64, question, answer string, toolMessages []llm.Message) {
	if handler.qaSessions == nil || sessionID == "" || answer == "" {
		return
	}
	if err := handler.qaSessions.EnsureSession(sessionID, userID, platform.TruncateForLog(question, 512)); err != nil {
		log.ErrorfCtx(ctx, "[qa] ensure session %s failed: %v", sessionID, err)
		return
	}
	messages := make([]llm.Message, 0, len(toolMessages)+2)
	messages = append(messages, llm.Message{Role: "user", Content: question})
	messages = append(messages, toolMessages...)
	messages = append(messages, llm.Message{Role: "assistant", Content: answer})
	turnNo, err := handler.qaSessions.AppendTurn(sessionID, runID, userID, messages)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] failed to append messages to session %s: %v", sessionID, err)
		return
	}
	log.InfofCtx(ctx, "[qa] saved turn %d to session %s", turnNo, sessionID)
	handler.archiveSessionHistoryAfterTurn(ctx, sessionID, userID)
}

func (handler *Handler) archiveSessionHistoryAfterTurn(ctx context.Context, sessionID string, userID int64) {
	if handler.qa == nil || handler.qaSessions == nil || handler.platform == nil {
		return
	}
	timeout := max(qaCompactionMinTimeout, time.Duration(handler.platform.AgentTimeout))
	archiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := agent.ArchiveSessionHistoryIfNeeded(
		archiveCtx, handler.qa.LLM(), handler.qaSessions, sessionID, userID,
		handler.platform.LLMContextWindow, handler.history,
	)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] post-turn history archive failed for %s: %v", sessionID, err)
		return
	}
	if result.Applied {
		log.InfofCtx(ctx, "[qa] archived session %s turns %d-%d after saved turn",
			sessionID, result.FromTurn, result.ToTurn)
	} else if result.Stale {
		log.InfofCtx(ctx, "[qa] ignored stale post-turn archive for session %s through turn %d",
			sessionID, result.ToTurn)
	}
}

func (handler *Handler) APIQARuns(w http.ResponseWriter, r *http.Request) {
	runs := handler.runStore()
	if runs == nil {
		httputil.WriteServiceUnavailable(w, "run store not available")
		return
	}
	q := httputil.Query(r)
	page, pageSize := q.Page(20, 200)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	list, err := runs.ListPage(currentUserID(r), q.Str("session_id"), agent.RunStatus(q.Str("status")), page, pageSize)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, list)
}

func (handler *Handler) APIQARunGet(w http.ResponseWriter, r *http.Request) {
	runs := handler.runStore()
	if runs == nil {
		httputil.WriteServiceUnavailable(w, "run store not available")
		return
	}
	detail, err := runs.Get(r.PathValue("id"))
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, detail)
}

func (handler *Handler) APIQARunControl(w http.ResponseWriter, r *http.Request) {
	hub := handler.qaHub()
	runs := handler.runStore()
	if hub == nil || runs == nil {
		httputil.WriteServiceUnavailable(w, "run control not available")
		return
	}
	var req qaRunControlReq
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	runID := r.PathValue("id")
	run, err := runs.Get(runID)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if userID := currentUserID(r); userID != 0 && run.UserID != userID {
		httputil.WriteErrStatus(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	switch req.Action {
	case "pause":
		if run.Status != agent.RunStatusRunning {
			httputil.WriteBadRequest(w, "only a running run can be paused")
			return
		}
		hub.Send(runID, agent.ControlSignal{Kind: agent.CtrlPause})
	case "resume":
		if run.Status != agent.RunStatusRunning && run.Status != agent.RunStatusPaused {
			httputil.WriteBadRequest(w, "only an active run can be resumed")
			return
		}
		if err := hub.Resume(runID); err != nil {
			httputil.WriteErr(w, err)
			return
		}
	case "abort":
		if run.Status != agent.RunStatusRunning && run.Status != agent.RunStatusPaused {
			httputil.WriteBadRequest(w, "only an active run can be aborted")
			return
		}
		hub.Send(runID, agent.ControlSignal{Kind: agent.CtrlAbort})
	case "nudge":
		if run.Status != agent.RunStatusRunning && run.Status != agent.RunStatusPaused {
			httputil.WriteBadRequest(w, "only an active run can be nudged")
			return
		}
		hub.Send(runID, agent.ControlSignal{Kind: agent.CtrlNudge, Message: req.Message})
	default:
		httputil.WriteBadRequest(w, "unknown action: "+req.Action)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "sent"})
}

func (handler *Handler) streamAgentEvents(result *agent.AskResult, hubCh chan agent.SSEEvent, answerText string, sseEvent func(string, string), r *http.Request) (string, *agent.RunTerminal) {
	for {
		if hubCh == nil {
			return answerText, nil
		}
		select {
		case ev, ok := <-hubCh:
			if !ok {
				return answerText, nil
			}
			answerText = emitHubEvent(answerText, ev, result.RunID, sseEvent)
			if ev.Terminal != nil {
				return answerText, ev.Terminal
			}
		case <-r.Context().Done():
			return answerText, nil
		}
	}
}

func emitHubEvent(answerText string, ev agent.SSEEvent, runID string, sseEvent func(string, string)) string {
	if ev.Trace != nil {
		sseEvent("trace", jsonStr(ev.Trace))
	}
	if ev.Step != nil {
		switch ev.Step.Kind {
		case agent.StepKindThink:
			sseEvent("progress", jsonStr(map[string]any{"step": ev.Step.StepNo, "text": ev.Step.Content}))
		case agent.StepKindToolCall:
			sseEvent("tool", jsonStr(map[string]any{"step": ev.Step.StepNo, "name": ev.Step.Tool, "args": ev.Step.Args}))
		case agent.StepKindToolResult:
			sseEvent("tool_result", jsonStr(map[string]any{"step": ev.Step.StepNo, "tool": ev.Step.Tool, "summary": ev.Step.ResultSummary, "duration_ms": ev.Step.DurationMs}))
		case agent.StepKindAnswer:
		}
	}
	if ev.Token != "" {
		answerText += ev.Token
		sseEvent("token", jsonStr(map[string]string{"text": ev.Token}))
	}
	if ev.Reasoning != "" {
		sseEvent("reasoning", jsonStr(map[string]string{"text": ev.Reasoning}))
	}
	if ev.LLMCall != nil {
		sseEvent("llm_timing", jsonStr(ev.LLMCall))
	}
	if ev.Phase != "" {
		sseEvent("phase", jsonStr(map[string]string{"text": ev.Phase}))
	}
	if ev.Terminal != nil {
		sseEvent("run_end", jsonStr(map[string]any{
			"run_id": runID, "status": ev.Terminal.Status, "evidence": ev.Terminal.Evidence,
		}))
		if ev.Terminal.Status == agent.RunStatusDone {
			sseEvent("done", "{}")
		} else {
			message := ev.Terminal.Error
			if message == "" {
				message = "run " + string(ev.Terminal.Status)
			}
			sseEvent("error", jsonStr(map[string]string{"message": message}))
		}
	}
	return answerText
}

type qaTraceRecorder struct {
	mu       sync.Mutex
	started  time.Time
	sequence int
	runID    string
	hub      *agent.RunHub
	live     bool
	buffered []domain.EvaluationTrace
}

func (recorder *qaTraceRecorder) RecordTrace(event domain.EvaluationTrace) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.sequence++
	event.Sequence = recorder.sequence
	if event.ElapsedMS == 0 {
		event.ElapsedMS = time.Since(recorder.started).Milliseconds()
	}
	if !recorder.live {
		recorder.buffered = append(recorder.buffered, event)
		return
	}
	recorder.hub.EmitTrace(recorder.runID, event)
}

func (recorder *qaTraceRecorder) Activate() []domain.EvaluationTrace {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.live = true
	events := append([]domain.EvaluationTrace(nil), recorder.buffered...)
	recorder.buffered = nil
	return events
}

func currentUserID(r *http.Request) int64 {
	if u := auth.UserFromContext(r.Context()); u != nil {
		return u.ID
	}
	return 0
}

func (handler *Handler) ensureQASessions(w http.ResponseWriter) bool {
	return handler.qaSessions != nil
}

func (handler *Handler) runStore() *agent.RunStore {
	return handler.persistentRunStore
}

func (handler *Handler) memoryStore() *memory.MemoryStore {
	if handler.qa == nil {
		return nil
	}
	return handler.qa.Memory()
}

func (handler *Handler) qaHub() *agent.RunHub {
	if handler.qa == nil {
		return nil
	}
	return handler.qa.Hub()
}

package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/platform/httputil"

	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

type qaAskRequest struct {
	Question       string               `json:"question"`
	History        []llm.Message        `json:"history"`
	SessionID      string               `json:"session_id"`
	SourceMode     string               `json:"source_mode"`
	Trace          bool                 `json:"trace"`
	WriteRequested bool                 `json:"write_requested"`
	EvidencePlan   *domain.EvidencePlan `json:"-"`
}

type qaRunControlReq struct {
	Action  string `json:"action"`
	Message string `json:"message"`
}

type qaMessageFeedbackRequest struct {
	MessageSeq int    `json:"message_seq"`
	RunID      string `json:"run_id"`
	Feedback   string `json:"feedback"`
}

type qaHistoryMessage struct {
	memory.SessionMessage
	Evidence *agentrun.EvidenceMetrics `json:"evidence,omitempty"`
}

type qaHistoryPage struct {
	Messages      []qaHistoryMessage `json:"messages"`
	NextBeforeSeq int                `json:"next_before_seq"`
	HasMore       bool               `json:"has_more"`
}

type sseWriter struct {
	w      http.ResponseWriter
	mu     sync.Mutex
	failed chan struct{}
	once   sync.Once
	err    error
}

const (
	qaSSEHeartbeatInterval = 10 * time.Second
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
	if handler.qaService() == nil {
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
	conversation, err := handler.loadSessionContext(
		r.Context(), req.SessionID, currentUserID(r), req.History,
	)
	if err != nil {
		log.ErrorfCtx(r.Context(), "[qa] session context preparation failed for %s: %v", req.SessionID, err)
		_ = stream.emit("run.finished", agentrun.Terminal{Status: agentrun.StatusFailed, Error: err.Error()})
		return
	}
	conversation.SessionID = req.SessionID
	handler.serveAgentSSE(
		r.Context(), req.Question, conversation, req.SessionID, req.Trace,
		req.EvidencePlan, req.WriteRequested, stream.emit, r,
	)
}

func (handler *Handler) emitSessionRestartRecommendation(ctx context.Context, sseEvent func(string, any) error,
	sessionID string, result agent.SessionCompactionResult, compactionFailed bool) {
	reason, message, recommend := compactionRestartRecommendation(result, compactionFailed)
	if !recommend {
		return
	}
	projectedTokens := result.ProjectedAfterTokens
	if projectedTokens == 0 {
		projectedTokens = result.ProjectedBeforeTokens
	}
	if err := sseEvent("session.restart_recommended", map[string]any{
		"text":                   message,
		"reason":                 reason,
		"archived_turns":         result.ArchivedTurnCount,
		"restart_turn_threshold": result.RestartTurnThreshold,
		"projected_tokens":       projectedTokens,
		"context_window":         handler.platformSettings().LLMContextWindow,
	}); err != nil {
		log.WarnfCtx(ctx, "[qa] emit session restart recommendation failed session=%s: %v", sessionID, err)
		return
	}
	log.WarnfCtx(ctx, "[qa] recommended new session session=%s reason=%s projected=%d window=%d archived_turns=%d restart_turn_threshold=%d",
		sessionID, reason, projectedTokens, handler.platformSettings().LLMContextWindow,
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
	if _, ok := w.(http.Flusher); !ok {
		return nil, fmt.Errorf("streaming not supported")
	}
	return &sseWriter{w: w, failed: make(chan struct{})}, nil
}

func (s *sseWriter) emit(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return s.fail(fmt.Errorf("encode SSE event %q: %w", event, err))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return s.failLocked(fmt.Errorf("write SSE event %q: %w", event, err))
	}
	if err := http.NewResponseController(s.w).Flush(); err != nil {
		return s.failLocked(fmt.Errorf("flush SSE event %q: %w", event, err))
	}
	return nil
}

func (s *sseWriter) fail(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failLocked(err)
}

func (s *sseWriter) failLocked(err error) error {
	s.once.Do(func() {
		s.err = err
		close(s.failed)
	})
	return s.err
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
			case <-s.failed:
				return
			case <-ticker.C:
				s.mu.Lock()
				if s.err == nil {
					if _, err := fmt.Fprint(s.w, ": keepalive\n\n"); err != nil {
						s.failLocked(fmt.Errorf("write SSE heartbeat: %w", err))
					} else if err := http.NewResponseController(s.w).Flush(); err != nil {
						s.failLocked(fmt.Errorf("flush SSE heartbeat: %w", err))
					}
				}
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
	sessions := handler.qaSessionStore()
	if sessionID == "" || sessions == nil {
		return agent.ConversationContext{SessionID: sessionID, Recent: fallback}, nil
	}
	sess, err := sessions.GetContextSnapshot(
		sessionID, userID, memory.RecentTurnMetadataLimit, memory.RecentDialogueTurnLimit,
	)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] session load error: %v", err)
		return agent.ConversationContext{}, fmt.Errorf("load bounded session context %q: %w", sessionID, err)
	}
	if sess == nil {
		return agent.ConversationContext{SessionID: sessionID, Recent: fallback}, nil
	}
	log.InfofCtx(ctx, "[qa] loaded session %s: candidateTurns=%d recentDialogue=%d compactedThrough=%d",
		sessionID, len(sess.RecentTurns), len(sess.RecentDialogue), sess.CompactedThroughTurn)
	return agent.ConversationContext{
		SessionID: sessionID, SessionTitle: sess.Title, CompactedThroughTurn: sess.CompactedThroughTurn,
		RecentTurns: sess.RecentTurns, RecentDialogue: sess.RecentDialogue,
	}, nil
}

func (handler *Handler) serveAgentSSE(ctx context.Context, question string, conversation agent.ConversationContext, sessionID string, traceEnabled bool, evidencePlan *domain.EvidencePlan, writeRequested bool, allowEmit func(string, any) error, r *http.Request) {
	runtime := handler.currentQARuntime()
	if runtime.QA == nil {
		_ = allowEmit("run.finished", agentrun.Terminal{Status: agentrun.StatusFailed, Error: "QA service not initialized"})
		return
	}
	userID := currentUserID(r)
	log.InfofCtx(ctx, "[qa] agent mode: question=%q userID=%d", platform.TruncateForLog(question, 12), userID)
	runCtx := context.WithoutCancel(ctx)
	sseEvent := func(event string, payload any) bool {
		if err := allowEmit(event, payload); err != nil {
			log.WarnfCtx(ctx, "[qa] SSE projection failed session=%s event=%s: %v", sessionID, event, err)
			return false
		}
		return true
	}

	// Subscribe before AskAgent starts.
	// AskAgent emits phase hints during synchronous preprocessing and retrieval.
	// Subscribing later would drop those early updates.
	runID := agent.NewRunID()
	var channel chan agentrun.SSEEvent
	hub := runtime.Hub
	if hub != nil {
		channel = hub.Subscribe(runID)
		defer hub.Unsubscribe(runID, channel)
	}
	if !sseEvent("run.started", map[string]any{"run_id": runID}) {
		return
	}
	if traceEnabled && hub != nil {
		runCtx = runtrace.WithEvaluation(runCtx, func(event domain.EvaluationTrace) {
			hub.EmitTrace(runID, event)
		})
	}

	user := auth.UserFromContext(r.Context())
	writeAuthorized := runtime.WriteAvailable && user != nil && user.IsAdmin
	type askResponse struct {
		result *agent.AskResult
		err    error
	}
	askDone := make(chan askResponse, 1)
	go func() {
		result, err := runtime.QA.Ask(runCtx, agent.QARequest{
			Question: question, Conversation: conversation, UserID: userID,
			RolePrompt: handler.rolePromptFor(userID), RunID: runID,
			EvidencePlan: evidencePlan, WriteAuthorized: writeAuthorized,
			WriteRequested: writeRequested,
		})
		askDone <- askResponse{result: result, err: err}
	}()

	// Ask submits the run and returns once the agent goroutine starts, so askDone
	// arrives long before run.finished. Both signals drive one loop: the hub
	// channel streams every event through to the terminal one, while askDone only
	// carries submission failure and the retrieved-context payload. Receiving on a
	// nil channel never becomes ready, so a run without a hub relies on askDone
	// alone — and since no run.finished can follow, it ends the stream there.
	var terminal *agentrun.Terminal
	for terminal == nil {
		select {
		case response := <-askDone:
			if response.err != nil {
				log.ErrorfCtx(ctx, "[qa] agent init error: %v", response.err)
				sseEvent("run.finished", &agentrun.Terminal{
					RunID: runID, Status: agentrun.StatusFailed, Error: response.err.Error(),
				})
				return
			}
			if rc := response.result.Context; rc != nil && len(rc.References) > 0 {
				if !sseEvent("context", map[string]any{
					"references": rc.References,
					"hitCount":   rc.HitCount,
				}) {
					return
				}
			}
			if channel == nil {
				return
			}
		case ev, ok := <-channel:
			if !ok {
				return
			}
			if ev.Type == agentrun.EventRunFinished {
				projected, err := handler.projectInvestigationTerminal(
					runCtx, runID, userID, ev.Data,
					runtime,
				)
				if err != nil {
					log.ErrorfCtx(runCtx, "[qa] load investigation delivery for SSE run %s: %v", runID, err)
					projected = &agentrun.Terminal{
						RunID: runID, Status: agentrun.StatusFailed,
						ErrorCode: "investigation_delivery_unavailable", Error: err.Error(),
					}
				}
				if !sseEvent(string(agentrun.EventRunFinished), projected) {
					return
				}
				terminal = projected
				continue
			}
			if !emitHubEvent(ev, sseEvent) {
				return
			}
			terminal = agentrun.TerminalFromEvent(ev)
		case <-r.Context().Done():
			return
		}
	}
}

func (handler *Handler) APIQASessions(w http.ResponseWriter, r *http.Request) {
	if !handler.ensureQASessions(w) {
		httputil.WriteServiceUnavailable(w, "qa session store not available")
		return
	}
	list, err := handler.qaSessionStore().List(currentUserID(r))
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

	var usage agentrun.UsageSummary
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

	runtime := handler.currentQARuntime()
	contextUsage := agentrun.ContextUsageEvent{}
	contextUsageAvailable := false
	contextRunID := runID
	if contextRunID == "" {
		contextRunID = usage.RunID
	}
	if runtime.Hub != nil && contextRunID != "" {
		contextUsage, contextUsageAvailable = runtime.Hub.ContextUsage(contextRunID)
	}

	status := "deactive"
	endpointDomain := ""
	model := ""
	roundMaxTokens := 0
	roundContextWindowSource := "platform"
	settings := handler.platformSettings()
	if settings != nil {
		endpointDomain = qaEndpointDomain(settings.LLMBaseURL)
		model = settings.LLMModel
		roundMaxTokens = settings.LLMContextWindow
		if settings.LLMEnabled() {
			status = "active"
		}
	}
	if contextUsageAvailable && contextUsage.ContextWindow > 0 {
		roundMaxTokens = contextUsage.ContextWindow
		roundContextWindowSource = "run"
	}
	var compactionStatus agentrun.SessionStatusEvent
	if runtime.QA != nil {
		compactionStatus = runtime.QA.CompactionStatus(sessionID)
	}
	roundActualInputTokens := usage.RoundPeakInputTokens
	roundActualReservedTokens := max(usage.RoundPeakInputTokens, usage.RoundPeakReservedTokens)
	roundProjectedTokens := contextUsage.PeakProjectedTokens
	roundProjectedAfterTokens := contextUsage.ProjectedAfterTokens
	roundHighWaterTokens := contextUsage.HighWaterTokens
	roundSafetyTokens := contextUsage.SafetyTokens
	roundSafeLimitTokens := contextUsage.SafeLimitTokens
	roundOutputReserveTokens := contextUsage.OutputReserveTokens
	if roundHighWaterTokens == 0 {
		roundHighWaterTokens = agentrun.ContextHighWaterTokens(roundMaxTokens)
	}
	if roundSafetyTokens == 0 {
		roundSafetyTokens = agentrun.ContextSafetyTokens(roundMaxTokens)
	}
	if roundSafeLimitTokens == 0 {
		roundSafeLimitTokens = agentrun.ContextSafeLimitTokens(roundMaxTokens)
	}
	roundCurrentTokens := int64(roundProjectedTokens)
	if roundCurrentTokens == 0 {
		roundCurrentTokens = roundActualReservedTokens
	}
	httputil.WriteJSON(w, map[string]any{
		"endpoint_domain":              endpointDomain,
		"endpoint_status":              status,
		"model":                        model,
		"token_usage_available":        usageAvailable || contextUsageAvailable,
		"cache_percent":                cachePercent(usage.RoundCachedInputTokens, usage.RoundInputTokens),
		"session_total_tokens":         usage.SessionTotalTokens,
		"round_total_tokens":           usage.RoundTotalTokens,
		"round_current_tokens":         roundCurrentTokens,
		"round_max_tokens":             roundMaxTokens,
		"round_context_window_source":  roundContextWindowSource,
		"round_input_tokens":           usage.RoundInputTokens,
		"round_cached_input_tokens":    usage.RoundCachedInputTokens,
		"round_projected_tokens":       roundProjectedTokens,
		"round_projected_after_tokens": roundProjectedAfterTokens,
		"round_actual_input_tokens":    roundActualInputTokens,
		"round_actual_reserved_tokens": roundActualReservedTokens,
		"round_high_water_tokens":      roundHighWaterTokens,
		"round_safe_limit_tokens":      roundSafeLimitTokens,
		"round_safety_tokens":          roundSafetyTokens,
		"round_output_reserve_tokens":  roundOutputReserveTokens,
		"session_compaction":           compactionStatus,
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
	sessions := handler.qaSessionStore()
	if turnLimit > 0 {
		page, err = sessions.ListTurnsBefore(r.PathValue("id"), currentUserID(r), beforeSeq, turnLimit)
	} else {
		page, err = sessions.ListMessagesBefore(r.PathValue("id"), currentUserID(r), beforeSeq, limit)
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

func (handler *Handler) APIQAMessageFeedback(w http.ResponseWriter, r *http.Request) {
	if !handler.ensureQASessions(w) {
		httputil.WriteServiceUnavailable(w, "qa session store not available")
		return
	}
	var req qaMessageFeedbackRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	req.RunID = strings.TrimSpace(req.RunID)
	req.Feedback = strings.TrimSpace(req.Feedback)
	if req.MessageSeq <= 0 && req.RunID == "" {
		httputil.WriteBadRequest(w, "message_seq or run_id is required")
		return
	}
	if req.MessageSeq > 0 && req.RunID != "" {
		httputil.WriteBadRequest(w, "message_seq and run_id are mutually exclusive")
		return
	}
	if req.Feedback != "" && req.Feedback != "like" && req.Feedback != "dislike" {
		httputil.WriteBadRequest(w, "feedback must be like, dislike, or empty")
		return
	}
	updated, err := handler.qaSessionStore().SetMessageFeedback(
		r.PathValue("id"), currentUserID(r), req.MessageSeq, req.RunID, req.Feedback,
	)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if !updated {
		httputil.WriteErrStatus(w, http.StatusNotFound, fmt.Errorf("assistant answer not found"))
		return
	}
	httputil.WriteJSON(w, map[string]string{"feedback": req.Feedback})
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
	if err := handler.qaSessionStore().Save(rec); err != nil {
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
	deleted, err := handler.qaSessionStore().Delete(id, userID)
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
	if rs := handler.runStore(); rs != nil {
		if err := rs.DeleteBySession(id, userID); err != nil {
			log.ErrorfCtx(r.Context(), "[qa] delete runs for session %s: %v", id, err)
		}
	}
	httputil.WriteJSON(w, map[string]string{"status": "deleted"})
}

func runQACompactionAsync(ctx context.Context, timeout time.Duration, compact func(context.Context)) {
	compactCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	go func() {
		defer cancel()
		compact(compactCtx)
	}()
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
	list, err := runs.ListPage(currentUserID(r), q.Str("session_id"), agentrun.Status(q.Str("status")), page, pageSize)
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
	detail, err := runs.GetForUser(r.PathValue("id"), currentUserID(r))
	if errors.Is(err, sql.ErrNoRows) {
		httputil.WriteErrStatus(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	response := struct {
		*agentrun.Detail
		InvestigationDelivery *investigation.DeliveryResult `json:"investigation_delivery,omitempty"`
	}{Detail: detail}
	// A delivered parent is projected from the investigation delivery snapshot. The old
	// QA terminal is retained only as the parent lifecycle record and is not the
	// source of the public investigation answer.
	if detail.RunKind == agentrun.KindQAParent &&
		detail.Status == agentrun.StatusDone &&
		detail.WorkflowRunID != "" {
		runtime := handler.currentQARuntime()
		if runtime.InvestigationReader != nil {
			delivery, deliveryErr := runtime.InvestigationReader.LoadDelivery(
				r.Context(), detail.WorkflowRunID,
			)
			if deliveryErr != nil {
				httputil.WriteErr(w, fmt.Errorf("load investigation delivery %q: %w", detail.WorkflowRunID, deliveryErr))
				return
			}
			response.InvestigationDelivery = &delivery
			if detail.Terminal != nil {
				projected := projectInvestigationDelivery(*detail.Terminal, delivery)
				detail.Terminal = &projected
			}
		}
	}
	httputil.WriteJSON(w, response)
}

// APIQARunEvents returns a bounded durable Parent event page.
func (handler *Handler) APIQARunEvents(w http.ResponseWriter, r *http.Request) {
	runs := handler.runStore()
	if runs == nil {
		httputil.WriteServiceUnavailable(w, "run store not available")
		return
	}
	afterSeq, err := parseQAEventCursor(r.URL.Query().Get("after_seq"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	query := httputil.Query(r)
	limit := query.Int("limit", 50)
	if query.Err() != nil || limit <= 0 || limit > 200 {
		if query.Err() != nil {
			httputil.WriteBadRequest(w, query.Err().Error())
		} else {
			httputil.WriteBadRequest(w, "limit must be between 1 and 200")
		}
		return
	}
	items, err := runs.ListParentEvents(
		r.Context(),
		r.PathValue("id"),
		currentUserID(r),
		afterSeq,
		limit,
	)
	if errors.Is(err, sql.ErrNoRows) {
		httputil.WriteErrStatus(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	nextAfterSeq := afterSeq
	if len(items) > 0 {
		nextAfterSeq = items[len(items)-1].Seq
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_after_seq": nextAfterSeq,
	})
}

func parseQAEventCursor(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	sequence, err := strconv.ParseInt(value, 10, 64)
	if err != nil || sequence < 0 {
		return 0, fmt.Errorf("after_seq must be a non-negative integer")
	}
	return sequence, nil
}

// APIQAToolResultArtifact reads a bounded chunk of one authoritative tool result.
func (handler *Handler) APIQAToolResultArtifact(w http.ResponseWriter, r *http.Request) {
	runs := handler.runStore()
	if runs == nil {
		httputil.WriteServiceUnavailable(w, "run store not available")
		return
	}
	q := httputil.Query(r)
	offset := q.Int("offset", 0)
	limit := q.Int("limit", 64<<10)
	if q.Err() != nil || offset < 0 || limit <= 0 {
		if q.Err() != nil {
			httputil.WriteBadRequest(w, q.Err().Error())
		} else {
			httputil.WriteBadRequest(w, "offset must be non-negative and limit must be positive")
		}
		return
	}
	artifact, err := runs.GetToolArtifact(
		currentUserID(r), q.Str("session_id"), r.PathValue("id"), int64(offset), limit,
	)
	if errors.Is(err, sql.ErrNoRows) {
		httputil.WriteErrStatus(w, http.StatusNotFound, errors.New("artifact not found"))
		return
	}
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, artifact)
}

func (handler *Handler) APIQARunControl(w http.ResponseWriter, r *http.Request) {
	runtime := handler.currentQARuntime()
	if runtime.RunStore == nil {
		httputil.WriteServiceUnavailable(w, "run control not available")
		return
	}
	var req qaRunControlReq
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	runID := r.PathValue("id")
	userID := currentUserID(r)
	record, err := runtime.RunStore.GetControlForUser(runID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		httputil.WriteErrStatus(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	switch record.RunKind {
	case agentrun.KindAgent:
		if runtime.Hub == nil {
			httputil.WriteServiceUnavailable(w, "agent run control not available")
			return
		}
		if !controlAgentRun(runtime.Hub, record, req, w) {
			return
		}
	case agentrun.KindQAParent:
		if req.Action != "abort" {
			httputil.WriteBadRequest(w, "QA parent runs only support abort")
			return
		}
		if !activeRunStatus(record.Status) {
			httputil.WriteBadRequest(w, "only an active run can be aborted")
			return
		}
		if runtime.InvestigationCanceller == nil {
			httputil.WriteServiceUnavailable(w, "QA investigation cancellation not available")
			return
		}
		if err := runtime.InvestigationCanceller.Cancel(r.Context(), runID, userID); err != nil {
			httputil.WriteErr(w, err)
			return
		}
	default:
		httputil.WriteErr(w, fmt.Errorf(
			"run %q has unsupported kind %q",
			record.ID,
			record.RunKind,
		))
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "sent"})
}

func controlAgentRun(
	hub *agentrun.Hub,
	record agentrun.ControlRecord,
	req qaRunControlReq,
	w http.ResponseWriter,
) bool {
	switch req.Action {
	case "pause":
		if record.Status != agentrun.StatusRunning {
			httputil.WriteBadRequest(w, "only a running run can be paused")
			return false
		}
		hub.Send(record.ID, agentrun.ControlSignal{Kind: agentrun.CtrlPause})
	case "resume":
		if !activeRunStatus(record.Status) {
			httputil.WriteBadRequest(w, "only an active run can be resumed")
			return false
		}
		if err := hub.Resume(record.ID); err != nil {
			httputil.WriteErr(w, err)
			return false
		}
	case "abort":
		if !activeRunStatus(record.Status) {
			httputil.WriteBadRequest(w, "only an active run can be aborted")
			return false
		}
		hub.Send(record.ID, agentrun.ControlSignal{Kind: agentrun.CtrlAbort})
	case "nudge":
		if !activeRunStatus(record.Status) {
			httputil.WriteBadRequest(w, "only an active run can be nudged")
			return false
		}
		hub.Send(record.ID, agentrun.ControlSignal{Kind: agentrun.CtrlNudge, Message: req.Message})
	default:
		httputil.WriteBadRequest(w, "unknown action: "+req.Action)
		return false
	}
	return true
}

func activeRunStatus(status agentrun.Status) bool {
	return status == agentrun.StatusRunning || status == agentrun.StatusPaused
}

func (handler *Handler) projectInvestigationTerminal(
	ctx context.Context,
	runID string,
	userID int64,
	data any,
	runtime QARuntime,
) (*agentrun.Terminal, error) {
	terminal, ok := data.(*agentrun.Terminal)
	if !ok || terminal == nil || runtime.RunStore == nil || runtime.InvestigationReader == nil {
		if terminal == nil {
			return nil, fmt.Errorf("run.finished event has no terminal")
		}
		return terminal, nil
	}
	detail, err := runtime.RunStore.GetForUser(runID, userID)
	if err != nil {
		return nil, fmt.Errorf("load QA parent %q: %w", runID, err)
	}
	if detail.RunKind != agentrun.KindQAParent ||
		detail.Status != agentrun.StatusDone ||
		strings.TrimSpace(detail.WorkflowRunID) == "" {
		return terminal, nil
	}
	delivery, err := runtime.InvestigationReader.LoadDelivery(ctx, detail.WorkflowRunID)
	if err != nil {
		if errors.Is(err, investigation.ErrNoDelivery) {
			return terminal, nil
		}
		return nil, fmt.Errorf("load investigation delivery %q: %w", detail.WorkflowRunID, err)
	}
	projected := projectInvestigationDelivery(*terminal, delivery)
	return &projected, nil
}

func projectInvestigationDelivery(
	terminal agentrun.Terminal,
	delivery investigation.DeliveryResult,
) agentrun.Terminal {
	terminal.Answer = delivery.Text
	terminal.InvestigationDelivery = &delivery
	// The native delivery is authoritative. Do not leak a stale parent error
	// when a partial answer was successfully persisted.
	terminal.ErrorCode = ""
	terminal.Error = ""
	if delivery.Failure != nil {
		terminal.ErrorCode = string(delivery.Failure.Code)
		terminal.Error = delivery.Failure.Message
	}
	if delivery.Status == investigation.DeliveryFailed {
		terminal.Status = agentrun.StatusFailed
		if terminal.ErrorCode == "" {
			terminal.ErrorCode = "delivery_failed"
		}
	} else {
		terminal.Status = agentrun.StatusDone
	}
	return terminal
}

func emitHubEvent(ev agentrun.SSEEvent, sseEvent func(string, any) bool) bool {
	return sseEvent(string(ev.Type), ev.Data)
}

func currentUserID(r *http.Request) int64 {
	if u := auth.UserFromContext(r.Context()); u != nil {
		return u.ID
	}
	return 0
}

func (handler *Handler) ensureQASessions(w http.ResponseWriter) bool {
	return handler.qaSessionStore() != nil
}

func (handler *Handler) runStore() *agentrun.Store {
	return handler.currentQARuntime().RunStore
}

func (handler *Handler) memoryStore() *memory.MemoryStore {
	qa := handler.qaService()
	if qa == nil {
		return nil
	}
	return qa.Memory()
}

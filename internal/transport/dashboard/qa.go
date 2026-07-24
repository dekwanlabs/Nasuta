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

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

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
	conversation := handler.loadSessionContext(r.Context(), req.SessionID, currentUserID(r), req.History)
	conversation.SessionID = req.SessionID
	handler.serveAgentSSE(r.Context(), req.Question, conversation, req.SessionID, req.Trace, req.EvidencePlan, stream.emit, r)
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
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	s.flusher.Flush()
}

func (handler *Handler) loadSessionContext(ctx context.Context, sessionID string, userID int64, fallback []llm.Message) agent.ConversationContext {
	if sessionID == "" || handler.qaSessions == nil {
		return agent.ConversationContext{Recent: fallback}
	}
	sess, err := handler.qaSessions.GetRecentSession(sessionID, userID, 6)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] session load error: %v", err)
		return agent.ConversationContext{Recent: fallback}
	}
	if sess == nil {
		return agent.ConversationContext{Recent: fallback}
	}
	log.InfofCtx(ctx, "[qa] loaded session %s: recent=%d summary=%d chars", sessionID, len(sess.Messages), len([]rune(sess.Summary)))
	return agent.ConversationContext{Summary: sess.Summary, Recent: sess.Messages}
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
	result, err := handler.qa.AskAgentWithContext(ctx, question, conversation, userID, handler.rolePromptFor(userID), runID, evidencePlan, allowWrite)
	if traceRecorder != nil {
		for _, event := range traceRecorder.Activate() {
			sseEvent("trace", jsonStr(event))
		}
	}
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] agent init error: %v", err)
		sseEvent("error", jsonStr(map[string]string{"message": err.Error()}))
		return
	}

	if result.Context != nil && len(result.Context.References) > 0 {
		sseEvent("context", jsonStr(map[string]any{
			"references": result.Context.References,
			"hitCount":   result.Context.HitCount,
		}))
	}

	answerText, terminal := handler.streamAgentEvents(result, channel, sseEvent, r)
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
	if handler.platform != nil {
		endpointDomain = qaEndpointDomain(handler.platform.LLMBaseURL)
		if handler.platform.LLMEnabled() {
			status = "active"
		}
	}
	httputil.WriteJSON(w, map[string]any{
		"endpoint_domain":           endpointDomain,
		"endpoint_status":           status,
		"token_usage_available":     usageAvailable,
		"cache_percent":             cachePercent(usage.RoundCachedInputTokens, usage.RoundInputTokens),
		"session_total_tokens":      usage.SessionTotalTokens,
		"round_total_tokens":        usage.RoundTotalTokens,
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
	page, err := handler.qaSessions.ListMessagesBefore(r.PathValue("id"), currentUserID(r), beforeSeq, limit)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, page)
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
	if err := handler.qaSessions.AppendMessages(sessionID, userID, messages); err != nil {
		log.ErrorfCtx(ctx, "[qa] failed to append messages to session %s: %v", sessionID, err)
		return
	}
	log.InfofCtx(ctx, "[qa] saved turn to session %s", sessionID)

	go func() {
		sess, err := handler.qaSessions.GetFullSession(sessionID, userID)
		if err != nil || sess == nil || len(sess.Messages) == 0 {
			return
		}
		bgCtx := log.WithTraceID(context.Background(), log.GenerateTraceID())
		bgCtx = handler.qa.UsageContext(bgCtx, runID, llm.PhaseSessionSummary)
		summary, err := agent.GeneratePersistentSummary(bgCtx, handler.qa.LLM(), sess.Messages)
		if err != nil {
			log.ErrorfCtx(bgCtx, "[qa] summary generation failed for session %s: %v", sessionID, err)
			return
		}
		if summary != "" {
			if err := handler.qaSessions.UpdateSummary(sessionID, userID, summary); err != nil {
				log.ErrorfCtx(bgCtx, "[qa] failed to persist summary for session %s: %v", sessionID, err)
			} else {
				log.InfofCtx(bgCtx, "[qa] summary updated for session %s (%d chars)", sessionID, len(summary))
			}
		}
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

func (handler *Handler) streamAgentEvents(result *agent.AskResult, hubCh chan agent.SSEEvent, sseEvent func(string, string), r *http.Request) (string, *agent.RunTerminal) {
	var answerText string
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
			sseEvent("tool_result", jsonStr(map[string]any{"step": ev.Step.StepNo, "tool": ev.Step.Tool, "summary": ev.Step.ResultSummary}))
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
	if ev.Phase != "" {
		sseEvent("phase", jsonStr(map[string]string{"text": ev.Phase}))
	}
	if ev.Terminal != nil {
		sseEvent("run_end", jsonStr(map[string]any{"run_id": runID, "status": ev.Terminal.Status}))
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
	if handler.qa == nil {
		return nil
	}
	return handler.qa.RunStore()
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

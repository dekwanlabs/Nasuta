package qa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/scope"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/delegation"
	"github.com/dekwanlabs/nasuta/internal/agent/messages"
	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

func (svc *Service) submitRun(
	ctx context.Context,
	managedRun agentapi.ManagedRun,
	prepared *preparation,
	conversation execution.ConversationContext,
	admitted *admittedEvidence,
) (*AskResult, error) {
	request := prepared.request
	definition := prepared.definition
	log.InfofCtx(ctx, "[qa] submit runID=%s agent=%s@%d", request.RunID, definition.ID, definition.Version)
	builtMessages := execution.BuildMessages(
		request.Question,
		prepared.analysis.QueryPlan,
		conversation,
		admitted.Retrieved,
		admitted.Plan,
		svc.domainKnowledge,
		0,
	)
	runStart := svc.buildRunStart(prepared, definition, prepared.selection)
	runRequest := agentapi.RunRequest{
		RunID:          runStart.RunID,
		Agent:          runStart.Agent,
		DefinitionHash: runStart.DefinitionHash,
		Selection:      runStart.Selection,
		Input:          runStart.Input,
		Messages:       messages.Public(builtMessages),
		Context:        contextBlocks(admitted.Retrieved),
		Permissions:    runStart.Permissions,
		ToolScope:      runStart.ToolScope,
		Policy:         runStart.Policy,
		Limits:         runStart.Limits,
		Delegation:     runStart.Delegation,
		Actor:          runStart.Actor,
		Correlation:    runStart.Correlation,
	}
	// Evidence admission and tool pruning are only known after preparation, so
	// they are layered onto the immutable boundary rather than duplicated here.
	runRequest.ToolScope.OfferedToolIDs = orderedToolIDs(
		prepared.candidateToolSet.Tools(), conversation.PrunedToolIDs,
	)
	runRequest.ToolScope.PruneApplied = conversation.PruneApplied
	runRequest.Policy.EvidenceRequired = !admitted.Plan.Direct()
	runRequest.Policy.EvidenceSeeded = conversation.EvidenceSeeded
	runRequest.Policy.WebResearch = admitted.Plan.Has(domain.Web)
	// The final RunRequest must use the re-assembled conversation session.
	runRequest.Correlation.SessionID = conversation.SessionID
	ctx = svc.withDelegationParentContext(ctx, prepared, runRequest)
	ctx = session.WithToolScope(ctx, conversation.SessionID, conversation.CompactedThroughTurn, request.UserID)

	go svc.executeSubmittedRun(ctx, managedRun, prepared, conversation, runRequest)
	return &AskResult{RunID: request.RunID, Context: admitted.Retrieved}, nil
}

func (svc *Service) withDelegationParentContext(
	ctx context.Context,
	prepared *preparation,
	request agentapi.RunRequest,
) context.Context {
	if !scenarioToolsContain(prepared.candidateToolSet, delegation.DelegateToolID) {
		return ctx
	}
	evidenceIndex, contextIndex := delegation.IndexContext(request.Context)
	answerDeadline := time.Time{}
	if !request.Limits.Deadline.IsZero() {
		reserve := svc.answerReserve
		if reserve <= 0 {
			reserve = 30 * time.Second
		}
		answerDeadline = request.Limits.Deadline.Add(-reserve)
	}
	return delegation.WithParentContext(ctx, delegation.ParentContext{
		RunID:           request.RunID,
		QuestionSummary: tooloutput.TruncateContent(prepared.request.Question, 2000),
		HighRisk:        prepared.execution.HighRisk,
		Actor:           request.Actor,
		Permissions:     request.Permissions,
		Correlation:     request.Correlation,
		Limits:          request.Limits,
		AnswerDeadline:  answerDeadline,
		Depth:           0,
		OutputContract:  request.Policy.OutputContract,
		Evidence:        evidenceIndex,
		Context:         contextIndex,
	})
}

func (svc *Service) executeSubmittedRun(
	ctx context.Context,
	managedRun agentapi.ManagedRun,
	prepared *preparation,
	conversation execution.ConversationContext,
	request agentapi.RunRequest,
) {
	defer prepared.closeTrace()
	result, err := managedRun.Execute(ctx, request)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] runtime run %s failed: %v", request.RunID, err)
		code := "runtime_failed"
		if errors.Is(err, agentapi.ErrBudgetExceeded) {
			code = "budget_exhausted"
		}
		svc.finishRunWithError(ctx, managedRun, request.RunID, code, err)
		return
	}

	outcomeRunner, ok := managedRun.(interface{ Outcome() run.Outcome })
	if !ok {
		svc.finishRunWithError(
			ctx, managedRun, request.RunID, "runtime_outcome_unavailable",
			fmt.Errorf("managed run does not expose a durable outcome"),
		)
		return
	}
	outcome := outcomeRunner.Outcome()
	svc.logRunOutcome(ctx, request.RunID, outcome)
	if outcome.Status == run.StatusDone {
		if err := svc.persistTurn(
			context.WithoutCancel(ctx), request.RunID, conversation.SessionID,
			request.Actor.UserID, prepared.request.Question, outcome,
		); err != nil {
			log.ErrorfCtx(ctx, "[qa] persist completed run %s session turn: %v", request.RunID, err)
			svc.finishRunWithError(
				ctx, managedRun, request.RunID, "session_persistence_failed", err,
			)
			return
		}
		svc.archiveHistoryAsync(
			ctx, request.RunID, conversation.SessionID, request.Actor.UserID,
			prepared.definition.Budget.ContextTokens,
			prepared.definition.Model.MaxOutputTokens,
		)
	}
	if err := managedRun.Finish(nil); err != nil {
		log.ErrorfCtx(ctx, "[qa] finish run %s: %v", request.RunID, err)
		return
	}
	svc.extractRunMemory(ctx, prepared, conversation, result, outcome)
}

func (svc *Service) finishRunWithError(
	ctx context.Context,
	managedRun agentapi.ManagedRun,
	runID string,
	code string,
	err error,
) {
	if finishErr := managedRun.Finish(&agentapi.RunError{Code: code, Message: err.Error()}); finishErr != nil {
		log.ErrorfCtx(ctx, "[qa] finish failed run %s code=%s: %v", runID, code, finishErr)
	}
}

func (svc *Service) logRunOutcome(
	ctx context.Context,
	runID string,
	outcome run.Outcome,
) {
	switch outcome.Status {
	case run.StatusFailed:
		log.ErrorfCtx(ctx,
			"[qa] runtime run %s completed with failed outcome code=%s error=%v",
			runID, outcome.ErrorCode, outcome.Err,
		)
	case run.StatusAborted:
		log.InfofCtx(ctx,
			"[qa] runtime run %s completed with aborted outcome code=%s error=%v",
			runID, outcome.ErrorCode, outcome.Err,
		)
	}
}

func (svc *Service) extractRunMemory(
	ctx context.Context,
	prepared *preparation,
	conversation execution.ConversationContext,
	result agentapi.RunResult,
	outcome run.Outcome,
) {
	userID := prepared.request.UserID
	if !memoryExtractionAllowed(outcome) || svc.memory == nil || userID == 0 {
		return
	}
	memCtx := llm.WithUsagePhase(context.WithoutCancel(ctx), llm.PhaseMemoryExtract)
	memCtx, cancel := context.WithTimeout(memCtx, 60*time.Second)
	defer cancel()

	question := tooloutput.TruncateContent(prepared.request.Question, 1000)
	answer := tooloutput.TruncateContent(result.Text, 2000)
	probe, err := buildMemoryProbe(memCtx, memoryProbeInput{
		Client: svc.helperLLM, Question: question,
	})
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] memory probe error: %v", err)
		return
	}
	if len(probe.Probes) == 0 {
		return
	}
	recalled, err := recallMemoriesForWrite(memCtx, writeRecallInput{
		Store: svc.memory, UserID: userID, Probes: probe.Probes,
	})
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] memory write recall error: %v", err)
		return
	}
	extraction, err := extractMemories(memCtx, memoryExtractInput{
		Client: svc.helperLLM, Question: question, Answer: answer,
		Existing:       recalled.Result.Matches,
		EvidenceStatus: run.EvidenceStatus(result.Evidence.Status),
	})
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] memory extraction error: %v", err)
		return
	}
	_, _ = writeMemories(memCtx, memoryWriteInput{
		Store: svc.memory, Decisions: extraction.Decisions, UserID: userID,
		SessionID: conversation.SessionID,
	})
	if len(extraction.Decisions) > 0 {
		log.InfofCtx(ctx, "[qa] consolidated %d memories for user %d", len(extraction.Decisions), userID)
	}
}

func (svc *Service) answerContext(
	ctx context.Context,
	conversation execution.ConversationContext,
	recalled []memory.MemoryRecord,
	rolePrompt string,
	rc *retrieval.RetrievedContext,
) execution.ConversationContext {
	instructions := append([]llm.Message{}, conversation.Instructions...)
	if len(recalled) > 0 {
		var memoryInjectSpec = runtrace.Spec[[]memory.MemoryRecord, string]{
			Operation: "memory.inject",
			Node:      "memory_inject",
			Output: func(records []memory.MemoryRecord, formatted string, _ error) map[string]any {
				return map[string]any{"records": len(records), "characters": len([]rune(formatted))}
			},
		}
		formatted, _ := runtrace.Invoke(ctx, memoryInjectSpec, recalled, func(
			_ context.Context,
			records []memory.MemoryRecord,
		) (string, error) {
			return memory.FormatMemories(records), nil
		})
		instructions = append(instructions, llm.Message{Role: "system", Content: formatted})
	}
	conversation.RolePrompt = rolePrompt
	conversation.Instructions = instructions
	conversation.EvidenceSeeded = len(recalled) > 0 || rc != nil && rc.Text != ""
	return conversation
}

func outputContractForQuery(query domain.QueryPlan) agentapi.RunOutputContract {
	if query.Kind != domain.QueryFlow {
		return agentapi.RunOutputContract{}
	}
	return agentapi.RunOutputContract{
		Kind:           "flow",
		RequireMermaid: true,
		Subjects:       flowSubjects(query),
		MaxHops:        6,
	}
}

func flowSubjects(query domain.QueryPlan) []string {
	const maxSubjects = 8
	seen := make(map[string]struct{}, maxSubjects)
	subjects := make([]string, 0, min(len(query.EntitySpecs), maxSubjects))
	for _, spec := range query.EntitySpecs {
		label := strings.TrimSpace(spec.Label)
		if label == "" && !domain.IsSynthesizedEntityID(spec.ID) {
			label = strings.TrimSpace(spec.ID)
		}
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		subjects = append(subjects, label)
		if len(subjects) == maxSubjects {
			return subjects
		}
	}
	for _, entity := range query.Entities {
		label := strings.TrimSpace(entity)
		if label == "" || domain.IsSynthesizedEntityID(label) {
			// Entities carries the canonical join key; a synthesized opaque
			// identity has no user-facing meaning, and its label is already
			// represented by the matching EntitySpec above.
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		subjects = append(subjects, label)
		if len(subjects) == maxSubjects {
			break
		}
	}
	return subjects
}

func runPermissions(allowWrite bool) agentapi.PermissionPolicy {
	scopes := []string{scope.KnowledgeRead}
	if allowWrite {
		scopes = append(scopes, scope.KnowledgeWrite)
	}
	return agentapi.PermissionPolicy{Scopes: scopes}
}

func runInput(question string) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Question string `json:"question"`
	}{Question: question})
	return payload
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	return evidence.CloneUnits(units)
}

func publicEvidenceConflicts(
	conflicts []evidence.Conflict,
) []agentapi.EvidenceConflict {
	return evidence.PublicConflicts(conflicts)
}
func orderedToolIDs(tools []tool.Tool, selected map[tool.ToolID]struct{}) []string {
	if len(selected) == 0 {
		return nil
	}
	ids := make([]string, 0, len(selected))
	for _, candidate := range tools {
		if _, ok := selected[candidate.ID]; ok {
			ids = append(ids, string(candidate.ID))
		}
	}
	return ids
}

func (svc *Service) persistTurn(ctx context.Context, runID, sessionID string, userID int64, question string, outcome run.Outcome) error {
	if svc.sessions == nil {
		return nil
	}
	return persistTurn(ctx, svc.sessions, runID, sessionID, userID, question, outcome)
}

func persistTurn(
	ctx context.Context,
	sessions sessionTurnStore,
	runID string,
	sessionID string,
	userID int64,
	question string,
	outcome run.Outcome,
) error {
	if sessionID == "" {
		return nil
	}
	if sessions == nil {
		return fmt.Errorf("session store is unavailable for session %q", sessionID)
	}
	if err := sessions.EnsureSession(sessionID, userID, platform.TruncateForLog(question, 256)); err != nil {
		return fmt.Errorf("ensure session %q: %w", sessionID, err)
	}
	messages := make([]llm.Message, 0, len(outcome.SessionMessages)+2)
	messages = append(messages, llm.Message{Role: "user", Content: question})
	messages = append(messages, outcome.SessionMessages...)
	messages = append(messages, llm.Message{Role: "assistant", Content: outcome.Answer})
	turnNo, err := sessions.AppendTurn(sessionID, runID, userID, messages)
	if err != nil {
		return fmt.Errorf("append run %q to session %q: %w", runID, sessionID, err)
	}
	log.InfofCtx(ctx, "[qa] saved run %s as turn %d in session %s", runID, turnNo, sessionID)
	return nil
}

func (svc *Service) archiveHistoryAsync(
	ctx context.Context,
	runID string,
	sessionID string,
	userID int64,
	contextWindow int,
	outputReserve int,
) {
	if svc.sessions == nil || sessionID == "" || contextWindow <= 0 {
		return
	}
	archiveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionArchiveTimeout)
	go func() {
		defer cancel()
		started := false
		fromTurn, toTurn := 0, 0
		result, err := session.ArchiveWithStatus(
			archiveCtx, svc.helperLLM, svc.sessions, sessionID, userID,
			session.CompactionUsage{
				ContextWindow:       contextWindow,
				OutputReserveTokens: outputReserve,
			},
			func(from, to int) {
				started = true
				fromTurn, toTurn = from, to
				svc.updateCompaction(
					runID, "start",
					fmt.Sprintf("正在压缩第 %d–%d 轮历史上下文…", from, to),
					fromTurn, toTurn,
				)
			},
			svc.history,
		)
		if err != nil {
			log.ErrorfCtx(archiveCtx, "[qa] post-turn history archive failed for %s: %v", sessionID, err)
			if started {
				svc.updateCompaction(
					runID, "failed", "历史上下文压缩失败",
					fromTurn, toTurn,
				)
			}
			return
		}
		if result.Applied {
			svc.updateCompaction(
				runID, "done", "历史上下文压缩完成",
				result.FromTurn, result.ToTurn,
			)
			log.InfofCtx(archiveCtx, "[qa] archived session %s turns %d-%d after saved turn",
				sessionID, result.FromTurn, result.ToTurn)
		} else if result.Stale {
			if started {
				svc.updateCompaction(
					runID, "done", "历史上下文压缩完成",
					result.FromTurn, result.ToTurn,
				)
			}
			log.InfofCtx(archiveCtx, "[qa] ignored stale post-turn archive for session %s through turn %d",
				sessionID, result.ToTurn)
		}
	}()
}

func NewRunID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Errorf("[agent] crypto/rand.Read failed: %v — falling back to timestamp-based id", err)
		return fmt.Sprintf("run_%d", time.Now().UnixNano())
	}
	return "run_" + hex.EncodeToString(b[:])
}

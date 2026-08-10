package qa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentsession "github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

const sessionArchiveTimeout = 2 * time.Minute

func (svc *QA) submitInvestigation(
	ctx context.Context,
	request QARequest,
	question string,
	conversation ConversationContext,
	userID int64,
	runID string,
	trace *executiontrace.Scope,
	ownsTrace bool,
) (*AskResult, error) {
	workflowRunID := "workflow_" + strings.TrimPrefix(NewRunID(), "run_")
	scenario, err := svc.scenarios.BeginScenario(ctx, ScenarioRunStart{
		RunID: runID, ParentRunID: request.ParentRunID, UserID: userID,
		SessionID: conversation.SessionID, Question: question, Mode: "multi_agent",
	})
	if err != nil {
		return nil, fmt.Errorf("begin QA parent run %q: %w", runID, err)
	}
	runCtx := context.WithoutCancel(scenario.Context(ctx))
	go func() {
		if ownsTrace {
			defer trace.Close()
		}
		result, runErr := svc.investigation.Run(runCtx, InvestigationRequest{
			WorkflowRunID: workflowRunID, ParentRunID: runID,
			Question: question, Actor: agentapi.Actor{UserID: userID},
		})
		outcome := investigationOutcome(result, runErr)
		if outcome.Status == RunStatusDone {
			if err := svc.persistSessionTurn(
				runCtx,
				runID,
				conversation.SessionID,
				userID,
				question,
				outcome,
			); err != nil {
				log.ErrorfCtx(runCtx, "[qa] persist completed parent run %s session turn: %v", runID, err)
				outcome.Status = RunStatusFailed
				outcome.ErrorCode = "session_persistence_failed"
				outcome.Err = err
			}
		}
		if outcome.Status == RunStatusDone {
			svc.archiveSessionHistoryAsync(runCtx, runID, conversation.SessionID, userID)
		}
		if finishErr := scenario.Finish(outcome); finishErr != nil {
			log.ErrorfCtx(runCtx, "[qa] finish parent run %s: %v", runID, finishErr)
			return
		}
	}()
	return &AskResult{RunID: runID}, nil
}

func (svc *QA) submitRun(
	ctx context.Context,
	run agentapi.ManagedRun,
	scenario QARequest,
	definition agentapi.Definition,
	selection agentapi.DefinitionSelection,
	question string,
	conversation ConversationContext,
	userID int64,
	rc *retrieval.RetrievedContext,
	runID string,
	plan domain.EvidencePlan,
	policy ToolPolicy,
	prepared ScenarioToolSet,
	trace *executiontrace.Scope,
	ownsTrace bool,
) (*AskResult, error) {
	log.InfofCtx(ctx, "[qa] submit runID=%s agent=%s@%d", runID, definition.ID, definition.Version)
	messages := buildAgentMessages(
		question, conversation, rc, plan, svc.domainKnowledge, 0,
	)
	request := agentapi.RunRequest{
		RunID: runID,
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
		DefinitionHash: definition.ContentHash,
		Selection:      selection,
		Input:          qaRunInput(question),
		Messages:       publicMessages(messages),
		Context:        qaContextBlocks(rc),
		Permissions:    qaRunPermissions(policy.AllowWrite),
		ToolScope: agentapi.ToolScope{
			AllowWrite:      policy.AllowWrite,
			RestrictVisible: true,
			VisibleToolIDs:  scenarioToolIDs(prepared.Tools()),
			OfferedToolIDs:  orderedToolIDs(prepared.Tools(), conversation.PrunedToolIDs),
			PruneApplied:    conversation.PruneApplied,
		},
		Policy: agentapi.RunPolicy{
			EvidenceRequired: !plan.Direct(),
			EvidenceSeeded:   conversation.EvidenceSeeded,
			WebResearch:      plan.Has(domain.Web),
		},
		Actor: agentapi.Actor{UserID: userID},
		Correlation: agentapi.Correlation{
			SessionID: conversation.SessionID, ParentRunID: scenario.ParentRunID,
			WorkflowRunID: scenario.WorkflowRunID, NodeID: scenario.WorkflowNodeID,
		},
	}
	ctx = withSessionToolScope(ctx, conversation, userID)

	go func() {
		if ownsTrace {
			defer trace.Close()
		}
		result, runErr := run.Execute(ctx, request)
		if runErr != nil {
			log.ErrorfCtx(ctx, "[qa] runtime run %s failed: %v", runID, runErr)
			if finishErr := run.Finish(&agentapi.RunError{
				Code: "runtime_failed", Message: runErr.Error(),
			}); finishErr != nil {
				log.ErrorfCtx(ctx, "[qa] finish failed run %s: %v", runID, finishErr)
			}
			return
		}
		outcome := outcomeFromPublicResult(result)
		if outcome.Status == RunStatusDone {
			if err := svc.persistSessionTurn(context.WithoutCancel(ctx), runID, conversation.SessionID, userID, question, outcome); err != nil {
				log.ErrorfCtx(ctx, "[qa] persist completed run %s session turn: %v", runID, err)
				if finishErr := run.Finish(&agentapi.RunError{
					Code: "session_persistence_failed", Message: err.Error(),
				}); finishErr != nil {
					log.ErrorfCtx(ctx, "[qa] finish session persistence failure %s: %v", runID, finishErr)
				}
				return
			}
		}
		if outcome.Status == RunStatusDone {
			svc.archiveSessionHistoryAsync(ctx, runID, conversation.SessionID, userID)
		}
		if finishErr := run.Finish(nil); finishErr != nil {
			log.ErrorfCtx(ctx, "[qa] finish run %s: %v", runID, finishErr)
			return
		}

		if memoryExtractionAllowed(outcome, internalResultFromPublic(result)) && svc.memory != nil && userID != 0 {
			memCtx := llm.WithUsagePhase(context.WithoutCancel(ctx), llm.PhaseMemoryExtract)
			memCtx, memCancel := context.WithTimeout(memCtx, 60*time.Second)
			memoryQuestion := tooloutput.TruncateContent(question, 1000)
			memoryAnswer := tooloutput.TruncateContent(result.Text, 2000)
			probe, probeErr := buildMemoryProbe(memCtx, memoryProbeInput{
				Client: svc.helperLLM, Question: memoryQuestion,
			})
			if probeErr != nil {
				log.ErrorfCtx(ctx, "[qa] memory probe error: %v", probeErr)
				memCancel()
				return
			}
			if len(probe.Probes) == 0 {
				memCancel()
				return
			}
			recalled, recallErr := recallMemoriesForWrite(memCtx, memoryRecallForWriteInput{
				Store: svc.memory, UserID: userID, Probes: probe.Probes,
			})
			if recallErr != nil {
				log.ErrorfCtx(ctx, "[qa] memory write recall error: %v", recallErr)
				memCancel()
				return
			}
			extraction, err := extractMemories(memCtx, memoryExtractInput{
				Client: svc.helperLLM, Question: memoryQuestion, Answer: memoryAnswer,
				Existing:       recalled.Result.Matches,
				EvidenceStatus: EvidenceStatus(result.Evidence.Status),
			})
			if err == nil {
				_, _ = writeMemories(memCtx, memoryWriteInput{
					Store: svc.memory, Decisions: extraction.Decisions, UserID: userID, SessionID: conversation.SessionID,
				})
				if len(extraction.Decisions) > 0 {
					log.InfofCtx(ctx, "[qa] consolidated %d memories for user %d", len(extraction.Decisions), userID)
				}
			} else {
				log.ErrorfCtx(ctx, "[qa] memory extraction error: %v", err)
			}
			memCancel()
		}
	}()

	return &AskResult{RunID: runID, Context: rc}, nil
}

func (svc *QA) prepareAnswerConversation(
	ctx context.Context,
	conversation ConversationContext,
	recalled []memory.MemoryRecord,
	rolePrompt string,
	rc *retrieval.RetrievedContext,
) ConversationContext {
	instructions := append([]llm.Message{}, conversation.Instructions...)
	if len(recalled) > 0 {
		formatted, _ := executiontrace.Invoke(ctx, memoryInjectSpec, recalled, func(
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

func qaRunPermissions(allowWrite bool) agentapi.PermissionPolicy {
	scopes := []string{knowledgeReadScope}
	if allowWrite {
		scopes = append(scopes, knowledgeWriteScope)
	}
	return agentapi.PermissionPolicy{Scopes: scopes}
}

func qaRunInput(question string) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Question string `json:"question"`
	}{Question: question})
	return payload
}

func qaContextBlocks(rc *retrieval.RetrievedContext) []agentapi.ContextBlock {
	if rc == nil || rc.Text == "" {
		return nil
	}
	references := make([]agentapi.Reference, 0, len(rc.References))
	for _, reference := range rc.References {
		references = append(references, agentapi.Reference{
			Type: reference.Type, Label: reference.Label, Target: reference.Target,
		})
	}
	return []agentapi.ContextBlock{{
		Source: "qa.evidence", Title: "QA Evidence", Content: rc.Text,
		References: references, Evidence: cloneEvidenceUnits(rc.EvidenceUnits),
		Complete: false, ContentHash: hashString(rc.Text),
	}}
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]tool.EvidenceUnit, len(units))
	for i, unit := range units {
		out[i] = unit
		out[i].Sections = append([]string(nil), unit.Sections...)
		out[i].Facets = append([]string(nil), unit.Facets...)
	}
	return out
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

func internalResultFromPublic(result agentapi.RunResult) *RunResult {
	return &RunResult{
		Answer: result.Text, ForcedConclusion: result.Evidence.ForcedConclusion,
		Evidence: EvidenceMetrics{Status: EvidenceStatus(result.Evidence.Status)},
	}
}

func publicResultMessages(messages []agentapi.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		out = append(out, internalMessage(message))
	}
	return out
}

func (svc *QA) persistSessionTurn(ctx context.Context, runID, sessionID string, userID int64, question string, outcome RunOutcome) error {
	if svc.sessions == nil || sessionID == "" {
		return nil
	}
	if err := svc.sessions.EnsureSession(sessionID, userID, platform.TruncateForLog(question, 256)); err != nil {
		return fmt.Errorf("ensure session %q: %w", sessionID, err)
	}
	messages := make([]llm.Message, 0, len(outcome.SessionMessages)+2)
	messages = append(messages, llm.Message{Role: "user", Content: question})
	messages = append(messages, outcome.SessionMessages...)
	messages = append(messages, llm.Message{Role: "assistant", Content: outcome.Answer})
	turnNo, err := svc.sessions.AppendTurn(sessionID, runID, userID, messages)
	if err != nil {
		return fmt.Errorf("append run %q to session %q: %w", runID, sessionID, err)
	}
	log.InfofCtx(ctx, "[qa] saved run %s as turn %d in session %s", runID, turnNo, sessionID)
	return nil
}

func (svc *QA) archiveSessionHistoryAsync(ctx context.Context, runID, sessionID string, userID int64) {
	if svc.sessions == nil || sessionID == "" || svc.contextWindow <= 0 {
		return
	}
	archiveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionArchiveTimeout)
	go func() {
		defer cancel()
		started := false
		fromTurn, toTurn := 0, 0
		result, err := agentsession.ArchiveSessionHistoryIfNeededWithStatus(
			archiveCtx, svc.helperLLM, svc.sessions, sessionID, userID,
			agentsession.SessionCompactionUsage{
				ContextWindow:       svc.contextWindow,
				OutputReserveTokens: svc.outputReserve,
			},
			func(from, to int) {
				started = true
				fromTurn, toTurn = from, to
				svc.updateSessionCompaction(
					runID, sessionID, "start",
					fmt.Sprintf("正在压缩第 %d–%d 轮历史上下文…", from, to),
					fromTurn, toTurn,
				)
			},
			svc.history,
		)
		if err != nil {
			log.ErrorfCtx(archiveCtx, "[qa] post-turn history archive failed for %s: %v", sessionID, err)
			if started {
				svc.updateSessionCompaction(
					runID, sessionID, "failed", "历史上下文压缩失败",
					fromTurn, toTurn,
				)
			}
			return
		}
		if result.Applied {
			svc.updateSessionCompaction(
				runID, sessionID, "done", "历史上下文压缩完成",
				result.FromTurn, result.ToTurn,
			)
			log.InfofCtx(archiveCtx, "[qa] archived session %s turns %d-%d after saved turn",
				sessionID, result.FromTurn, result.ToTurn)
		} else if result.Stale {
			if started {
				svc.updateSessionCompaction(
					runID, sessionID, "done", "历史上下文压缩完成",
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

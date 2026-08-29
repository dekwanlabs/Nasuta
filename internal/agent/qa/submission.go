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

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/delegation"
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

const sessionArchiveTimeout = 2 * time.Minute

func (svc *Service) submitInvestigation(
	prepared *preparation,
) (*AskResult, error) {
	request := prepared.request
	ctx := prepared.ctx
	runID := request.RunID
	question := request.Question
	conversation := request.Conversation
	userID := request.UserID
	definition, _, err := svc.resolveAgentDefinition(prepared)
	if err != nil {
		return nil, err
	}
	prepared.runLimits = svc.parentRunLimits(prepared, definition)
	workflowRunID := "workflow_" + strings.TrimPrefix(NewRunID(), "run_")
	scenario, err := svc.scenarios.Start(ctx, ScenarioRunStart{
		RunID: runID, ParentRunID: request.ParentRunID, UserID: userID,
		WorkflowRunID: workflowRunID, SessionID: conversation.SessionID,
		Question: question, Mode: "multi_agent",
		Limits: prepared.runLimits,
	})
	if err != nil {
		return nil, fmt.Errorf("begin QA parent run %q: %w", runID, err)
	}
	runCtx := scenario.Context(ctx)
	evidencePlan := prepared.planning.Effective.Plan
	webUnavailable := evidencePlan.Has(domain.Web) && !svc.cfg.WebSearchEnabled
	if webUnavailable {
		log.WarnfCtx(runCtx, "[qa] retrieval source unavailable: web")
	}
	evidence, err := svc.prepareEvidence(
		runCtx,
		prepared,
		evidencePlan,
		webUnavailable,
		scenario,
	)
	if err != nil {
		scenario.Release()
		prepared.closeTrace()
		outcome := RunOutcome{
			Status: RunStatusFailed, ErrorCode: "preparation_failed", Err: err,
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}
		completeErr := svc.scenarios.Complete(
			context.WithoutCancel(runCtx),
			runID,
			outcome,
		)
		if completeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("prepare QA investigation workflow %q: %w", workflowRunID, err),
				completeErr,
			)
		}
		return nil, fmt.Errorf("prepare QA investigation workflow %q: %w", workflowRunID, err)
	}
	seedMaterial := contextBlocks(evidence.retrieved)
	if recalled := memoryContextBlock(evidence.recalled); recalled != nil {
		seedMaterial = append(seedMaterial, *recalled)
	}
	seedEvidence := contextBlockEvidence(seedMaterial)
	if err := recordEvidenceLedger(
		runCtx,
		scenario,
		seedEvidence,
	); err != nil {
		scenario.Release()
		prepared.closeTrace()
		outcome := RunOutcome{
			Status: RunStatusFailed, ErrorCode: "preparation_failed", Err: err,
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}
		completeErr := svc.scenarios.Complete(
			context.WithoutCancel(runCtx),
			runID,
			outcome,
		)
		if completeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("persist QA evidence ledger for %q: %w", workflowRunID, err),
				completeErr,
			)
		}
		return nil, fmt.Errorf(
			"persist QA evidence ledger for %q: %w",
			workflowRunID,
			err,
		)
	}
	var (
		startErr       error
		startErrorCode = "investigation_start_failed"
	)
	contract := applyDiscoverThenSelect(contractFromPreparation(prepared, seedMaterial))
	proposal := cloneTaskGraphProposal(prepared.taskGraphProposal)
	proposal, err = prepareInvestigationProposal(
		proposal,
		contract,
		seedEvidence,
	)
	if err != nil {
		startErrorCode = "investigation_plan_failed"
		startErr = err
	}
	if startErr == nil {
		startErr = svc.investigation.Start(runCtx, InvestigationRequest{
			WorkflowRunID: workflowRunID,
			Contract:      contract,
			Proposal:      proposal,
			SeedEvidence:  seedEvidence,
			Actor:         agentapi.Actor{UserID: userID},
		})
	}
	scenario.Release()
	prepared.closeTrace()
	if startErr != nil {
		outcome := RunOutcome{
			Status: RunStatusFailed, ErrorCode: startErrorCode, Err: startErr,
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}
		completeErr := svc.scenarios.Complete(
			context.WithoutCancel(runCtx),
			runID,
			outcome,
		)
		if completeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("start QA investigation workflow %q: %w", workflowRunID, startErr),
				completeErr,
			)
		}
		return nil, fmt.Errorf("start QA investigation workflow %q: %w", workflowRunID, startErr)
	}
	waitCtx := context.WithoutCancel(runCtx)
	go func() {
		if err := svc.coordinator.Await(waitCtx, runID, workflowRunID); err != nil {
			log.ErrorfCtx(waitCtx, "[qa] converge parent run %s: %v", runID, err)
			return
		}
		svc.archiveHistoryAsync(
			waitCtx,
			runID,
			conversation.SessionID,
			userID,
			svc.contextWindow,
			svc.outputReserve,
		)
	}()
	return &AskResult{RunID: runID}, nil
}

func cloneTaskGraphProposal(
	proposal *agentapi.TaskGraphProposal,
) *agentapi.TaskGraphProposal {
	if proposal == nil {
		return nil
	}
	cloned := *proposal
	cloned.Tasks = make([]agentapi.TaskSpec, len(proposal.Tasks))
	for index, task := range proposal.Tasks {
		task.InvestigationGoalIDs = append(
			[]string(nil),
			task.InvestigationGoalIDs...,
		)
		task.EvidenceGoalIDs = append([]string(nil), task.EvidenceGoalIDs...)
		task.RequiredFacets = append([]string(nil), task.RequiredFacets...)
		task.InputRefs = cloneProposalEvidenceRefs(task.InputRefs)
		cloned.Tasks[index] = task
	}
	cloned.Edges = append([]agentapi.TaskEdge(nil), proposal.Edges...)
	return &cloned
}

func cloneProposalEvidenceRefs(
	refs []agentapi.EvidenceRef,
) []agentapi.EvidenceRef {
	if refs == nil {
		return nil
	}
	return append(make([]agentapi.EvidenceRef, 0, len(refs)), refs...)
}

func memoryContextBlock(records []memory.MemoryRecord) *agentapi.ContextBlock {
	content := memory.FormatMemories(records)
	if content == "" {
		return nil
	}
	return &agentapi.ContextBlock{
		Source: "qa.memory", Title: "Recalled Memory", Content: content,
		Complete: false, ContentHash: hashString(content),
	}
}

func (svc *Service) submitRun(
	ctx context.Context,
	run agentapi.ManagedRun,
	scenario Request,
	definition agentapi.Definition,
	selection agentapi.DefinitionSelection,
	question string,
	conversation ConversationContext,
	userID int64,
	rc *retrieval.RetrievedContext,
	runID string,
	query domain.QueryPlan,
	plan domain.EvidencePlan,
	policy ToolPolicy,
	prepared ScenarioToolSet,
	highRisk bool,
	limits agentapi.RunLimits,
	trace *runtrace.Scope,
	ownsTrace bool,
) (*AskResult, error) {
	log.InfofCtx(ctx, "[qa] submit runID=%s agent=%s@%d", runID, definition.ID, definition.Version)
	messages := buildAgentMessages(
		question, query, conversation, rc, plan, svc.domainKnowledge, 0,
	)
	request := agentapi.RunRequest{
		RunID: runID,
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
		DefinitionHash: definition.ContentHash,
		Selection:      selection,
		Input:          runInput(question),
		Messages:       publicMessages(messages),
		Context:        contextBlocks(rc),
		Permissions:    runPermissions(policy.AllowWrite),
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
		Limits: limits,
		Actor:  agentapi.Actor{UserID: userID},
		Correlation: agentapi.Correlation{
			SessionID: conversation.SessionID, ParentRunID: scenario.ParentRunID,
			WorkflowRunID: scenario.WorkflowRunID, NodeID: scenario.WorkflowNodeID,
		},
	}
	if scenarioToolsContain(prepared, delegation.DelegateToolID) {
		evidenceIndex, contextIndex := delegation.IndexContext(request.Context)
		ctx = delegation.WithParentContext(ctx, delegation.ParentContext{
			RunID:           runID,
			QuestionSummary: tooloutput.TruncateContent(question, 2000),
			HighRisk:        highRisk,
			Actor:           request.Actor,
			Permissions:     request.Permissions,
			Correlation:     request.Correlation,
			Limits:          limits,
			Depth:           0,
			Evidence:        evidenceIndex,
			Context:         contextIndex,
		})
	}
	ctx = withSessionToolScope(ctx, conversation, userID)

	go func() {
		if ownsTrace {
			defer trace.Close()
		}
		result, runErr := run.Execute(ctx, request)
		if runErr != nil {
			log.ErrorfCtx(ctx, "[qa] runtime run %s failed: %v", runID, runErr)
			code := "runtime_failed"
			if errors.Is(runErr, agentapi.ErrBudgetExceeded) {
				code = "budget_exhausted"
			}
			if finishErr := run.Finish(&agentapi.RunError{
				Code: code, Message: runErr.Error(),
			}); finishErr != nil {
				log.ErrorfCtx(ctx, "[qa] finish failed run %s: %v", runID, finishErr)
			}
			return
		}
		outcomeRunner, ok := run.(interface{ Outcome() RunOutcome })
		if !ok {
			if finishErr := run.Finish(&agentapi.RunError{
				Code: "runtime_outcome_unavailable", Message: "managed run does not expose a durable outcome",
			}); finishErr != nil {
				log.ErrorfCtx(ctx, "[qa] finish outcome-unavailable run %s: %v", runID, finishErr)
			}
			return
		}
		outcome := outcomeRunner.Outcome()
		if outcome.Status == RunStatusDone {
			if err := svc.persistTurn(context.WithoutCancel(ctx), runID, conversation.SessionID, userID, question, outcome); err != nil {
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
			svc.archiveHistoryAsync(
				ctx, runID, conversation.SessionID, userID,
				definition.Budget.ContextTokens, definition.Model.MaxOutputTokens,
			)
		}
		if finishErr := run.Finish(nil); finishErr != nil {
			log.ErrorfCtx(ctx, "[qa] finish run %s: %v", runID, finishErr)
			return
		}

		if memoryExtractionAllowed(outcome, resultFromPublic(result)) && svc.memory != nil && userID != 0 {
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
			recalled, recallErr := recallMemoriesForWrite(memCtx, writeRecallInput{
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

func (svc *Service) answerContext(
	ctx context.Context,
	conversation ConversationContext,
	recalled []memory.MemoryRecord,
	rolePrompt string,
	rc *retrieval.RetrievedContext,
) ConversationContext {
	instructions := append([]llm.Message{}, conversation.Instructions...)
	if len(recalled) > 0 {
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

func runPermissions(allowWrite bool) agentapi.PermissionPolicy {
	scopes := []string{knowledgeReadScope}
	if allowWrite {
		scopes = append(scopes, knowledgeWriteScope)
	}
	return agentapi.PermissionPolicy{Scopes: scopes}
}

func runInput(question string) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Question string `json:"question"`
	}{Question: question})
	return payload
}

func contextBlocks(rc *retrieval.RetrievedContext) []agentapi.ContextBlock {
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
		EvidenceConflicts: publicEvidenceConflicts(rc.EvidenceConflicts),
		Complete:          false, ContentHash: hashString(rc.Text),
	}}
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	return evidence.CloneUnits(units)
}

func publicEvidenceConflicts(
	conflicts []evidence.Conflict,
) []agentapi.EvidenceConflict {
	if len(conflicts) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceConflict, len(conflicts))
	for index, conflict := range conflicts {
		out[index] = agentapi.EvidenceConflict{
			Identity: agentapi.EvidenceIdentity{
				SourceKind: conflict.Key.SourceKind,
				Target:     conflict.Key.Target,
				Section:    conflict.Key.Section,
				Version:    conflict.Key.Version,
				TimeRange:  conflict.Key.TimeRange,
			},
			Current:        evidence.CloneUnit(conflict.Current),
			Incoming:       evidence.CloneUnit(conflict.Incoming),
			CurrentOrigin:  conflict.CurrentOrigin,
			IncomingOrigin: conflict.IncomingOrigin,
		}
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

func resultFromPublic(result agentapi.RunResult) *RunResult {
	return &RunResult{
		Answer: result.Text, ForcedConclusion: result.Evidence.ForcedConclusion,
		Evidence: EvidenceMetrics{Status: EvidenceStatus(result.Evidence.Status)},
	}
}

func (svc *Service) persistTurn(ctx context.Context, runID, sessionID string, userID int64, question string, outcome RunOutcome) error {
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
	outcome RunOutcome,
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
				svc.updateCompaction(
					runID, sessionID, "failed", "历史上下文压缩失败",
					fromTurn, toTurn,
				)
			}
			return
		}
		if result.Applied {
			svc.updateCompaction(
				runID, sessionID, "done", "历史上下文压缩完成",
				result.FromTurn, result.ToTurn,
			)
			log.InfofCtx(archiveCtx, "[qa] archived session %s turns %d-%d after saved turn",
				sessionID, result.FromTurn, result.ToTurn)
		} else if result.Stale {
			if started {
				svc.updateCompaction(
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

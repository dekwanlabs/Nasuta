package featurereviewworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentcatalog"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

func TestSchemasValidateReviewHandoffs(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(Schemas()); err != nil {
		t.Fatal(err)
	}
	completedAt := time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC)
	report := featuredelivery.ReviewReport{
		ID: "report-1", RoundID: "round-1", AssignmentID: "assignment-1",
		ReviewerID: "architecture", SubjectHash: "subject-hash",
		Coverage: []featuredelivery.CoverageItem{{
			Category: "architecture", Covered: true, Summary: "covered",
		}},
		Findings: []featuredelivery.Finding{{
			ID: "finding-1", ReportID: "report-1", Category: "architecture",
			Severity: featuredelivery.SeverityLow,
			Claim:    "A claim", Impact: "An impact",
			Evidence: []featuredelivery.FindingEvidence{{
				Kind: "subject", Ref: "component", Hash: "evidence-hash",
				Summary: "Evidence summary",
			}},
			Recommendation: "Apply the recommendation.", Confidence: 0.9,
			Fingerprint: "fingerprint", ContentHash: "finding-hash",
		}},
		Uncertainties: []featuredelivery.Uncertainty{{
			Category: "architecture", Summary: "One uncertainty",
		}},
		Summary: "Review complete.", ReportHash: "semantic-hash",
		ContentHash: "report-hash", CompletedAt: completedAt,
	}
	reportPayload := mustJSON(t, report)
	if err := registry.Validate(reportSchema, reportPayload); err != nil {
		t.Fatal(err)
	}
	if err := registry.Validate(reportListSchema, mustJSON(t, []featuredelivery.ReviewReport{report})); err != nil {
		t.Fatal(err)
	}
	gate := featuredelivery.ReviewGateResult{
		ID: "gate-1", RoundID: "round-1", SubjectHash: "subject-hash",
		Decision:    featuredelivery.GatePass,
		ReasonCodes: []string{}, BlockingIDs: []string{}, ConflictIDs: []string{},
		CoverageGaps: []string{}, PolicyHash: "policy-hash",
		ReportHashes: []string{"report-hash"}, AdjudicationHashes: []string{},
		ContentHash: "gate-hash", CreatedAt: completedAt,
	}
	if err := registry.Validate(gateSchema, mustJSON(t, gate)); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionBuildsStableParallelPanel(t *testing.T) {
	policy := workflowTestPolicy(t)
	definition, err := Definition(policy)
	if err != nil {
		t.Fatal(err)
	}
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(Schemas()); err != nil {
		t.Fatal(err)
	}
	catalog := agentworkflow.NewCatalog(registry, agentcatalog.New(registry))
	if err := catalog.Publish([]agentworkflow.WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	if len(definition.Nodes) != len(policy.Reviewers)+3 ||
		len(definition.Edges) != len(policy.Reviewers)+2 {
		t.Fatalf("nodes = %d, edges = %d", len(definition.Nodes), len(definition.Edges))
	}
	for index, reviewer := range policy.Reviewers {
		node := definition.Nodes[index]
		if node.ID != reviewerNodeID(reviewer.ID) ||
			node.TransformID != TransformAssignment ||
			node.Optional != !reviewer.Required ||
			node.Retry.MaxAttempts != 2 {
			t.Fatalf("reviewer node %d = %+v", index, node)
		}
		again, err := agentRunID("review.round-1", node.ID, 2)
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := agentRunID("review.round-1", node.ID, 2)
		if err != nil {
			t.Fatal(err)
		}
		firstAttempt, err := agentRunID("review.round-1", node.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if again != repeated || again == firstAttempt {
			t.Fatalf("agent run ids = %q, %q, %q", again, repeated, firstAttempt)
		}
	}
	var reviewerBudget agentworkflow.NodeBudget
	for _, node := range definition.Nodes[:len(policy.Reviewers)] {
		reviewerBudget.MaxInputTokens += node.Budget.MaxInputTokens
		reviewerBudget.MaxOutputTokens += node.Budget.MaxOutputTokens
		reviewerBudget.MaxTotalTokens += node.Budget.MaxTotalTokens
		reviewerBudget.MaxToolCalls += node.Budget.MaxToolCalls
		reviewerBudget.MaxCostMicros += node.Budget.MaxCostMicros
	}
	adjudication := definition.Nodes[len(policy.Reviewers)+1]
	if reviewerBudget.MaxInputTokens+adjudication.Budget.MaxInputTokens != policy.MaxInputTokens ||
		reviewerBudget.MaxOutputTokens+adjudication.Budget.MaxOutputTokens != policy.MaxOutputTokens ||
		reviewerBudget.MaxTotalTokens+adjudication.Budget.MaxTotalTokens != policy.MaxTotalTokens ||
		reviewerBudget.MaxToolCalls+adjudication.Budget.MaxToolCalls != policy.MaxToolCalls ||
		reviewerBudget.MaxCostMicros+adjudication.Budget.MaxCostMicros != policy.MaxCostMicros {
		t.Fatalf(
			"node budgets do not cover policy budget: reviewers=%+v adjudication=%+v policy=%+v",
			reviewerBudget, adjudication.Budget, policy,
		)
	}
}

func TestDefinitionForRoundUsesOnlyFrozenPanel(t *testing.T) {
	policy := workflowTestPolicy(t)
	policy.ContentHash = ""
	policy.Reviewers = append(policy.Reviewers, featuredelivery.ReviewerSpec{
		ID: "operations",
		Agent: agentapi.DefinitionRef{
			ID: "review.operations", Version: 1,
		},
		DefinitionHash: "operations-hash",
		Categories:     []string{"operations"}, ReadOnly: true,
	})
	policy.MaxParallelism = 3
	policy.MaxInputTokens++
	policy.MaxOutputTokens++
	policy.MaxTotalTokens++
	policy.MaxToolCalls++
	policy.MaxCostMicros++
	policy.RiskRuleVersion = "change-risk.v1"
	policy.RiskRules = []featuredelivery.ReviewRiskRule{{
		ID: "large-change",
		Conditions: []featuredelivery.ReviewRiskCondition{{
			Fact:     featuredelivery.RiskFactFilesChanged,
			Operator: featuredelivery.RiskGreaterThanOrEqual,
			Value:    10,
		}},
		ReviewerIDs: []string{"operations"},
	}}
	var err error
	policy, err = featuredelivery.PrepareReviewPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	round := workflowTestRound(t, policy, featuredelivery.ReviewRound{ID: "round-1"})

	definition, err := DefinitionForRound(policy, round)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.Nodes) != 5 || len(definition.Edges) != 4 {
		t.Fatalf("nodes = %d, edges = %d", len(definition.Nodes), len(definition.Edges))
	}
	for _, node := range definition.Nodes {
		if node.ID == reviewerNodeID("operations") {
			t.Fatalf("unselected reviewer node was generated: %+v", node)
		}
	}
}

type retryableReviewError struct{}

func (retryableReviewError) Error() string   { return "temporary review failure" }
func (retryableReviewError) Retryable() bool { return true }

type reviewExecutionStub struct {
	evaluateErr error
	failCalls   int
}

func (*reviewExecutionStub) LoadReviewWorkflowSnapshot(
	context.Context,
	string,
	bool,
) (*featuredelivery.ReviewWorkflowSnapshot, error) {
	return nil, featuredelivery.ErrNotFound
}

func (*reviewExecutionStub) ExecuteReviewAssignmentAttempt(
	context.Context,
	string,
	string,
	string,
	string,
	int,
	agentapi.Actor,
	bool,
) (*featuredelivery.ReviewReport, error) {
	return nil, featuredelivery.ErrNotFound
}

func (stub *reviewExecutionStub) EvaluateReviewWorkflow(
	context.Context,
	string,
	agentapi.Actor,
	bool,
) ([]featuredelivery.ReviewReport, error) {
	return nil, stub.evaluateErr
}

func (*reviewExecutionStub) CompleteReviewWorkflow(
	context.Context,
	string,
	bool,
) (*featuredelivery.ReviewGateResult, error) {
	return nil, featuredelivery.ErrNotFound
}

func (stub *reviewExecutionStub) FailReviewWorkflow(
	context.Context,
	string,
	error,
	bool,
) error {
	stub.failCalls++
	return nil
}

func TestExecutorDefersRoundFailureUntilFinalRetry(t *testing.T) {
	stub := &reviewExecutionStub{evaluateErr: retryableReviewError{}}
	executor := NewExecutor(stub)
	request := agentworkflow.NodeRequest{
		WorkflowRunID: "review.round-1",
		Node: agentworkflow.NodeDefinition{
			ID: NodeAdjudicate, TransformID: TransformAdjudication,
			Retry: agentworkflow.RetryPolicy{MaxAttempts: 2},
		},
		Inputs:  []agentworkflow.Handoff{{Payload: json.RawMessage(`[]`)}},
		Attempt: 1,
		Actor:   agentapi.Actor{UserID: 7},
		EffectivePermissions: agentapi.PermissionPolicy{
			Scopes: []string{platformscope.FeatureDelivery},
		},
	}
	if _, err := executor.Execute(context.Background(), request); err == nil {
		t.Fatal("retryable adjudication unexpectedly succeeded")
	}
	if stub.failCalls != 0 {
		t.Fatalf("round failed before final attempt: %d", stub.failCalls)
	}
	request.Attempt = 2
	if _, err := executor.Execute(context.Background(), request); err == nil {
		t.Fatal("final adjudication attempt unexpectedly succeeded")
	}
	if stub.failCalls != 1 {
		t.Fatalf("round failure calls = %d, want 1", stub.failCalls)
	}
}

func TestExecutorRequiresFeatureDeliveryPermission(t *testing.T) {
	stub := &reviewExecutionStub{}
	_, err := NewExecutor(stub).Execute(t.Context(), agentworkflow.NodeRequest{
		WorkflowRunID: "review.round-1",
		Node: agentworkflow.NodeDefinition{
			ID: NodeAdjudicate, TransformID: TransformAdjudication,
		},
		Inputs: []agentworkflow.Handoff{{Payload: json.RawMessage(`[]`)}},
	})
	if err == nil || !strings.Contains(err.Error(), platformscope.FeatureDelivery) {
		t.Fatalf("Execute error = %v, want feature delivery permission rejection", err)
	}
	if stub.failCalls != 0 {
		t.Fatalf("permission rejection failed review round %d times", stub.failCalls)
	}
}

type reviewCoordinatorStub struct {
	snapshot      featuredelivery.ReviewWorkflowSnapshot
	gate          featuredelivery.ReviewGateResult
	startCalls    int
	completeCalls int
	failCalls     int
	cancelCalls   int
}

func (stub *reviewCoordinatorStub) LoadReviewWorkflowSnapshot(
	context.Context,
	string,
	bool,
) (*featuredelivery.ReviewWorkflowSnapshot, error) {
	snapshot := stub.snapshot
	return &snapshot, nil
}

func (stub *reviewCoordinatorStub) StartReviewWorkflow(
	_ context.Context,
	_ string,
	runID string,
	_ bool,
) (*featuredelivery.ReviewWorkflowSnapshot, error) {
	stub.startCalls++
	stub.snapshot.Round.WorkflowRunID = runID
	stub.snapshot.Round.Status = featuredelivery.RoundRunning
	snapshot := stub.snapshot
	return &snapshot, nil
}

func (stub *reviewCoordinatorStub) CompleteReviewWorkflow(
	context.Context,
	string,
	bool,
) (*featuredelivery.ReviewGateResult, error) {
	stub.completeCalls++
	return &stub.gate, nil
}

func (stub *reviewCoordinatorStub) FailReviewWorkflow(
	context.Context,
	string,
	error,
	bool,
) error {
	stub.failCalls++
	return nil
}

func (stub *reviewCoordinatorStub) CancelReviewRound(
	context.Context,
	string,
	bool,
) error {
	stub.cancelCalls++
	return nil
}

type workflowCoordinatorStub struct {
	run            agentworkflow.WorkflowRunRecord
	resume         agentworkflow.ResumeResult
	executeCalls   int
	resumeCalls    int
	cancelCalls    int
	publishedCalls int
}

func (stub *workflowCoordinatorStub) Execute(
	context.Context,
	agentworkflow.ExecuteRequest,
) (agentworkflow.Result, error) {
	stub.executeCalls++
	return agentworkflow.Result{}, nil
}

func (stub *workflowCoordinatorStub) Resume(
	context.Context,
	string,
) (agentworkflow.ResumeResult, error) {
	stub.resumeCalls++
	return stub.resume, nil
}

func (stub *workflowCoordinatorStub) GetRun(
	context.Context,
	string,
	int64,
	bool,
) (*agentworkflow.WorkflowRunRecord, error) {
	run := stub.run
	return &run, nil
}

func (stub *workflowCoordinatorStub) Cancel(
	context.Context,
	string,
	int64,
	bool,
) (agentworkflow.CancelTransition, error) {
	stub.cancelCalls++
	return agentworkflow.CancelTransition{}, nil
}

func (stub *workflowCoordinatorStub) PublishDefinitions(
	[]agentworkflow.WorkflowDefinition,
	bool,
) error {
	stub.publishedCalls++
	return nil
}

func TestCoordinatorExecutesResumesAndCancels(t *testing.T) {
	policy := workflowTestPolicy(t)
	gate := featuredelivery.ReviewGateResult{
		ID: "gate-1", RoundID: "round-1", Decision: featuredelivery.GatePass,
	}
	for _, test := range []struct {
		name          string
		status        featuredelivery.ReviewRoundStatus
		workflowRunID string
		runStatus     agentworkflow.RunStatus
		wantExecute   int
		wantResume    int
	}{
		{
			name: "execute new", status: featuredelivery.RoundCreated,
			wantExecute: 1,
		},
		{
			name: "resume evaluating", status: featuredelivery.RoundEvaluating,
			workflowRunID: "review.round-1", runStatus: agentworkflow.RunRunning,
			wantResume: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reviews := &reviewCoordinatorStub{
				snapshot: featuredelivery.ReviewWorkflowSnapshot{
					Round: workflowTestRound(t, policy, featuredelivery.ReviewRound{
						ID: "round-1", WorkflowRunID: test.workflowRunID,
						Status: test.status,
					}),
					Policy: policy,
				},
				gate: gate,
			}
			workflows := &workflowCoordinatorStub{
				run: agentworkflow.WorkflowRunRecord{
					ID: "review.round-1", Status: test.runStatus,
				},
				resume: agentworkflow.ResumeResult{
					RunID: "review.round-1", Status: agentworkflow.RunSucceeded,
				},
			}
			coordinator := NewCoordinator(reviews, workflows)

			result, err := coordinator.Execute(
				context.Background(),
				"round-1",
				agentapi.Actor{UserID: 7},
				true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.ID != gate.ID ||
				workflows.executeCalls != test.wantExecute ||
				workflows.resumeCalls != test.wantResume ||
				workflows.publishedCalls != 1 ||
				reviews.completeCalls != 1 {
				t.Fatalf("result = %+v, reviews = %+v, workflows = %+v", result, reviews, workflows)
			}
		})
	}

	reviews := &reviewCoordinatorStub{
		snapshot: featuredelivery.ReviewWorkflowSnapshot{
			Round: workflowTestRound(t, policy, featuredelivery.ReviewRound{
				ID: "round-1", WorkflowRunID: "review.round-1",
				Status: featuredelivery.RoundRunning,
			}),
			Policy: policy,
		},
	}
	workflows := &workflowCoordinatorStub{}
	coordinator := NewCoordinator(reviews, workflows)
	if err := coordinator.Cancel(
		context.Background(),
		"round-1",
		agentapi.Actor{UserID: 7},
		true,
	); err != nil {
		t.Fatal(err)
	}
	if workflows.cancelCalls != 1 || reviews.cancelCalls != 1 {
		t.Fatalf("reviews = %+v, workflows = %+v", reviews, workflows)
	}
}

func TestCoordinatorRejectsTerminalRound(t *testing.T) {
	policy := workflowTestPolicy(t)
	reviews := &reviewCoordinatorStub{
		snapshot: featuredelivery.ReviewWorkflowSnapshot{
			Round: workflowTestRound(t, policy, featuredelivery.ReviewRound{
				ID: "round-1", Status: featuredelivery.RoundFailed,
			}),
			Policy: policy,
		},
	}
	_, err := NewCoordinator(reviews, &workflowCoordinatorStub{}).Execute(
		context.Background(),
		"round-1",
		agentapi.Actor{UserID: 7},
		true,
	)
	if !errors.Is(err, featuredelivery.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestCoordinatorReconcilesRecoveredTerminalRuns(t *testing.T) {
	for _, test := range []struct {
		name         string
		runID        string
		status       agentworkflow.RunStatus
		wantHandled  bool
		wantComplete int
		wantFail     int
		wantCancel   int
	}{
		{
			name: "succeeded", runID: "review.round-1",
			status: agentworkflow.RunSucceeded, wantHandled: true, wantComplete: 1,
		},
		{
			name: "failed", runID: "review.round-1",
			status: agentworkflow.RunFailed, wantHandled: true, wantFail: 1,
		},
		{
			name: "timed out", runID: "review.round-1",
			status: agentworkflow.RunTimedOut, wantHandled: true, wantFail: 1,
		},
		{
			name: "cancelled", runID: "review.round-1",
			status: agentworkflow.RunCancelled, wantHandled: true, wantCancel: 1,
		},
		{
			name: "waiting", runID: "review.round-1",
			status: agentworkflow.RunWaitingHuman,
		},
		{
			name: "other workflow", runID: "delivery.round-1",
			status: agentworkflow.RunFailed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reviews := &reviewCoordinatorStub{}
			coordinator := NewCoordinator(reviews, &workflowCoordinatorStub{})
			handled, err := coordinator.ReconcileRecoveredRun(
				t.Context(),
				test.runID,
				test.status,
				errors.New("recovery failed"),
			)
			if err != nil {
				t.Fatal(err)
			}
			if handled != test.wantHandled ||
				reviews.completeCalls != test.wantComplete ||
				reviews.failCalls != test.wantFail ||
				reviews.cancelCalls != test.wantCancel {
				t.Fatalf("handled = %t, reviews = %+v", handled, reviews)
			}
		})
	}
}

func workflowTestPolicy(t *testing.T) featuredelivery.ReviewPolicy {
	t.Helper()
	policy, err := featuredelivery.PrepareReviewPolicy(featuredelivery.ReviewPolicy{
		ID: "review-system-design", Version: 1,
		SubjectKind: featuredelivery.SubjectSystemDesign,
		Reviewers: []featuredelivery.ReviewerSpec{
			{
				ID: "architecture",
				Agent: agentapi.DefinitionRef{
					ID: "review.architecture", Version: 1,
				},
				DefinitionHash: "architecture-hash",
				Categories:     []string{"architecture"}, Required: true, ReadOnly: true,
			},
			{
				ID: "security",
				Agent: agentapi.DefinitionRef{
					ID: "review.security", Version: 1,
				},
				DefinitionHash: "security-hash",
				Categories:     []string{"security"}, Required: true, ReadOnly: true,
			},
		},
		Adjudicator: &featuredelivery.AdjudicatorSpec{
			Agent: agentapi.DefinitionRef{
				ID: "review.adjudicator", Version: 1,
			},
			DefinitionHash: "adjudicator-hash", ReadOnly: true,
		},
		BlockingSeverities: []featuredelivery.Severity{
			featuredelivery.SeverityCritical,
			featuredelivery.SeverityHigh,
		},
		RequiredCategories:     []string{"architecture", "security"},
		MaxParallelism:         2,
		MaxInputTokens:         3,
		MaxOutputTokens:        3,
		MaxTotalTokens:         3,
		MaxToolCalls:           3,
		MaxCostMicros:          3,
		MaxRetries:             1,
		Timeout:                time.Minute,
		OptionalReviewerAction: featuredelivery.OptionalReviewerContinue,
		CreatedAt:              time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func workflowTestRound(
	t *testing.T,
	policy featuredelivery.ReviewPolicy,
	round featuredelivery.ReviewRound,
) featuredelivery.ReviewRound {
	t.Helper()
	facts, err := featuredelivery.BuildArtifactReviewRiskFacts(featuredelivery.Artifact{})
	if err != nil {
		t.Fatal(err)
	}
	facts, riskHash, reviewers, panelHash, err := featuredelivery.PrepareReviewPanel(policy, facts)
	if err != nil {
		t.Fatal(err)
	}
	round.PolicyID = policy.ID
	round.PolicyVersion = policy.Version
	round.PolicyHash = policy.ContentHash
	round.RiskFacts = facts
	round.RiskHash = riskHash
	round.RuleVersion = policy.RiskRuleVersion
	round.Reviewers = reviewers
	round.PanelHash = panelHash
	return round
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

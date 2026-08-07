package app

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentcatalog"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	"github.com/dekwanlabs/nasuta/internal/featurepipeline"
	"github.com/dekwanlabs/nasuta/internal/featurereviewworkflow"
	"github.com/dekwanlabs/nasuta/platform/config"
)

type appReviewStore struct {
	featuredelivery.Store
	policies    []featuredelivery.ReviewPolicy
	round       *featuredelivery.ReviewRound
	assignments []featuredelivery.ReviewAssignment
}

type appAgentRuntimeFunc func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error)

func (store *appReviewStore) SaveReviewPolicies(
	_ context.Context,
	policies []featuredelivery.ReviewPolicy,
) error {
	store.policies = append([]featuredelivery.ReviewPolicy(nil), policies...)
	return nil
}

func (store *appReviewStore) GetReviewPolicy(
	_ context.Context,
	id string,
	version int64,
) (*featuredelivery.ReviewPolicy, error) {
	for index := range store.policies {
		if store.policies[index].ID == id && store.policies[index].Version == version {
			policy := store.policies[index]
			return &policy, nil
		}
	}
	return nil, featuredelivery.ErrNotFound
}

func (store *appReviewStore) GetReviewRound(
	_ context.Context,
	id string,
) (*featuredelivery.ReviewRound, error) {
	if store.round == nil || store.round.ID != id {
		return nil, featuredelivery.ErrNotFound
	}
	round := *store.round
	return &round, nil
}

func (store *appReviewStore) ListReviewAssignments(
	_ context.Context,
	roundID string,
	_ featuredelivery.ReviewAssignmentCursor,
	_ int,
) ([]featuredelivery.ReviewAssignment, error) {
	if store.round == nil || store.round.ID != roundID {
		return nil, featuredelivery.ErrNotFound
	}
	return append([]featuredelivery.ReviewAssignment(nil), store.assignments...), nil
}

func (run appAgentRuntimeFunc) Run(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	return run(ctx, request)
}

func TestFeatureGenerationUsesLargestConfiguredAnswerBudget(t *testing.T) {
	settings := &config.PlatformSettings{
		LLMMaxTokens: 4000, LLMAnswerMaxTokens: 6000, LLMConclusionMaxTokens: 12000,
	}
	if got := featureGenerationTokenBudget(settings); got != 12000 {
		t.Fatalf("feature generation token budget=%d, want 12000", got)
	}
}

func TestFeatureDeliveryStatusReportsCodingInitializationFailure(t *testing.T) {
	runtime := featureDeliveryRuntime{
		service:      featuredelivery.NewService(nil, nil, 0),
		codingReason: "workspace_unavailable",
	}

	status := runtime.status(context.Background())
	if status.Coding.Reason != "workspace_unavailable" {
		t.Fatalf("Coding.Reason = %q, want workspace_unavailable", status.Coding.Reason)
	}
}

func TestFeatureReviewRuntimeRejectsConfiguredLLMWithoutRuntime(t *testing.T) {
	platform := &Platform{
		featureDelivery: featureDeliveryRuntime{
			service: featuredelivery.NewService(&appReviewStore{}, nil, 0),
		},
	}
	if err := platform.configureFeatureReviewRuntime(enabledAgentSettings(), nil, nil); err == nil {
		t.Fatal("configured LLM accepted a missing definition runtime")
	}
}

func TestFeatureReviewStartupUsesPublishedPositiveDefinitionVersion(t *testing.T) {
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, 1)
	if err != nil {
		t.Fatal(err)
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(agentcatalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	catalog := agentcatalog.New(schemas)
	if err := catalog.Publish(definitions); err != nil {
		t.Fatal(err)
	}
	runtime := appAgentRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		return agentapi.RunResult{}, nil
	})
	platform := &Platform{
		settings: settings, agentCatalog: catalog, agentDefinitionVer: 1,
		definitionRuntime: runtime,
	}

	activeSettings, activeRuntime, activeDefinitions, err := platform.currentFeatureReviewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if activeSettings == nil || activeRuntime == nil || len(activeDefinitions) != len(definitions) {
		t.Fatalf("active runtime = %v, definitions = %d", activeRuntime, len(activeDefinitions))
	}
	for index := range definitions {
		if activeDefinitions[index].Version != 1 ||
			activeDefinitions[index].ContentHash != definitions[index].ContentHash {
			t.Fatalf("active definition %d = %+v", index, activeDefinitions[index])
		}
	}

	platform.agentDefinitionVer = 0
	if _, _, _, err := platform.currentFeatureReviewRuntime(); err == nil {
		t.Fatal("zero definition version was accepted at feature delivery startup")
	}
}

func TestFeatureReviewRuntimeClearsRunnerWhenLLMIsDisabled(t *testing.T) {
	store := &appReviewStore{}
	service := featuredelivery.NewService(store, nil, 0)
	platform := reviewWorkflowTestPlatform(t, service)
	runtime := appAgentRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		return agentapi.RunResult{}, nil
	})
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.configureFeatureReviewRuntime(settings, runtime, definitions); err != nil {
		t.Fatal(err)
	}
	if len(store.policies) != 8 {
		t.Fatalf("default policies = %d, want 8", len(store.policies))
	}
	policy := store.policies[0]
	facts, err := featuredelivery.BuildArtifactReviewRiskFacts(featuredelivery.Artifact{})
	if err != nil {
		t.Fatal(err)
	}
	facts, riskHash, reviewers, panelHash, err := featuredelivery.PrepareReviewPanel(policy, facts)
	if err != nil {
		t.Fatal(err)
	}
	store.round = &featuredelivery.ReviewRound{
		ID: "round-1",
		Subject: featuredelivery.ReviewSubject{
			Kind: policy.SubjectKind,
		},
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		RiskFacts: facts, RiskHash: riskHash, RuleVersion: policy.RiskRuleVersion,
		Reviewers: reviewers, PanelHash: panelHash, Status: featuredelivery.RoundCreated,
	}
	store.assignments = make([]featuredelivery.ReviewAssignment, 0, len(reviewers))
	for _, reviewer := range reviewers {
		store.assignments = append(store.assignments, featuredelivery.ReviewAssignment{
			ID: "assignment." + reviewer.ID, RoundID: store.round.ID,
			ReviewerID: reviewer.ID, Agent: reviewer.Agent,
			DefinitionHash: reviewer.DefinitionHash,
			Categories:     append([]string(nil), reviewer.Categories...),
			Required:       reviewer.Required, Status: featuredelivery.AssignmentQueued,
		})
	}
	disabled := &config.PlatformSettings{}
	disabled.Apply(nil)
	if err := platform.configureFeatureReviewRuntime(disabled, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteReviewRound(
		context.Background(), "round-1", agentapi.Actor{}, true,
	); !errors.Is(err, featuredelivery.ErrUnavailable) {
		t.Fatalf("execute error = %v, want unavailable", err)
	}
}

func reviewWorkflowTestPlatform(
	t *testing.T,
	service *featuredelivery.Service,
) *Platform {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schemas := agentapi.NewSchemaRegistry()
	for _, definitions := range [][]agentapi.SchemaDefinition{
		agentcatalog.DefaultSchemas(),
		featurepipeline.Schemas(),
		featurereviewworkflow.Schemas(),
	} {
		if err := schemas.Publish(definitions); err != nil {
			t.Fatal(err)
		}
	}
	agents := agentcatalog.New(schemas)
	workflows := agentworkflow.NewCatalog(schemas, agents)
	workflowStore, err := agentworkflow.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	workflowService, err := agentworkflow.NewService(workflows, workflowStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Platform{
		schemaRegistry:  schemas,
		agentCatalog:    agents,
		workflowCatalog: workflows,
		workflowStore:   workflowStore,
		workflowService: workflowService,
		featureDelivery: featureDeliveryRuntime{service: service},
	}
}

package featuredelivery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type failureStore struct {
	Store
	status        RunStatus
	event         EventKind
	summary       string
	transitionErr error
}

type eventBatchStore struct {
	Store
	mu      sync.Mutex
	batches [][]RunEvent
	nextSeq int64
	notify  chan struct{}
}

type statusRunner struct {
	statuses map[string]CodingProviderStatus
}

type workflowStore struct {
	Store
	mu           sync.Mutex
	artifacts    map[string]*Artifact
	status       RunStatus
	events       []RunEvent
	change       *ChangeSet
	errorSummary string
}

type workflowRunner struct{}

func (runner statusRunner) Run(context.Context, CodingRequest, EventSink) (CodingResult, error) {
	return CodingResult{}, nil
}

func (runner statusRunner) ProviderStatus(context.Context) map[string]CodingProviderStatus {
	return runner.statuses
}

func (workflowRunner) Run(ctx context.Context, request CodingRequest, sink EventSink) (CodingResult, error) {
	if err := os.WriteFile(filepath.Join(request.WorktreePath, "message.txt"), []byte("changed\n"), 0o600); err != nil {
		return CodingResult{}, err
	}
	if err := sink(ctx, ProviderEvent{Kind: EventProviderMessage, Summary: "editing requested file"}); err != nil {
		return CodingResult{}, err
	}
	if err := sink(ctx, ProviderEvent{Kind: EventFileChanged, Summary: "message.txt"}); err != nil {
		return CodingResult{}, err
	}
	return CodingResult{
		ProviderVersion: "test-1", ExitCode: 0, Summary: "updated message",
	}, nil
}

func (workflowRunner) ProviderStatus(context.Context) map[string]CodingProviderStatus {
	return map[string]CodingProviderStatus{"test": {
		Enabled: true, BinaryFound: true, ContractCompatible: true, CredentialIsolated: true,
	}}
}

func (store *workflowStore) GetArtifact(_ context.Context, id string) (*Artifact, error) {
	artifact, ok := store.artifacts[id]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *artifact
	return &copy, nil
}

func (store *workflowStore) TransitionImplementation(_ context.Context, _ string, _ string, from, to RunStatus, _ RunUpdate) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.status != from || !CanTransitionRun(from, to) {
		return ErrConflict
	}
	store.status = to
	return nil
}

func (store *workflowStore) AppendRunEvent(_ context.Context, event RunEvent) (*RunEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	event.Seq = int64(len(store.events) + 1)
	store.events = append(store.events, event)
	return &event, nil
}

func (store *workflowStore) AppendRunEvents(_ context.Context, events []RunEvent) ([]RunEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	persisted := make([]RunEvent, len(events))
	for index := range events {
		persisted[index] = events[index]
		persisted[index].Seq = int64(len(store.events) + 1)
		store.events = append(store.events, persisted[index])
	}
	return persisted, nil
}

func (store *workflowStore) SaveChangeSetAndFinish(_ context.Context, change ChangeSet, status RunStatus, errorSummary string, _ time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.status != RunValidating {
		return ErrConflict
	}
	copy := change
	store.change = &copy
	store.status = status
	store.errorSummary = errorSummary
	return nil
}

func (store *workflowStore) snapshot() (RunStatus, *ChangeSet, []RunEvent, string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var change *ChangeSet
	if store.change != nil {
		copy := *store.change
		change = &copy
	}
	return store.status, change, append([]RunEvent(nil), store.events...), store.errorSummary
}

func (store *eventBatchStore) AppendRunEvents(_ context.Context, events []RunEvent) ([]RunEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	persisted := make([]RunEvent, len(events))
	copy(persisted, events)
	for index := range persisted {
		store.nextSeq++
		persisted[index].Seq = store.nextSeq
	}
	store.batches = append(store.batches, persisted)
	if store.notify != nil {
		select {
		case store.notify <- struct{}{}:
		default:
		}
	}
	return persisted, nil
}

func (store *eventBatchStore) snapshot() [][]RunEvent {
	store.mu.Lock()
	defer store.mu.Unlock()
	batches := make([][]RunEvent, len(store.batches))
	for index := range store.batches {
		batches[index] = append([]RunEvent(nil), store.batches[index]...)
	}
	return batches
}

func (store *failureStore) TransitionImplementation(_ context.Context, _ string, _ string, _ RunStatus, to RunStatus, update RunUpdate) error {
	store.status = to
	store.summary = update.ErrorSummary
	return store.transitionErr
}

func (store *failureStore) AppendRunEvent(_ context.Context, event RunEvent) (*RunEvent, error) {
	store.event = event.Kind
	event.Seq = 1
	return &event, nil
}

func TestImplementationFailureClassification(t *testing.T) {
	tests := []struct {
		name        string
		cause       error
		runErr      error
		wantStatus  RunStatus
		wantEvent   EventKind
		wantSummary string
	}{
		{
			name: "administrator cancellation", cause: errImplementationCancelled,
			runErr: context.Canceled, wantStatus: RunCancelled,
			wantEvent: EventRunCancelled, wantSummary: "implementation cancelled",
		},
		{
			name: "timeout", cause: errImplementationTimedOut,
			runErr: context.DeadlineExceeded, wantStatus: RunFailed,
			wantEvent: EventRunFailed, wantSummary: "implementation timed out",
		},
		{
			name: "shutdown", cause: context.Canceled,
			runErr: context.Canceled, wantStatus: RunInterrupted,
			wantEvent: EventRunInterrupted, wantSummary: "implementation interrupted",
		},
		{
			name: "lease loss", cause: errImplementationLeaseLost,
			runErr: context.Canceled, wantStatus: RunInterrupted,
			wantEvent: EventRunInterrupted, wantSummary: "worker lease lost",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runCtx, cancel := context.WithCancelCause(context.Background())
			cancel(test.cause)
			store := &failureStore{}
			manager := &ImplementationManager{
				store: store, hub: NewEventHub(),
				config: ImplementationConfig{WorktreeTTL: time.Hour},
				now:    func() time.Time { return time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC) },
			}
			manager.finishFailureWithResult(
				runCtx,
				ImplementationRun{ID: "run-1"},
				"worker-1",
				RunRunning,
				CodingResult{},
				test.runErr,
			)
			if store.status != test.wantStatus || store.event != test.wantEvent || store.summary != test.wantSummary {
				t.Fatalf("got status=%s event=%s summary=%q", store.status, store.event, store.summary)
			}
		})
	}
}

func TestImplementationFailureDoesNotEmitAfterLostTransition(t *testing.T) {
	runCtx, cancel := context.WithCancelCause(context.Background())
	cancel(errImplementationLeaseLost)
	store := &failureStore{transitionErr: ErrConflict}
	manager := &ImplementationManager{
		store: store, hub: NewEventHub(),
		config: ImplementationConfig{WorktreeTTL: time.Hour},
		now:    func() time.Time { return time.Now().UTC() },
	}
	manager.finishFailure(runCtx, ImplementationRun{ID: "run-1"}, "worker-1", RunRunning, context.Canceled)
	if store.event != "" {
		t.Fatalf("event emitted after failed transition: %s", store.event)
	}
	if !errors.Is(store.transitionErr, ErrConflict) {
		t.Fatal("test setup lost transition conflict")
	}
}

func TestValidateReimplementationParent(t *testing.T) {
	rejected := &ChangeReview{Decision: DecisionRejected}
	base := ImplementationRun{
		RequestID: "feat-1", Repo: "team/nasuta", Status: RunSucceeded, Review: rejected,
	}
	if err := validateReimplementationParent(base, "feat-1", "team/nasuta"); err != nil {
		t.Fatalf("valid parent rejected: %v", err)
	}
	tests := []struct {
		name   string
		parent ImplementationRun
	}{
		{name: "different feature", parent: func() ImplementationRun { value := base; value.RequestID = "feat-2"; return value }()},
		{name: "different repository", parent: func() ImplementationRun { value := base; value.Repo = "team/other"; return value }()},
		{name: "failed run", parent: func() ImplementationRun { value := base; value.Status = RunFailed; return value }()},
		{name: "not reviewed", parent: func() ImplementationRun { value := base; value.Review = nil; return value }()},
		{name: "approved", parent: func() ImplementationRun {
			value := base
			value.Review = &ChangeReview{Decision: DecisionApproved}
			return value
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateReimplementationParent(test.parent, "feat-1", "team/nasuta"); !errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCanTransitionRun(t *testing.T) {
	allowed := map[[2]RunStatus]struct{}{
		{RunQueued, RunPreparing}:       {},
		{RunQueued, RunCancelled}:       {},
		{RunPreparing, RunRunning}:      {},
		{RunPreparing, RunFailed}:       {},
		{RunPreparing, RunCancelled}:    {},
		{RunPreparing, RunInterrupted}:  {},
		{RunRunning, RunValidating}:     {},
		{RunRunning, RunFailed}:         {},
		{RunRunning, RunCancelled}:      {},
		{RunRunning, RunInterrupted}:    {},
		{RunValidating, RunSucceeded}:   {},
		{RunValidating, RunFailed}:      {},
		{RunValidating, RunCancelled}:   {},
		{RunValidating, RunInterrupted}: {},
	}
	statuses := []RunStatus{
		RunQueued, RunPreparing, RunRunning, RunValidating,
		RunSucceeded, RunFailed, RunCancelled, RunInterrupted,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			_, want := allowed[[2]RunStatus{from, to}]
			if got := CanTransitionRun(from, to); got != want {
				t.Fatalf("CanTransitionRun(%s, %s) = %t, want %t", from, to, got, want)
			}
		}
	}
}

func TestImplementationStatusRequiresReadyEnabledProvider(t *testing.T) {
	ready := CodingProviderStatus{
		Enabled: true, BinaryFound: true, ContractCompatible: true, CredentialIsolated: true,
	}
	tests := []struct {
		name     string
		statuses map[string]CodingProviderStatus
		want     bool
	}{
		{name: "ready configured provider", statuses: map[string]CodingProviderStatus{"codex": ready}, want: true},
		{name: "configured provider not ready", statuses: map[string]CodingProviderStatus{"codex": {Enabled: true, BinaryFound: false, Reason: "binary_not_found"}}},
		{name: "only unconfigured provider ready", statuses: map[string]CodingProviderStatus{"claude": ready}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewImplementationManager(nil, nil, &GitManager{}, statusRunner{statuses: test.statuses}, ImplementationConfig{
				EnabledProviders: []string{"codex"},
			})
			status := manager.Status(context.Background())
			if status.Enabled != test.want || !status.GitFound {
				t.Fatalf("status = %+v", status)
			}
		})
	}
}

func TestImplementationExecutesInIsolatedWorktreeAndPersistsValidatedChangeSet(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	workspaceRoot := t.TempDir()
	repository := "team/service"
	repoPath := filepath.Join(workspaceRoot, "repos", filepath.FromSlash(repository))
	if err := os.MkdirAll(filepath.Join(repoPath, ".nasuta"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repoPath, "init")
	runGit(t, git, repoPath, "config", "user.email", "test@example.com")
	runGit(t, git, repoPath, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repoPath, "message.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	delivery := []byte(`{"validation":[{"argv":["sh","-c","test \"$(cat message.txt)\" = changed"],"timeout":"5s"}]}`)
	if err := os.WriteFile(filepath.Join(repoPath, ".nasuta", "delivery.json"), delivery, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repoPath, "add", ".")
	runGit(t, git, repoPath, "commit", "-m", "base")
	baseCommit := gitText(t, git, repoPath, "rev-parse", "HEAD")

	planID := "art_plan"
	designID := "art_design"
	store := &workflowStore{
		status: RunPreparing,
		artifacts: map[string]*Artifact{
			planID: {
				ID: planID, Kind: KindImplementationPlan, Version: 1, ParentArtifactID: designID,
				DocumentJSON:     json.RawMessage(`{"repositories":[{"repository":"team/service","expected_paths":["message.txt"],"steps":[{"description":"update message","done_when":["validation passes"]}]}]}`),
				RenderedMarkdown: "# Implementation plan",
			},
			designID: {
				ID: designID, Kind: KindSystemDesign, Version: 1,
				DocumentJSON:     json.RawMessage(`{"architecture_boundaries":["repository"]}`),
				RenderedMarkdown: "# System design",
			},
		},
	}
	codingRoot := t.TempDir()
	workspaces, err := NewWorkspaceManager(store, codingRoot)
	if err != nil {
		t.Fatal(err)
	}
	gitManager, err := NewGitManager(workspaceRoot, codingRoot, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewImplementationManager(store, workspaces, gitManager, workflowRunner{}, ImplementationConfig{
		Timeout: 10 * time.Second, WorktreeTTL: time.Hour,
		EnabledProviders: []string{"test"}, MaxConcurrency: 1,
	})
	run := ImplementationRun{
		ID: "run_test", RequestID: "feat_test", DesignArtifactID: designID, PlanArtifactID: planID,
		Repo: repository, BaseRef: "HEAD", BaseCommit: baseCommit,
		WorkspaceUserID: 7, WorkspaceUsername: "developer", Provider: "test", Status: RunPreparing,
	}
	manager.execute(context.Background(), "worker-test", run)

	status, change, events, errorSummary := store.snapshot()
	if status != RunSucceeded || errorSummary != "" {
		t.Fatalf("terminal status=%s error=%q", status, errorSummary)
	}
	if change == nil || change.FilesChanged != 1 || len(change.Files) != 1 || change.Files[0].Path != "message.txt" {
		t.Fatalf("change set = %+v", change)
	}
	if len(change.ValidationResults) != 1 || change.ValidationResults[0].Status != "passed" {
		t.Fatalf("validation results = %+v", change.ValidationResults)
	}
	patch, err := os.ReadFile(filepath.Join(gitManager.artifactsRoot, filepath.FromSlash(change.PatchRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "-before") || !strings.Contains(string(patch), "+changed") {
		t.Fatalf("patch does not contain expected change:\n%s", patch)
	}
	original, err := os.ReadFile(filepath.Join(repoPath, "message.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != "before\n" {
		t.Fatalf("source repository was modified: %q", original)
	}
	wantEvents := map[EventKind]struct{}{
		EventRunPreparing: {}, EventProviderStarted: {}, EventProviderMessage: {}, EventFileChanged: {},
		EventProviderFinished: {}, EventValidationStarted: {}, EventValidationFinished: {},
		EventChangeSetReady: {}, EventRunSucceeded: {},
	}
	for _, event := range events {
		delete(wantEvents, event.Kind)
	}
	if len(wantEvents) != 0 {
		t.Fatalf("missing lifecycle events: %v", wantEvents)
	}
}

func gitText(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	command := exec.Command(git, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestReconcilePlanDeviationsUsesPathPrefixesAndReportedReasons(t *testing.T) {
	deviations := reconcilePlanDeviations(
		[]ChangedFile{
			{Path: "internal/featuredelivery/service.go"},
			{Path: "README.md"},
			{Path: "internal/transport/handler.go"},
			{Path: "docs/new.md"},
		},
		[]string{"internal/featuredelivery", "README.md"},
		[]PlanDeviation{{Path: "internal/transport/handler.go", Reason: "the API contract also changed", Explained: true}},
	)
	if len(deviations) != 2 {
		t.Fatalf("deviations = %#v", deviations)
	}
	if deviations[0].Path != "internal/transport/handler.go" || !deviations[0].Explained ||
		deviations[0].Reason != "the API contract also changed" {
		t.Fatalf("reported deviation = %#v", deviations[0])
	}
	if deviations[1].Path != "docs/new.md" || deviations[1].Explained {
		t.Fatalf("unexplained deviation = %#v", deviations[1])
	}
}

func TestTaskPackageCarriesApprovedChainAndCodingBoundaries(t *testing.T) {
	requirement := &Artifact{
		ID: "artifact-requirement", Kind: KindRequirement, Version: 1,
		RenderedMarkdown: "# Product Requirement\n\nBuild export.",
	}
	analysis := &Artifact{
		ID: "artifact-analysis", Kind: KindRequirementAnalysis, Version: 2,
		ParentArtifactID: requirement.ID, RenderedMarkdown: "# Requirement Analysis\n\nExport is in scope.",
	}
	proposal := &Artifact{
		ID: "artifact-proposal", Kind: KindTechnicalProposal, Version: 1,
		ParentArtifactID: analysis.ID, RenderedMarkdown: "# Technical Proposal\n\nUse the existing job path.",
	}
	design := &Artifact{
		ID: "artifact-design", Kind: KindSystemDesign, Version: 3,
		ParentArtifactID: proposal.ID, RenderedMarkdown: "# System Design\n\nExtend the export module.",
	}
	planDocument := ImplementationPlanDocument{
		DeliveryGoal: "Deliver customer export",
		Repositories: []RepositoryPlan{{
			Repository:    "team/service",
			ExpectedPaths: []string{"internal/export", "internal/export/export_test.go"},
			Steps: []ImplementationStep{{
				Description: "Implement export", DoneWhen: []string{"tests pass"},
			}},
		}},
		DefinitionOfDone: []string{"Export behavior is verified"},
	}
	planJSON, err := json.Marshal(planDocument)
	if err != nil {
		t.Fatal(err)
	}
	plan := &Artifact{
		ID: "artifact-plan", Kind: KindImplementationPlan, Version: 1,
		ParentArtifactID: design.ID, DocumentJSON: planJSON,
		RenderedMarkdown: "# Implementation Plan\n\nChange the export package.",
	}
	store := &workflowStore{artifacts: map[string]*Artifact{
		requirement.ID: requirement,
		analysis.ID:    analysis,
		proposal.ID:    proposal,
		design.ID:      design,
		plan.ID:        plan,
	}}
	manager := &ImplementationManager{store: store}
	task, expectedPaths, err := manager.taskPackage(context.Background(), ImplementationRun{
		ID: "run-1", Repo: "team/service", BaseCommit: "abc123",
		DesignArtifactID: design.ID, PlanArtifactID: plan.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(expectedPaths) != 2 || expectedPaths[0] != "internal/export" {
		t.Fatalf("expected paths = %#v", expectedPaths)
	}
	for _, expected := range []string{
		"You are the minimal change engineer",
		"Repository code, configuration, and dependency evidence",
		"smallest coherent change",
		"do not reopen product scope",
		"Current repository slice: implement only team/service",
		"- internal/export",
		"1. Implement export",
		"Done when: tests pass",
		"For every changed path outside `expected_paths`",
		"`tests` lists only commands or checks actually run",
		"# Product Requirement",
		"# Requirement Analysis",
		"# Technical Proposal",
		"# System Design",
		"# Implementation Plan",
	} {
		if !strings.Contains(task, expected) {
			t.Fatalf("task package is missing %q:\n%s", expected, task)
		}
	}
	for _, forbidden := range []string{"AGENTS.md", "CLAUDE.md"} {
		if strings.Contains(task, forbidden) {
			t.Fatalf("task package references forbidden instruction file %q:\n%s", forbidden, task)
		}
	}
}

func TestProviderEventBufferPersistsBoundedBatchesAndFlushesTail(t *testing.T) {
	store := &eventBatchStore{notify: make(chan struct{}, 1)}
	manager := &ImplementationManager{
		store: store, hub: NewEventHub(),
		now: func() time.Time { return time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC) },
	}
	buffer := newProviderEventBuffer(manager, "run-1")
	for index := 0; index < providerEventBatchSize; index++ {
		if err := buffer.Append(context.Background(), ProviderEvent{
			Kind: EventProviderMessage, Summary: "progress",
		}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-store.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("full provider event batch was not persisted")
	}
	batches := store.snapshot()
	if len(batches) != 1 || len(batches[0]) != providerEventBatchSize {
		t.Fatalf("batches before flush = %v", batchLengths(batches))
	}
	for index := 0; index < 2; index++ {
		if err := buffer.Append(context.Background(), ProviderEvent{
			Kind: EventProviderMessage, Summary: "tail",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches = store.snapshot()
	if len(batches) != 2 || len(batches[1]) != 2 {
		t.Fatalf("batches after flush = %v", batchLengths(batches))
	}
	if batches[0][0].Seq != 1 || batches[1][1].Seq != int64(providerEventBatchSize+2) {
		t.Fatalf("event sequences are not monotonic: first=%d last=%d", batches[0][0].Seq, batches[1][1].Seq)
	}
}

func TestProviderEventBufferFlushesSparseProgress(t *testing.T) {
	store := &eventBatchStore{notify: make(chan struct{}, 1)}
	buffer := newProviderEventBuffer(&ImplementationManager{
		store: store, hub: NewEventHub(), now: time.Now,
	}, "run-1")
	if err := buffer.Append(context.Background(), ProviderEvent{
		Kind: EventCommandStarted, Summary: "go test ./...",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("sparse provider event was not flushed")
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches := store.snapshot()
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("sparse event batches = %v", batchLengths(batches))
	}
}

func TestProviderEventBufferRejectsLifecycleEvents(t *testing.T) {
	buffer := newProviderEventBuffer(&ImplementationManager{
		store: &eventBatchStore{}, hub: NewEventHub(), now: time.Now,
	}, "run-1")
	if err := buffer.Append(context.Background(), ProviderEvent{Kind: EventRunSucceeded}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("lifecycle event error = %v", err)
	}
	if err := buffer.Append(context.Background(), ProviderEvent{
		Kind: EventProviderMessage, Detail: json.RawMessage(`{"broken"`),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid detail error = %v", err)
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func batchLengths(batches [][]RunEvent) []int {
	lengths := make([]int, len(batches))
	for index := range batches {
		lengths[index] = len(batches[index])
	}
	return lengths
}

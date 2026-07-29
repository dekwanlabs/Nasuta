package featuredelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/log"
)

const (
	implementationLease     = 30 * time.Second
	implementationPoll      = time.Second
	implementationCleanup   = time.Minute
	implementationRecovery  = 30 * time.Second
	providerEventBatchSize  = 32
	providerEventFlush      = 250 * time.Millisecond
	eventFlushTimeout       = 5 * time.Second
	maxProviderDetailBytes  = 64 << 10
	maxClientRequestIDBytes = 128
	maxRunErrorBytes        = 2048
)

var (
	errImplementationCancelled = errors.New("implementation cancelled by administrator")
	errImplementationTimedOut  = errors.New("implementation timed out")
	errImplementationLeaseLost = errors.New("implementation worker lease lost")
)

type ImplementationOptions struct {
	ClientRequestID  string `json:"client_request_id"`
	DesignArtifactID string `json:"design_artifact_id"`
	PlanArtifactID   string `json:"plan_artifact_id"`
	ParentRunID      string `json:"parent_run_id,omitempty"`
	Repository       string `json:"repository"`
	BaseRef          string `json:"base_ref"`
	Provider         string `json:"provider"`
	Model            string `json:"model,omitempty"`
	NetworkEnabled   bool   `json:"network_enabled"`
}

type ImplementationConfig struct {
	Timeout          time.Duration
	WorktreeTTL      time.Duration
	MaxConcurrency   int
	AllowNetwork     bool
	EnabledProviders []string
	DefaultModels    map[string]string
}

// ImplementationManager owns the lifecycle that cannot be derived from artifacts.
type ImplementationManager struct {
	store      Store
	workspaces *WorkspaceManager
	git        *GitManager
	runner     CodingRunner
	hub        *EventHub
	config     ImplementationConfig
	enabled    map[string]struct{}
	now        func() time.Time

	cancelMu sync.Mutex
	cancels  map[string]context.CancelCauseFunc
}

func NewImplementationManager(store Store, workspaces *WorkspaceManager, git *GitManager, runner CodingRunner, config ImplementationConfig) *ImplementationManager {
	enabled := make(map[string]struct{}, len(config.EnabledProviders))
	for _, provider := range config.EnabledProviders {
		enabled[provider] = struct{}{}
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Minute
	}
	if config.WorktreeTTL <= 0 {
		config.WorktreeTTL = 72 * time.Hour
	}
	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = 1
	}
	return &ImplementationManager{
		store: store, workspaces: workspaces, git: git, runner: runner,
		hub: NewEventHub(), config: config, enabled: enabled,
		now:     func() time.Time { return time.Now().UTC() },
		cancels: make(map[string]context.CancelCauseFunc),
	}
}

func (manager *ImplementationManager) Create(ctx context.Context, feature FeatureRequest, lineage Lineage, options ImplementationOptions, requestedBy int64) (*ImplementationRun, bool, error) {
	if manager == nil || manager.git == nil || manager.runner == nil {
		return nil, false, ErrUnavailable
	}
	if options.ClientRequestID == "" || len(options.ClientRequestID) > maxClientRequestIDBytes {
		return nil, false, fmt.Errorf("client_request_id must be between 1 and %d bytes: %w", maxClientRequestIDBytes, ErrInvalid)
	}
	if lineage.SystemDesign == nil || lineage.ImplementationPlan == nil ||
		lineage.SystemDesign.ID != options.DesignArtifactID || lineage.ImplementationPlan.ID != options.PlanArtifactID {
		return nil, false, ErrConflict
	}
	repo, err := NormalizeRepository(options.Repository)
	if err != nil {
		return nil, false, err
	}
	if !planContainsRepository(*lineage.ImplementationPlan, repo) {
		return nil, false, ErrConflict
	}
	if options.ParentRunID != "" {
		parent, err := manager.store.GetImplementation(ctx, options.ParentRunID)
		if err != nil {
			return nil, false, err
		}
		if err := validateReimplementationParent(*parent, feature.ID, repo); err != nil {
			return nil, false, err
		}
	}
	if _, ok := manager.enabled[options.Provider]; !ok {
		return nil, false, fmt.Errorf("coding provider %q is not enabled: %w", options.Provider, ErrUnavailable)
	}
	status, ok := manager.runner.ProviderStatus(ctx)[options.Provider]
	if !ok || !providerReady(status) {
		reason := status.Reason
		if reason == "" {
			reason = "provider_not_ready"
		}
		return nil, false, fmt.Errorf("coding provider %q is unavailable (%s): %w", options.Provider, reason, ErrUnavailable)
	}
	if options.NetworkEnabled && !manager.config.AllowNetwork {
		return nil, false, fmt.Errorf("network is disabled by platform policy: %w", ErrInvalid)
	}
	model := strings.TrimSpace(options.Model)
	if model == "" {
		model = manager.config.DefaultModels[options.Provider]
	}
	_, baseCommit, err := manager.git.ResolveBaseCommit(ctx, repo, options.BaseRef)
	if err != nil {
		return nil, false, err
	}
	identity, err := manager.store.GetOwnerIdentity(ctx, feature.CreatedBy)
	if err != nil {
		return nil, false, err
	}
	workspace, _, err := manager.workspaces.ResolveUserWorkspace(ctx, identity)
	if err != nil {
		return nil, false, err
	}
	runID, err := NewID("run")
	if err != nil {
		return nil, false, err
	}
	requestHash := implementationRequestHash(options, repo, baseCommit, model, workspace.UserID, workspace.UsernameKey)
	now := manager.now()
	run := ImplementationRun{
		ID: runID, RequestID: feature.ID, ClientRequestID: options.ClientRequestID,
		RequestHash: requestHash, DesignArtifactID: options.DesignArtifactID,
		PlanArtifactID: options.PlanArtifactID, ParentRunID: options.ParentRunID,
		Repo: repo, BaseRef: options.BaseRef, BaseCommit: baseCommit,
		WorkspaceUserID: workspace.UserID, WorkspaceUsername: workspace.UsernameKey,
		Provider: options.Provider, Model: model, NetworkEnabled: options.NetworkEnabled,
		Status: RunQueued, RequestedBy: requestedBy, CreatedAt: now,
	}
	saved, created, err := manager.store.CreateImplementation(ctx, run)
	if err != nil {
		return nil, false, err
	}
	if created {
		_, _ = manager.appendEvent(context.WithoutCancel(ctx), saved.ID, EventRunQueued, "implementation queued", nil)
	}
	return saved, created, nil
}

func (manager *ImplementationManager) Cancel(ctx context.Context, runID string) error {
	status, err := manager.store.RequestCancel(ctx, runID)
	if err != nil {
		return err
	}
	if status == RunCancelled {
		_, _ = manager.appendEvent(context.WithoutCancel(ctx), runID, EventRunCancelled, "implementation cancelled", nil)
		return nil
	}
	manager.cancelMu.Lock()
	cancel := manager.cancels[runID]
	manager.cancelMu.Unlock()
	if cancel != nil {
		cancel(errImplementationCancelled)
	}
	return nil
}

func (manager *ImplementationManager) Subscribe(runID string) (<-chan RunEvent, func()) {
	return manager.hub.Subscribe(runID)
}

func (manager *ImplementationManager) Run(ctx context.Context) {
	if manager == nil || manager.store == nil || manager.git == nil || manager.runner == nil {
		return
	}
	log.InfofCtx(ctx, "[feature-delivery] implementation workers starting deployment=single_instance isolation=local_process concurrency=%d", manager.config.MaxConcurrency)
	manager.recoverExpired(ctx)
	for worker := 0; worker < manager.config.MaxConcurrency; worker++ {
		go manager.worker(ctx, worker)
	}
	go manager.cleaner(ctx)
	go manager.recoverer(ctx)
}

func (manager *ImplementationManager) worker(ctx context.Context, index int) {
	workerID, err := NewID(fmt.Sprintf("worker%d", index))
	if err != nil {
		log.ErrorfCtx(ctx, "[feature-delivery] create worker id index=%d: %v", index, err)
		return
	}
	ticker := time.NewTicker(implementationPoll)
	defer ticker.Stop()
	for {
		run, err := manager.store.ClaimNextImplementation(ctx, workerID, manager.now().Add(implementationLease))
		if err == nil {
			manager.execute(ctx, workerID, *run)
			continue
		}
		if !errors.Is(err, ErrNotFound) && ctx.Err() == nil {
			log.WarnfCtx(ctx, "[feature-delivery] claim implementation worker=%s: %v", workerID, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(implementationPoll):
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (manager *ImplementationManager) execute(parent context.Context, workerID string, run ImplementationRun) {
	executionStarted := manager.now()
	log.InfofCtx(parent, "[feature-delivery] implementation started run=%s feature=%s provider=%s repo=%s", run.ID, run.RequestID, run.Provider, run.Repo)
	timeoutCtx, timeoutCancel := context.WithTimeoutCause(parent, manager.config.Timeout, errImplementationTimedOut)
	runCtx, cancel := context.WithCancelCause(timeoutCtx)
	manager.registerCancel(run.ID, cancel)
	defer func() {
		manager.unregisterCancel(run.ID)
		cancel(nil)
		timeoutCancel()
	}()
	leaseDone := make(chan struct{})
	defer close(leaseDone)
	go manager.renewLease(runCtx, cancel, leaseDone, workerID, run.ID)

	_, _ = manager.appendEvent(runCtx, run.ID, EventRunPreparing, "preparing isolated worktree", nil)
	workspace := UserWorkspace{UserID: run.WorkspaceUserID, UsernameKey: run.WorkspaceUsername}
	prepared, err := manager.git.PrepareWorktree(runCtx, workspace, run.ID, run.Repo, run.BaseRef, run.BaseCommit)
	if err != nil {
		manager.finishFailure(runCtx, run, workerID, RunPreparing, err)
		return
	}
	task, expectedPaths, err := manager.taskPackage(runCtx, run)
	if err != nil {
		manager.finishFailure(runCtx, run, workerID, RunPreparing, err)
		return
	}
	if err := manager.store.TransitionImplementation(runCtx, run.ID, workerID, RunPreparing, RunRunning, RunUpdate{}); err != nil {
		manager.finishFailure(runCtx, run, workerID, RunPreparing, err)
		return
	}
	_, _ = manager.appendEvent(runCtx, run.ID, EventProviderStarted, run.Provider+" started", nil)
	providerEvents := newProviderEventBuffer(manager, run.ID)
	result, err := manager.runner.Run(runCtx, CodingRequest{
		RunID: run.ID, Provider: run.Provider, Model: run.Model,
		WorktreePath: prepared.WorktreePath, BaseCommit: run.BaseCommit,
		TaskPackage: task, NetworkEnabled: run.NetworkEnabled, Timeout: manager.config.Timeout,
	}, providerEvents.Append)
	flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(runCtx), eventFlushTimeout)
	flushErr := providerEvents.Flush(flushCtx)
	flushCancel()
	if err == nil {
		err = flushErr
	} else if flushErr != nil {
		err = errors.Join(err, flushErr)
	}
	if err != nil {
		manager.finishFailureWithResult(runCtx, run, workerID, RunRunning, result, err)
		return
	}
	_, _ = manager.appendEvent(runCtx, run.ID, EventProviderFinished, "coding provider finished", nil)
	exitCode := result.ExitCode
	if err := manager.store.TransitionImplementation(runCtx, run.ID, workerID, RunRunning, RunValidating, RunUpdate{
		ProviderVersion: result.ProviderVersion, ProviderSessionID: result.ProviderSessionID, ExitCode: &exitCode,
	}); err != nil {
		manager.finishFailureWithResult(runCtx, run, workerID, RunRunning, result, err)
		return
	}
	_, _ = manager.appendEvent(runCtx, run.ID, EventValidationStarted, "independent validation started", nil)
	change, err := manager.git.BuildChangeSet(runCtx, *prepared, run.ID, result.Summary)
	if err != nil {
		manager.finishFailureWithResult(runCtx, run, workerID, RunValidating, result, err)
		return
	}
	change.PlanDeviations = reconcilePlanDeviations(change.Files, expectedPaths, result.Deviations)
	validations, err := manager.git.RunValidation(runCtx, *prepared, run.ID)
	change.ValidationResults = validations
	if err != nil {
		if context.Cause(runCtx) != nil {
			manager.finishFailureWithResult(runCtx, run, workerID, RunValidating, result, err)
			return
		}
		summary := truncateText(err.Error(), maxRunErrorBytes)
		_, _ = manager.appendEvent(runCtx, run.ID, EventValidationFinished, summary, nil)
		_, _ = manager.appendEvent(runCtx, run.ID, EventChangeSetReady, "change set ready with failed validation", nil)
		if saveErr := manager.store.SaveChangeSetAndFinish(
			context.Background(), change, RunFailed, summary, manager.now().Add(manager.config.WorktreeTTL),
		); saveErr != nil {
			manager.finishFailureWithResult(runCtx, run, workerID, RunValidating, result, saveErr)
			return
		}
		_, _ = manager.appendEvent(context.Background(), run.ID, EventRunFailed, summary, nil)
		log.WarnfCtx(parent, "[feature-delivery] implementation finished run=%s status=%s duration_ms=%d", run.ID, RunFailed, manager.now().Sub(executionStarted).Milliseconds())
		return
	}
	_, _ = manager.appendEvent(runCtx, run.ID, EventValidationFinished, "independent validation finished", nil)
	_, _ = manager.appendEvent(runCtx, run.ID, EventChangeSetReady, "change set ready", nil)
	if err := manager.store.SaveChangeSetAndFinish(
		runCtx, change, RunSucceeded, "", manager.now().Add(manager.config.WorktreeTTL),
	); err != nil {
		manager.finishFailureWithResult(runCtx, run, workerID, RunValidating, result, err)
		return
	}
	_, _ = manager.appendEvent(context.Background(), run.ID, EventRunSucceeded, "implementation succeeded", nil)
	log.InfofCtx(parent, "[feature-delivery] implementation finished run=%s status=%s duration_ms=%d", run.ID, RunSucceeded, manager.now().Sub(executionStarted).Milliseconds())
}

func (manager *ImplementationManager) renewLease(ctx context.Context, cancel context.CancelCauseFunc, done <-chan struct{}, workerID, runID string) {
	ticker := time.NewTicker(implementationLease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, cancelRequested, err := manager.store.RenewImplementationLease(ctx, runID, workerID, manager.now().Add(implementationLease))
			switch {
			case cancelRequested:
				cancel(errImplementationCancelled)
				return
			case err != nil:
				cancel(fmt.Errorf("%w: %v", errImplementationLeaseLost, err))
				return
			case !ok:
				cancel(errImplementationLeaseLost)
				return
			}
		}
	}
}

func (manager *ImplementationManager) finishFailure(runCtx context.Context, run ImplementationRun, workerID string, from RunStatus, err error) {
	manager.finishFailureWithResult(runCtx, run, workerID, from, CodingResult{}, err)
}

func (manager *ImplementationManager) finishFailureWithResult(runCtx context.Context, run ImplementationRun, workerID string, from RunStatus, result CodingResult, runErr error) {
	status := RunFailed
	event := EventRunFailed
	summary := truncateText(runErr.Error(), maxRunErrorBytes)
	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, errImplementationCancelled):
		status = RunCancelled
		event = EventRunCancelled
		summary = "implementation cancelled"
	case errors.Is(cause, errImplementationTimedOut), errors.Is(runErr, context.DeadlineExceeded):
		summary = "implementation timed out"
	case errors.Is(cause, errImplementationLeaseLost):
		status = RunInterrupted
		event = EventRunInterrupted
		summary = "worker lease lost"
	case cause != nil, errors.Is(runErr, context.Canceled):
		status = RunInterrupted
		event = EventRunInterrupted
		summary = "implementation interrupted"
	}
	update := RunUpdate{
		ProviderVersion: result.ProviderVersion, ProviderSessionID: result.ProviderSessionID,
		ErrorSummary: summary,
	}
	if from != RunPreparing || result.ProviderVersion != "" || result.ProviderSessionID != "" || result.EventCount > 0 {
		exitCode := result.ExitCode
		update.ExitCode = &exitCode
	}
	retain := manager.now().Add(manager.config.WorktreeTTL)
	update.RetainUntil = &retain
	if err := manager.store.TransitionImplementation(context.Background(), run.ID, workerID, from, status, update); err != nil {
		log.ErrorfCtx(context.Background(), "[feature-delivery] persist terminal implementation run=%s status=%s: %v", run.ID, status, err)
		return
	}
	_, _ = manager.appendEvent(context.Background(), run.ID, event, summary, nil)
	log.WarnfCtx(context.Background(), "[feature-delivery] implementation finished run=%s status=%s provider=%s repo=%s", run.ID, status, run.Provider, run.Repo)
}

func (manager *ImplementationManager) appendEvent(ctx context.Context, runID string, kind EventKind, summary string, detail json.RawMessage) (*RunEvent, error) {
	event, err := manager.store.AppendRunEvent(ctx, RunEvent{
		RunID: runID, Kind: kind, Summary: summary, Detail: detail, CreatedAt: manager.now(),
	})
	if err != nil {
		log.WarnfCtx(ctx, "[feature-delivery] append run event run=%s kind=%s: %v", runID, kind, err)
		return nil, err
	}
	manager.hub.Publish(*event)
	return event, nil
}

type providerEventBuffer struct {
	manager  *ImplementationManager
	runID    string
	requests chan providerEventRequest
	done     chan struct{}
	err      error
}

type providerEventRequest struct {
	event *ProviderEvent
	flush chan error
	ctx   context.Context
}

func newProviderEventBuffer(manager *ImplementationManager, runID string) *providerEventBuffer {
	buffer := &providerEventBuffer{
		manager: manager, runID: runID,
		requests: make(chan providerEventRequest, 1), done: make(chan struct{}),
	}
	go buffer.run()
	return buffer
}

func (buffer *providerEventBuffer) Append(ctx context.Context, event ProviderEvent) error {
	if !isProviderEvent(event.Kind) {
		return fmt.Errorf("unsupported provider event kind %q: %w", event.Kind, ErrInvalid)
	}
	if len(event.Detail) > maxProviderDetailBytes || (len(event.Detail) > 0 && !json.Valid(event.Detail)) {
		return fmt.Errorf("provider event detail is invalid or exceeds %d bytes: %w", maxProviderDetailBytes, ErrInvalid)
	}
	request := providerEventRequest{event: &event}
	select {
	case <-buffer.done:
		return buffer.err
	default:
	}
	select {
	case buffer.requests <- request:
		return nil
	case <-buffer.done:
		return buffer.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (buffer *providerEventBuffer) Flush(ctx context.Context) error {
	response := make(chan error, 1)
	request := providerEventRequest{flush: response, ctx: ctx}
	select {
	case <-buffer.done:
		return buffer.err
	default:
	}
	select {
	case buffer.requests <- request:
	case <-buffer.done:
		return buffer.err
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-response:
		return err
	case <-buffer.done:
		return buffer.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (buffer *providerEventBuffer) run() {
	ticker := time.NewTicker(providerEventFlush)
	defer ticker.Stop()
	defer close(buffer.done)
	events := make([]RunEvent, 0, providerEventBatchSize)
	flush := func(ctx context.Context) error {
		if len(events) == 0 {
			return nil
		}
		persisted, err := buffer.manager.store.AppendRunEvents(ctx, events)
		if err != nil {
			return err
		}
		if len(persisted) != len(events) {
			return fmt.Errorf("persisted %d of %d provider events", len(persisted), len(events))
		}
		for _, event := range persisted {
			buffer.manager.hub.Publish(event)
		}
		events = events[:0]
		return nil
	}
	for {
		select {
		case request := <-buffer.requests:
			if request.flush != nil {
				buffer.err = flush(request.ctx)
				request.flush <- buffer.err
				return
			}
			event := request.event
			events = append(events, RunEvent{
				RunID: buffer.runID, Kind: event.Kind, Summary: truncateText(event.Summary, 4000),
				Detail: append(json.RawMessage(nil), event.Detail...), CreatedAt: buffer.manager.now(),
			})
			if len(events) == providerEventBatchSize {
				flushCtx, cancel := context.WithTimeout(context.Background(), eventFlushTimeout)
				buffer.err = flush(flushCtx)
				cancel()
				if buffer.err != nil {
					return
				}
			}
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(context.Background(), eventFlushTimeout)
			buffer.err = flush(flushCtx)
			cancel()
			if buffer.err != nil {
				return
			}
		}
	}
}

func isProviderEvent(kind EventKind) bool {
	switch kind {
	case EventProviderMessage, EventCommandStarted, EventCommandFinished, EventFileChanged:
		return true
	default:
		return false
	}
}

func (manager *ImplementationManager) taskPackage(ctx context.Context, run ImplementationRun) (string, []string, error) {
	plan, err := manager.store.GetArtifact(ctx, run.PlanArtifactID)
	if err != nil {
		return "", nil, err
	}
	design, err := manager.store.GetArtifact(ctx, run.DesignArtifactID)
	if err != nil {
		return "", nil, err
	}
	var planDocument ImplementationPlanDocument
	if err := json.Unmarshal(plan.DocumentJSON, &planDocument); err != nil {
		return "", nil, fmt.Errorf("decode implementation plan: %w", err)
	}
	var expectedPaths []string
	var repositoryPlan RepositoryPlan
	foundRepository := false
	for index := range planDocument.Repositories {
		if planDocument.Repositories[index].Repository == run.Repo {
			foundRepository = true
			repositoryPlan = planDocument.Repositories[index]
			expectedPaths = append([]string(nil), repositoryPlan.ExpectedPaths...)
			break
		}
	}
	if !foundRepository {
		return "", nil, fmt.Errorf("implementation plan does not contain repository %q: %w", run.Repo, ErrConflict)
	}
	chain := []*Artifact{plan, design}
	parentID := design.ParentArtifactID
	for parentID != "" && len(chain) < 5 {
		parent, err := manager.store.GetArtifact(ctx, parentID)
		if err != nil {
			return "", nil, err
		}
		chain = append(chain, parent)
		parentID = parent.ParentArtifactID
	}
	task, err := buildCodingTaskPrompt(run, repositoryPlan, chain)
	if err != nil {
		return "", nil, err
	}
	return task, expectedPaths, nil
}

func reconcilePlanDeviations(files []ChangedFile, expectedPaths []string, reported []PlanDeviation) []PlanDeviation {
	expected := make(map[string]struct{}, len(expectedPaths))
	for _, path := range expectedPaths {
		expected[path] = struct{}{}
	}
	reasons := make(map[string]string, len(reported))
	for _, deviation := range reported {
		reasons[deviation.Path] = deviation.Reason
	}
	deviations := make([]PlanDeviation, 0)
	for _, file := range files {
		if planCoversPath(expected, file.Path) {
			continue
		}
		reason, explained := reasons[file.Path]
		if !explained {
			reason = "coding provider did not explain this unplanned change"
		}
		deviations = append(deviations, PlanDeviation{Path: file.Path, Reason: reason, Explained: explained})
	}
	return deviations
}

func planCoversPath(expected map[string]struct{}, file string) bool {
	for candidate := file; candidate != ""; {
		if _, ok := expected[candidate]; ok {
			return true
		}
		separator := strings.LastIndexByte(candidate, '/')
		if separator < 0 {
			return false
		}
		candidate = candidate[:separator]
	}
	return false
}

func (manager *ImplementationManager) cleaner(ctx context.Context) {
	ticker := time.NewTicker(implementationCleanup)
	defer ticker.Stop()
	for {
		manager.cleanupPage(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (manager *ImplementationManager) recoverer(ctx context.Context) {
	ticker := time.NewTicker(implementationRecovery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.recoverExpired(ctx)
		}
	}
}

func (manager *ImplementationManager) recoverExpired(ctx context.Context) {
	for ctx.Err() == nil {
		now := manager.now()
		interrupted, err := manager.store.InterruptActiveImplementations(ctx, now, now.Add(manager.config.WorktreeTTL))
		if err != nil {
			log.WarnfCtx(ctx, "[feature-delivery] recover expired implementations: %v", err)
			return
		}
		if len(interrupted) == 0 {
			return
		}
		log.WarnfCtx(ctx, "[feature-delivery] recovered expired implementations count=%d", len(interrupted))
		for _, runID := range interrupted {
			_, _ = manager.appendEvent(ctx, runID, EventRunInterrupted, "worker lease expired", nil)
		}
	}
}

func (manager *ImplementationManager) cleanupPage(ctx context.Context) {
	runs, err := manager.store.ListExpiredWorktrees(ctx, manager.now(), 20)
	if err != nil {
		log.WarnfCtx(ctx, "[feature-delivery] list expired worktrees: %v", err)
		return
	}
	for _, run := range runs {
		err := manager.git.RemoveWorktree(ctx, run)
		summary := ""
		if err != nil {
			summary = truncateText(err.Error(), maxRunErrorBytes)
			log.WarnfCtx(ctx, "[feature-delivery] remove worktree run=%s repo=%s: %v", run.ID, run.Repo, err)
		}
		if markErr := manager.store.MarkWorktreeCleaned(ctx, run.ID, summary); markErr != nil {
			log.WarnfCtx(ctx, "[feature-delivery] mark worktree cleanup run=%s: %v", run.ID, markErr)
		}
	}
}

func (manager *ImplementationManager) PatchPath(relative string) (string, error) {
	if manager == nil || manager.git == nil {
		return "", ErrUnavailable
	}
	return manager.git.PatchPath(relative)
}

func (manager *ImplementationManager) ValidationOutputPath(relative string) (string, error) {
	if manager == nil || manager.git == nil {
		return "", ErrUnavailable
	}
	return manager.git.ArtifactPath(relative)
}

func (manager *ImplementationManager) registerCancel(runID string, cancel context.CancelCauseFunc) {
	manager.cancelMu.Lock()
	manager.cancels[runID] = cancel
	manager.cancelMu.Unlock()
}

func (manager *ImplementationManager) Status(ctx context.Context) CodingCapabilityStatus {
	status := CodingCapabilityStatus{Providers: map[string]CodingProviderStatus{}}
	if manager == nil {
		return status
	}
	status.GitFound = manager.git != nil
	status.Isolation = "local_process"
	if manager.runner != nil {
		status.Providers = manager.runner.ProviderStatus(ctx)
	}
	if manager.git != nil {
		for provider := range manager.enabled {
			if providerReady(status.Providers[provider]) {
				status.Enabled = true
				break
			}
		}
	}
	return status
}

func providerReady(status CodingProviderStatus) bool {
	return status.Enabled && status.BinaryFound && status.ContractCompatible &&
		status.CredentialIsolated && status.Reason == ""
}

func (manager *ImplementationManager) unregisterCancel(runID string) {
	manager.cancelMu.Lock()
	delete(manager.cancels, runID)
	manager.cancelMu.Unlock()
}

func planContainsRepository(artifact Artifact, repo string) bool {
	var document ImplementationPlanDocument
	if err := json.Unmarshal(artifact.DocumentJSON, &document); err != nil {
		return false
	}
	for _, repository := range document.Repositories {
		if repository.Repository == repo {
			return true
		}
	}
	return false
}

func validateReimplementationParent(parent ImplementationRun, requestID, repo string) error {
	if parent.RequestID != requestID || parent.Repo != repo || parent.Status != RunSucceeded ||
		parent.Review == nil || parent.Review.Decision != DecisionRejected {
		return fmt.Errorf("parent_run_id must reference a rejected successful run for the same feature and repository: %w", ErrConflict)
	}
	return nil
}

func implementationRequestHash(options ImplementationOptions, repo, commit, model string, userID int64, username string) string {
	payload, _ := json.Marshal(struct {
		Options  ImplementationOptions `json:"options"`
		Repo     string                `json:"repo"`
		Commit   string                `json:"commit"`
		Model    string                `json:"model"`
		UserID   int64                 `json:"user_id"`
		Username string                `json:"username"`
	}{options, repo, commit, model, userID, username})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

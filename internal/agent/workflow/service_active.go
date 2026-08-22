package workflow

import (
	"context"
	"fmt"
	"sync"
)

func (service *Service) registerActive(
	ctx context.Context,
	runID string,
	detached bool,
) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if detached {
		ctx = context.WithoutCancel(ctx)
	}
	runCtx, cancel := context.WithCancel(ctx)
	active := &activeRun{cancel: cancel, done: make(chan struct{})}
	service.activeMu.Lock()
	if service.closed {
		service.activeMu.Unlock()
		cancel()
		return nil, nil, ErrUnavailable
	}
	if _, exists := service.active[runID]; exists {
		service.activeMu.Unlock()
		cancel()
		return nil, nil, fmt.Errorf(
			"workflow run %q is already active: %w",
			runID,
			ErrConflict,
		)
	}
	service.active[runID] = active
	service.activeWG.Add(1)
	service.activeMu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			service.activeMu.Lock()
			if service.active[runID] == active {
				delete(service.active, runID)
			}
			close(active.done)
			service.activeMu.Unlock()
			service.activeWG.Done()
		})
	}
	return runCtx, release, nil
}

func (service *Service) cancelActive(runID string) {
	service.activeMu.Lock()
	active := service.active[runID]
	service.activeMu.Unlock()
	if active != nil {
		active.cancel()
	}
}

// AwaitTerminal joins local execution when present, then reads durable terminal facts.
func (service *Service) AwaitTerminal(
	ctx context.Context,
	runID string,
) (TerminalResult, error) {
	if service == nil || service.store == nil {
		return TerminalResult{}, ErrUnavailable
	}
	if err := validateRunID(runID); err != nil {
		return TerminalResult{}, err
	}
	service.activeMu.Lock()
	active := service.active[runID]
	service.activeMu.Unlock()
	if active != nil {
		select {
		case <-ctx.Done():
			return TerminalResult{}, ctx.Err()
		case <-active.done:
		}
	}
	return service.store.LoadTerminalResult(ctx, runID)
}

// LoadTerminalResult reads durable terminal facts without joining local execution.
func (service *Service) LoadTerminalResult(
	ctx context.Context,
	runID string,
) (TerminalResult, error) {
	if service == nil || service.store == nil {
		return TerminalResult{}, ErrUnavailable
	}
	if err := validateRunID(runID); err != nil {
		return TerminalResult{}, err
	}
	return service.store.LoadTerminalResult(ctx, runID)
}

// LoadFullRunState reads the durable checkpoint needed to recover intermediate
// evidence when the canonical workflow output is absent or incomplete.
func (service *Service) LoadFullRunState(
	ctx context.Context,
	runID string,
) (*RunState, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	return service.store.LoadFullRunState(ctx, runID)
}

// Close prevents new Runs, cancels active execution, and waits for persistence cleanup.
func (service *Service) Close() {
	if service == nil {
		return
	}
	service.activeMu.Lock()
	if service.closed {
		service.activeMu.Unlock()
		service.activeWG.Wait()
		return
	}
	service.closed = true
	cancels := make([]context.CancelFunc, 0, len(service.active))
	for _, active := range service.active {
		cancels = append(cancels, active.cancel)
	}
	service.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	service.activeWG.Wait()
}

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
	service.active[runID] = cancel
	service.activeWG.Add(1)
	service.activeMu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			service.activeMu.Lock()
			delete(service.active, runID)
			service.activeMu.Unlock()
			service.activeWG.Done()
		})
	}
	return runCtx, release, nil
}

func (service *Service) cancelActive(runID string) {
	service.activeMu.Lock()
	cancel := service.active[runID]
	service.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
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
	for _, cancel := range service.active {
		cancels = append(cancels, cancel)
	}
	service.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	service.activeWG.Wait()
}

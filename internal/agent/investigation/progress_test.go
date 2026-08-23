package investigation

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCoordinatorEmitsProgressEvents(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []ProgressEvent
	observer := func(event ProgressEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:  catalog,
		Schemas:  testSchemas(),
		Store:    NewMemoryRunStore(),
		Observer: observer,
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
		})),
		BudgetLimit: BudgetVector{},
		MaxRounds:   1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	if _, err := coordinator.Execute(context.Background(), contract); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	seen := make(map[ProgressKind]bool)
	for _, event := range events {
		seen[event.Kind] = true
	}
	if !seen[ProgressWorkflowStarted] || !seen[ProgressTaskStarted] ||
		!seen[ProgressTaskCompleted] || !seen[ProgressWorkflowCompleted] {
		t.Fatalf("progress events = %#v", events)
	}
}

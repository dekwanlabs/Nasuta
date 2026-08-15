package qa

import (
	"context"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

type candidateDiscoveryHistoryStub struct {
	started           chan struct{}
	release           chan struct{}
	recallCalls       int
	materializeCalls  int
	recallBudget      int
	materializeBudget int
}

func (stub *candidateDiscoveryHistoryStub) PrepareRecords([]memory.TurnContextRecord) {}

func (stub *candidateDiscoveryHistoryStub) Recall(_ context.Context, _ int64, _ string, _ string, _ string, budget int) (string, error) {
	stub.recallCalls++
	stub.recallBudget = budget
	return "recall", nil
}

func (stub *candidateDiscoveryHistoryStub) Find(context.Context, int64, string, string, int, int) (string, error) {
	return "", nil
}

func (stub *candidateDiscoveryHistoryStub) Discover(context.Context, int64, string, string) (HistoryCandidates, error) {
	close(stub.started)
	<-stub.release
	return HistoryCandidates{Mode: "dense_lexical", Refs: []string{"turn-1"}}, nil
}

func (stub *candidateDiscoveryHistoryStub) Materialize(_ context.Context, _ int64, _ string, _ HistoryCandidates, _ int, budget int, _ bool) (string, error) {
	stub.materializeCalls++
	stub.materializeBudget = budget
	return "materialized", nil
}

func TestHistoryCandidateDiscoveryStartsAsynchronously(t *testing.T) {
	stub := &candidateDiscoveryHistoryStub{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	task := startHistoryDiscovery(
		context.Background(), stub, 42,
		ConversationContext{SessionID: "session-1", CompactedThroughTurn: 3},
		"继续看刚才的问题",
	)
	if task == nil {
		t.Fatal("candidate discovery channel is nil")
	}
	defer task.cancel()
	select {
	case <-stub.started:
	case <-time.After(time.Second):
		t.Fatal("candidate discovery did not start")
	}
	select {
	case <-task.result:
		t.Fatal("candidate discovery completed before release")
	case <-time.After(20 * time.Millisecond):
	}
	close(stub.release)
	select {
	case result := <-task.result:
		if result.err != nil || len(result.candidates.Refs) != 1 {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("candidate discovery did not complete")
	}
}

func TestReassemblePreparedConversationUsesDefinitionHistoryBudget(t *testing.T) {
	stub := &candidateDiscoveryHistoryStub{}
	svc := &Service{
		history:       stub,
		contextWindow: 128000,
		outputReserve: 16000,
	}
	source := ConversationContext{
		SessionID: "session-1", CompactedThroughTurn: 3,
	}
	prepared := &preparation{
		request: Request{
			Question: "继续看刚才的证据", UserID: 42,
			Conversation: ConversationContext{RetrievedHistory: "assembled-with-platform-default"},
		},
		sourceConversation: source,
		analysis: queryAnalysisOutput{
			History: retrieval.HistoryRelation{NeedsPriorEvidence: true},
		},
	}

	if err := svc.reassembleConversation(
		t.Context(), prepared, 8192, 2048,
	); err != nil {
		t.Fatal(err)
	}
	if stub.recallCalls != 1 || stub.recallBudget != 655 {
		t.Fatalf("recall calls=%d budget=%d, want one call with definition budget 655",
			stub.recallCalls, stub.recallBudget)
	}
	if prepared.request.Conversation.RetrievedHistory != "recall" {
		t.Fatalf("conversation was not rebuilt from source snapshot: %+v",
			prepared.request.Conversation)
	}
}

func TestAssembleContextMaterializesEarlyCandidatesUnlessHistoryDependencyRequiresRecall(t *testing.T) {
	stub := &candidateDiscoveryHistoryStub{}
	svc := &Service{
		history:       stub,
		contextWindow: 4096,
	}
	conversation := ConversationContext{
		SessionID: "session-1", CompactedThroughTurn: 3,
	}
	candidates := &HistoryCandidates{Mode: "dense_lexical", Refs: []string{"turn-1"}}

	output, err := svc.assembleContext(t.Context(), contextAssembleInput{
		Question: "查一下这个服务", UserID: 42, Conversation: conversation,
		Candidates: candidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Conversation.RetrievedHistory != "materialized" || stub.materializeCalls != 1 || stub.recallCalls != 0 {
		t.Fatalf("output=%+v materialize=%d recall=%d", output.Conversation, stub.materializeCalls, stub.recallCalls)
	}

	output, err = svc.assembleContext(t.Context(), contextAssembleInput{
		Question: "继续看刚才的证据", UserID: 42, Conversation: conversation,
		Relation:   retrieval.HistoryRelation{NeedsPriorEvidence: true},
		Candidates: candidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Conversation.RetrievedHistory != "recall" || stub.recallCalls != 1 {
		t.Fatalf("output=%+v materialize=%d recall=%d", output.Conversation, stub.materializeCalls, stub.recallCalls)
	}
}

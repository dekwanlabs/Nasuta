package qa

import (
	"context"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

type candidateDiscoveryHistoryStub struct {
	started          chan struct{}
	release          chan struct{}
	recallCalls      int
	materializeCalls int
}

func (stub *candidateDiscoveryHistoryStub) PrepareRecords([]memory.TurnContextRecord) {}

func (stub *candidateDiscoveryHistoryStub) Recall(context.Context, int64, string, string, string, int) (string, error) {
	stub.recallCalls++
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

func (stub *candidateDiscoveryHistoryStub) Materialize(context.Context, int64, string, HistoryCandidates, int, int, bool) (string, error) {
	stub.materializeCalls++
	return "materialized", nil
}

func TestHistoryCandidateDiscoveryStartsAsynchronously(t *testing.T) {
	stub := &candidateDiscoveryHistoryStub{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	task := startHistoryCandidateDiscovery(
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

func TestAssembleContextMaterializesEarlyCandidatesUnlessHistoryDependencyRequiresRecall(t *testing.T) {
	stub := &candidateDiscoveryHistoryStub{}
	svc := &QA{
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

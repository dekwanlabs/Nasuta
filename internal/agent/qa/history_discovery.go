package qa

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
)

type historyDiscoveryResult struct {
	candidates HistoryCandidates
	err        error
}

type historyDiscoveryTask struct {
	result <-chan historyDiscoveryResult
	cancel context.CancelFunc
}

func startHistoryCandidateDiscovery(
	ctx context.Context,
	history SessionHistory,
	userID int64,
	conversation ConversationContext,
	question string,
) *historyDiscoveryTask {
	if history == nil || conversation.CompactedThroughTurn <= 0 {
		return nil
	}
	discovery, ok := history.(CandidateDiscovery)
	if !ok {
		return nil
	}
	discoveryCtx, cancel := context.WithCancel(ctx)
	resultCh := make(chan historyDiscoveryResult, 1)
	go func() {
		candidates, err := discovery.Discover(discoveryCtx, userID, conversation.SessionID, question)
		resultCh <- historyDiscoveryResult{candidates: candidates, err: err}
	}()
	return &historyDiscoveryTask{result: resultCh, cancel: cancel}
}

func resolveHistoryCandidates(
	ctx context.Context,
	task *historyDiscoveryTask,
	relation retrieval.HistoryRelation,
) *HistoryCandidates {
	if task == nil {
		return nil
	}
	if historyNeedsContinuity(relation) {
		task.cancel()
		return nil
	}

	discovered := <-task.result
	task.cancel()
	if discovered.err != nil {
		log.WarnfCtx(ctx, "[qa] session history candidate discovery degraded: %v", discovered.err)
		return nil
	}
	return &discovered.candidates
}

func historyNeedsContinuity(relation retrieval.HistoryRelation) bool {
	return relation.NeedsPriorEntities || relation.NeedsPriorConclusion || relation.NeedsPriorEvidence
}

package qa

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func TestSessionCompactionIncomingTokensIncludesRetrievedContext(t *testing.T) {
	plan := domain.EvidencePlan{Sources: domain.Internal}
	withHistory := ConversationContext{
		Recent:       []llm.Message{{Role: "user", Content: strings.Repeat("old history ", 1000)}},
		Instructions: []llm.Message{{Role: "system", Content: "request instruction"}},
	}
	withoutHistory := withHistory
	withoutHistory.Recent = nil
	retrieved := &retrieval.RetrievedContext{Text: strings.Repeat("retrieved evidence ", 1000), HitCount: 3}

	got := sessionCompactionIncomingTokens("current question", withHistory, retrieved, plan, "")
	want := sessionCompactionIncomingTokens("current question", withoutHistory, retrieved, plan, "")
	withoutRetrieval := sessionCompactionIncomingTokens("current question", withoutHistory, nil, plan, "")

	if got != want {
		t.Fatalf("session history was counted as incoming context: got=%d want=%d", got, want)
	}
	if got <= withoutRetrieval {
		t.Fatalf("retrieved context was not counted: with=%d without=%d", got, withoutRetrieval)
	}
}

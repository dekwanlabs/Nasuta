package definition

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestDefinitionEvidenceSeeded(t *testing.T) {
	tests := []struct {
		name   string
		policy agentapi.RunPolicy
		blocks []agentapi.ContextBlock
		want   bool
	}{
		{
			name:   "explicit policy",
			policy: agentapi.RunPolicy{EvidenceSeeded: true},
			want:   true,
		},
		{
			name: "textual context",
			blocks: []agentapi.ContextBlock{{
				Source: "feature_artifact",
			}},
			want: true,
		},
		{
			name: "empty workflow handoff",
			blocks: []agentapi.ContextBlock{{
				Source: "workflow.handoff",
			}},
		},
		{
			name: "workflow handoff evidence",
			blocks: []agentapi.ContextBlock{{
				Source: "workflow.handoff",
				Evidence: []tool.EvidenceUnit{{
					SourceKind: "runbook",
					Target:     "service-a",
				}},
			}},
			want: true,
		},
		{
			name: "mixed context",
			blocks: []agentapi.ContextBlock{
				{Source: "workflow.handoff"},
				{Source: "feature_artifact"},
			},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := definitionEvidenceSeeded(test.policy, test.blocks); got != test.want {
				t.Fatalf("definitionEvidenceSeeded() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestContextEvidenceConflictsPreservesIdentityAndVersions(t *testing.T) {
	conflicts := contextEvidenceConflicts([]agentapi.ContextBlock{{
		EvidenceConflicts: []agentapi.EvidenceConflict{{
			Identity: agentapi.EvidenceIdentity{
				SourceKind: "runtime", Target: "trace-1", Section: "events",
			},
			Current: tool.EvidenceUnit{
				SourceKind: "runtime", Target: "trace-1", Sections: []string{"events"},
				ContentHash: "version-a",
			},
			Incoming: tool.EvidenceUnit{
				SourceKind: "runtime", Target: "trace-1", Sections: []string{"events"},
				ContentHash: "version-b",
			},
			CurrentOrigin: "retrieval", IncomingOrigin: "preload",
		}},
	}})
	if len(conflicts) != 1 ||
		conflicts[0].Key != (evidence.Key{
			SourceKind: "runtime", Target: "trace-1", Section: "events",
		}) ||
		conflicts[0].Current.ContentHash != "version-a" ||
		conflicts[0].Incoming.ContentHash != "version-b" ||
		conflicts[0].CurrentOrigin != "retrieval" ||
		conflicts[0].IncomingOrigin != "preload" {
		t.Fatalf("conflicts = %#v", conflicts)
	}
}

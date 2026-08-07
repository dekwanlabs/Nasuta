package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	"github.com/dekwanlabs/nasuta/internal/featurepipeline"
	"github.com/dekwanlabs/nasuta/internal/featurereviewworkflow"
)

func TestWorkflowTransformDispatcherRoutesKnownFamilies(t *testing.T) {
	dispatcher := newWorkflowTransformDispatcher(
		featurepipeline.NewExecutor(nil),
		featurereviewworkflow.NewExecutor(nil),
	)
	for _, transformID := range []string{
		featurepipeline.TransformRequirementAnalysis,
		featurereviewworkflow.TransformAssignment,
	} {
		_, err := dispatcher.Execute(context.Background(), agentworkflow.NodeRequest{
			Node: agentworkflow.NodeDefinition{TransformID: transformID},
		})
		if !errors.Is(err, featuredelivery.ErrUnavailable) {
			t.Fatalf("transform %q error = %v, want unavailable", transformID, err)
		}
	}
}

func TestWorkflowTransformDispatcherRejectsUnavailableAndUnknownTransforms(t *testing.T) {
	dispatcher := newWorkflowTransformDispatcher(
		featurepipeline.NewExecutor(nil),
		nil,
	)
	for _, test := range []struct {
		name        string
		transformID string
		want        string
	}{
		{
			name:        "review unavailable",
			transformID: featurereviewworkflow.TransformAssignment,
			want:        "feature review transform",
		},
		{
			name:        "unknown",
			transformID: "feature.unknown",
			want:        "is unsupported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := dispatcher.Execute(context.Background(), agentworkflow.NodeRequest{
				Node: agentworkflow.NodeDefinition{TransformID: test.transformID},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestWorkflowTransformDispatcherDisablesWithoutExecutors(t *testing.T) {
	if dispatcher := newWorkflowTransformDispatcher(nil, nil); dispatcher != nil {
		t.Fatalf("dispatcher = %T, want nil", dispatcher)
	}
}

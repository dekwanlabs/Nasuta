package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/feature/pipeline"
	"github.com/dekwanlabs/nasuta/internal/feature/reviewworkflow"
)

func TestWorkflowTransformDispatcherRoutesKnownFamilies(t *testing.T) {
	dispatcher := newWorkflowTransformDispatcher(
		pipeline.NewExecutor(nil),
		reviewworkflow.NewExecutor(nil),
	)
	for _, transformID := range []string{
		pipeline.TransformRequirementAnalysis,
		reviewworkflow.TransformAssignment,
	} {
		_, err := dispatcher.Execute(context.Background(), workflow.NodeRequest{
			Node: workflow.NodeDefinition{TransformID: transformID},
		})
		if !errors.Is(err, delivery.ErrUnavailable) {
			t.Fatalf("transform %q error = %v, want unavailable", transformID, err)
		}
	}
}

func TestWorkflowTransformDispatcherRejectsUnavailableAndUnknownTransforms(t *testing.T) {
	dispatcher := newWorkflowTransformDispatcher(
		pipeline.NewExecutor(nil),
		nil,
	)
	for _, test := range []struct {
		name        string
		transformID string
		want        string
	}{
		{
			name:        "review unavailable",
			transformID: reviewworkflow.TransformAssignment,
			want:        "feature review transform",
		},
		{
			name:        "unknown",
			transformID: "feature.unknown",
			want:        "is unsupported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := dispatcher.Execute(context.Background(), workflow.NodeRequest{
				Node: workflow.NodeDefinition{TransformID: test.transformID},
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

// Package featurereviewworkflow adapts Feature Delivery review rounds to Workflow.
package featurereviewworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

const (
	RequestSchemaID    = "feature.review.request"
	ReportSchemaID     = "feature.review.report"
	ReportListSchemaID = "feature.review.report_list"
	GateSchemaID       = "feature.review.gate"

	TransformAssignment   = "feature.review.assignment"
	TransformAdjudication = "feature.review.adjudicate"
	TransformGate         = "feature.review.gate"

	NodeReportsJoin = "reports.join"
	NodeAdjudicate  = "adjudicate"
	NodeGate        = "gate"

	workflowIDPrefix   = "feature.review."
	runIDPrefix        = "review."
	reviewerNodePrefix = "reviewer."
)

var canonicalID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

var (
	requestSchema    = schemaRef(RequestSchemaID)
	reportSchema     = schemaRef(ReportSchemaID)
	reportListSchema = schemaRef(ReportListSchemaID)
	gateSchema       = schemaRef(GateSchemaID)
)

// Request fixes the Review Round consumed by one Workflow Run.
type Request struct {
	RoundID string `json:"round_id"`
}

func schemaRef(id string) agentapi.SchemaRef {
	return agentapi.SchemaRef{ID: id, Version: 1}
}

func workflowID(policy featuredelivery.ReviewPolicy, panelHash string) (string, error) {
	prepared, err := featuredelivery.PrepareReviewPolicy(policy)
	if err != nil {
		return "", err
	}
	if panelHash != "" {
		if len(panelHash) != 64 {
			return "", fmt.Errorf("review panel hash is invalid: %w", featuredelivery.ErrInvalid)
		}
		return workflowIDPrefix + prepared.ContentHash[:12] + "." + panelHash[:12], nil
	}
	return workflowIDPrefix + prepared.ContentHash[:24], nil
}

// WorkflowRef returns the immutable Workflow identity derived from one Policy.
func WorkflowRef(policy featuredelivery.ReviewPolicy) (string, int64, error) {
	id, err := workflowID(policy, "")
	if err != nil {
		return "", 0, err
	}
	return id, policy.Version, nil
}

// RunID fixes a Review Round to one recoverable Workflow Run.
func RunID(roundID string) (string, error) {
	roundID = strings.TrimSpace(roundID)
	runID := runIDPrefix + roundID
	if roundID == "" || !canonicalID.MatchString(runID) {
		return "", fmt.Errorf("review round id %q cannot form a workflow run id: %w", roundID, featuredelivery.ErrInvalid)
	}
	return runID, nil
}

func roundIDFromRunID(runID string) (string, error) {
	runID = strings.TrimSpace(runID)
	if !strings.HasPrefix(runID, runIDPrefix) {
		return "", fmt.Errorf("workflow run %q is not a feature review: %w", runID, featuredelivery.ErrConflict)
	}
	roundID := strings.TrimPrefix(runID, runIDPrefix)
	if roundID == "" || !canonicalID.MatchString(runID) {
		return "", fmt.Errorf("workflow run %q has an invalid review round binding: %w", runID, featuredelivery.ErrConflict)
	}
	return roundID, nil
}

func reviewerNodeID(reviewerID string) string {
	sum := sha256.Sum256([]byte(reviewerID))
	return reviewerNodePrefix + hex.EncodeToString(sum[:12])
}

func reviewerIDForNode(
	reviewers []featuredelivery.ReviewerSpec,
	nodeID string,
) (string, error) {
	for _, reviewer := range reviewers {
		if reviewerNodeID(reviewer.ID) == nodeID {
			return reviewer.ID, nil
		}
	}
	return "", fmt.Errorf(
		"reviewer node %q is not present in the round panel: %w",
		nodeID,
		featuredelivery.ErrConflict,
	)
}

func agentRunID(workflowRunID, nodeID string, attempt int) (string, error) {
	if attempt <= 0 {
		return "", featuredelivery.ErrInvalid
	}
	sum := sha256.Sum256([]byte(
		workflowRunID + "\x00" + nodeID + "\x00" + strconv.Itoa(attempt),
	))
	return "reviewagent." + hex.EncodeToString(sum[:12]), nil
}

func decodeRequest(payload json.RawMessage) (Request, error) {
	var request Request
	if err := json.Unmarshal(payload, &request); err != nil {
		return Request{}, fmt.Errorf("decode feature review request: %w", err)
	}
	request.RoundID = strings.TrimSpace(request.RoundID)
	if request.RoundID == "" {
		return Request{}, fmt.Errorf("review round id is required: %w", featuredelivery.ErrInvalid)
	}
	return request, nil
}

// Package featurepipeline adapts Feature Delivery to the generic Workflow runtime.
package featurepipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

const (
	WorkflowID      = "feature.delivery.pipeline"
	WorkflowVersion = int64(1)

	RequestSchemaID = "feature.pipeline.request"
	StateSchemaID   = "feature.pipeline.state"
	ResultSchemaID  = "feature.pipeline.result"

	TransformRequirementAnalysis = "feature.pipeline.generate.requirement_analysis"
	TransformTechnicalProposal   = "feature.pipeline.generate.technical_proposal"
	TransformSystemDesign        = "feature.pipeline.generate.system_design"
	TransformImplementationPlan  = "feature.pipeline.generate.implementation_plan"
	TransformCoding              = "feature.pipeline.coding"
	TransformValidation          = "feature.pipeline.validation"

	NodeGenerateRequirementAnalysis = "generate.requirement_analysis"
	NodeApproveRequirementAnalysis  = "approve.requirement_analysis"
	NodeGenerateTechnicalProposal   = "generate.technical_proposal"
	NodeApproveTechnicalProposal    = "approve.technical_proposal"
	NodeGenerateSystemDesign        = "generate.system_design"
	NodeApproveSystemDesign         = "approve.system_design"
	NodeGenerateImplementationPlan  = "generate.implementation_plan"
	NodeApproveImplementationPlan   = "approve.implementation_plan"
	NodeCoding                      = "coding"
	NodeValidation                  = "validation"
)

var (
	requestSchema = schemaRef(RequestSchemaID)
	stateSchema   = schemaRef(StateSchemaID)
	resultSchema  = schemaRef(ResultSchemaID)
)

// Request is the canonical input fixed into a pipeline Workflow Run.
type Request struct {
	FeatureID       string `json:"feature_id"`
	ClientRequestID string `json:"client_request_id"`
	Repository      string `json:"repository"`
	BaseRef         string `json:"base_ref"`
	Provider        string `json:"provider"`
	Model           string `json:"model,omitempty"`
	NetworkEnabled  bool   `json:"network_enabled"`
}

// ArtifactSummary carries lineage without loading the document body into a handoff.
type ArtifactSummary struct {
	ID               string                       `json:"id"`
	ParentArtifactID string                       `json:"parent_artifact_id,omitempty"`
	Kind             featuredelivery.ArtifactKind `json:"kind"`
	Version          int                          `json:"version"`
	ContentHash      string                       `json:"content_hash"`
}

type ImplementationSummary struct {
	ID              string                    `json:"id"`
	ClientRequestID string                    `json:"client_request_id"`
	Status          featuredelivery.RunStatus `json:"status"`
}

// State is the durable payload passed between artifact stages and approval gates.
type State struct {
	FeatureID       string                 `json:"feature_id"`
	Options         RequestOptions         `json:"options"`
	Artifacts       []ArtifactSummary      `json:"artifacts"`
	CurrentArtifact *ArtifactSummary       `json:"current_artifact"`
	Implementation  *ImplementationSummary `json:"implementation,omitempty"`
}

type RequestOptions struct {
	ClientRequestID  string `json:"client_request_id"`
	Repository       string `json:"repository"`
	BaseRef          string `json:"base_ref"`
	Provider         string `json:"provider"`
	Model            string `json:"model,omitempty"`
	NetworkEnabled   bool   `json:"network_enabled"`
	DesignArtifactID string `json:"design_artifact_id,omitempty"`
	PlanArtifactID   string `json:"plan_artifact_id,omitempty"`
}

type ValidationSummary struct {
	Sequence      int    `json:"sequence"`
	Status        string `json:"status"`
	ExitCode      int    `json:"exit_code"`
	DurationMS    int64  `json:"duration_ms"`
	OutputSummary string `json:"output_summary"`
	OutputRelPath string `json:"output_rel_path,omitempty"`
	OutputSHA256  string `json:"output_sha256,omitempty"`
	OutputBytes   int64  `json:"output_bytes"`
	TimedOut      bool   `json:"timed_out"`
}

type ChangedFileSummary struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
}

type ChangeSetSummary struct {
	RunID             string                          `json:"run_id"`
	WorktreeHead      string                          `json:"worktree_head"`
	PatchRelPath      string                          `json:"patch_rel_path"`
	PatchSHA256       string                          `json:"patch_sha256"`
	PatchBytes        int64                           `json:"patch_bytes"`
	FilesChanged      int                             `json:"files_changed"`
	Additions         int                             `json:"additions"`
	Deletions         int                             `json:"deletions"`
	Files             []ChangedFileSummary            `json:"files"`
	PlanDeviations    []featuredelivery.PlanDeviation `json:"plan_deviations"`
	ValidationResults []ValidationSummary             `json:"validation_results"`
	ProviderSummary   string                          `json:"provider_summary"`
	CreatedAt         time.Time                       `json:"created_at"`
}

// Result is the bounded terminal output of the pipeline.
type Result struct {
	FeatureID         string                `json:"feature_id"`
	Artifacts         []ArtifactSummary     `json:"artifacts"`
	FinalArtifact     ArtifactSummary       `json:"final_artifact"`
	Implementation    ImplementationSummary `json:"implementation"`
	ChangeSet         ChangeSetSummary      `json:"change_set"`
	ValidationResults []ValidationSummary   `json:"validation_results"`
}

func schemaRef(id string) agentapi.SchemaRef {
	return agentapi.SchemaRef{ID: id, Version: 1}
}

func normalizeRequest(request Request) (Request, error) {
	request.FeatureID = strings.TrimSpace(request.FeatureID)
	request.ClientRequestID = strings.TrimSpace(request.ClientRequestID)
	request.BaseRef = strings.TrimSpace(request.BaseRef)
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.Model = strings.TrimSpace(request.Model)
	repository, err := featuredelivery.NormalizeRepository(request.Repository)
	if err != nil {
		return Request{}, err
	}
	request.Repository = repository
	if request.FeatureID == "" {
		return Request{}, fmt.Errorf("feature_id is required: %w", featuredelivery.ErrInvalid)
	}
	if request.ClientRequestID == "" {
		return Request{}, fmt.Errorf("client_request_id is required: %w", featuredelivery.ErrInvalid)
	}
	if len(request.ClientRequestID) > 128 {
		return Request{}, fmt.Errorf("client_request_id exceeds 128 bytes: %w", featuredelivery.ErrInvalid)
	}
	if request.BaseRef == "" {
		request.BaseRef = "HEAD"
	}
	if request.Provider == "" {
		return Request{}, fmt.Errorf("provider is required: %w", featuredelivery.ErrInvalid)
	}
	return request, nil
}

func optionsFromRequest(request Request) RequestOptions {
	return RequestOptions{
		ClientRequestID: request.ClientRequestID,
		Repository:      request.Repository,
		BaseRef:         request.BaseRef,
		Provider:        request.Provider,
		Model:           request.Model,
		NetworkEnabled:  request.NetworkEnabled,
	}
}

func (options RequestOptions) implementationOptions() featuredelivery.ImplementationOptions {
	return featuredelivery.ImplementationOptions{
		ClientRequestID:  options.ClientRequestID,
		DesignArtifactID: options.DesignArtifactID,
		PlanArtifactID:   options.PlanArtifactID,
		Repository:       options.Repository,
		BaseRef:          options.BaseRef,
		Provider:         options.Provider,
		Model:            options.Model,
		NetworkEnabled:   options.NetworkEnabled,
	}
}

func (state State) payload() (json.RawMessage, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal pipeline state: %w", err)
	}
	return raw, nil
}

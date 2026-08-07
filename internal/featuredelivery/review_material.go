package featuredelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	maxReviewMaterialBytes         = 256 << 10
	maxReviewPatchBytes            = 192 << 10
	maxReviewValidationBytes       = 64 << 10
	maxReviewValidationResultBytes = 16 << 10
)

type artifactReviewMaterial struct {
	ArtifactID       string          `json:"artifact_id"`
	Kind             ArtifactKind    `json:"kind"`
	Version          int             `json:"version"`
	ContentHash      string          `json:"content_hash"`
	DocumentJSON     json.RawMessage `json:"document_json"`
	RenderedMarkdown string          `json:"rendered_markdown"`
	Evidence         []EvidenceRef   `json:"evidence"`
}

type validationReviewMaterial struct {
	ValidationResult
	OutputComplete bool   `json:"output_complete"`
	OutputExcerpt  string `json:"output_excerpt,omitempty"`
}

func (service *Service) buildReviewContext(
	ctx context.Context,
	subject ReviewSubject,
) (agentapi.ContextBlock, error) {
	switch subject.Kind {
	case SubjectRequirement, SubjectRequirementAnalysis, SubjectTechnicalProposal,
		SubjectSystemDesign, SubjectImplementationPlan:
		return service.buildArtifactReviewContext(ctx, subject)
	case SubjectChangeSet:
		return service.buildChangeSetReviewContext(ctx, subject)
	case SubjectValidationBundle:
		return service.buildValidationReviewContext(ctx, subject)
	case SubjectDeliveryBundle:
		return service.buildDeliveryReviewContext(ctx, subject)
	default:
		return agentapi.ContextBlock{}, ErrUnavailable
	}
}

func (service *Service) buildArtifactReviewContext(
	ctx context.Context,
	subject ReviewSubject,
) (agentapi.ContextBlock, error) {
	artifact, err := service.store.GetArtifact(ctx, subject.ID)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	current, err := BuildArtifactReviewSubject(*artifact)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	if current != subject {
		return agentapi.ContextBlock{}, fmt.Errorf("artifact review subject changed: %w", ErrConflict)
	}
	raw, err := json.Marshal(reviewArtifactMaterial(*artifact))
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf("marshal artifact review material: %w", err)
	}
	content, complete := boundedReviewContent(raw, maxReviewMaterialBytes)
	return newReviewContextBlock(
		"feature_delivery.artifact",
		fmt.Sprintf("%s artifact %s version %d", artifact.Kind, artifact.ID, artifact.Version),
		content,
		complete,
		artifactEvidenceReferences(artifact.Evidence),
	), nil
}

func (service *Service) buildChangeSetReviewContext(
	ctx context.Context,
	subject ReviewSubject,
) (agentapi.ContextBlock, error) {
	run, err := service.store.GetImplementation(ctx, subject.ID)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	current, err := BuildChangeSetReviewSubject(*run)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	if current != subject {
		return agentapi.ContextBlock{}, fmt.Errorf("change set review subject changed: %w", ErrConflict)
	}
	if service.implementations == nil || run.ChangeSet == nil {
		return agentapi.ContextBlock{}, ErrUnavailable
	}
	patchPath, err := service.implementations.PatchPath(run.ChangeSet.PatchRelPath)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	patch, patchComplete, err := readVerifiedReviewPatch(
		patchPath,
		run.ChangeSet.PatchSHA256,
		run.ChangeSet.PatchBytes,
		maxReviewPatchBytes,
	)
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf("load review patch: %w", err)
	}
	raw, err := json.Marshal(struct {
		RunID           string          `json:"run_id"`
		Repository      string          `json:"repository"`
		BaseCommit      string          `json:"base_commit"`
		HeadCommit      string          `json:"head_commit"`
		Files           []ChangedFile   `json:"files"`
		PlanDeviations  []PlanDeviation `json:"plan_deviations"`
		ProviderSummary string          `json:"provider_summary"`
		PatchSHA256     string          `json:"patch_sha256"`
		PatchBytes      int64           `json:"patch_bytes"`
		PatchComplete   bool            `json:"patch_complete"`
		Patch           string          `json:"patch"`
	}{
		RunID: run.ID, Repository: run.Repo, BaseCommit: run.BaseCommit,
		HeadCommit: run.ChangeSet.WorktreeHead, Files: run.ChangeSet.Files,
		PlanDeviations:  run.ChangeSet.PlanDeviations,
		ProviderSummary: run.ChangeSet.ProviderSummary,
		PatchSHA256:     run.ChangeSet.PatchSHA256, PatchBytes: run.ChangeSet.PatchBytes,
		PatchComplete: patchComplete, Patch: patch,
	})
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf("marshal change set review material: %w", err)
	}
	content, materialComplete := boundedReviewContent(raw, maxReviewMaterialBytes)
	return newReviewContextBlock(
		"feature_delivery.change_set",
		fmt.Sprintf("change set %s for %s", run.ID, run.Repo),
		content,
		patchComplete && materialComplete,
		nil,
	), nil
}

func (service *Service) buildValidationReviewContext(
	ctx context.Context,
	subject ReviewSubject,
) (agentapi.ContextBlock, error) {
	run, err := service.store.GetImplementation(ctx, subject.ID)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	current, err := BuildValidationBundleReviewSubject(*run)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	if current != subject {
		return agentapi.ContextBlock{}, fmt.Errorf("validation review subject changed: %w", ErrConflict)
	}
	results, complete, err := service.loadValidationReviewMaterial(run.ChangeSet.ValidationResults)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	changeSubject, err := BuildChangeSetReviewSubject(*run)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	raw, err := json.Marshal(struct {
		RunID         string                     `json:"run_id"`
		Repository    string                     `json:"repository"`
		BaseCommit    string                     `json:"base_commit"`
		HeadCommit    string                     `json:"head_commit"`
		ChangeSetHash string                     `json:"change_set_hash"`
		Results       []validationReviewMaterial `json:"results"`
	}{
		RunID: run.ID, Repository: run.Repo, BaseCommit: run.BaseCommit,
		HeadCommit: run.ChangeSet.WorktreeHead, ChangeSetHash: changeSubject.ContentHash,
		Results: results,
	})
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf("marshal validation review material: %w", err)
	}
	content, materialComplete := boundedReviewContent(raw, maxReviewMaterialBytes)
	return newReviewContextBlock(
		"feature_delivery.validation_bundle",
		fmt.Sprintf("validation bundle %s for %s", run.ID, run.Repo),
		content,
		complete && materialComplete,
		nil,
	), nil
}

func (service *Service) buildDeliveryReviewContext(
	ctx context.Context,
	subject ReviewSubject,
) (agentapi.ContextBlock, error) {
	run, err := service.store.GetImplementation(ctx, subject.ID)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	design, plan, err := service.loadDeliveryArtifacts(ctx, *run)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	current, err := BuildDeliveryBundleReviewSubject(*run, design, plan)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	if current != subject {
		return agentapi.ContextBlock{}, fmt.Errorf("delivery review subject changed: %w", ErrConflict)
	}
	changeSubject, err := BuildChangeSetReviewSubject(*run)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	validationSubject, err := BuildValidationBundleReviewSubject(*run)
	if err != nil {
		return agentapi.ContextBlock{}, err
	}
	raw, err := json.Marshal(struct {
		RunID      string                 `json:"run_id"`
		Repository string                 `json:"repository"`
		BaseCommit string                 `json:"base_commit"`
		HeadCommit string                 `json:"head_commit"`
		Design     artifactReviewMaterial `json:"design"`
		Plan       artifactReviewMaterial `json:"plan"`
		ChangeSet  struct {
			SubjectHash     string          `json:"subject_hash"`
			PatchSHA256     string          `json:"patch_sha256"`
			PatchBytes      int64           `json:"patch_bytes"`
			FilesChanged    int             `json:"files_changed"`
			Additions       int             `json:"additions"`
			Deletions       int             `json:"deletions"`
			Files           []ChangedFile   `json:"files"`
			PlanDeviations  []PlanDeviation `json:"plan_deviations"`
			ProviderSummary string          `json:"provider_summary"`
		} `json:"change_set"`
		Validation struct {
			SubjectHash string             `json:"subject_hash"`
			Results     []ValidationResult `json:"results"`
		} `json:"validation"`
	}{
		RunID: run.ID, Repository: run.Repo, BaseCommit: run.BaseCommit,
		HeadCommit: run.ChangeSet.WorktreeHead,
		Design:     reviewArtifactMaterial(design),
		Plan:       reviewArtifactMaterial(plan),
		ChangeSet: struct {
			SubjectHash     string          `json:"subject_hash"`
			PatchSHA256     string          `json:"patch_sha256"`
			PatchBytes      int64           `json:"patch_bytes"`
			FilesChanged    int             `json:"files_changed"`
			Additions       int             `json:"additions"`
			Deletions       int             `json:"deletions"`
			Files           []ChangedFile   `json:"files"`
			PlanDeviations  []PlanDeviation `json:"plan_deviations"`
			ProviderSummary string          `json:"provider_summary"`
		}{
			SubjectHash: changeSubject.ContentHash,
			PatchSHA256: run.ChangeSet.PatchSHA256, PatchBytes: run.ChangeSet.PatchBytes,
			FilesChanged: run.ChangeSet.FilesChanged,
			Additions:    run.ChangeSet.Additions, Deletions: run.ChangeSet.Deletions,
			Files: run.ChangeSet.Files, PlanDeviations: run.ChangeSet.PlanDeviations,
			ProviderSummary: run.ChangeSet.ProviderSummary,
		},
		Validation: struct {
			SubjectHash string             `json:"subject_hash"`
			Results     []ValidationResult `json:"results"`
		}{
			SubjectHash: validationSubject.ContentHash,
			Results:     run.ChangeSet.ValidationResults,
		},
	})
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf("marshal delivery review material: %w", err)
	}
	content, complete := boundedReviewContent(raw, maxReviewMaterialBytes)
	references := artifactEvidenceReferences(design.Evidence)
	references = append(references, artifactEvidenceReferences(plan.Evidence)...)
	return newReviewContextBlock(
		"feature_delivery.delivery_bundle",
		fmt.Sprintf("delivery bundle %s for %s", run.ID, run.Repo),
		content,
		complete,
		references,
	), nil
}

func (service *Service) loadValidationReviewMaterial(
	results []ValidationResult,
) ([]validationReviewMaterial, bool, error) {
	material := make([]validationReviewMaterial, 0, len(results))
	remaining := maxReviewValidationBytes
	complete := true
	for _, result := range results {
		item := validationReviewMaterial{
			ValidationResult: result,
			OutputComplete:   result.OutputRelPath == "",
		}
		if result.OutputRelPath != "" {
			if service.implementations == nil {
				return nil, false, ErrUnavailable
			}
			path, err := service.implementations.ValidationOutputPath(result.OutputRelPath)
			if err != nil {
				return nil, false, err
			}
			limit := min(maxReviewValidationResultBytes, remaining)
			output, outputComplete, err := readVerifiedReviewArtifact(
				path,
				result.OutputSHA256,
				result.OutputBytes,
				maxValidationOutput,
				limit,
			)
			if err != nil {
				return nil, false, fmt.Errorf(
					"load validation output %d: %w",
					result.Sequence,
					err,
				)
			}
			item.OutputComplete = outputComplete
			item.OutputExcerpt = output
			remaining -= len(output)
			complete = complete && outputComplete
		}
		material = append(material, item)
	}
	return material, complete, nil
}

func readVerifiedReviewPatch(path, expectedHash string, expectedBytes int64, limit int) (string, bool, error) {
	return readVerifiedReviewArtifact(
		path,
		expectedHash,
		expectedBytes,
		maxGitOutputBytes,
		limit,
	)
}

func readVerifiedReviewArtifact(
	path, expectedHash string,
	expectedBytes, maxBytes int64,
	limit int,
) (string, bool, error) {
	if expectedBytes < 0 || expectedBytes > maxBytes {
		return "", false, fmt.Errorf("artifact size is invalid")
	}
	if len(expectedHash) != sha256.Size*2 || !isHex(expectedHash) {
		return "", false, fmt.Errorf("artifact hash is invalid")
	}
	if limit < 0 {
		return "", false, fmt.Errorf("artifact excerpt limit is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()

	reader := io.LimitReader(file, expectedBytes+1)
	prefix, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return "", false, err
	}
	hash := sha256.New()
	if _, err := hash.Write(prefix); err != nil {
		return "", false, err
	}
	remaining, err := io.Copy(hash, reader)
	if err != nil {
		return "", false, err
	}
	total := int64(len(prefix)) + remaining
	if total != expectedBytes {
		return "", false, fmt.Errorf("artifact size mismatch")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedHash) {
		return "", false, fmt.Errorf("artifact hash mismatch")
	}
	complete := len(prefix) <= limit
	if !complete {
		prefix = prefix[:limit]
	}
	for !utf8.Valid(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return string(prefix), complete, nil
}

func reviewArtifactMaterial(artifact Artifact) artifactReviewMaterial {
	return artifactReviewMaterial{
		ArtifactID: artifact.ID, Kind: artifact.Kind, Version: artifact.Version,
		ContentHash: artifact.ContentHash, DocumentJSON: artifact.DocumentJSON,
		RenderedMarkdown: artifact.RenderedMarkdown, Evidence: artifact.Evidence,
	}
}

func boundedReviewContent(raw []byte, limit int) (string, bool) {
	if len(raw) <= limit {
		return string(raw), true
	}
	suffix := []byte("\n...[review material truncated by server]")
	end := limit - len(suffix)
	if end < 0 {
		end = 0
	}
	for end > 0 && !utf8.Valid(raw[:end]) {
		end--
	}
	content := make([]byte, 0, end+len(suffix))
	content = append(content, raw[:end]...)
	content = append(content, suffix...)
	return string(content), false
}

func newReviewContextBlock(
	source, title, content string,
	complete bool,
	references []agentapi.Reference,
) agentapi.ContextBlock {
	source = platform.RedactSensitiveText(source)
	title = platform.RedactSensitiveText(title)
	content = platform.RedactSensitiveText(content)
	references = append([]agentapi.Reference(nil), references...)
	for index := range references {
		references[index].Label = platform.RedactSensitiveText(references[index].Label)
		references[index].Target = platform.RedactSensitiveText(references[index].Target)
	}
	sum := sha256.Sum256([]byte(content))
	return agentapi.ContextBlock{
		Source: source, Title: title, Content: content,
		References: references, Complete: complete,
		ContentHash: hex.EncodeToString(sum[:]),
	}
}

func artifactEvidenceReferences(evidence []EvidenceRef) []agentapi.Reference {
	references := make([]agentapi.Reference, 0, len(evidence))
	for _, item := range evidence {
		target := item.Path
		if target == "" {
			target = item.Service
		}
		if target == "" {
			target = item.Repo
		}
		if target == "" {
			target = item.Hash
		}
		references = append(references, agentapi.Reference{
			Type: item.Kind, Label: item.Summary, Target: target,
		})
	}
	return references
}

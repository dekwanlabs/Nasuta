package featuredelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestBuildArtifactReviewContextPinsStoredSubject(t *testing.T) {
	artifact := executionReviewArtifact()
	subject, err := BuildArtifactReviewSubject(artifact)
	if err != nil {
		t.Fatal(err)
	}
	store := &executionReviewStore{artifacts: map[string]Artifact{artifact.ID: artifact}}
	service := NewService(store, nil, 0)

	block, err := service.buildReviewContext(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if block.Source != "feature_delivery.artifact" || !block.Complete {
		t.Fatalf("context = %+v", block)
	}
	var material struct {
		DocumentJSON     json.RawMessage `json:"document_json"`
		RenderedMarkdown string          `json:"rendered_markdown"`
		Evidence         []EvidenceRef   `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(block.Content), &material); err != nil {
		t.Fatal(err)
	}
	if string(material.DocumentJSON) != string(artifact.DocumentJSON) ||
		material.RenderedMarkdown != artifact.RenderedMarkdown ||
		len(material.Evidence) != len(artifact.Evidence) {
		t.Fatalf("context material = %+v", material)
	}
	sum := sha256.Sum256([]byte(block.Content))
	if block.ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("content hash = %q", block.ContentHash)
	}
	if len(block.References) != 1 ||
		block.References[0].Target != artifact.Evidence[0].Path {
		t.Fatalf("references = %+v", block.References)
	}

	artifact.ContentHash = "changed-artifact-hash"
	store.artifacts[artifact.ID] = artifact
	if _, err := service.buildReviewContext(
		context.Background(), subject,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("drift error = %v, want conflict", err)
	}
}

func TestReviewContextRedactsSensitiveMaterialBeforeHashing(t *testing.T) {
	block := newReviewContextBlock(
		"mysql://app:source-secret@db/service",
		"Authorization: Bearer title-secret",
		`{"authorization":"Bearer content-secret","dsn":"postgres://app:database-secret@db/service"}`,
		true,
		[]agentapi.Reference{{
			Label:  "api_key=label-secret",
			Target: "https://app:target-secret@example.com/path",
		}},
	)

	assertReviewSecretsAbsent(t, block, []string{
		"source-secret", "title-secret", "content-secret",
		"database-secret", "label-secret", "target-secret",
	})
	sum := sha256.Sum256([]byte(block.Content))
	if block.ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("content hash = %q", block.ContentHash)
	}
}

func TestReadVerifiedReviewPatchRejectsHashAndSizeMismatch(t *testing.T) {
	content := []byte("diff --git a/file.go b/file.go\n+changed\n")
	path := writeReviewPatch(t, content)
	hash := reviewPatchHash(content)

	if _, _, err := readVerifiedReviewPatch(
		path, strings.Repeat("0", sha256.Size*2), int64(len(content)), 1024,
	); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("hash mismatch error = %v", err)
	}
	if _, _, err := readVerifiedReviewPatch(
		path, hash, int64(len(content)+1), 1024,
	); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("size mismatch error = %v", err)
	}
}

func TestReadVerifiedReviewPatchReturnsUTF8SafePrefix(t *testing.T) {
	const limit = 32
	content := []byte(strings.Repeat("a", limit-1) + "界tail")
	path := writeReviewPatch(t, content)

	patch, complete, err := readVerifiedReviewPatch(
		path, reviewPatchHash(content), int64(len(content)), limit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("oversized patch reported complete")
	}
	if !utf8.ValidString(patch) {
		t.Fatalf("patch is not valid UTF-8: %q", patch)
	}
	if patch != strings.Repeat("a", limit-1) {
		t.Fatalf("patch prefix = %q", patch)
	}
}

func TestBuildChangeSetReviewContextExcludesValidationResults(t *testing.T) {
	run, _, _ := implementationReviewFixture()
	patch := []byte("diff --git a/review.go b/review.go\n+change-set-only-marker\n")
	run.ChangeSet.PatchSHA256 = reviewPatchHash(patch)
	run.ChangeSet.PatchBytes = int64(len(patch))
	run.ChangeSet.ValidationResults[0].OutputSummary = "validation-only-marker"
	service := reviewMaterialService(t, run, nil, map[string][]byte{
		run.ChangeSet.PatchRelPath: patch,
	})
	subject, err := BuildChangeSetReviewSubject(run)
	if err != nil {
		t.Fatal(err)
	}

	block, err := service.buildReviewContext(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block.Content, "change-set-only-marker") {
		t.Fatalf("change set material omitted patch: %s", block.Content)
	}
	if strings.Contains(block.Content, "validation-only-marker") ||
		strings.Contains(block.Content, "validation_results") {
		t.Fatalf("change set material leaked validation results: %s", block.Content)
	}
}

func TestBuildValidationReviewContextRejectsOutputDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ValidationResult)
		want   string
	}{
		{
			name: "hash",
			mutate: func(result *ValidationResult) {
				result.OutputSHA256 = strings.Repeat("0", sha256.Size*2)
			},
			want: "hash mismatch",
		},
		{
			name: "size",
			mutate: func(result *ValidationResult) {
				result.OutputBytes++
			},
			want: "size mismatch",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, _, _ := implementationReviewFixture()
			output := []byte("verified validation output")
			result := &run.ChangeSet.ValidationResults[0]
			result.OutputSHA256 = reviewPatchHash(output)
			result.OutputBytes = int64(len(output))
			test.mutate(result)
			service := reviewMaterialService(t, run, nil, map[string][]byte{
				result.OutputRelPath: output,
			})
			subject, err := BuildValidationBundleReviewSubject(run)
			if err != nil {
				t.Fatal(err)
			}

			_, err = service.buildReviewContext(context.Background(), subject)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildValidationReviewContextUsesUTF8SafeBoundedExcerpts(t *testing.T) {
	run, _, _ := implementationReviewFixture()
	output := []byte(strings.Repeat("a", maxReviewValidationResultBytes-1) + "界tail")
	result := &run.ChangeSet.ValidationResults[0]
	result.OutputSHA256 = reviewPatchHash(output)
	result.OutputBytes = int64(len(output))
	service := reviewMaterialService(t, run, nil, map[string][]byte{
		result.OutputRelPath: output,
	})
	subject, err := BuildValidationBundleReviewSubject(run)
	if err != nil {
		t.Fatal(err)
	}

	block, err := service.buildReviewContext(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	var material struct {
		Results []validationReviewMaterial `json:"results"`
	}
	if err := json.Unmarshal([]byte(block.Content), &material); err != nil {
		t.Fatal(err)
	}
	if block.Complete || len(material.Results) != 1 || material.Results[0].OutputComplete {
		t.Fatalf("validation material completeness = block:%t results:%+v", block.Complete, material.Results)
	}
	if !utf8.ValidString(material.Results[0].OutputExcerpt) ||
		material.Results[0].OutputExcerpt != strings.Repeat("a", maxReviewValidationResultBytes-1) {
		t.Fatalf("validation excerpt is not the expected UTF-8 prefix")
	}
}

func TestBuildDeliveryReviewContextUsesSummariesWithoutPatchOrLogs(t *testing.T) {
	run, design, plan := implementationReviewFixture()
	run.ChangeSet.ProviderSummary = "delivery-provider-summary"
	run.ChangeSet.ValidationResults[0].OutputSummary = "delivery-validation-summary"
	service := reviewMaterialService(t, run, []Artifact{design, plan}, nil)
	subject, err := BuildDeliveryBundleReviewSubject(run, design, plan)
	if err != nil {
		t.Fatal(err)
	}

	block, err := service.buildReviewContext(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if !block.Complete ||
		!strings.Contains(block.Content, design.RenderedMarkdown) ||
		!strings.Contains(block.Content, plan.RenderedMarkdown) ||
		!strings.Contains(block.Content, "delivery-provider-summary") ||
		!strings.Contains(block.Content, "delivery-validation-summary") {
		t.Fatalf("delivery material is incomplete: %s", block.Content)
	}
	if strings.Contains(block.Content, `"patch":`) ||
		strings.Contains(block.Content, "output_excerpt") {
		t.Fatalf("delivery material included full patch or validation logs: %s", block.Content)
	}
	if len(block.References) != len(design.Evidence)+len(plan.Evidence) {
		t.Fatalf("delivery references = %+v", block.References)
	}
}

func reviewMaterialService(
	t *testing.T,
	run ImplementationRun,
	artifacts []Artifact,
	files map[string][]byte,
) *Service {
	t.Helper()
	artifactRoot := t.TempDir()
	for relative, content := range files {
		path := filepath.Join(artifactRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifactByID := make(map[string]Artifact, len(artifacts))
	for _, artifact := range artifacts {
		artifactByID[artifact.ID] = artifact
	}
	store := &executionReviewStore{
		implementation: run,
		artifacts:      artifactByID,
	}
	service := NewService(store, nil, 0)
	service.SetImplementationManager(&ImplementationManager{
		git: &GitManager{artifactsRoot: artifactRoot},
	})
	return service
}

func writeReviewPatch(t *testing.T, content []byte) string {
	t.Helper()
	path := t.TempDir() + "/review.patch"
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reviewPatchHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

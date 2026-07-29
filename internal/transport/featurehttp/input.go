package featurehttp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

const (
	maxRequirementListItems = 100
	maxRequirementItemBytes = 16 << 10
)

func normalizeFeatureInput(title string, requirement featuredelivery.RequirementDocument) (string, featuredelivery.RequirementDocument, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", requirement, fmt.Errorf("title is required")
	}
	normalized, err := normalizeRequirement(requirement)
	return title, normalized, err
}

func normalizeRequirement(value featuredelivery.RequirementDocument) (featuredelivery.RequirementDocument, error) {
	value.Description = strings.TrimSpace(value.Description)
	if value.Description == "" {
		return value, fmt.Errorf("requirement description is required")
	}
	var err error
	for _, target := range []struct {
		name   string
		values *[]string
	}{
		{"business_constraints", &value.BusinessConstraints},
		{"attachments", &value.Attachments},
		{"acceptance_criteria", &value.AcceptanceCriteria},
	} {
		*target.values, err = normalizeTextList(target.name, *target.values)
		if err != nil {
			return value, err
		}
	}
	return value, nil
}

func normalizeTextList(name string, values []string) ([]string, error) {
	if len(values) > maxRequirementListItems {
		return nil, fmt.Errorf("%s exceeds %d items", name, maxRequirementListItems)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > maxRequirementItemBytes {
			return nil, fmt.Errorf("%s item exceeds %d bytes", name, maxRequirementItemBytes)
		}
		out = append(out, value)
	}
	return out, nil
}

func normalizeImplementationOptions(options *featuredelivery.ImplementationOptions) error {
	options.ClientRequestID = strings.TrimSpace(options.ClientRequestID)
	options.DesignArtifactID = strings.TrimSpace(options.DesignArtifactID)
	options.PlanArtifactID = strings.TrimSpace(options.PlanArtifactID)
	options.ParentRunID = strings.TrimSpace(options.ParentRunID)
	options.BaseRef = strings.TrimSpace(options.BaseRef)
	if options.BaseRef == "" {
		options.BaseRef = "HEAD"
	}
	options.Provider = strings.ToLower(strings.TrimSpace(options.Provider))
	options.Model = strings.TrimSpace(options.Model)
	repository, err := featuredelivery.NormalizeRepository(options.Repository)
	if err != nil {
		return err
	}
	options.Repository = repository
	return nil
}

func requestLimit(r *http.Request, defaultLimit, maxLimit int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return defaultLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}
	return limit, nil
}

type featureCursorPayload struct {
	UpdatedAt time.Time `json:"updated_at"`
	ID        string    `json:"id"`
}

type runCursorPayload struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

type artifactCursorPayload struct {
	Kind    featuredelivery.ArtifactKind `json:"kind"`
	Version int                          `json:"version"`
}

func decodeFeatureCursor(value string) (featuredelivery.FeatureCursor, error) {
	if strings.TrimSpace(value) == "" {
		return featuredelivery.FeatureCursor{}, nil
	}
	var payload featureCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.UpdatedAt.IsZero() || payload.ID == "" {
		return featuredelivery.FeatureCursor{}, fmt.Errorf("invalid feature cursor")
	}
	return featuredelivery.FeatureCursor{UpdatedAt: payload.UpdatedAt, ID: payload.ID}, nil
}

func decodeRunCursor(value string) (featuredelivery.RunCursor, error) {
	if strings.TrimSpace(value) == "" {
		return featuredelivery.RunCursor{}, nil
	}
	var payload runCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.CreatedAt.IsZero() || payload.ID == "" {
		return featuredelivery.RunCursor{}, fmt.Errorf("invalid implementation cursor")
	}
	return featuredelivery.RunCursor{CreatedAt: payload.CreatedAt, ID: payload.ID}, nil
}

func decodeArtifactCursor(value string) (featuredelivery.ArtifactCursor, error) {
	if strings.TrimSpace(value) == "" {
		return featuredelivery.ArtifactCursor{}, nil
	}
	var payload artifactCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.Version < 1 {
		return featuredelivery.ArtifactCursor{}, fmt.Errorf("invalid artifact cursor")
	}
	if _, err := featuredelivery.ParseArtifactKind(string(payload.Kind)); err != nil {
		return featuredelivery.ArtifactCursor{}, fmt.Errorf("invalid artifact cursor")
	}
	return featuredelivery.ArtifactCursor{Kind: payload.Kind, Version: payload.Version}, nil
}

func decodeGenerationCursor(value string) (featuredelivery.GenerationCursor, error) {
	if strings.TrimSpace(value) == "" {
		return featuredelivery.GenerationCursor{}, nil
	}
	var payload runCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.CreatedAt.IsZero() || payload.ID == "" {
		return featuredelivery.GenerationCursor{}, fmt.Errorf("invalid generation cursor")
	}
	return featuredelivery.GenerationCursor{StartedAt: payload.CreatedAt, ID: payload.ID}, nil
}

func decodeCursor(value string, payload any) error {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, payload)
}

func nextFeatureCursor(items []featuredelivery.FeatureRequest, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(featureCursorPayload{UpdatedAt: last.UpdatedAt, ID: last.ID})
}

func nextRunCursor(items []featuredelivery.ImplementationRun, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(runCursorPayload{CreatedAt: last.CreatedAt, ID: last.ID})
}

func nextArtifactCursor(items []featuredelivery.ArtifactSummary, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(artifactCursorPayload{Kind: last.Kind, Version: last.Version})
}

func nextGenerationCursor(items []featuredelivery.GenerationRun, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(runCursorPayload{CreatedAt: last.StartedAt, ID: last.ID})
}

func encodeCursor(payload any) string {
	raw, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(raw)
}

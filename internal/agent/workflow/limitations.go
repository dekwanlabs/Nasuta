package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"golang.org/x/text/unicode/norm"
)

const (
	LimitationsNormalizationVersion = "limitations-v1"
	PrimaryLimitationsDisplayLimit  = 10
	LimitationsDetailArtifactKind   = "investigation.limitations.detail"
)

type limitationSeverity string

const (
	limitationCritical limitationSeverity = "critical"
	limitationHigh     limitationSeverity = "high"
	limitationMedium   limitationSeverity = "medium"
	limitationLow      limitationSeverity = "low"
)

// LimitationRecord is the canonical, auditable representation of one limitation.
// It intentionally remains internal to the workflow package; the public answer
// only exposes the short primary text list and a detail reference.
type LimitationRecord struct {
	ID              string             `json:"id"`
	Text            string             `json:"text"`
	Severity        limitationSeverity `json:"severity"`
	Category        string             `json:"category"`
	Confidence      float64            `json:"confidence"`
	EvidenceRefs    []string           `json:"evidence_refs,omitempty"`
	ProducerNodeIDs []string           `json:"producer_node_ids,omitempty"`
	MergeKey        string             `json:"merge_key"`
	MergedFromIDs   []string           `json:"merged_from_ids,omitempty"`
	MergeReason     string             `json:"merge_reason,omitempty"`
	MergeMethod     string             `json:"merge_method,omitempty"`
	Displayed       bool               `json:"displayed"`
	Rank            int                `json:"rank"`
}

type limitationsDetailPayload struct {
	SchemaID             string             `json:"schema_id"`
	SchemaVersion        int                `json:"schema_version"`
	WorkflowRunID        string             `json:"workflow_run_id"`
	NormalizationVersion string             `json:"normalization_version"`
	RawCount             int                `json:"raw_count"`
	DeduplicatedCount    int                `json:"deduplicated_count"`
	MergedCount          int                `json:"merged_count"`
	DisplayedCount       int                `json:"displayed_count"`
	OmittedCount         int                `json:"omitted_count"`
	Limitations          []LimitationRecord `json:"limitations"`
}

type limitationsDetailRef struct {
	ArtifactID           string `json:"artifact_id"`
	TotalCount           int    `json:"total_count"`
	DisplayedCount       int    `json:"displayed_count"`
	OmittedCount         int    `json:"omitted_count"`
	NormalizationVersion string `json:"normalization_version"`
}

type rawLimitation struct {
	Text            string
	ProducerNodeIDs []string
	EvidenceRefs    []string
	FirstSeen       int
}

type normalizedLimitations struct {
	Primary []string
	Detail  WorkflowArtifact
	Ref     limitationsDetailRef
}

func normalizeLimitations(workflowRunID string, raw []rawLimitation) (normalizedLimitations, error) {
	canonical := make(map[string]*LimitationRecord, len(raw))
	firstSeen := make(map[string]int, len(raw))
	for index, item := range raw {
		text := strings.TrimSpace(norm.NFKC.String(item.Text))
		text = normalizeLimitationText(text)
		if text == "" {
			continue
		}
		key := limitationKey(text)
		record, ok := canonical[key]
		if !ok {
			record = &LimitationRecord{
				ID:            "lim_" + key[:16],
				Text:          text,
				Severity:      inferLimitationSeverity(text),
				Category:      inferLimitationCategory(text),
				Confidence:    0,
				MergeKey:      key,
				MergedFromIDs: []string{"lim_raw_" + fmt.Sprintf("%03d", index+1)},
				MergeMethod:   "exact_normalized_text",
			}
			canonical[key] = record
			firstSeen[key] = item.FirstSeen
		} else {
			record.MergedFromIDs = append(record.MergedFromIDs, "lim_raw_"+fmt.Sprintf("%03d", index+1))
			record.MergeReason = "same normalized limitation text"
		}
		record.ProducerNodeIDs = unionStrings(record.ProducerNodeIDs, item.ProducerNodeIDs)
		record.EvidenceRefs = unionStrings(record.EvidenceRefs, item.EvidenceRefs)
		if severityRank(itemSeverity(item.Text)) > severityRank(record.Severity) {
			record.Severity = itemSeverity(item.Text)
		}
	}

	records := make([]LimitationRecord, 0, len(canonical))
	for _, record := range canonical {
		records = append(records, *record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) > severityRank(right.Severity)
		}
		if len(left.EvidenceRefs) != len(right.EvidenceRefs) {
			return len(left.EvidenceRefs) > len(right.EvidenceRefs)
		}
		if firstSeen[left.MergeKey] != firstSeen[right.MergeKey] {
			return firstSeen[left.MergeKey] < firstSeen[right.MergeKey]
		}
		return left.ID < right.ID
	})

	for index := range records {
		records[index].Rank = index + 1
		records[index].Displayed = index < PrimaryLimitationsDisplayLimit
	}
	primary := make([]string, 0, min(PrimaryLimitationsDisplayLimit, len(records)))
	for _, record := range records {
		if !record.Displayed {
			continue
		}
		primary = append(primary, record.Text)
	}

	payload := limitationsDetailPayload{
		SchemaID:             LimitationsDetailArtifactKind,
		SchemaVersion:        1,
		WorkflowRunID:        workflowRunID,
		NormalizationVersion: LimitationsNormalizationVersion,
		RawCount:             len(raw),
		DeduplicatedCount:    len(canonical),
		MergedCount:          countMerged(records),
		DisplayedCount:       len(primary),
		OmittedCount:         max(0, len(records)-len(primary)),
		Limitations:          records,
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return normalizedLimitations{}, fmt.Errorf("marshal limitations detail: %w", err)
	}
	contentHash := sha256.Sum256(content)
	artifact := WorkflowArtifact{
		ID:             limitationArtifactID(workflowRunID, LimitationsNormalizationVersion),
		WorkflowRunID:  workflowRunID,
		ProducerNodeID: "evidence.verify",
		Kind:           LimitationsDetailArtifactKind,
		Schema:         agentapi.SchemaRef{ID: LimitationsDetailArtifactKind, Version: 1},
		ContentHash:    hex.EncodeToString(contentHash[:]),
		Content:        content,
	}
	return normalizedLimitations{
		Primary: primary,
		Detail:  artifact,
		Ref: limitationsDetailRef{
			ArtifactID:           artifact.ID,
			TotalCount:           len(records),
			DisplayedCount:       len(primary),
			OmittedCount:         max(0, len(records)-len(primary)),
			NormalizationVersion: LimitationsNormalizationVersion,
		},
	}, nil
}

func normalizeLimitationText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func limitationKey(text string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(text)))
	return hex.EncodeToString(sum[:])
}

func inferLimitationSeverity(text string) limitationSeverity {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "security"), strings.Contains(lower, "data loss"), strings.Contains(lower, "critical"):
		return limitationCritical
	case strings.Contains(lower, "unavailable"), strings.Contains(lower, "excluded"), strings.Contains(lower, "unresolved"), strings.Contains(lower, "does not match"), strings.Contains(lower, "partial"):
		return limitationHigh
	case strings.Contains(lower, "omitted"), strings.Contains(lower, "missing"), strings.Contains(lower, "cannot"), strings.Contains(lower, "unable"):
		return limitationMedium
	default:
		return limitationMedium
	}
}

func itemSeverity(text string) limitationSeverity {
	return inferLimitationSeverity(normalizeLimitationText(text))
}

func inferLimitationCategory(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "evidence"), strings.Contains(lower, "finding"), strings.Contains(lower, "ledger"):
		return "coverage"
	case strings.Contains(lower, "task"), strings.Contains(lower, "investigation"):
		return "availability"
	default:
		return "unspecified"
	}
}

func severityRank(value limitationSeverity) int {
	switch value {
	case limitationCritical:
		return 4
	case limitationHigh:
		return 3
	case limitationMedium:
		return 2
	case limitationLow:
		return 1
	default:
		return 0
	}
}

func unionStrings(left, right []string) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	out := make([]string, 0, len(left)+len(right))
	for _, values := range [][]string{left, right} {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func countMerged(records []LimitationRecord) int {
	count := 0
	for _, record := range records {
		if len(record.MergedFromIDs) > 1 {
			count++
		}
	}
	return count
}

func limitationArtifactID(workflowRunID, version string) string {
	sum := sha256.Sum256([]byte(workflowRunID + "\x00" + version))
	bytes := sum[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("art_%s-%s-%s-%s-%s",
		hex.EncodeToString(bytes[0:4]),
		hex.EncodeToString(bytes[4:6]),
		hex.EncodeToString(bytes[6:8]),
		hex.EncodeToString(bytes[8:10]),
		hex.EncodeToString(bytes[10:16]),
	)
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

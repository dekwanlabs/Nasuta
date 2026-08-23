package execution

import (
	"bytes"
	"encoding/json"
	"strconv"

	canonicalevidence "github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

type promptEvidenceIdentity struct {
	EvidenceID string `json:"evidence_id"`
	SourceKind string `json:"source_kind"`
	Target     string `json:"target"`
	Section    string `json:"section,omitempty"`
	Version    string `json:"version,omitempty"`
	TimeRange  string `json:"time_range,omitempty"`
}

func modelFacingToolContent(
	runID string,
	toolCallID string,
	content string,
	contract tool.AnswerContract,
	units []tool.EvidenceUnit,
	maxBytes int,
) (string, string) {
	if len(units) == 0 {
		if maxBytes <= 0 {
			return content, ""
		}
		return boundedToolPrompt(runID, toolCallID, content, contract, maxBytes)
	}
	manifestBudget := 0
	if maxBytes > 0 {
		manifestBudget = maxBytes / 3
		manifestBudget = max(512, manifestBudget)
		manifestBudget = min(8*1024, manifestBudget)
		manifestBudget = min(manifestBudget, maxBytes/2)
	}
	manifest := evidenceManifest(units, manifestBudget)
	if manifest == "" {
		if maxBytes <= 0 {
			return content, ""
		}
		return boundedToolPrompt(runID, toolCallID, content, contract, maxBytes)
	}
	const separator = "\n"
	if maxBytes <= 0 {
		return content + separator + manifest, ""
	}
	contentBudget := maxBytes - len(separator) - len(manifest)
	if contentBudget < 64 {
		return boundedToolPrompt(runID, toolCallID, content, contract, maxBytes)
	}
	bounded, artifactID := boundedToolPrompt(
		runID, toolCallID, content, contract, contentBudget,
	)
	return bounded + separator + manifest, artifactID
}

func evidenceManifest(units []tool.EvidenceUnit, maxBytes int) string {
	expanded := canonicalevidence.Expand(units)
	encoded := make([][]byte, 0, len(expanded))
	seen := make(map[string]struct{}, len(expanded))
	for _, unit := range expanded {
		key, ok := canonicalevidence.UnitKey(unit)
		if !ok {
			continue
		}
		handle := key.Handle()
		if _, duplicate := seen[handle]; duplicate {
			continue
		}
		seen[handle] = struct{}{}
		item, err := json.Marshal(promptEvidenceIdentity{
			EvidenceID: handle,
			SourceKind: key.SourceKind,
			Target:     key.Target,
			Section:    key.Section,
			Version:    key.Version,
			TimeRange:  key.TimeRange,
		})
		if err != nil {
			continue
		}
		encoded = append(encoded, item)
	}
	if len(encoded) == 0 {
		return ""
	}

	const prefix = `{"_nasuta_evidence_manifest":{"version":1,"items":[`
	const suffix = `]}}`
	var buffer bytes.Buffer
	buffer.Grow(len(prefix) + len(suffix) + len(encoded)*96)
	buffer.WriteString(prefix)
	included := 0
	for index, item := range encoded {
		separatorBytes := 0
		if included > 0 {
			separatorBytes = 1
		}
		remaining := len(encoded) - index - 1
		footerBytes := len(suffix)
		if remaining > 0 {
			footerBytes += len(`,"omitted":`) + len(strconv.Itoa(remaining))
		}
		if maxBytes > 0 && buffer.Len()+separatorBytes+len(item)+footerBytes > maxBytes {
			break
		}
		if included > 0 {
			buffer.WriteByte(',')
		}
		buffer.Write(item)
		included++
	}
	if included == 0 {
		return ""
	}
	buffer.WriteByte(']')
	if omitted := len(encoded) - included; omitted > 0 {
		buffer.WriteString(`,"omitted":`)
		buffer.WriteString(strconv.Itoa(omitted))
	}
	buffer.WriteString(`}}`)
	if maxBytes > 0 && buffer.Len() > maxBytes {
		return ""
	}
	return buffer.String()
}

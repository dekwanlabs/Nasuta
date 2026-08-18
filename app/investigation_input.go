package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	investigationSeedReferenceLimit = 8
	investigationSeedEvidenceLimit  = 8
	investigationSeedConflictLimit  = 4
)

func marshalInvestigationContract(
	contract agent.TaskContract,
	maxTokens int,
) ([]byte, error) {
	if maxTokens <= 0 {
		return nil, fmt.Errorf(
			"investigator payload budget must be positive: %d",
			maxTokens,
		)
	}

	prepared := cloneInvestigationContract(contract)
	prepared.Objective = investigation.BoundedSummary(prepared.Objective)
	if prepared.Objective == "" {
		return nil, fmt.Errorf("investigation task objective is required")
	}
	originalBlocks := prepared.Context.SeedMaterial
	prepared.Context.SeedMaterial = nil
	base, err := json.Marshal(prepared)
	if err != nil {
		return nil, err
	}
	baseTokens := tooloutput.EstimateTokens(string(base))
	if baseTokens > maxTokens {
		return nil, fmt.Errorf(
			"investigation task contract exceeds investigator payload budget: base=%d budget=%d",
			baseTokens,
			maxTokens,
		)
	}
	if len(originalBlocks) == 0 {
		return base, nil
	}

	prepared.Context.SeedMaterial = compactSeedMetadata(originalBlocks)
	for index := range prepared.Context.SeedMaterial {
		prepared.Context.SeedMaterial[index].Content = ""
		prepared.Context.SeedMaterial[index].ContentHash = hashInvestigationContent("")
	}
	metadata, err := json.Marshal(prepared)
	if err != nil {
		return nil, err
	}
	metadataTokens := tooloutput.EstimateTokens(string(metadata))
	if metadataTokens > maxTokens {
		return nil, fmt.Errorf(
			"investigation seed metadata exceeds investigator payload budget: metadata=%d budget=%d",
			metadataTokens,
			maxTokens,
		)
	}

	contentCosts := make([]int, len(originalBlocks))
	totalContentTokens := 0
	for index, block := range originalBlocks {
		contentCosts[index] = tooloutput.EstimateTokens(block.Content)
		totalContentTokens += contentCosts[index]
	}
	remainingTokens := maxTokens - metadataTokens
	contentLimits := proportionalContentLimits(contentCosts, remainingTokens)
	for {
		applySeedContent(prepared.Context.SeedMaterial, originalBlocks, contentLimits)
		input, err := json.Marshal(prepared)
		if err != nil {
			return nil, err
		}
		inputTokens := tooloutput.EstimateTokens(string(input))
		if inputTokens <= maxTokens {
			return input, nil
		}
		if totalContentTokens == 0 || !reduceContentLimits(contentLimits, inputTokens-maxTokens) {
			return nil, fmt.Errorf(
				"investigation task contract cannot fit investigator payload budget: payload=%d budget=%d",
				inputTokens,
				maxTokens,
			)
		}
	}
}

func (p *Platform) investigatorPayloadBudget(version int64) (int, error) {
	if p == nil || p.agents.catalog == nil {
		return 0, workflow.ErrUnavailable
	}
	definitions := []string{
		"investigator.code", "investigator.runtime", "investigator.docs",
		"investigator.web", "investigator.memory",
	}
	minimum := 0
	for _, id := range definitions {
		definition, err := p.agents.catalog.Resolve(agentapi.DefinitionRef{
			ID: id, Version: version,
		})
		if err != nil {
			return 0, fmt.Errorf(
				"resolve investigation agent %q version %d: %w",
				id,
				version,
				err,
			)
		}
		budget, err := agentPayloadBudget(definition)
		if err != nil {
			return 0, err
		}
		if minimum == 0 || budget < minimum {
			minimum = budget
		}
	}
	return minimum, nil
}

func cloneInvestigationContract(contract agent.TaskContract) agent.TaskContract {
	contract.Context.SeedMaterial = cloneInvestigationBlocks(
		contract.Context.SeedMaterial,
	)
	return contract
}

func cloneInvestigationBlocks(
	blocks []agentapi.ContextBlock,
) []agentapi.ContextBlock {
	if len(blocks) == 0 {
		return nil
	}
	cloned := make([]agentapi.ContextBlock, len(blocks))
	for index, block := range blocks {
		block.References = append([]agentapi.Reference(nil), block.References...)
		block.Evidence = append([]tool.EvidenceUnit(nil), block.Evidence...)
		block.EvidenceConflicts = append(
			[]agentapi.EvidenceConflict(nil),
			block.EvidenceConflicts...,
		)
		cloned[index] = block
	}
	return cloned
}

func compactSeedMetadata(
	blocks []agentapi.ContextBlock,
) []agentapi.ContextBlock {
	compacted := cloneInvestigationBlocks(blocks)
	for index := range compacted {
		block := &compacted[index]
		if len(block.References) > investigationSeedReferenceLimit {
			block.References = block.References[:investigationSeedReferenceLimit]
		}
		if len(block.Evidence) > investigationSeedEvidenceLimit {
			block.Evidence = block.Evidence[:investigationSeedEvidenceLimit]
		}
		if len(block.EvidenceConflicts) > investigationSeedConflictLimit {
			block.EvidenceConflicts = block.EvidenceConflicts[:investigationSeedConflictLimit]
		}
	}
	return compacted
}

func proportionalContentLimits(costs []int, budget int) []int {
	limits := make([]int, len(costs))
	if budget <= 0 {
		return limits
	}
	total := 0
	for _, cost := range costs {
		total += cost
	}
	if total == 0 || total <= budget {
		copy(limits, costs)
		return limits
	}
	for index, cost := range costs {
		limits[index] = cost * budget / total
	}
	return limits
}

func applySeedContent(
	prepared []agentapi.ContextBlock,
	original []agentapi.ContextBlock,
	limits []int,
) {
	for index := range prepared {
		prepared[index].Content = tooloutput.TruncateContent(
			original[index].Content,
			limits[index],
		)
		prepared[index].ContentHash = hashInvestigationContent(
			prepared[index].Content,
		)
	}
}

func reduceContentLimits(limits []int, excess int) bool {
	active := 0
	for _, limit := range limits {
		if limit > 0 {
			active++
		}
	}
	if active == 0 {
		return false
	}
	if excess < active {
		excess = active
	}
	decrement := (excess + active - 1) / active
	for index, limit := range limits {
		if limit <= 0 {
			continue
		}
		if decrement >= limit {
			limits[index] = 0
			continue
		}
		limits[index] = limit - decrement
	}
	return true
}

func hashInvestigationContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

package qa

import (
	"context"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

// admittedEvidence is the pre-answer evidence handoff for a normal agent run.
// The retrieved prose and memory blocks are assembled once before the agent loop.
type admittedEvidence struct {
	Retrieved *retrieval.RetrievedContext
	Recalled  []memory.MemoryRecord
	Material  []agentapi.ContextBlock
	Units     []tool.EvidenceUnit
	Plan      domain.EvidencePlan
}

func assembleAdmittedEvidence(evidence *preparedEvidence) admittedEvidence {
	if evidence == nil {
		return admittedEvidence{}
	}
	material := contextBlocks(evidence.retrieved)
	if recalled := memoryContextBlock(evidence.recalled); recalled != nil {
		material = append(material, *recalled)
	}
	return admittedEvidence{
		Retrieved: evidence.retrieved,
		Recalled:  evidence.recalled,
		Material:  material,
		Units:     contextBlockEvidence(material),
	}
}

func (svc *Service) acquireEvidence(
	ctx context.Context,
	prepared *preparation,
	stepRecorder preparationStepRecorder,
	ledgerOwner any,
) (*admittedEvidence, error) {
	planning := prepared.planning
	log.InfofCtx(ctx, "[qa] evidence plan proposed=%s proposed_sources=%v confidence=%.2f origin=%s effective=%s effective_sources=%v effective_confidence=%.2f effective_origin=%s",
		planning.Decision.Plan.String(), planning.Decision.Plan.SourceNames(), planning.Decision.Confidence, planning.Decision.Origin,
		planning.Effective.Plan.String(), planning.Effective.Plan.SourceNames(), planning.Effective.Confidence, planning.Effective.Origin,
	)
	evidencePlan := planning.Effective.Plan
	webUnavailable := evidencePlan.Has(domain.Web) && !svc.cfg.WebSearchEnabled
	if webUnavailable {
		log.WarnfCtx(ctx, "[qa] retrieval source unavailable: web")
	}
	evidence, err := svc.prepareEvidence(
		ctx, prepared, evidencePlan, webUnavailable, stepRecorder,
	)
	if err != nil {
		return nil, err
	}
	admitted := assembleAdmittedEvidence(evidence)
	admitted.Plan = evidencePlan
	if err := recordEvidenceLedger(ctx, ledgerOwner, admitted.Units); err != nil {
		return nil, fmt.Errorf("persist QA evidence ledger: %w", err)
	}
	return &admitted, nil
}

func contextBlocks(rc *retrieval.RetrievedContext) []agentapi.ContextBlock {
	if rc == nil || rc.Text == "" {
		return nil
	}
	references := make([]agentapi.Reference, 0, len(rc.References))
	for _, reference := range rc.References {
		references = append(references, agentapi.Reference{
			Type: reference.Type, Label: reference.Label, Target: reference.Target,
		})
	}
	return []agentapi.ContextBlock{{
		Source: "qa.evidence", Title: "QA Evidence", Content: rc.Text,
		References: references, Evidence: cloneEvidenceUnits(rc.EvidenceUnits),
		EvidenceConflicts: publicEvidenceConflicts(rc.EvidenceConflicts),
		Complete:          false, ContentHash: hashString(rc.Text),
	}}
}

func memoryContextBlock(records []memory.MemoryRecord) *agentapi.ContextBlock {
	content := memory.FormatMemories(records)
	if content == "" {
		return nil
	}
	return &agentapi.ContextBlock{
		Source: "qa.memory", Title: "Recalled Memory", Content: content,
		Complete: false, ContentHash: hashString(content),
	}
}

func contextBlockEvidence(blocks []agentapi.ContextBlock) []tool.EvidenceUnit {
	ledger := evidence.New(nil, "")
	for _, block := range blocks {
		ledger.Add(block.Evidence, "preloaded")
	}
	return ledger.Units()
}

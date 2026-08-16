package delegation

import (
	"context"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

// ParentContext is server-owned state inherited by delegate_investigation.
type ParentContext struct {
	RunID           string
	QuestionSummary string
	HighRisk        bool
	Actor           agentapi.Actor
	Permissions     agentapi.PermissionPolicy
	Correlation     agentapi.Correlation
	Limits          agentapi.RunLimits
	Depth           int
	Evidence        map[string]tool.EvidenceUnit
	Context         map[string]agentapi.ContextBlock
}

type parentContextKey struct{}

func WithParentContext(ctx context.Context, parent ParentContext) context.Context {
	parent.Permissions.Scopes = append([]string(nil), parent.Permissions.Scopes...)
	parent.Evidence = cloneEvidenceIndex(parent.Evidence)
	parent.Context = cloneContextIndex(parent.Context)
	return context.WithValue(ctx, parentContextKey{}, parent)
}

func ParentContextFrom(ctx context.Context) (ParentContext, bool) {
	parent, ok := ctx.Value(parentContextKey{}).(ParentContext)
	if !ok {
		return ParentContext{}, false
	}
	parent.Permissions.Scopes = append([]string(nil), parent.Permissions.Scopes...)
	parent.Evidence = cloneEvidenceIndex(parent.Evidence)
	parent.Context = cloneContextIndex(parent.Context)
	return parent, true
}

// IndexContext creates the server-owned aliases accepted by evidence_refs.
func IndexContext(
	blocks []agentapi.ContextBlock,
) (map[string]tool.EvidenceUnit, map[string]agentapi.ContextBlock) {
	if len(blocks) == 0 {
		return nil, nil
	}
	evidence := make(map[string]tool.EvidenceUnit)
	contextByRef := make(map[string]agentapi.ContextBlock)
	for _, block := range blocks {
		aliases := make(map[string]struct{})
		addAlias(aliases, block.ContentHash)
		for _, reference := range block.References {
			addAlias(aliases, reference.Target)
			if reference.Type != "" && reference.Target != "" {
				addAlias(aliases, reference.Type+":"+reference.Target)
			}
		}
		for _, unit := range block.Evidence {
			unitAliases := evidenceAliases(unit)
			for _, alias := range unitAliases {
				if _, exists := evidence[alias]; !exists {
					evidence[alias] = cloneEvidenceUnit(unit)
				}
				addAlias(aliases, alias)
			}
		}
		cloned := cloneContextBlock(block)
		for alias := range aliases {
			if _, exists := contextByRef[alias]; !exists {
				contextByRef[alias] = cloned
			}
		}
	}
	if len(evidence) == 0 {
		evidence = nil
	}
	if len(contextByRef) == 0 {
		contextByRef = nil
	}
	return evidence, contextByRef
}

// AddEvidenceUnits extends a ledger with stable aliases for child evidence.
func AddEvidenceUnits(
	ledger map[string]tool.EvidenceUnit,
	units []tool.EvidenceUnit,
) map[string]tool.EvidenceUnit {
	if ledger == nil && len(units) > 0 {
		ledger = make(map[string]tool.EvidenceUnit)
	}
	for _, unit := range units {
		for _, alias := range evidenceAliases(unit) {
			if _, exists := ledger[alias]; !exists {
				ledger[alias] = cloneEvidenceUnit(unit)
			}
		}
	}
	return ledger
}

func cloneEvidenceIndex(source map[string]tool.EvidenceUnit) map[string]tool.EvidenceUnit {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]tool.EvidenceUnit, len(source))
	for key, unit := range source {
		out[key] = cloneEvidenceUnit(unit)
	}
	return out
}

func cloneContextIndex(
	source map[string]agentapi.ContextBlock,
) map[string]agentapi.ContextBlock {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]agentapi.ContextBlock, len(source))
	for key, block := range source {
		out[key] = cloneContextBlock(block)
	}
	return out
}

func cloneContextBlock(block agentapi.ContextBlock) agentapi.ContextBlock {
	block.References = append([]agentapi.Reference(nil), block.References...)
	block.Evidence = make([]tool.EvidenceUnit, len(block.Evidence))
	for index, unit := range block.Evidence {
		block.Evidence[index] = cloneEvidenceUnit(unit)
	}
	block.EvidenceConflicts = make(
		[]agentapi.EvidenceConflict,
		len(block.EvidenceConflicts),
	)
	for index, conflict := range block.EvidenceConflicts {
		conflict.Current = cloneEvidenceUnit(conflict.Current)
		conflict.Incoming = cloneEvidenceUnit(conflict.Incoming)
		block.EvidenceConflicts[index] = conflict
	}
	return block
}

func cloneEvidenceUnit(unit tool.EvidenceUnit) tool.EvidenceUnit {
	unit.Sections = append([]string(nil), unit.Sections...)
	unit.Facets = append([]string(nil), unit.Facets...)
	return unit
}

func evidenceAliases(unit tool.EvidenceUnit) []string {
	aliases := make([]string, 0, 3)
	if value := strings.TrimSpace(unit.ContentHash); value != "" {
		aliases = append(aliases, value)
	}
	if value := strings.TrimSpace(unit.Target); value != "" {
		aliases = append(aliases, value)
		if source := strings.TrimSpace(unit.SourceKind); source != "" {
			aliases = append(aliases, source+":"+value)
		}
	}
	return aliases
}

func addAlias(aliases map[string]struct{}, value string) {
	if value = strings.TrimSpace(value); value != "" {
		aliases[value] = struct{}{}
	}
}

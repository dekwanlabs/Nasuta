package delegation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

// ParentContext is server-owned state inherited by delegate_investigation.
type ParentContext struct {
	RunID           string
	InvocationID    string
	QuestionSummary string
	HighRisk        bool
	Actor           agentapi.Actor
	Permissions     agentapi.PermissionPolicy
	Correlation     agentapi.Correlation
	Limits          agentapi.RunLimits
	// OutputContract is the parent request shape. Children use it to select
	// server-owned specialized budgets without inheriting the parent's final
	// answer rendering contract.
	OutputContract agentapi.RunOutputContract
	Depth          int
	Evidence       map[string]tool.EvidenceUnit
	Context        map[string]agentapi.ContextBlock
}

type parentContextKey struct{}

func WithParentContext(ctx context.Context, parent ParentContext) context.Context {
	parent.Permissions.Scopes = append([]string(nil), parent.Permissions.Scopes...)
	parent.Evidence = cloneEvidenceIndex(parent.Evidence)
	parent.Context = cloneContextIndex(parent.Context)
	parent.OutputContract.Subjects = append([]string(nil), parent.OutputContract.Subjects...)
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
	parent.OutputContract.Subjects = append([]string(nil), parent.OutputContract.Subjects...)
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
		// Only admit evidence that can survive the definition runtime's
		// context validation. A malformed unit must not hitchhike into a
		// context block selected by its content hash or reference alias.
		for _, rawUnit := range block.Evidence {
			unit, ok := canonicalContextEvidenceUnit(rawUnit)
			if !ok {
				continue
			}
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

// WithLiveEvidence adds mid-run parent evidence to the delegation ledger so
// model-visible ev_ handles from earlier tool results can be authorized.
func WithLiveEvidence(ctx context.Context, units []tool.EvidenceUnit) context.Context {
	parent, ok := ParentContextFrom(ctx)
	if !ok {
		return ctx
	}
	parent.Evidence = AddEvidenceUnits(parent.Evidence, units)
	return WithParentContext(ctx, parent)
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
	for key, rawUnit := range source {
		unit, ok := canonicalEvidenceIdentity(rawUnit)
		if !ok {
			continue
		}
		out[key] = unit
	}
	if len(out) == 0 {
		return nil
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
	rawEvidence := block.Evidence
	block.Evidence = nil
	for _, rawUnit := range rawEvidence {
		if unit, ok := canonicalContextEvidenceUnit(rawUnit); ok {
			block.Evidence = append(block.Evidence, unit)
		}
	}
	rawConflicts := block.EvidenceConflicts
	block.EvidenceConflicts = nil
	for _, rawConflict := range rawConflicts {
		if conflict, ok := canonicalContextEvidenceConflict(rawConflict); ok {
			block.EvidenceConflicts = append(block.EvidenceConflicts, conflict)
		}
	}
	return block
}

// canonicalEvidenceIdentity normalizes the identity fields used by the
// evidence ledger without imposing the stricter context-body checks. Tool
// results may use opaque content hashes, but a source and target are required
// before an evidence handle can be authorized.
func canonicalEvidenceIdentity(raw tool.EvidenceUnit) (tool.EvidenceUnit, bool) {
	unit := cloneEvidenceUnit(raw)
	unit.SourceKind = strings.TrimSpace(unit.SourceKind)
	unit.Target = strings.TrimSpace(unit.Target)
	unit.Version = strings.TrimSpace(unit.Version)
	unit.TimeRange = strings.TrimSpace(unit.TimeRange)
	if unit.SourceKind == "" || unit.Target == "" {
		return tool.EvidenceUnit{}, false
	}
	if len(unit.Sections) > 0 {
		seen := make(map[string]struct{}, len(unit.Sections))
		for index, section := range unit.Sections {
			section = strings.TrimSpace(section)
			if section == "" {
				return tool.EvidenceUnit{}, false
			}
			if _, duplicate := seen[section]; duplicate {
				return tool.EvidenceUnit{}, false
			}
			seen[section] = struct{}{}
			unit.Sections[index] = section
		}
	}
	return unit, true
}

// canonicalContextEvidenceUnit returns only evidence units that are safe to
// place in an agent RunRequest.Context. It deliberately drops units whose
// identity or structural metadata cannot be repaired; inventing a source or
// target would make a verifier appear more certain than the evidence allows.
func canonicalContextEvidenceUnit(raw tool.EvidenceUnit) (tool.EvidenceUnit, bool) {
	unit, ok := canonicalEvidenceIdentity(raw)
	if !ok {
		return tool.EvidenceUnit{}, false
	}
	if unit.Coverage.Complete && unit.Coverage.Partial || unit.TokenCost < 0 {
		return tool.EvidenceUnit{}, false
	}
	if unit.ContentHash != "" {
		unit.ContentHash = strings.TrimSpace(unit.ContentHash)
		if len(unit.ContentHash) != sha256.Size*2 {
			return tool.EvidenceUnit{}, false
		}
		if _, err := hex.DecodeString(unit.ContentHash); err != nil {
			return tool.EvidenceUnit{}, false
		}
	}
	return unit, true
}

func canonicalContextEvidenceConflict(
	raw agentapi.EvidenceConflict,
) (agentapi.EvidenceConflict, bool) {
	conflict := raw
	conflict.Identity.SourceKind = strings.TrimSpace(conflict.Identity.SourceKind)
	conflict.Identity.Target = strings.TrimSpace(conflict.Identity.Target)
	conflict.Identity.Section = strings.TrimSpace(conflict.Identity.Section)
	conflict.Identity.Version = strings.TrimSpace(conflict.Identity.Version)
	conflict.Identity.TimeRange = strings.TrimSpace(conflict.Identity.TimeRange)
	if conflict.Identity.SourceKind == "" || conflict.Identity.Target == "" {
		return agentapi.EvidenceConflict{}, false
	}
	current, currentOK := canonicalContextEvidenceUnit(conflict.Current)
	incoming, incomingOK := canonicalContextEvidenceUnit(conflict.Incoming)
	if !currentOK || !incomingOK ||
		!contextEvidenceIdentityMatches(conflict.Identity, current) ||
		!contextEvidenceIdentityMatches(conflict.Identity, incoming) {
		return agentapi.EvidenceConflict{}, false
	}
	conflict.Current = current
	conflict.Incoming = incoming
	return conflict, true
}

func contextEvidenceIdentityMatches(
	identity agentapi.EvidenceIdentity,
	unit tool.EvidenceUnit,
) bool {
	if identity.SourceKind != unit.SourceKind ||
		identity.Target != unit.Target ||
		identity.Version != unit.Version ||
		identity.TimeRange != unit.TimeRange {
		return false
	}
	if identity.Section == "" {
		return len(unit.Sections) == 0
	}
	return len(unit.Sections) == 1 && unit.Sections[0] == identity.Section
}

func cloneEvidenceUnit(unit tool.EvidenceUnit) tool.EvidenceUnit {
	unit.Sections = append([]string(nil), unit.Sections...)
	unit.Facets = append([]string(nil), unit.Facets...)
	return unit
}

func evidenceAliases(raw tool.EvidenceUnit) []string {
	unit, ok := canonicalEvidenceIdentity(raw)
	if !ok {
		return nil
	}
	aliases := make([]string, 0, 4)
	if value := strings.TrimSpace(unit.ContentHash); value != "" {
		aliases = append(aliases, value)
	}
	if value := strings.TrimSpace(unit.Target); value != "" {
		aliases = append(aliases, value)
		if source := strings.TrimSpace(unit.SourceKind); source != "" {
			aliases = append(aliases, source+":"+value)
		}
	}
	for _, expanded := range evidence.Expand([]tool.EvidenceUnit{unit}) {
		if handle, ok := evidence.UnitHandle(expanded); ok {
			aliases = append(aliases, handle)
		}
	}
	return aliases
}

func addAlias(aliases map[string]struct{}, value string) {
	if value = strings.TrimSpace(value); value != "" {
		aliases[value] = struct{}{}
	}
}

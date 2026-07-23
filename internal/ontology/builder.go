package ontology

import (
	"fmt"
	"sort"
)

type builder struct {
	entities    []Entity
	entityIndex map[string]int
	facts       []Fact
	factIndex   map[string]int
	err         error
}

func newBuilder() *builder {
	return &builder{
		entityIndex: make(map[string]int),
		factIndex:   make(map[string]int),
	}
}

func (build *builder) addEntity(entity Entity) {
	if build.err != nil {
		return
	}
	if index, exists := build.entityIndex[entity.ID]; exists {
		current := &build.entities[index]
		if current.Class != entity.Class || current.Key != entity.Key || current.Name != entity.Name {
			build.err = fmt.Errorf("conflicting ontology entity %q", entity.ID)
			return
		}
		current.Confidence = max(current.Confidence, entity.Confidence)
		current.Aliases = mergeStrings(current.Aliases, entity.Aliases)
		return
	}
	entity.Properties = nonNilMap(entity.Properties)
	entity.Aliases = mergeStrings(nil, entity.Aliases)
	build.entityIndex[entity.ID] = len(build.entities)
	build.entities = append(build.entities, entity)
}

func (build *builder) addFact(fact Fact) {
	if build.err != nil {
		return
	}
	fact.Qualifiers = nonNilMap(fact.Qualifiers)
	fact.ID = FactID(fact.SubjectID, fact.Predicate, fact.ObjectID, fact.Qualifiers)
	if index, exists := build.factIndex[fact.ID]; exists {
		current := &build.facts[index]
		current.Confidence = max(current.Confidence, fact.Confidence)
		current.Evidence = mergeEvidence(current.Evidence, fact.Evidence)
		return
	}
	fact.Evidence = mergeEvidence(nil, fact.Evidence)
	build.factIndex[fact.ID] = len(build.facts)
	build.facts = append(build.facts, fact)
}

func (build *builder) snapshot() (Snapshot, error) {
	if build.err != nil {
		return Snapshot{}, build.err
	}
	sort.Slice(build.entities, func(i, j int) bool { return build.entities[i].ID < build.entities[j].ID })
	sort.Slice(build.facts, func(i, j int) bool { return build.facts[i].ID < build.facts[j].ID })
	return Snapshot{SchemaVersion: CurrentSchemaVersion, Entities: build.entities, Facts: build.facts}, nil
}

func mergeStrings(current, additions []string) []string {
	seen := make(map[string]struct{}, len(current)+len(additions))
	out := make([]string, 0, len(current)+len(additions))
	for _, values := range [][]string{current, additions} {
		for _, value := range values {
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func mergeEvidence(current, additions []Evidence) []Evidence {
	seen := make(map[string]struct{}, len(current)+len(additions))
	out := make([]Evidence, 0, len(current)+len(additions))
	for _, values := range [][]Evidence{current, additions} {
		for _, evidence := range values {
			key := fmt.Sprintf("%s\x00%d\x00%s\x00%s", evidence.Path, evidence.Line, evidence.Symbol, evidence.Source)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, evidence)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func nonNilMap(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return values
}

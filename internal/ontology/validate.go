package ontology

import (
	"fmt"
	"math"
	"path"
	"strings"
)

func ValidateSnapshot(snapshot Snapshot) error {
	if snapshot.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported ontology schema version %d", snapshot.SchemaVersion)
	}
	entities := make(map[string]Entity, len(snapshot.Entities))
	canonical := make(map[string]string, len(snapshot.Entities))
	for _, entity := range snapshot.Entities {
		if err := validateEntity(entity); err != nil {
			return err
		}
		if _, exists := entities[entity.ID]; exists {
			return fmt.Errorf("duplicate ontology entity %q", entity.ID)
		}
		key := string(entity.Class) + "\x00" + entity.Key
		if previous, exists := canonical[key]; exists {
			return fmt.Errorf("duplicate canonical entity %q and %q", previous, entity.ID)
		}
		entities[entity.ID] = entity
		canonical[key] = entity.ID
	}

	facts := make(map[string]struct{}, len(snapshot.Facts))
	for _, fact := range snapshot.Facts {
		if _, exists := facts[fact.ID]; exists {
			return fmt.Errorf("duplicate ontology fact %q", fact.ID)
		}
		if err := validateFact(fact, entities); err != nil {
			return err
		}
		facts[fact.ID] = struct{}{}
	}
	return nil
}

func validateEntity(entity Entity) error {
	definition, ok := classSchema[entity.Class]
	if !ok {
		return fmt.Errorf("unregistered ontology class %q", entity.Class)
	}
	if entity.ID == "" || entity.Key == "" || entity.Name == "" {
		return fmt.Errorf("incomplete %s entity %q", entity.Class, entity.ID)
	}
	if expected := definition.ID(entity.Key); expected == "" || entity.ID != expected {
		return fmt.Errorf("invalid %s entity ID %q", entity.Class, entity.ID)
	}
	if err := validateConfidence(entity.Confidence); err != nil {
		return fmt.Errorf("entity %q: %w", entity.ID, err)
	}
	for property := range entity.Properties {
		if _, ok := definition.Properties[property]; !ok {
			return fmt.Errorf("entity %q has unsupported property %q", entity.ID, property)
		}
	}
	seenAliases := make(map[string]struct{}, len(entity.Aliases))
	for _, alias := range entity.Aliases {
		if alias == "" || alias != strings.ToLower(alias) || strings.TrimSpace(alias) != alias {
			return fmt.Errorf("entity %q has non-canonical alias %q", entity.ID, alias)
		}
		if _, exists := seenAliases[alias]; exists {
			return fmt.Errorf("entity %q has duplicate alias %q", entity.ID, alias)
		}
		seenAliases[alias] = struct{}{}
	}
	return nil
}

func validateFact(fact Fact, entities map[string]Entity) error {
	definition, ok := relationSchema[fact.Predicate]
	if !ok {
		return fmt.Errorf("unregistered ontology predicate %q", fact.Predicate)
	}
	subject, subjectOK := entities[fact.SubjectID]
	object, objectOK := entities[fact.ObjectID]
	if !subjectOK || !objectOK {
		return fmt.Errorf("fact %q references missing subject or object", fact.ID)
	}
	if _, ok := definition.SubjectClasses[subject.Class]; !ok {
		return fmt.Errorf("predicate %q does not allow subject class %q", fact.Predicate, subject.Class)
	}
	if _, ok := definition.ObjectClasses[object.Class]; !ok {
		return fmt.Errorf("predicate %q does not allow object class %q", fact.Predicate, object.Class)
	}
	for qualifier := range fact.Qualifiers {
		if _, ok := definition.Qualifiers[qualifier]; !ok {
			return fmt.Errorf("fact %q has unsupported qualifier %q", fact.ID, qualifier)
		}
	}
	if expected := FactID(fact.SubjectID, fact.Predicate, fact.ObjectID, fact.Qualifiers); fact.ID != expected {
		return fmt.Errorf("invalid ontology fact ID %q", fact.ID)
	}
	if err := validateConfidence(fact.Confidence); err != nil {
		return fmt.Errorf("fact %q: %w", fact.ID, err)
	}
	seenEvidence := make(map[string]struct{}, len(fact.Evidence))
	for _, evidence := range fact.Evidence {
		if err := validateEvidence(evidence); err != nil {
			return fmt.Errorf("fact %q: %w", fact.ID, err)
		}
		key := strings.Join([]string{evidence.Path, fmt.Sprint(evidence.Line), evidence.Symbol, string(evidence.Source)}, "\x00")
		if _, exists := seenEvidence[key]; exists {
			return fmt.Errorf("fact %q has duplicate evidence", fact.ID)
		}
		seenEvidence[key] = struct{}{}
	}
	return nil
}

func validateConfidence(confidence float64) error {
	if math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence < 0 || confidence > 1 {
		return fmt.Errorf("confidence %.3f is outside [0,1]", confidence)
	}
	return nil
}

func validateEvidence(evidence Evidence) error {
	if evidence.Path == "" || evidence.Path != path.Clean(evidence.Path) || path.IsAbs(evidence.Path) || strings.Contains(evidence.Path, "\\") {
		return fmt.Errorf("non-canonical evidence path %q", evidence.Path)
	}
	if evidence.Line < 0 {
		return fmt.Errorf("negative evidence line %d", evidence.Line)
	}
	if evidence.Source != EvidenceSourceDoc &&
		evidence.Source != EvidenceSourceCodeScan &&
		evidence.Source != EvidenceSourceConfig {
		return fmt.Errorf("unsupported evidence source %q", evidence.Source)
	}
	return nil
}

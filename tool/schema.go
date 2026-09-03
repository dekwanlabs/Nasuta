package tool

import (
	"fmt"
	"math"
	"strings"
)

func validateTool(candidate Tool) error {
	id := strings.TrimSpace(string(candidate.ID))
	if id == "" {
		return fmt.Errorf("tool id is required")
	}
	if id != string(candidate.ID) {
		return fmt.Errorf("tool id %q must be canonical", candidate.ID)
	}
	if strings.TrimSpace(candidate.Description) == "" {
		return fmt.Errorf("tool %q description is required", candidate.ID)
	}
	if candidate.Kind != KindRead && candidate.Kind != KindWrite {
		return fmt.Errorf("tool %q has invalid kind %q", candidate.ID, candidate.Kind)
	}
	if candidate.Handler == nil {
		return fmt.Errorf("tool %q handler is required", candidate.ID)
	}
	if candidate.InputSchema == nil {
		return fmt.Errorf("tool %q input schema is required", candidate.ID)
	}
	if err := validateSchema(candidate.InputSchema, "input"); err != nil {
		return fmt.Errorf("tool %q schema: %w", candidate.ID, err)
	}
	if err := validateReferenceInputs(candidate); err != nil {
		return err
	}
	if err := validatePrefetch(candidate); err != nil {
		return err
	}
	if err := validateRouting(candidate); err != nil {
		return err
	}
	return nil
}

func validateReferenceInputs(candidate Tool) error {
	for _, input := range candidate.ReferenceInputs {
		if strings.TrimSpace(input.Argument) == "" || input.Argument != strings.TrimSpace(input.Argument) {
			return fmt.Errorf("tool %q reference argument %q must be canonical", candidate.ID, input.Argument)
		}
		if len(input.Accepts) == 0 {
			return fmt.Errorf("tool %q reference argument %q requires accepted types", candidate.ID, input.Argument)
		}
		for _, accepted := range input.Accepts {
			switch accepted {
			case ReferenceRunbook, ReferenceService, ReferenceSymbol:
			default:
				return fmt.Errorf("tool %q reference argument %q has invalid type %q", candidate.ID, input.Argument, accepted)
			}
		}
	}
	return nil
}

func validatePrefetch(candidate Tool) error {
	if candidate.Prefetch == nil {
		return nil
	}
	if candidate.Kind != KindRead {
		return fmt.Errorf("tool %q prefetch requires read kind", candidate.ID)
	}
	if strings.TrimSpace(candidate.Prefetch.Description) == "" {
		return fmt.Errorf("tool %q prefetch description is required", candidate.ID)
	}
	if candidate.Prefetch.Timeout < 0 {
		return fmt.Errorf("tool %q prefetch timeout cannot be negative", candidate.ID)
	}
	return nil
}

func validateRouting(candidate Tool) error {
	if candidate.Routing == nil {
		return nil
	}
	if candidate.Kind != KindRead {
		return fmt.Errorf("tool %q routing requires read kind", candidate.ID)
	}
	if strings.TrimSpace(candidate.Routing.Intent) == "" {
		return fmt.Errorf("tool %q routing intent is required", candidate.ID)
	}
	switch candidate.Routing.EvidenceSource {
	case "", RoutingEvidenceInternal, RoutingEvidenceMemory,
		RoutingEvidenceWeb, RoutingEvidenceRuntime:
	default:
		return fmt.Errorf(
			"tool %q routing evidence source %q is invalid",
			candidate.ID,
			candidate.Routing.EvidenceSource,
		)
	}
	return nil
}

func validateSchema(schema map[string]any, path string) error {
	typ := schemaType(schema)
	if typ == "" {
		return validateSchemaAlternatives(schema, path)
	}
	switch typ {
	case TypeObject:
		return validateSchemaObject(schema, path)
	case TypeArray:
		return validateSchemaArray(schema, path)
	case TypeString, TypeInt, TypeNumber, TypeBool:
	default:
		return fmt.Errorf("%s has unsupported type %q", path, typ)
	}
	return nil
}

func validateSchemaObject(schema map[string]any, path string) error {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s properties must be an object", path)
	}
	if err := validateSchemaProperties(properties, path); err != nil {
		return err
	}
	return validateSchemaRequired(schema, properties, path)
}

func validateSchemaProperties(properties map[string]any, path string) error {
	for name, raw := range properties {
		child, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.%s must be a schema object", path, name)
		}
		if err := validateSchema(child, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func validateSchemaRequired(schema map[string]any, properties map[string]any, path string) error {
	required, ok := schema["required"]
	if !ok {
		return nil
	}
	items, err := schemaRequiredNames(required, path)
	if err != nil {
		return err
	}
	for _, name := range items {
		if _, ok := properties[name]; !ok {
			return fmt.Errorf("%s required property %q is undefined", path, name)
		}
	}
	return nil
}

func schemaRequiredNames(required any, path string) ([]string, error) {
	if items, ok := required.([]string); ok {
		return items, nil
	}
	rawItems, ok := required.([]any)
	if !ok {
		return nil, fmt.Errorf("%s required must be an array", path)
	}
	items := make([]string, 0, len(rawItems))
	for _, raw := range rawItems {
		name, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s required entries must be strings", path)
		}
		items = append(items, name)
	}
	return items, nil
}

func validateSchemaArray(schema map[string]any, path string) error {
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s items must be a schema object", path)
	}
	return validateSchema(items, path+"[]")
}

func validateSchemaAlternatives(schema map[string]any, path string) error {
	for _, key := range []string{"oneOf", "anyOf"} {
		raw, exists := schema[key]
		if !exists {
			continue
		}
		alternatives, ok := raw.([]any)
		if !ok || len(alternatives) == 0 {
			return fmt.Errorf("%s %s must be a non-empty array", path, key)
		}
		for i, alternative := range alternatives {
			child, ok := alternative.(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s[%d] must be a schema object", path, key, i)
			}
			if err := validateSchema(child, fmt.Sprintf("%s.%s[%d]", path, key, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateArguments(schema map[string]any, value any, path string) error {
	typ := schemaType(schema)
	if typ == "" {
		return validateArgumentAlternatives(schema, value, path)
	}
	if err := validateArgumentsByType(typ, schema, value, path); err != nil {
		return err
	}
	return validateArgumentConstraints(schema, value, path)
}

func validateArgumentsByType(typ SchemaType, schema map[string]any, value any, path string) error {
	switch typ {
	case TypeObject:
		return validateArgumentObject(schema, value, path)
	case TypeArray:
		return validateArgumentArray(schema, value, path)
	case TypeString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case TypeInt:
		return validateArgumentInt(value, path)
	case TypeNumber:
		return validateArgumentNumber(value, path)
	case TypeBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	}
	return nil
}

func validateArgumentObject(schema map[string]any, value any, path string) error {
	object, ok := value.(map[string]any)
	if !ok {
		if args, argsOK := value.(Arguments); argsOK {
			object = map[string]any(args)
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("%s must be an object", path)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range requiredNames(schema["required"]) {
		if _, exists := object[name]; !exists {
			return fmt.Errorf("%s.%s is required", path, name)
		}
	}
	for name, raw := range object {
		property, exists := properties[name]
		if !exists {
			if additional, configured := schema["additionalProperties"].(bool); configured && !additional {
				return fmt.Errorf("%s.%s is not allowed", path, name)
			}
			continue
		}
		child, _ := property.(map[string]any)
		if err := validateArguments(child, raw, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func validateArgumentArray(schema map[string]any, value any, path string) error {
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array", path)
	}
	if minimum, ok := schemaInt(schema["minItems"]); ok && len(items) < minimum {
		return fmt.Errorf("%s must contain at least %d items", path, minimum)
	}
	if maximum, ok := schemaInt(schema["maxItems"]); ok && len(items) > maximum {
		return fmt.Errorf("%s must contain at most %d items", path, maximum)
	}
	itemSchema, _ := schema["items"].(map[string]any)
	for i, item := range items {
		if err := validateArguments(itemSchema, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func validateArgumentInt(value any, path string) error {
	switch number := value.(type) {
	case int:
	case float64:
		if math.Trunc(number) != number {
			return fmt.Errorf("%s must be an integer", path)
		}
	default:
		return fmt.Errorf("%s must be an integer", path)
	}
	return nil
}

func validateArgumentNumber(value any, path string) error {
	switch value.(type) {
	case int, float64:
		return nil
	default:
		return fmt.Errorf("%s must be a number", path)
	}
}

func validateArgumentConstraints(schema map[string]any, value any, path string) error {
	if number, ok := schemaNumber(value); ok {
		if minimum, exists := schemaNumber(schema["minimum"]); exists && number < minimum {
			return fmt.Errorf("%s must be at least %v", path, schema["minimum"])
		}
		if maximum, exists := schemaNumber(schema["maximum"]); exists && number > maximum {
			return fmt.Errorf("%s must be at most %v", path, schema["maximum"])
		}
	}
	if enum, ok := schema["enum"].([]any); ok && !enumContains(enum, value) {
		return fmt.Errorf("%s has an unsupported value", path)
	}
	if expected, ok := schema["const"]; ok && fmt.Sprint(expected) != fmt.Sprint(value) {
		return fmt.Errorf("%s must equal %v", path, expected)
	}
	return nil
}

func validateArgumentAlternatives(schema map[string]any, value any, path string) error {
	for _, keyword := range []string{"oneOf", "anyOf"} {
		raw, exists := schema[keyword]
		if !exists {
			continue
		}
		alternatives, _ := raw.([]any)
		matches := 0
		for _, alternative := range alternatives {
			child, _ := alternative.(map[string]any)
			if validateArguments(child, value, path) == nil {
				matches++
			}
		}
		if (keyword == "oneOf" && matches != 1) || (keyword == "anyOf" && matches == 0) {
			return fmt.Errorf("%s does not match %s", path, keyword)
		}
		return nil
	}
	return nil
}

func schemaInt(value any) (int, bool) {
	number, ok := schemaNumber(value)
	return int(number), ok && math.Trunc(number) == number
}

func schemaNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}

func schemaType(schema map[string]any) SchemaType {
	switch value := schema["type"].(type) {
	case SchemaType:
		return value
	case string:
		return SchemaType(value)
	default:
		return ""
	}
}

func requiredNames(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if name, ok := value.(string); ok {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

func enumContains(enum []any, value any) bool {
	for _, candidate := range enum {
		if fmt.Sprint(candidate) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

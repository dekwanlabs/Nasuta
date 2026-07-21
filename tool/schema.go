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
	if candidate.Prefetch != nil {
		if candidate.Kind != KindRead {
			return fmt.Errorf("tool %q prefetch requires read kind", candidate.ID)
		}
		if strings.TrimSpace(candidate.Prefetch.Description) == "" {
			return fmt.Errorf("tool %q prefetch description is required", candidate.ID)
		}
		if candidate.Prefetch.Timeout < 0 {
			return fmt.Errorf("tool %q prefetch timeout cannot be negative", candidate.ID)
		}
	}
	if candidate.Routing != nil {
		if candidate.Kind != KindRead {
			return fmt.Errorf("tool %q routing requires read kind", candidate.ID)
		}
		if strings.TrimSpace(candidate.Routing.Intent) == "" {
			return fmt.Errorf("tool %q routing intent is required", candidate.ID)
		}
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
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("%s properties must be an object", path)
		}
		for name, raw := range properties {
			child, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s must be a schema object", path, name)
			}
			if err := validateSchema(child, path+"."+name); err != nil {
				return err
			}
		}
		if required, ok := schema["required"]; ok {
			items, ok := required.([]string)
			if !ok {
				rawItems, rawOK := required.([]any)
				if !rawOK {
					return fmt.Errorf("%s required must be an array", path)
				}
				items = make([]string, 0, len(rawItems))
				for _, raw := range rawItems {
					name, ok := raw.(string)
					if !ok {
						return fmt.Errorf("%s required entries must be strings", path)
					}
					items = append(items, name)
				}
			}
			for _, name := range items {
				if _, ok := properties[name]; !ok {
					return fmt.Errorf("%s required property %q is undefined", path, name)
				}
			}
		}
	case TypeArray:
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return fmt.Errorf("%s items must be a schema object", path)
		}
		return validateSchema(items, path+"[]")
	case TypeString, TypeInt, TypeNumber, TypeBool:
	default:
		return fmt.Errorf("%s has unsupported type %q", path, typ)
	}
	return nil
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
	switch typ {
	case TypeObject:
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
	case TypeArray:
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		itemSchema, _ := schema["items"].(map[string]any)
		for i, item := range items {
			if err := validateArguments(itemSchema, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case TypeString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case TypeInt:
		switch number := value.(type) {
		case int:
		case float64:
			if math.Trunc(number) != number {
				return fmt.Errorf("%s must be an integer", path)
			}
		default:
			return fmt.Errorf("%s must be an integer", path)
		}
	case TypeNumber:
		switch value.(type) {
		case int, float64:
		default:
			return fmt.Errorf("%s must be a number", path)
		}
	case TypeBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	}
	if enum, ok := schema["enum"].([]any); ok && !enumContains(enum, value) {
		return fmt.Errorf("%s has an unsupported value", path)
	}
	return nil
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

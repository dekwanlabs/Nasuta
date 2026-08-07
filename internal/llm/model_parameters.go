package llm

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// ModelParameters is the provider-validated subset of definition parameters.
// Its fields are typed so execution cannot silently drop an accepted value.
type ModelParameters struct {
	Temperature      *float64
	TopP             *float64
	Stop             []string
	FrequencyPenalty *float64
	PresencePenalty  *float64
	TopK             *int
}

// PrepareModelParameters validates provider-specific request options and
// returns the canonical snapshot representation used by a Run.
func PrepareModelParameters(provider string, raw map[string]any) (ModelParameters, error) {
	provider = normalizeProvider(provider)
	if provider != "openai" && provider != "anthropic" {
		return ModelParameters{}, fmt.Errorf("unsupported LLM provider %q", provider)
	}

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var prepared ModelParameters
	for _, key := range keys {
		value := raw[key]
		switch key {
		case "temperature":
			number, err := parameterNumber(key, value)
			if err != nil {
				return ModelParameters{}, err
			}
			maximum := 1.0
			if provider == "openai" {
				maximum = 2
			}
			if number < 0 || number > maximum {
				return ModelParameters{}, fmt.Errorf(
					"model parameter %q must be between 0 and %g for %s",
					key, maximum, provider,
				)
			}
			prepared.Temperature = &number
		case "top_p":
			number, err := parameterNumber(key, value)
			if err != nil {
				return ModelParameters{}, err
			}
			if number < 0 || number > 1 {
				return ModelParameters{}, fmt.Errorf(
					"model parameter %q must be between 0 and 1", key,
				)
			}
			prepared.TopP = &number
		case "stop":
			stop, err := parameterStops(key, value)
			if err != nil {
				return ModelParameters{}, err
			}
			prepared.Stop = stop
		case "frequency_penalty", "presence_penalty":
			if provider != "openai" {
				return ModelParameters{}, fmt.Errorf(
					"model parameter %q is not supported by %s",
					key, provider,
				)
			}
			number, err := parameterNumber(key, value)
			if err != nil {
				return ModelParameters{}, err
			}
			if number < -2 || number > 2 {
				return ModelParameters{}, fmt.Errorf(
					"model parameter %q must be between -2 and 2", key,
				)
			}
			if key == "frequency_penalty" {
				prepared.FrequencyPenalty = &number
			} else {
				prepared.PresencePenalty = &number
			}
		case "top_k":
			if provider != "anthropic" {
				return ModelParameters{}, fmt.Errorf(
					"model parameter %q is not supported by %s",
					key, provider,
				)
			}
			number, err := parameterNumber(key, value)
			if err != nil {
				return ModelParameters{}, err
			}
			if number < 0 || math.Trunc(number) != number ||
				number > float64(maxInt()) {
				return ModelParameters{}, fmt.Errorf(
					"model parameter %q must be a non-negative integer", key,
				)
			}
			topK := int(number)
			prepared.TopK = &topK
		default:
			return ModelParameters{}, fmt.Errorf(
				"model parameter %q is not supported by %s", key, provider,
			)
		}
	}
	prepared.Stop = append([]string(nil), prepared.Stop...)
	return prepared, nil
}

// Snapshot returns a detached, canonical map for RunSnapshot persistence.
func (parameters ModelParameters) Snapshot() map[string]any {
	snapshot := make(map[string]any, 6)
	if parameters.Temperature != nil {
		snapshot["temperature"] = *parameters.Temperature
	}
	if parameters.TopP != nil {
		snapshot["top_p"] = *parameters.TopP
	}
	if len(parameters.Stop) > 0 {
		snapshot["stop"] = append([]string(nil), parameters.Stop...)
	}
	if parameters.FrequencyPenalty != nil {
		snapshot["frequency_penalty"] = *parameters.FrequencyPenalty
	}
	if parameters.PresencePenalty != nil {
		snapshot["presence_penalty"] = *parameters.PresencePenalty
	}
	if parameters.TopK != nil {
		snapshot["top_k"] = *parameters.TopK
	}
	if len(snapshot) == 0 {
		return nil
	}
	return snapshot
}

func normalizeProvider(provider string) string {
	switch provider {
	case "openai", "anthropic":
		return provider
	default:
		return provider
	}
}

func parameterNumber(key string, value any) (float64, error) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int8:
		number = float64(typed)
	case int16:
		number = float64(typed)
	case int32:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case uint:
		number = float64(typed)
	case uint8:
		number = float64(typed)
	case uint16:
		number = float64(typed)
	case uint32:
		number = float64(typed)
	case uint64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("model parameter %q must be numeric: %w", key, err)
		}
		number = parsed
	default:
		return 0, fmt.Errorf("model parameter %q must be numeric", key)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, fmt.Errorf("model parameter %q must be finite", key)
	}
	return number, nil
}

func parameterStops(key string, value any) ([]string, error) {
	var stops []string
	switch typed := value.(type) {
	case string:
		stops = []string{typed}
	case []string:
		stops = append(stops, typed...)
	case []any:
		stops = make([]string, 0, len(typed))
		for index, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf(
					"model parameter %q item %d must be a string", key, index,
				)
			}
			stops = append(stops, text)
		}
	default:
		return nil, fmt.Errorf(
			"model parameter %q must be a string or string array", key,
		)
	}
	if len(stops) == 0 || len(stops) > 4 {
		return nil, fmt.Errorf(
			"model parameter %q must contain between 1 and 4 stop sequences", key,
		)
	}
	for index, stop := range stops {
		if stop == "" {
			return nil, fmt.Errorf(
				"model parameter %q item %d must not be empty", key, index,
			)
		}
	}
	return stops, nil
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

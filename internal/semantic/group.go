package semantic

import "fmt"

// DeduplicateHits keeps the first hit for each metadata group, preserving the
// input order and returning at most limit hits. Callers should pass a positive
// limit, as required by Query validation.
func DeduplicateHits(hits []Hit, field string, limit int) []Hit {
	seen := make(map[string]struct{}, min(len(hits), limit))
	out := make([]Hit, 0, min(len(hits), limit))
	for _, hit := range hits {
		group := fmt.Sprint(hit.Metadata[field])
		if _, exists := seen[group]; exists {
			continue
		}
		seen[group] = struct{}{}
		out = append(out, hit)
		if len(out) == limit {
			break
		}
	}
	return out
}

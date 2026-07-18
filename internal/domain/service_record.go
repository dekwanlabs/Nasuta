package types

import (
	"github.com/dekwanlabs/astris/platform"
)

// MergeServices folds raw per-source records into one canonical service module.
// Doc records contribute business metadata, while code records contribute hard evidence.
// The merge is order-independent.
func MergeServices(all []ServiceRecord) []ServiceRecord {
	acc := map[string]*ServiceRecord{}
	count := map[string]int{}
	order := []string{}
	for i := range all {
		r := all[i]
		key := r.ServiceName
		if r.Repo != "" && r.ModulePath != "" {
			key = r.Repo + "\x00" + r.ModulePath
		}
		count[key]++
		cur, ok := acc[key]
		if !ok {
			cp := r
			acc[key] = &cp
			order = append(order, key)
			continue
		}
		mergeServiceInto(cur, r)
	}
	out := make([]ServiceRecord, 0, len(order))
	for _, key := range order {
		s := acc[key]
		if count[key] > 1 && s.Confidence < 0.95 {
			s.Confidence = 0.95
		}
		out = append(out, *s)
	}
	return out
}

func mergeServiceInto(dst *ServiceRecord, src ServiceRecord) {
	dst.Owner = platform.FirstNonEmpty(dst.Owner, src.Owner)
	dst.Status = platform.FirstNonEmpty(dst.Status, src.Status)
	dst.Summary = platform.FirstNonEmpty(dst.Summary, src.Summary)
	dst.Layer = platform.FirstNonEmpty(dst.Layer, src.Layer)
	dst.Scope = platform.FirstNonEmpty(dst.Scope, src.Scope)
	dst.ModulePath = platform.FirstNonEmpty(dst.ModulePath, src.ModulePath)
	dst.Runtime = platform.FirstNonEmpty(dst.Runtime, src.Runtime)
	if dst.Language == "" || dst.Language == "unknown" {
		dst.Language = src.Language
	}
	dst.Tags = platform.Dedupe(append(dst.Tags, src.Tags...))
	dst.Docs = platform.Dedupe(append(dst.Docs, src.Docs...))
	dst.SourceOfTruth = platform.Dedupe(append(dst.SourceOfTruth, src.SourceOfTruth...))
	dst.Entrypoints = append(dst.Entrypoints, src.Entrypoints...)
	dst.Ports = platform.Dedupe(append(dst.Ports, src.Ports...))
	dst.Confidence = max(dst.Confidence, src.Confidence)
}

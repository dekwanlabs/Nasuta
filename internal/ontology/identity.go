package ontology

import (
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/platform"
)

func RepositoryID(repo string) string {
	return platform.UUIDFromString("repository\x00" + repo)
}

func ServiceID(serviceKey string) string { return serviceKey }

func APIEndpointID(serviceKey, method, path string) string {
	return platform.UUIDFromString(strings.Join([]string{"api", serviceKey, method, path}, "\x00"))
}

func CodeSymbolID(repo, file, qualifiedName string) string {
	return platform.UUIDFromString(strings.Join([]string{"code_symbol", repo, file, qualifiedName}, "\x00"))
}

func ExternalSystemID(target string) string {
	return platform.UUIDFromString("external_system\x00" + target)
}

func RunbookID(repo, key string) string {
	return platform.UUIDFromString(strings.Join([]string{"runbook", repo, key}, "\x00"))
}

func FactID(subjectID string, predicate Predicate, objectID string, qualifiers map[string]string) string {
	return platform.UUIDFromString(strings.Join([]string{
		"fact", subjectID, string(predicate), objectID, canonicalMap(qualifiers),
	}, "\x00"))
}

func canonicalMap(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		out.WriteString(key)
		out.WriteByte('=')
		out.WriteString(values[key])
		out.WriteByte('\x00')
	}
	return out.String()
}

func compoundID2(build func(string, string) string) func(string) string {
	return func(key string) string {
		values := strings.Split(key, "\x00")
		if len(values) != 2 {
			return ""
		}
		return build(values[0], values[1])
	}
}

func compoundID3(build func(string, string, string) string) func(string) string {
	return func(key string) string {
		values := strings.Split(key, "\x00")
		if len(values) != 3 {
			return ""
		}
		return build(values[0], values[1], values[2])
	}
}

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

func expectedEntityID(entity Entity) string {
	switch entity.Class {
	case ClassRepository:
		return RepositoryID(entity.Key)
	case ClassService:
		return ServiceID(entity.Key)
	case ClassAPIEndpoint:
		parts := strings.Split(entity.Key, "\x00")
		if len(parts) != 3 {
			return ""
		}
		return APIEndpointID(parts[0], parts[1], parts[2])
	case ClassCodeSymbol:
		parts := strings.Split(entity.Key, "\x00")
		if len(parts) != 3 {
			return ""
		}
		return CodeSymbolID(parts[0], parts[1], parts[2])
	case ClassExternalSystem:
		return ExternalSystemID(entity.Key)
	case ClassRunbook:
		parts := strings.Split(entity.Key, "\x00")
		if len(parts) != 2 {
			return ""
		}
		return RunbookID(parts[0], parts[1])
	default:
		return ""
	}
}

package indexer

import (
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform"
)

// serviceIdentity is the stable ownership carried through scanner output.
// Names remain display labels; repo and module path disambiguate duplicates.
type serviceIdentity struct {
	Name       string
	Repo       string
	ModulePath string
	Key        string
}

func serviceIdentityFromRecord(service domain.ServiceRecord) serviceIdentity {
	service = canonicalService(service)
	return serviceIdentity{
		Name:       service.ServiceName,
		Repo:       service.Repo,
		ModulePath: service.ModulePath,
		Key:        platform.UUIDFromString(service.Repo + "\x00" + service.ModulePath),
	}
}

func serviceIdentityForModule(root, moduleRoot, name string) serviceIdentity {
	path := canonicalPath(relativeTo(root, moduleRoot))
	repo, modulePath, ok := splitRepoPath(path)
	if !ok {
		repo = canonicalRepo(topSegment(path))
		modulePath = path
	}
	if modulePath == "" {
		modulePath = "."
	}
	return serviceIdentity{
		Name:       strings.TrimSpace(name),
		Repo:       repo,
		ModulePath: modulePath,
		Key:        platform.UUIDFromString(repo + "\x00" + modulePath),
	}
}

// dependencyIdentity derives caller ownership from the module containing the
// evidence file. It never infers identity from a target name or source path.
func dependencyIdentity(root, file string) serviceIdentity {
	var moduleRoot, name string
	switch filepath.Ext(file) {
	case ".java":
		moduleRoot = findJavaModuleRoot(root, file)
		name = inferJavaServiceName(root, file)
	case ".kt":
		moduleRoot = findKotlinModuleRoot(root, file)
		name = inferKotlinServiceName(root, file)
	case ".py":
		moduleRoot = findPythonDependencyRoot(root, file)
		name = readPythonAppName(moduleRoot)
		if name == "" {
			name = readPythonProjectName(moduleRoot)
		}
	case ".go":
		moduleRoot = findModuleRoot(root, file, "go.mod")
		name = readGoModuleName(moduleRoot)
	case ".cs":
		moduleRoot = findCSharpModuleRoot(root, file)
		name = readCSharpProjectName(moduleRoot)
	case ".js", ".ts", ".mjs", ".cjs":
		moduleRoot = findNodeJSModuleRoot(root, file)
		name = readNodeJSPackageName(moduleRoot)
	case ".swift", ".m", ".mm":
		moduleRoot = findIOSModuleRoot(root, file)
		name = readIOSAppName(moduleRoot)
	}
	if name == "" || name == "." {
		name = "unknown"
	}
	if moduleRoot == "" {
		return serviceIdentity{Name: name}
	}
	return serviceIdentityForModule(root, moduleRoot, name)
}

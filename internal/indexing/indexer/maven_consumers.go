package indexer

import (
	"encoding/xml"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

type mavenCoordinate struct {
	groupID    string
	artifactID string
}

type mavenDependency struct {
	coordinate mavenCoordinate
	scope      string
	optional   bool
}

type mavenModule struct {
	path         string
	coordinate   mavenCoordinate
	dependencies []mavenDependency
}

type mavenProjectXML struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Packaging  string `xml:"packaging"`
	Parent     struct {
		GroupID string `xml:"groupId"`
	} `xml:"parent"`
	Dependencies []struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Scope      string `xml:"scope"`
		Optional   bool   `xml:"optional"`
	} `xml:"dependencies>dependency"`
}

func mavenRuntimeConsumers(
	root string,
	dirs []string,
	services []domain.ServiceRecord,
) map[string][]serviceIdentity {
	modules := scanMavenModules(root, dirs)
	if len(modules) == 0 {
		return nil
	}
	runtimeByPath := make(map[string]serviceIdentity)
	for _, service := range services {
		if service.Runtime == "spring-boot" {
			runtimeByPath[canonicalPath(service.ModulePath)] = serviceIdentityFromRecord(service)
		}
	}

	byCoordinate := make(map[mavenCoordinate][]int, len(modules))
	byArtifact := make(map[string][]int, len(modules))
	for i, module := range modules {
		byCoordinate[module.coordinate] = append(byCoordinate[module.coordinate], i)
		byArtifact[module.coordinate.artifactID] = append(byArtifact[module.coordinate.artifactID], i)
	}

	consumerSets := make(map[string]map[serviceIdentity]struct{})
	for i, module := range modules {
		application := runtimeByPath[module.path]
		if application.Name == "" {
			continue
		}
		visited := map[int]struct{}{i: {}}
		pending := make([]int, 0, len(module.dependencies))
		for _, dependency := range module.dependencies {
			if !mavenDependencyIsReachable(dependency, true) {
				continue
			}
			pending = append(pending, localMavenModules(dependency.coordinate, byCoordinate, byArtifact)...)
		}
		for len(pending) > 0 {
			current := pending[0]
			pending = pending[1:]
			if _, seen := visited[current]; seen {
				continue
			}
			visited[current] = struct{}{}
			dependencyModule := modules[current]
			applications := consumerSets[dependencyModule.path]
			if applications == nil {
				applications = make(map[serviceIdentity]struct{})
				consumerSets[dependencyModule.path] = applications
			}
			applications[application] = struct{}{}
			for _, dependency := range dependencyModule.dependencies {
				if !mavenDependencyIsReachable(dependency, false) {
					continue
				}
				pending = append(pending, localMavenModules(dependency.coordinate, byCoordinate, byArtifact)...)
			}
		}
	}

	consumers := make(map[string][]serviceIdentity, len(consumerSets))
	for modulePath, applications := range consumerSets {
		ordered := make([]serviceIdentity, 0, len(applications))
		for application := range applications {
			ordered = append(ordered, application)
		}
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].Name != ordered[j].Name {
				return ordered[i].Name < ordered[j].Name
			}
			if ordered[i].Repo != ordered[j].Repo {
				return ordered[i].Repo < ordered[j].Repo
			}
			return ordered[i].ModulePath < ordered[j].ModulePath
		})
		consumers[modulePath] = ordered
	}
	return consumers
}

func scanMavenModules(root string, dirs []string) []mavenModule {
	poms := walkFiles(root, dirs, isPom)
	modules := make([]mavenModule, 0, len(poms))
	for _, pom := range poms {
		var project mavenProjectXML
		if xml.Unmarshal([]byte(readFile(pom)), &project) != nil {
			continue
		}
		if strings.TrimSpace(project.Packaging) == "pom" {
			continue
		}
		groupID := strings.TrimSpace(project.GroupID)
		if groupID == "" {
			groupID = strings.TrimSpace(project.Parent.GroupID)
		}
		artifactID := strings.TrimSpace(project.ArtifactID)
		if artifactID == "" {
			continue
		}
		dependencies := make([]mavenDependency, 0, len(project.Dependencies))
		for _, dependency := range project.Dependencies {
			dependencyArtifactID := strings.TrimSpace(dependency.ArtifactID)
			if dependencyArtifactID == "" {
				continue
			}
			dependencies = append(dependencies, mavenDependency{
				coordinate: mavenCoordinate{
					groupID:    strings.TrimSpace(dependency.GroupID),
					artifactID: dependencyArtifactID,
				},
				scope:    strings.TrimSpace(dependency.Scope),
				optional: dependency.Optional,
			})
		}
		modules = append(modules, mavenModule{
			path:         canonicalPath(relativeTo(root, filepath.Dir(pom))),
			coordinate:   mavenCoordinate{groupID: groupID, artifactID: artifactID},
			dependencies: dependencies,
		})
	}
	return modules
}

func localMavenModules(
	coordinate mavenCoordinate,
	byCoordinate map[mavenCoordinate][]int,
	byArtifact map[string][]int,
) []int {
	if matches := byCoordinate[coordinate]; len(matches) > 0 {
		return matches
	}
	matches := byArtifact[coordinate.artifactID]
	if len(matches) == 1 {
		return matches
	}
	return nil
}

func mavenDependencyIsReachable(dependency mavenDependency, direct bool) bool {
	switch dependency.scope {
	case "test", "system", "import":
		return false
	case "provided":
		return direct
	}
	return direct || !dependency.optional
}

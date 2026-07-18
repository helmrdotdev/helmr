package deployment

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type dependencyTopology struct {
	codeLinks       map[string]string
	dependencyLinks map[string]string
	binLinks        map[string]string
	directories     map[string]struct{}
	binRoots        map[string]struct{}
}

type dependencyTopologySource struct {
	manifest                   func(PackageEndpoint) (packageManifest, error)
	requireExecutable          func(PackageEndpoint, string) error
	requireDependencyDirectory func(string) error
}

func deriveDependencyTopology(
	graph PackageGraph,
	source dependencyTopologySource,
) (dependencyTopology, error) {
	if source.manifest == nil ||
		source.requireExecutable == nil ||
		source.requireDependencyDirectory == nil {
		return dependencyTopology{}, fmt.Errorf("dependency topology source is incomplete")
	}

	topology := dependencyTopology{
		codeLinks:       make(map[string]string),
		dependencyLinks: make(map[string]string),
		binLinks:        make(map[string]string),
		directories: map[string]struct{}{
			".":            {},
			".helmr":       {},
			".helmr/views": {},
		},
		binRoots: make(map[string]struct{}),
	}
	localByPath := make(map[string]LocalPackage, len(graph.LocalPackages))
	for _, local := range graph.LocalPackages {
		localByPath[local.Path] = local
	}

	for _, local := range graph.LocalPackages {
		container := "."
		if local.Path != "." {
			container = path.Join(".helmr/views", *local.ViewKey)
			topology.directories[container] = struct{}{}
			link := path.Join(local.Path, "node_modules")
			target := absoluteDependency(container)
			topology.codeLinks[link] = canonicalRelativeTarget(absoluteCode(link), target)
		}
		topology.binRoots[path.Join(container, ".bin")] = struct{}{}
	}
	for _, registry := range graph.RegistryPackages {
		container := path.Join(registry.InstallPath, "node_modules")
		topology.binRoots[path.Join(container, ".bin")] = struct{}{}
		addAncestorDirectories(topology.directories, registry.InstallPath, ".")
	}

	logicalPaths := make(map[string]struct{}, len(graph.Resolutions))
	for _, resolution := range graph.Resolutions {
		container, err := dependencyResolutionContainer(resolution.From, localByPath)
		if err != nil {
			return dependencyTopology{}, err
		}
		logical := path.Join(container, resolution.Dependency)
		if _, exists := logicalPaths[logical]; exists {
			return dependencyTopology{}, fmt.Errorf(
				"multiple graph resolutions derive dependency path %q",
				logical,
			)
		}
		logicalPaths[logical] = struct{}{}
		addAncestorDirectories(topology.directories, logical, container)

		target := dependencyEndpointAbsolute(resolution.To)
		logicalAbsolute := absoluteDependency(logical)
		if logicalAbsolute == target {
			if err := source.requireDependencyDirectory(logical); err != nil {
				return dependencyTopology{}, fmt.Errorf(
					"physical dependency edge %q: %w",
					logical,
					err,
				)
			}
		} else {
			topology.dependencyLinks[logical] = canonicalRelativeTarget(logicalAbsolute, target)
		}
		if err := deriveDependencyBinLinks(&topology, source, container, resolution.To); err != nil {
			return dependencyTopology{}, fmt.Errorf(
				"dependency %q from %q: %w",
				resolution.Dependency,
				container,
				err,
			)
		}
	}
	return topology, nil
}

func dependencyResolutionContainer(
	endpoint PackageEndpoint,
	localByPath map[string]LocalPackage,
) (string, error) {
	switch endpoint.Kind {
	case PackageKindLocal:
		if *endpoint.Path == "." {
			return ".", nil
		}
		local, exists := localByPath[*endpoint.Path]
		if !exists {
			return "", fmt.Errorf("local package %q is absent from the graph", *endpoint.Path)
		}
		return path.Join(".helmr/views", *local.ViewKey), nil
	case PackageKindRegistry:
		return path.Join(*endpoint.InstallPath, "node_modules"), nil
	default:
		return "", fmt.Errorf("package endpoint kind %q is unsupported", endpoint.Kind)
	}
}

func dependencyEndpointAbsolute(endpoint PackageEndpoint) string {
	if endpoint.Kind == PackageKindLocal {
		return absoluteCode(*endpoint.Path)
	}
	return absoluteDependency(*endpoint.InstallPath)
}

func deriveDependencyBinLinks(
	topology *dependencyTopology,
	source dependencyTopologySource,
	container string,
	endpoint PackageEndpoint,
) error {
	manifest, err := source.manifest(endpoint)
	if err != nil {
		return err
	}
	commands := make([]string, 0, len(manifest.Bins))
	for command := range manifest.Bins {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	for _, command := range commands {
		target := manifest.Bins[command]
		linkPath := path.Join(container, ".bin", command)
		if _, exists := topology.binLinks[linkPath]; exists {
			return fmt.Errorf("bin command %q collides in container %q", command, container)
		}
		targetPath := target
		var absoluteTarget string
		if endpoint.Kind == PackageKindLocal {
			targetPath = joinArtifactPath(*endpoint.Path, target)
			absoluteTarget = absoluteCode(targetPath)
		} else {
			targetPath = path.Join(*endpoint.InstallPath, target)
			absoluteTarget = absoluteDependency(targetPath)
		}
		if err := source.requireExecutable(endpoint, targetPath); err != nil {
			return fmt.Errorf("bin target %q: %w", targetPath, err)
		}
		topology.binLinks[linkPath] = canonicalRelativeTarget(
			absoluteDependency(linkPath),
			absoluteTarget,
		)
		topology.directories[path.Join(container, ".bin")] = struct{}{}
		addAncestorDirectories(topology.directories, linkPath, container)
	}
	return nil
}

func (verifier *pairVerifier) deriveTopology() error {
	topology, err := deriveDependencyTopology(
		verifier.graph,
		dependencyTopologySource{
			manifest: func(endpoint PackageEndpoint) (packageManifest, error) {
				if endpoint.Kind == PackageKindLocal {
					manifestPath := "package.json"
					if *endpoint.Path != "." {
						manifestPath = path.Join(*endpoint.Path, "package.json")
					}
					manifest, exists := verifier.codeManifests[manifestPath]
					if !exists {
						return packageManifest{}, fmt.Errorf(
							"local package manifest %q is absent",
							manifestPath,
						)
					}
					return manifest, nil
				}
				manifestPath := path.Join(*endpoint.InstallPath, "package.json")
				manifest, exists := verifier.depManifests[manifestPath]
				if !exists {
					return packageManifest{}, fmt.Errorf(
						"registry package manifest %q is absent",
						manifestPath,
					)
				}
				return manifest, nil
			},
			requireExecutable: func(endpoint PackageEndpoint, targetPath string) error {
				artifact := verifier.deps
				if endpoint.Kind == PackageKindLocal {
					artifact = verifier.code
				}
				entry, err := artifact.require(targetPath, artifactEntryRegular)
				if err != nil {
					return err
				}
				if entry.Mode != 0755 {
					return fmt.Errorf("mode = %#o, want 0755", entry.Mode)
				}
				return nil
			},
			requireDependencyDirectory: func(value string) error {
				_, err := verifier.deps.require(value, artifactEntryDirectory)
				return err
			},
		},
	)
	if err != nil {
		return err
	}
	verifier.codeLinks = topology.codeLinks
	verifier.depLinks = topology.dependencyLinks
	verifier.binLinks = topology.binLinks
	verifier.depDirs = topology.directories
	verifier.binRoots = topology.binRoots
	return nil
}

func addAncestorDirectories(set map[string]struct{}, value, stop string) {
	for parent := path.Dir(value); parent != "." && parent != stop; parent = path.Dir(parent) {
		set[parent] = struct{}{}
	}
	if stop != "." {
		set[stop] = struct{}{}
	}
}

func absoluteCode(value string) string {
	if value == "." {
		return programMountPath
	}
	return programMountPath + "/" + value
}

func absoluteDependency(value string) string {
	if value == "." {
		return dependencyMountPath
	}
	return dependencyMountPath + "/" + value
}

func canonicalRelativeTarget(link, target string) string {
	parent := strings.Split(strings.TrimPrefix(path.Dir(link), "/"), "/")
	targetParts := strings.Split(strings.TrimPrefix(target, "/"), "/")
	common := 0
	for common < len(parent) && common < len(targetParts) && parent[common] == targetParts[common] {
		common++
	}
	result := make([]string, 0, len(parent)-common+len(targetParts)-common)
	for range parent[common:] {
		result = append(result, "..")
	}
	result = append(result, targetParts[common:]...)
	return strings.Join(result, "/")
}

package deployment

import (
	"fmt"
	"io"
	"path"
	"strings"
)

func (verifier *pairVerifier) deriveTopology() error {
	verifier.codeLinks = make(map[string]string)
	verifier.depLinks = make(map[string]string)
	verifier.depDirs = map[string]struct{}{
		".":            {},
		".helmr":       {},
		".helmr/views": {},
	}
	verifier.shims = make(map[string][]byte)
	verifier.binRoots = make(map[string]struct{})

	for _, local := range verifier.graph.LocalPackages {
		container := "."
		if local.Path != "." {
			container = path.Join(".helmr/views", *local.ViewKey)
			verifier.depDirs[container] = struct{}{}
			link := path.Join(local.Path, "node_modules")
			target := absoluteDependency(container)
			verifier.codeLinks[link] = canonicalRelativeTarget(absoluteCode(link), target)
		}
		verifier.binRoots[path.Join(container, ".bin")] = struct{}{}
	}
	for _, registry := range verifier.graph.RegistryPackages {
		container := path.Join(registry.InstallPath, "node_modules")
		verifier.binRoots[path.Join(container, ".bin")] = struct{}{}
		addAncestorDirectories(verifier.depDirs, registry.InstallPath, ".")
	}

	logicalPaths := make(map[string]struct{}, len(verifier.graph.Resolutions))
	for _, resolution := range verifier.graph.Resolutions {
		container := verifier.resolutionContainer(resolution.From)
		logical := path.Join(container, resolution.Dependency)
		if _, exists := logicalPaths[logical]; exists {
			return fmt.Errorf("multiple graph resolutions derive dependency path %q", logical)
		}
		logicalPaths[logical] = struct{}{}
		addAncestorDirectories(verifier.depDirs, logical, container)

		target := verifier.endpointAbsolute(resolution.To)
		logicalAbsolute := absoluteDependency(logical)
		if logicalAbsolute == target {
			if _, err := verifier.deps.require(logical, artifactEntryDirectory); err != nil {
				return fmt.Errorf("physical dependency edge %q: %w", logical, err)
			}
		} else {
			verifier.depLinks[logical] = canonicalRelativeTarget(logicalAbsolute, target)
		}
		if err := verifier.deriveShims(container, resolution.To); err != nil {
			return fmt.Errorf("dependency %q from %q: %w", resolution.Dependency, container, err)
		}
	}
	return nil
}

func (verifier *pairVerifier) resolutionContainer(endpoint PackageEndpoint) string {
	switch endpoint.Kind {
	case PackageKindLocal:
		if *endpoint.Path == "." {
			return "."
		}
		local := verifier.localPackage(*endpoint.Path)
		return path.Join(".helmr/views", *local.ViewKey)
	case PackageKindRegistry:
		return path.Join(*endpoint.InstallPath, "node_modules")
	default:
		panic("validated package endpoint has unknown kind")
	}
}

func (verifier *pairVerifier) endpointAbsolute(endpoint PackageEndpoint) string {
	if endpoint.Kind == PackageKindLocal {
		return absoluteCode(*endpoint.Path)
	}
	return absoluteDependency(*endpoint.InstallPath)
}

func (verifier *pairVerifier) localPackage(packagePath string) LocalPackage {
	if local, exists := verifier.localByPath[packagePath]; exists {
		return local
	}
	panic("validated graph local package is absent")
}

func (verifier *pairVerifier) endpointManifest(endpoint PackageEndpoint) packageManifest {
	if endpoint.Kind == PackageKindLocal {
		manifestPath := "package.json"
		if *endpoint.Path != "." {
			manifestPath = path.Join(*endpoint.Path, "package.json")
		}
		return verifier.codeManifests[manifestPath]
	}
	return verifier.depManifests[path.Join(*endpoint.InstallPath, "package.json")]
}

func (verifier *pairVerifier) deriveShims(container string, endpoint PackageEndpoint) error {
	manifest := verifier.endpointManifest(endpoint)
	for command, target := range manifest.Bins {
		shimPath := path.Join(container, ".bin", command)
		if _, exists := verifier.shims[shimPath]; exists {
			return fmt.Errorf("bin command %q collides in container %q", command, container)
		}
		targetPath := target
		var artifact *inspectedArtifact
		var absolute string
		if endpoint.Kind == PackageKindLocal {
			targetPath = joinArtifactPath(*endpoint.Path, target)
			artifact = verifier.code
			absolute = absoluteCode(targetPath)
		} else {
			targetPath = path.Join(*endpoint.InstallPath, target)
			artifact = verifier.deps
			absolute = absoluteDependency(targetPath)
		}
		entry, err := artifact.require(targetPath, artifactEntryRegular)
		if err != nil {
			return fmt.Errorf("bin target %q: %w", targetPath, err)
		}
		if err := verifier.validateBinTarget(artifact, entry); err != nil {
			return fmt.Errorf("bin target %q: %w", targetPath, err)
		}
		escaped := strings.ReplaceAll(absolute, "'", "'\\''")
		verifier.shims[shimPath] = []byte(
			"#!/opt/helmr/runtime/bin/helmr-sh\n" +
				"exec /opt/helmr/runtime/bin/node " +
				"--import=file:///opt/helmr/runtime/helmr/preload.mjs '" +
				escaped + "' \"$@\"\n",
		)
		verifier.depDirs[path.Join(container, ".bin")] = struct{}{}
		addAncestorDirectories(verifier.depDirs, shimPath, container)
	}
	return nil
}

func (verifier *pairVerifier) validateBinTarget(
	artifact *inspectedArtifact,
	entry artifactEntry,
) error {
	switch path.Ext(entry.Path) {
	case ".js", ".mjs", ".cjs":
		return nil
	case "":
	default:
		return fmt.Errorf("extension %q is unsupported", path.Ext(entry.Path))
	}
	reader, err := artifact.reader.Open(verifier.ctx, entry.Path)
	if err != nil {
		return err
	}
	defer reader.Close()
	prefix, err := io.ReadAll(io.LimitReader(reader, 21))
	if err != nil {
		return err
	}
	for _, shebang := range []string{"#!/usr/bin/env node", "#!/usr/bin/node"} {
		if string(prefix) == shebang && entry.SizeBytes == int64(len(shebang)) ||
			len(prefix) > len(shebang) && string(prefix[:len(shebang)+1]) == shebang+"\n" {
			return nil
		}
	}
	return fmt.Errorf("extensionless target has no exact admitted Node shebang")
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

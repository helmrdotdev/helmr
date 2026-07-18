package deployment

import (
	"fmt"
	"path"
	"strings"
)

func (verifier *pairVerifier) verifyLinks() error {
	for _, entry := range verifier.code.ordered {
		if entry.Kind != artifactEntrySymlink {
			continue
		}
		finalArtifact, finalPath, err := verifier.resolve(absoluteCode(entry.Path))
		if err != nil {
			return fmt.Errorf("code link %q: %w", entry.Path, err)
		}
		if _, generated := verifier.codeLinks[entry.Path]; generated {
			continue
		}
		if finalArtifact != verifier.code || !verifier.isFirstPartyPath(finalPath) {
			return fmt.Errorf("source link %q escapes the first-party tree", entry.Path)
		}
	}
	for _, entry := range verifier.deps.ordered {
		if entry.Kind != artifactEntrySymlink {
			continue
		}
		finalArtifact, finalPath, err := verifier.resolve(absoluteDependency(entry.Path))
		if err != nil {
			return fmt.Errorf("dependency link %q: %w", entry.Path, err)
		}
		if _, generated := verifier.depLinks[entry.Path]; generated {
			continue
		}
		if _, generated := verifier.binLinks[entry.Path]; generated {
			continue
		}
		owner := verifier.registryOwner(entry.Path)
		if owner == "" || finalArtifact != verifier.deps || verifier.registryOwner(finalPath) != owner {
			return fmt.Errorf("registry-owned link %q escapes package root %q", entry.Path, owner)
		}
	}
	return nil
}

func (verifier *pairVerifier) resolve(absolute string) (*inspectedArtifact, string, error) {
	if err := validateResolvedAbsolute(absolute); err != nil {
		return nil, "", err
	}
	relative := strings.TrimPrefix(strings.TrimPrefix(absolute, programMountPath), "/")
	if relative == "" {
		return verifier.code, ".", nil
	}
	pending := strings.Split(relative, "/")
	resolved := make([]string, 0, len(pending))
	visited := make(map[string]struct{})
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return nil, "", fmt.Errorf("link walk escapes %s", programMountPath)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}

		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := programMountPath + "/" + strings.Join(candidateParts, "/")
		if err := validateResolvedAbsolute(candidate); err != nil {
			return nil, "", err
		}
		artifact, entryPath, entry, exists := verifier.absoluteEntry(candidate)
		if !exists {
			return nil, "", fmt.Errorf("resolved path %q is dangling", candidate)
		}
		if entry.Kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return nil, "", fmt.Errorf("link walk exceeds %d hops", maxSymlinkHops)
			}
			state := candidate + "\x00" + strings.Join(pending, "\x00")
			if _, exists := visited[state]; exists {
				return nil, "", fmt.Errorf("link walk contains a cycle at %q", candidate)
			}
			visited[state] = struct{}{}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		resolved = candidateParts
		if len(pending) != 0 && entry.Kind != artifactEntryDirectory {
			return nil, "", fmt.Errorf("resolved path traverses non-directory %q", candidate)
		}
		if len(pending) == 0 {
			return artifact, entryPath, nil
		}
	}
	if len(resolved) == 0 {
		return verifier.code, ".", nil
	}
	candidate := programMountPath + "/" + strings.Join(resolved, "/")
	artifact, entryPath, _, exists := verifier.absoluteEntry(candidate)
	if !exists {
		return nil, "", fmt.Errorf("resolved path %q is dangling", candidate)
	}
	return artifact, entryPath, nil
}

func (verifier *pairVerifier) absoluteEntry(
	absolute string,
) (*inspectedArtifact, string, artifactEntry, bool) {
	if absolute == dependencyMountPath {
		entry, exists := verifier.deps.entries["."]
		return verifier.deps, ".", entry, exists
	}
	if strings.HasPrefix(absolute, dependencyMountPath+"/") {
		value := strings.TrimPrefix(absolute, dependencyMountPath+"/")
		entry, exists := verifier.deps.entries[value]
		return verifier.deps, value, entry, exists
	}
	if absolute == programMountPath {
		entry, exists := verifier.code.entries["."]
		return verifier.code, ".", entry, exists
	}
	if strings.HasPrefix(absolute, programMountPath+"/") {
		value := strings.TrimPrefix(absolute, programMountPath+"/")
		entry, exists := verifier.code.entries[value]
		return verifier.code, value, entry, exists
	}
	return nil, "", artifactEntry{}, false
}

func validateResolvedAbsolute(value string) error {
	if value != programMountPath && !strings.HasPrefix(value, programMountPath+"/") {
		return fmt.Errorf("resolved path %q escapes %s", value, programMountPath)
	}
	if len(value)+1 > maxMountedPackagePath {
		return fmt.Errorf("resolved path %q exceeds the mounted path bound", value)
	}
	mount := programMountPath
	if value == dependencyMountPath || strings.HasPrefix(value, dependencyMountPath+"/") {
		mount = dependencyMountPath
	}
	relative := strings.TrimPrefix(strings.TrimPrefix(value, mount), "/")
	if relative == "" {
		return nil
	}
	components := strings.Split(relative, "/")
	if len(components) > maxArtifactDepth {
		return fmt.Errorf("resolved path depth exceeds %d", maxArtifactDepth)
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." ||
			len(component) > maxPackagePathComponent {
			return fmt.Errorf("resolved path %q has an invalid component", value)
		}
	}
	return nil
}

func (verifier *pairVerifier) isFirstPartyPath(value string) bool {
	if value == "." || value == "helmr" || strings.HasPrefix(value, "helmr/") ||
		value == "node_modules" || strings.HasPrefix(value, "node_modules/") ||
		value == ".helmr" || strings.HasPrefix(value, ".helmr/") ||
		hasNodeModulesComponent(value) {
		return false
	}
	for current := value; current != "."; current = path.Dir(current) {
		if _, exists := verifier.reservedCode[current]; exists {
			return false
		}
	}
	_, exists := verifier.code.entries[value]
	return exists
}

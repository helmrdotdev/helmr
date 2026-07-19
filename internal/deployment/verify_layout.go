package deployment

import (
	"fmt"
	"path"
	"strings"
)

func (verifier *pairVerifier) verifyLayouts() error {
	sidecars := moduleSidecarSet(verifier.modules)
	for _, required := range []string{".", "helmr", "node_modules"} {
		if _, err := verifier.code.require(required, artifactEntryDirectory); err != nil {
			return fmt.Errorf("code layout: %w", err)
		}
	}
	if len(verifier.modules.Modules) == 0 {
		if _, exists := verifier.code.entries["helmr/files"]; exists {
			return fmt.Errorf("helmr/files must be absent when the module map is empty")
		}
	} else {
		for _, required := range []string{"helmr/files", "helmr/files/modules"} {
			if _, err := verifier.code.require(required, artifactEntryDirectory); err != nil {
				return fmt.Errorf("code layout: %w", err)
			}
		}
	}
	for codePath := range sidecars {
		if _, err := verifier.code.require(codePath, artifactEntryRegular); err != nil {
			return fmt.Errorf("module sidecar: %w", err)
		}
	}
	for link, target := range verifier.codeLinks {
		entry, err := verifier.code.require(link, artifactEntrySymlink)
		if err != nil {
			return fmt.Errorf("generated code link: %w", err)
		}
		if entry.LinkTarget != target {
			return fmt.Errorf("generated code link %q target = %q, want %q", link, entry.LinkTarget, target)
		}
	}
	for _, entry := range verifier.code.ordered {
		if err := verifier.verifyCodePath(entry, sidecars); err != nil {
			return err
		}
	}

	for _, required := range []string{".", ".helmr", ".helmr/views"} {
		if _, err := verifier.deps.require(required, artifactEntryDirectory); err != nil {
			return fmt.Errorf("dependency layout: %w", err)
		}
	}
	for _, required := range []string{
		".helmr/dependencies.json",
		".helmr/dependency-plan.json",
		".helmr/package-graph.json",
	} {
		if _, err := verifier.deps.require(required, artifactEntryRegular); err != nil {
			return fmt.Errorf("dependency layout: %w", err)
		}
	}
	for directory := range verifier.depDirs {
		if _, err := verifier.deps.require(directory, artifactEntryDirectory); err != nil {
			return fmt.Errorf("derived dependency directory: %w", err)
		}
	}
	for link, target := range verifier.depLinks {
		entry, err := verifier.deps.require(link, artifactEntrySymlink)
		if err != nil {
			return fmt.Errorf("graph-derived dependency link: %w", err)
		}
		if entry.LinkTarget != target {
			return fmt.Errorf(
				"graph-derived dependency link %q target = %q, want %q",
				link,
				entry.LinkTarget,
				target,
			)
		}
	}
	for link, target := range verifier.binLinks {
		entry, err := verifier.deps.require(link, artifactEntrySymlink)
		if err != nil {
			return fmt.Errorf("generated bin link: %w", err)
		}
		if entry.LinkTarget != target {
			return fmt.Errorf(
				"generated bin link %q target = %q, want %q",
				link,
				entry.LinkTarget,
				target,
			)
		}
	}
	for _, entry := range verifier.deps.ordered {
		if err := verifier.verifyDependencyPath(entry); err != nil {
			return err
		}
	}
	return nil
}

func moduleSidecarSet(moduleMap ModuleMap) map[string]struct{} {
	sidecars := make(map[string]struct{}, len(moduleMap.Modules))
	for _, module := range moduleMap.Modules {
		sidecars[module.CodePath] = struct{}{}
	}
	return sidecars
}

func (verifier *pairVerifier) verifyCodePath(
	entry artifactEntry,
	sidecars map[string]struct{},
) error {
	switch entry.Path {
	case ".", "helmr", "helmr/program.json", "helmr/entry.mjs", "helmr/modules.json",
		"node_modules":
		return nil
	}
	if strings.HasPrefix(entry.Path, "helmr/") {
		if len(verifier.modules.Modules) != 0 &&
			(entry.Path == "helmr/files" || entry.Path == "helmr/files/modules") {
			return nil
		}
		if _, exists := sidecars[entry.Path]; exists {
			return nil
		}
		return fmt.Errorf("code Artifact contains unlisted Helmr path %q", entry.Path)
	}
	if entry.Path == ".helmr" || strings.HasPrefix(entry.Path, ".helmr/") {
		return fmt.Errorf("code Artifact contains reserved path %q", entry.Path)
	}
	if hasNodeModulesComponent(entry.Path) {
		if _, exists := verifier.codeLinks[entry.Path]; exists {
			return nil
		}
		return fmt.Errorf("code Artifact contains unlisted node_modules path %q", entry.Path)
	}
	for current := entry.Path; current != "."; current = path.Dir(current) {
		if _, exists := verifier.reservedCode[current]; exists {
			return fmt.Errorf("local package contains reserved path %q", entry.Path)
		}
	}
	switch path.Base(entry.Path) {
	case ".npmrc", "bunfig.toml":
		return fmt.Errorf("code Artifact contains package-manager configuration %q", entry.Path)
	}
	if entry.Path == "bun.lockb" || entry.Path == "npm-shrinkwrap.json" {
		return fmt.Errorf("code Artifact contains unsupported root lockfile %q", entry.Path)
	}
	if entry.Kind == artifactEntryRegular &&
		(strings.HasSuffix(entry.Path, ".tsx") || strings.HasSuffix(entry.Path, ".jsx")) {
		return fmt.Errorf("code Artifact contains unsupported JSX source %q", entry.Path)
	}
	return nil
}

func (verifier *pairVerifier) verifyDependencyPath(entry artifactEntry) error {
	switch entry.Path {
	case ".", ".helmr", ".helmr/views", ".helmr/dependencies.json",
		".helmr/dependency-plan.json", ".helmr/package-graph.json":
		return nil
	}
	if _, exists := verifier.depLinks[entry.Path]; exists {
		return nil
	}
	if _, exists := verifier.binLinks[entry.Path]; exists {
		return nil
	}
	if _, exists := verifier.depDirs[entry.Path]; exists {
		if entry.Kind != artifactEntryDirectory {
			return fmt.Errorf("derived dependency path %q must be a directory", entry.Path)
		}
		return nil
	}
	if strings.HasPrefix(entry.Path, ".helmr/") {
		return fmt.Errorf("dependency Artifact contains unlisted .helmr path %q", entry.Path)
	}
	if binRoot := verifier.containingBinRoot(entry.Path); binRoot != "" {
		return fmt.Errorf("dependency container %q contains unlisted .bin path %q", binRoot, entry.Path)
	}
	owner := verifier.registryOwner(entry.Path)
	if owner == "" {
		return fmt.Errorf("dependency Artifact contains unowned path %q", entry.Path)
	}
	relative := strings.TrimPrefix(strings.TrimPrefix(entry.Path, owner), "/")
	if hasNodeModulesComponent(relative) {
		return fmt.Errorf("registry package %q contains unlisted node_modules path %q", owner, entry.Path)
	}
	return nil
}

func (verifier *pairVerifier) containingBinRoot(value string) string {
	for current := value; current != "."; current = path.Dir(current) {
		if _, exists := verifier.binRoots[current]; exists {
			return current
		}
	}
	return ""
}

func (verifier *pairVerifier) registryOwner(value string) string {
	for current := value; current != "."; current = path.Dir(current) {
		if _, exists := verifier.registryRoots[current]; exists {
			return current
		}
	}
	return ""
}

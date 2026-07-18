package deployment

import (
	"context"
	"fmt"
	"path"
	"strings"
)

type pairVerifier struct {
	ctx             context.Context
	artifacts       programArtifacts
	code            *inspectedArtifact
	deps            *inspectedArtifact
	index           ProgramIndex
	dependencyIndex DependencyIndex
	graph           PackageGraph
	modules         ModuleMap
	codeManifests   map[string]packageManifest
	depManifests    map[string]packageManifest
	codeLinks       map[string]string
	depLinks        map[string]string
	binLinks        map[string]string
	depDirs         map[string]struct{}
	binRoots        map[string]struct{}
	localByPath     map[string]LocalPackage
	registryRoots   map[string]struct{}
	reservedCode    map[string]struct{}
}

func (verifier *pairVerifier) verify() error {
	if err := verifier.readDocuments(); err != nil {
		return err
	}
	verifier.indexGraph()
	if err := verifier.verifyManifests(); err != nil {
		return err
	}
	if err := verifier.deriveTopology(); err != nil {
		return err
	}
	if err := verifier.verifyModules(); err != nil {
		return err
	}
	if err := verifier.verifyLayouts(); err != nil {
		return err
	}
	if err := verifier.verifyLinks(); err != nil {
		return err
	}
	return nil
}

func (verifier *pairVerifier) indexGraph() {
	verifier.localByPath = make(map[string]LocalPackage, len(verifier.graph.LocalPackages))
	verifier.registryRoots = make(map[string]struct{}, len(verifier.graph.RegistryPackages))
	verifier.reservedCode = make(map[string]struct{}, 2*len(verifier.graph.LocalPackages))
	for _, local := range verifier.graph.LocalPackages {
		verifier.localByPath[local.Path] = local
		if local.Path != "." {
			verifier.reservedCode[path.Join(local.Path, "helmr")] = struct{}{}
			verifier.reservedCode[path.Join(local.Path, ".helmr")] = struct{}{}
		}
	}
	for _, registry := range verifier.graph.RegistryPackages {
		verifier.registryRoots[registry.InstallPath] = struct{}{}
	}
}

func (verifier *pairVerifier) readDocuments() error {
	programRaw, err := verifier.code.read(verifier.ctx, "helmr/program.json", maxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	verifier.index, err = ParseProgramIndex(programRaw)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	if verifier.index.Dependencies.Digest != verifier.artifacts.Dependencies.Digest ||
		verifier.index.Dependencies.SizeBytes != verifier.artifacts.Dependencies.SizeBytes ||
		verifier.index.Dependencies.MediaType != verifier.artifacts.Dependencies.MediaType {
		return fmt.Errorf("program index dependency descriptor does not match the dependency Artifact")
	}

	dependencyRaw, err := verifier.deps.read(
		verifier.ctx,
		".helmr/dependencies.json",
		maxDependencyIndexSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("dependency index: %w", err)
	}
	verifier.dependencyIndex, err = ParseDependencyIndex(dependencyRaw)
	if err != nil {
		return fmt.Errorf("dependency index: %w", err)
	}
	if verifier.index.RuntimeDigest != verifier.dependencyIndex.RuntimeDigest ||
		verifier.index.Architecture != verifier.dependencyIndex.Architecture {
		return fmt.Errorf("program and dependency indexes disagree on runtime or architecture")
	}

	graphRaw, err := verifier.deps.read(
		verifier.ctx,
		".helmr/package-graph.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("package graph: %w", err)
	}
	if int64(len(graphRaw)) != verifier.index.PackageGraph.SizeBytes ||
		int64(len(graphRaw)) != verifier.dependencyIndex.PackageGraphSizeBytes ||
		digestBytes(graphRaw) != verifier.index.PackageGraph.Digest ||
		digestBytes(graphRaw) != verifier.dependencyIndex.PackageGraphDigest {
		return fmt.Errorf("package graph identity does not match both indexes")
	}
	verifier.graph, err = ParsePackageGraph(graphRaw)
	if err != nil {
		return fmt.Errorf("package graph: %w", err)
	}

	moduleRaw, err := verifier.code.read(verifier.ctx, "helmr/modules.json", maxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("module map: %w", err)
	}
	if int64(len(moduleRaw)) != verifier.index.Modules.SizeBytes ||
		digestBytes(moduleRaw) != verifier.index.Modules.Digest {
		return fmt.Errorf("module map identity does not match the program index")
	}
	verifier.modules, err = ParseModuleMap(moduleRaw)
	if err != nil {
		return fmt.Errorf("module map: %w", err)
	}

	entry, err := verifier.code.require("helmr/entry.mjs", artifactEntryRegular)
	if err != nil {
		return err
	}
	if entry.SizeBytes > maxProgramFileSizeBytes {
		return fmt.Errorf("helmr/entry.mjs exceeds %d bytes", maxProgramFileSizeBytes)
	}

	lockfile := verifier.dependencyIndex.Lockfile.Name
	lockEntry, err := verifier.code.require(lockfile, artifactEntryRegular)
	if err != nil {
		return fmt.Errorf("selected lockfile: %w", err)
	}
	if lockEntry.SizeBytes > maxLockfileBytes {
		return fmt.Errorf("selected lockfile exceeds %d bytes", maxLockfileBytes)
	}
	lockDigest, err := verifier.code.digest(verifier.ctx, lockfile)
	if err != nil {
		return err
	}
	if lockDigest != verifier.dependencyIndex.Lockfile.Digest {
		return fmt.Errorf("selected lockfile digest does not match the dependency index")
	}
	alternate := "bun.lock"
	if lockfile == alternate {
		alternate = "package-lock.json"
	}
	if _, exists := verifier.code.entries[alternate]; exists {
		return fmt.Errorf("unselected recognized root lockfile %q is present", alternate)
	}
	return nil
}

func (verifier *pairVerifier) verifyManifests() error {
	verifier.codeManifests = make(map[string]packageManifest)
	verifier.depManifests = make(map[string]packageManifest)
	localRoots := make(map[string]bool, len(verifier.graph.LocalPackages))
	for _, local := range verifier.graph.LocalPackages {
		manifestPath := "package.json"
		if local.Path != "." {
			manifestPath = path.Join(local.Path, "package.json")
		}
		localRoots[manifestPath] = local.Path == "."
	}
	registryRoots := make(map[string]struct{}, len(verifier.graph.RegistryPackages))
	for _, registry := range verifier.graph.RegistryPackages {
		registryRoots[path.Join(registry.InstallPath, "package.json")] = struct{}{}
	}

	codeManifests, err := manifestEntries(verifier.code, "helmr", "code")
	if err != nil {
		return err
	}
	for _, entry := range codeManifests {
		raw, err := verifier.code.read(verifier.ctx, entry.Path, maxPackageManifestSizeBytes)
		if err != nil {
			return err
		}
		var manifest packageManifest
		if root, exists := localRoots[entry.Path]; exists {
			manifest, err = parseLocalPackageManifest(raw, root)
		} else {
			manifest, err = parsePackageScope(raw)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Path, err)
		}
		verifier.codeManifests[entry.Path] = manifest
	}

	dependencyManifests, err := manifestEntries(verifier.deps, ".helmr", "dependency")
	if err != nil {
		return err
	}
	for _, entry := range dependencyManifests {
		raw, err := verifier.deps.read(verifier.ctx, entry.Path, maxPackageManifestSizeBytes)
		if err != nil {
			return err
		}
		var manifest packageManifest
		if _, exists := registryRoots[entry.Path]; exists {
			manifest, err = parseRegistryPackageManifest(raw)
		} else {
			manifest, err = parsePackageScope(raw)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Path, err)
		}
		verifier.depManifests[entry.Path] = manifest
	}

	localManifests := LocalManifests{
		FormatVersion: LocalManifestsFormatVersion,
		Entries:       make([]LocalManifestEntry, 0, len(verifier.graph.LocalPackages)),
	}
	for _, local := range verifier.graph.LocalPackages {
		manifestPath := "package.json"
		if local.Path != "." {
			manifestPath = path.Join(local.Path, "package.json")
			if _, err := verifier.code.require(local.Path, artifactEntryDirectory); err != nil {
				return fmt.Errorf("local package %q: %w", local.Path, err)
			}
		}
		manifest, exists := verifier.codeManifests[manifestPath]
		if !exists {
			return fmt.Errorf("local package %q has no regular package.json", local.Path)
		}
		digest, err := verifier.code.digest(verifier.ctx, manifestPath)
		if err != nil {
			return err
		}
		if digest != local.ManifestDigest {
			return fmt.Errorf("local package %q manifest digest does not match the graph", local.Path)
		}
		if !pointerStringsEqual(manifest.Name, local.Name) ||
			!pointerStringsEqual(manifest.Version, local.Version) {
			return fmt.Errorf("local package %q manifest identity does not match the graph", local.Path)
		}
		if len(manifest.AutomaticScripts) != 0 {
			return fmt.Errorf(
				"local package %q defines automatic lifecycle script %q",
				local.Path,
				manifest.AutomaticScripts[0],
			)
		}
		localManifests.Entries = append(localManifests.Entries, LocalManifestEntry{
			ManifestDigest: digest,
			Path:           local.Path,
		})
		if local.Path == "." {
			want := string(verifier.dependencyIndex.PackageManager.Name) + "@" +
				verifier.dependencyIndex.PackageManager.Version
			if manifest.PackageManager == nil || *manifest.PackageManager != want {
				return fmt.Errorf("root packageManager does not equal %q", want)
			}
		}
	}
	localDigest, err := LocalManifestsDigest(localManifests)
	if err != nil {
		return err
	}
	if "sha256:"+fmt.Sprintf("%x", localDigest) != verifier.dependencyIndex.LocalManifestsDigest {
		return fmt.Errorf("local manifest set does not match the dependency index")
	}

	for _, registry := range verifier.graph.RegistryPackages {
		if _, err := verifier.deps.require(registry.InstallPath, artifactEntryDirectory); err != nil {
			return fmt.Errorf("registry package %q: %w", registry.InstallPath, err)
		}
		manifestPath := path.Join(registry.InstallPath, "package.json")
		manifest, exists := verifier.depManifests[manifestPath]
		if !exists {
			return fmt.Errorf("registry package %q has no regular package.json", registry.InstallPath)
		}
		if manifest.Name == nil || *manifest.Name != registry.Name ||
			manifest.Version == nil || *manifest.Version != registry.Version {
			return fmt.Errorf("registry package %q manifest identity does not match the graph", registry.InstallPath)
		}
	}
	return nil
}

func manifestEntries(
	artifact *inspectedArtifact,
	reservedRoot string,
	label string,
) ([]artifactEntry, error) {
	entries := make([]artifactEntry, 0)
	var logicalBytes int64
	for _, entry := range artifact.ordered {
		if path.Base(entry.Path) != "package.json" ||
			entry.Path == reservedRoot || strings.HasPrefix(entry.Path, reservedRoot+"/") {
			continue
		}
		if entry.Kind != artifactEntryRegular {
			return nil, fmt.Errorf("%s Artifact package.json %q is not a regular file", label, entry.Path)
		}
		if logicalBytes > maxPackageJSONBytes-entry.SizeBytes {
			return nil, fmt.Errorf(
				"%s Artifact package.json bytes exceed %d",
				label,
				maxPackageJSONBytes,
			)
		}
		logicalBytes += entry.SizeBytes
		entries = append(entries, entry)
	}
	return entries, nil
}

func (verifier *pairVerifier) verifyModules() error {
	byPath := make(map[string]Module, len(verifier.modules.Modules))
	sidecars := make(map[string]struct{}, len(verifier.modules.Modules))
	for _, module := range verifier.modules.Modules {
		byPath[module.Path] = module
		sidecars[module.CodePath] = struct{}{}
	}

	for _, entry := range verifier.code.ordered {
		if entry.Kind != artifactEntryRegular || strings.HasPrefix(entry.Path, "helmr/") {
			continue
		}
		if !isRuntimeTypeScript(entry.Path) {
			continue
		}
		module, exists := byPath[entry.Path]
		if !exists {
			return fmt.Errorf("TypeScript source %q is absent from the module map", entry.Path)
		}
		sourceDigest, err := verifier.code.digest(verifier.ctx, entry.Path)
		if err != nil {
			return err
		}
		if sourceDigest != module.SourceDigest {
			return fmt.Errorf("TypeScript source %q digest does not match the module map", entry.Path)
		}
		codeDigest, err := verifier.code.digest(verifier.ctx, module.CodePath)
		if err != nil {
			return err
		}
		if codeDigest != module.CodeDigest {
			return fmt.Errorf("TypeScript sidecar %q digest does not match the module map", module.CodePath)
		}
		wantFormat, err := verifier.moduleFormat(entry.Path)
		if err != nil {
			return err
		}
		if module.Format != wantFormat {
			return fmt.Errorf("TypeScript source %q format does not match its nearest manifest", entry.Path)
		}
		delete(byPath, entry.Path)
	}
	if len(byPath) != 0 {
		for source := range byPath {
			return fmt.Errorf("module map contains non-regular or ineligible source %q", source)
		}
	}
	for _, entry := range verifier.code.ordered {
		if strings.HasPrefix(entry.Path, "helmr/files/modules/") {
			if _, exists := sidecars[entry.Path]; !exists {
				return fmt.Errorf("unlisted module sidecar path %q", entry.Path)
			}
		}
	}
	return nil
}

func isRuntimeTypeScript(value string) bool {
	if strings.HasSuffix(value, ".d.ts") || strings.HasSuffix(value, ".d.mts") ||
		strings.HasSuffix(value, ".d.cts") {
		return false
	}
	return strings.HasSuffix(value, ".ts") || strings.HasSuffix(value, ".mts") ||
		strings.HasSuffix(value, ".cts")
}

func (verifier *pairVerifier) moduleFormat(source string) (ModuleFormat, error) {
	switch {
	case strings.HasSuffix(source, ".mts"):
		return ModuleFormatESM, nil
	case strings.HasSuffix(source, ".cts"):
		return ModuleFormatCommonJS, nil
	}
	parent := path.Dir(source)
	for {
		manifestPath := path.Join(parent, "package.json")
		if parent == "." {
			manifestPath = "package.json"
		}
		if manifest, exists := verifier.codeManifests[manifestPath]; exists {
			if manifest.Type == "module" {
				return ModuleFormatESM, nil
			}
			return ModuleFormatCommonJS, nil
		}
		if parent == "." {
			return "", fmt.Errorf("TypeScript source %q has no package.json ancestor", source)
		}
		parent = path.Dir(parent)
	}
}

func hasPathComponent(value, component string) bool {
	for _, item := range strings.Split(value, "/") {
		if item == component {
			return true
		}
	}
	return false
}

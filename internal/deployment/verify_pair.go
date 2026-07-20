package deployment

import (
	"context"
	"fmt"
	"path"
	"strings"
)

type pairVerifier struct {
	ctx       context.Context
	artifacts programArtifacts
	code      *inspectedArtifact
	deps      *inspectedArtifact
	index     ProgramIndex
	modules   ModuleMap
	manifests map[string]packageManifest
}

func (verifier *pairVerifier) verify() error {
	if err := verifier.readDocuments(); err != nil {
		return err
	}
	if err := verifier.verifyCodeLayout(); err != nil {
		return err
	}
	if err := verifier.verifyDependencyLayout(); err != nil {
		return err
	}
	if err := verifier.readManifests(); err != nil {
		return err
	}
	if err := verifier.verifyModules(); err != nil {
		return err
	}
	return verifier.verifyLinks()
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
	if verifier.index.DependenciesDigest != verifier.artifacts.Dependencies.Digest {
		return fmt.Errorf("program index dependenciesDigest does not match the dependency Artifact")
	}

	moduleRaw, err := verifier.code.read(verifier.ctx, "helmr/modules.json", maxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("module map: %w", err)
	}
	if digestBytes(moduleRaw) != verifier.index.ModuleMapDigest {
		return fmt.Errorf("module map digest does not match the program index")
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

	return nil
}

func (verifier *pairVerifier) verifyCodeLayout() error {
	for _, required := range []string{".", "helmr", "node_modules"} {
		if _, err := verifier.code.require(required, artifactEntryDirectory); err != nil {
			return fmt.Errorf("code layout: %w", err)
		}
	}
	for _, required := range []string{
		"helmr/entry.mjs",
		"helmr/modules.json",
		"helmr/program.json",
	} {
		if _, err := verifier.code.require(required, artifactEntryRegular); err != nil {
			return fmt.Errorf("code layout: %w", err)
		}
	}
	for _, entry := range verifier.code.ordered {
		if strings.HasPrefix(entry.Path, "node_modules/") {
			return fmt.Errorf("code Artifact dependency mountpoint is not empty at %q", entry.Path)
		}
	}
	return nil
}

func (verifier *pairVerifier) verifyDependencyLayout() error {
	if _, err := verifier.deps.require(".", artifactEntryDirectory); err != nil {
		return fmt.Errorf("dependency layout: %w", err)
	}
	return nil
}

func (verifier *pairVerifier) readManifests() error {
	verifier.manifests = make(map[string]packageManifest)
	var totalBytes int64
	for _, entry := range verifier.code.ordered {
		if path.Base(entry.Path) != "package.json" ||
			entry.Path == "helmr" || strings.HasPrefix(entry.Path, "helmr/") {
			continue
		}
		if entry.Kind != artifactEntryRegular {
			return fmt.Errorf("code Artifact package.json %q is not a regular file", entry.Path)
		}
		if totalBytes > maxPackageJSONBytes-entry.SizeBytes {
			return fmt.Errorf("code Artifact package.json bytes exceed %d", maxPackageJSONBytes)
		}
		totalBytes += entry.SizeBytes
		raw, err := verifier.code.read(verifier.ctx, entry.Path, maxPackageManifestSizeBytes)
		if err != nil {
			return err
		}
		manifest, err := parsePackageScope(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Path, err)
		}
		verifier.manifests[entry.Path] = manifest
	}
	return nil
}

func (verifier *pairVerifier) verifyModules() error {
	byPath := make(map[string]Module, len(verifier.modules.Modules))
	sidecars := make(map[string]struct{}, len(verifier.modules.Modules))
	for _, module := range verifier.modules.Modules {
		byPath[module.Path] = module
		sidecars[module.CodePath] = struct{}{}
	}

	for _, entry := range verifier.code.ordered {
		if entry.Kind != artifactEntryRegular || strings.HasPrefix(entry.Path, "helmr/") ||
			!isRuntimeTypeScript(entry.Path) {
			continue
		}
		module, exists := byPath[entry.Path]
		if !exists {
			return fmt.Errorf("TypeScript source %q is absent from the module map", entry.Path)
		}
		source := entry
		if source.SizeBytes > maxProgramFileSizeBytes {
			return fmt.Errorf("TypeScript source %q exceeds %d bytes", entry.Path, maxProgramFileSizeBytes)
		}
		sourceDigest, err := verifier.code.digest(verifier.ctx, entry.Path)
		if err != nil {
			return err
		}
		if sourceDigest != module.SourceDigest {
			return fmt.Errorf("TypeScript source %q digest does not match the module map", entry.Path)
		}
		if _, err := verifier.code.require(module.CodePath, artifactEntryRegular); err != nil {
			return fmt.Errorf("TypeScript sidecar %q: %w", module.CodePath, err)
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
			if _, exists := sidecars[entry.Path]; !exists && entry.Kind == artifactEntryRegular {
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
		if manifest, exists := verifier.manifests[manifestPath]; exists {
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

func (verifier *pairVerifier) verifyLinks() error {
	for _, entry := range verifier.code.ordered {
		if entry.Kind == artifactEntrySymlink {
			if err := verifier.verifyLink(entry.Path, entry.LinkTarget); err != nil {
				return fmt.Errorf("code link %q: %w", entry.Path, err)
			}
		}
	}
	for _, entry := range verifier.deps.ordered {
		if entry.Kind == artifactEntrySymlink {
			link := "node_modules"
			if entry.Path != "." {
				link += "/" + entry.Path
			}
			if err := verifier.verifyLink(link, entry.LinkTarget); err != nil {
				return fmt.Errorf("dependency link %q: %w", entry.Path, err)
			}
		}
	}
	return nil
}

func (verifier *pairVerifier) verifyLink(link, target string) error {
	pending := append(
		strings.Split(path.Dir(link), "/"),
		strings.Split(target, "/")...,
	)
	resolved := make([]string, 0, len(pending))
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return fmt.Errorf("target escapes the combined Program namespace")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(resolved, component), "/")
		entry, exists := verifier.combinedEntry(candidate)
		if !exists {
			return nil
		}
		if entry.Kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return fmt.Errorf("target exceeds %d symbolic-link hops", maxSymlinkHops)
			}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		if entry.Kind != artifactEntryDirectory && len(pending) != 0 {
			return nil
		}
		resolved = append(resolved, component)
	}
	return nil
}

func (verifier *pairVerifier) combinedEntry(name string) (artifactEntry, bool) {
	if name == "node_modules" {
		entry, exists := verifier.code.entries[name]
		return entry, exists
	}
	if strings.HasPrefix(name, "node_modules/") {
		entry, exists := verifier.deps.entries[strings.TrimPrefix(name, "node_modules/")]
		return entry, exists
	}
	entry, exists := verifier.code.entries[name]
	return entry, exists
}

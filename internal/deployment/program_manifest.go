package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const ProgramManifestFormatVersion = 0

// ProgramManifest binds the executable closure, static compile policy and actual
// compiled bytes. Package-manager and build-tool provenance are not authority.
type ProgramManifest struct {
	FormatVersion      int                     `json:"formatVersion"`
	Config             ProgramPathDigest       `json:"config"`
	ExternalEdges      []ProgramExternalEdge   `json:"externalEdges"`
	CompileSelection   ProgramCompileSelection `json:"compileSelection"`
	CompiledInputs     []ProgramPathDigest     `json:"compiledInputs"`
	Modules            []ProgramModule         `json:"modules"`
	ProgramIndexDigest string                  `json:"programIndexDigest"`
}

type ProgramPathDigest struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
}

type ProgramModule struct {
	ModuleDigest    string `json:"moduleDigest"`
	ModulePath      string `json:"modulePath"`
	SourceMapDigest string `json:"sourceMapDigest"`
	SourceMapPath   string `json:"sourceMapPath"`
	SourcePath      string `json:"sourcePath"`
}

type ProgramExternalEdge struct {
	Importer     string `json:"importer"`
	Kind         string `json:"kind"`
	LogicalPath  string `json:"logicalPath"`
	ResolvedPath string `json:"resolvedPath"`
	RuntimePath  string `json:"runtimePath"`
	Specifier    string `json:"specifier"`
}

func ParseProgramManifest(raw []byte) (ProgramManifest, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ProgramManifest{}, fmt.Errorf(
			"program manifest size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramManifest{}, fmt.Errorf("canonicalize program manifest: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramManifest{}, errors.New(
			"program manifest is not RFC 8785 canonical JSON",
		)
	}
	var manifest ProgramManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ProgramManifest{}, fmt.Errorf("decode program manifest: %w", err)
	}
	if err := ensureEOF(decoder, "program manifest"); err != nil {
		return ProgramManifest{}, err
	}
	if err := validateProgramManifest(manifest); err != nil {
		return ProgramManifest{}, err
	}
	complete, err := canonicalProgramManifest(manifest)
	if err != nil {
		return ProgramManifest{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramManifest{}, errors.New(
			"program manifest does not match the complete canonical v0 shape",
		)
	}
	return manifest, nil
}

func canonicalProgramManifest(manifest ProgramManifest) ([]byte, error) {
	if err := validateProgramManifest(manifest); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return jsoncanon.Transform(raw)
}

func validateProgramManifest(manifest ProgramManifest) error {
	if manifest.FormatVersion != ProgramManifestFormatVersion {
		return errors.New("program manifest formatVersion is invalid")
	}
	if manifest.Config.Path != "helmr/config.json" ||
		!sha256DigestPattern.MatchString(manifest.Config.Digest) {
		return errors.New("program manifest config authority is invalid")
	}
	if !sha256DigestPattern.MatchString(manifest.ProgramIndexDigest) {
		return errors.New("program manifest index digest is invalid")
	}
	if manifest.ExternalEdges == nil ||
		manifest.Modules == nil {
		return errors.New("program manifest collections must be arrays")
	}
	for index, edge := range manifest.ExternalEdges {
		if err := validateProgramExternalEdge(edge); err != nil {
			return fmt.Errorf("program manifest external edge %d: %w", index, err)
		}
		if index > 0 && compareProgramExternalEdge(
			manifest.ExternalEdges[index-1],
			edge,
		) >= 0 {
			return errors.New("program manifest external edges are not in canonical order")
		}
	}
	if err := validateProgramCompileSelection(manifest.CompileSelection); err != nil {
		return err
	}
	if err := validateCompiledInputs(manifest.CompiledInputs, manifest.CompileSelection); err != nil {
		return err
	}
	if len(manifest.Modules) == 0 {
		return errors.New("program manifest modules must not be empty")
	}
	for index, module := range manifest.Modules {
		if err := validateProgramModule(module); err != nil {
			return fmt.Errorf("program manifest module %d: %w", index, err)
		}
		if index > 0 && manifest.Modules[index-1].ModulePath >= module.ModulePath {
			return errors.New("program manifest modules are not in canonical order")
		}
	}
	return nil
}

func validateProgramExternalEdge(edge ProgramExternalEdge) error {
	switch path.Ext(edge.ResolvedPath) {
	case ".ts", ".tsx", ".mts", ".cts":
		return errors.New("external TypeScript under node_modules must be selected for compilation")
	}

	if edge.Importer == "" || validateArtifactPath(edge.Importer, programArtifact) != nil {
		return errors.New("importer is invalid")
	}
	if edge.Kind == "" {
		return errors.New("kind is required")
	}
	if edge.Specifier == "" {
		return errors.New("specifier is required")
	}
	if validateArtifactPath(edge.LogicalPath, programArtifact) != nil ||
		!hasNodeModulesComponent(edge.LogicalPath) {
		return errors.New("logical path is invalid")
	}
	if validateArtifactPath(edge.ResolvedPath, programArtifact) != nil ||
		!hasNodeModulesComponent(edge.ResolvedPath) {
		return errors.New("resolved path is invalid")
	}
	if !strings.HasPrefix(edge.RuntimePath, "/opt/helmr/program/") ||
		validateArtifactPath(
			strings.TrimPrefix(edge.RuntimePath, "/opt/helmr/program/"),
			programArtifact,
		) != nil ||
		!hasNodeModulesComponent(edge.RuntimePath) {
		return errors.New("runtime path is invalid")
	}
	if strings.TrimPrefix(edge.RuntimePath, "/opt/helmr/program/") !=
		edge.LogicalPath {
		return errors.New("runtime path is invalid")
	}
	return nil
}

func validateProgramModule(module ProgramModule) error {
	if err := validateDeclarationModulePath(module.ModulePath); err != nil {
		return fmt.Errorf("modulePath: %w", err)
	}
	if module.ModulePath != generatedDeclarationModulePath(module.SourcePath) ||
		module.SourceMapPath != module.ModulePath+".map" ||
		!sha256DigestPattern.MatchString(module.ModuleDigest) ||
		!sha256DigestPattern.MatchString(module.SourceMapDigest) ||
		validateArtifactPath(module.SourcePath, programArtifact) != nil ||
		hasNodeModulesComponent(module.SourcePath) ||
		hasReservedOutputSegment(module.SourcePath) ||
		strings.HasPrefix(module.SourcePath, "helmr/") {
		return errors.New("shape is invalid")
	}
	return nil
}

func programManifestFromCompilerResult(
	result ProgramCompilerResult,
	indexDigest string,
) ProgramManifest {
	return ProgramManifest{
		FormatVersion:      ProgramManifestFormatVersion,
		Config:             result.Config,
		ExternalEdges:      slices.Clone(result.ExternalEdges),
		CompileSelection:   ProgramCompileSelection{PackageJSONDigest: result.CompileSelection.PackageJSONDigest, Packages: slices.Clone(result.CompileSelection.Packages)},
		CompiledInputs:     slices.Clone(result.Inputs),
		Modules:            slices.Clone(result.Outputs),
		ProgramIndexDigest: indexDigest,
	}
}

func verifyProgramManifestFiles(
	ctx context.Context,
	artifact *inspectedArtifact,
	manifest ProgramManifest,
) error {
	if err := verifyProgramCompileSelection(ctx, artifact, manifest.CompileSelection); err != nil {
		return err
	}
	if err := verifyProgramExternalEdges(artifact, manifest.ExternalEdges); err != nil {
		return err
	}
	if err := verifyProgramPathDigest(ctx, artifact, manifest.Config); err != nil {
		return err
	}
	inputSet := make(map[string]struct{}, len(manifest.CompiledInputs))
	for _, input := range manifest.CompiledInputs {
		if err := verifyProgramPathDigest(ctx, artifact, input); err != nil {
			return err
		}
		inputSet[input.Path] = struct{}{}
	}
	for _, module := range manifest.Modules {
		if _, err := artifact.require(module.SourcePath, artifactEntryRegular); err != nil {
			return fmt.Errorf("program module source %q: %w", module.SourcePath, err)
		}
		if err := verifyProgramPathDigest(ctx, artifact, ProgramPathDigest{
			Digest: module.ModuleDigest,
			Path:   module.ModulePath,
		}); err != nil {
			return err
		}
		if err := verifyProgramPathDigest(ctx, artifact, ProgramPathDigest{
			Digest: module.SourceMapDigest,
			Path:   module.SourceMapPath,
		}); err != nil {
			return err
		}
		if err := verifyProgramSourceMap(
			ctx,
			artifact,
			module.SourceMapPath,
			manifest.CompileSelection,
			inputSet,
		); err != nil {
			return err
		}
	}
	return nil
}

func verifyProgramExternalEdges(
	artifact *inspectedArtifact,
	externalEdges []ProgramExternalEdge,
) error {
	for _, edge := range externalEdges {
		if _, err := artifact.require(edge.Importer, artifactEntryRegular); err != nil {
			return fmt.Errorf("program external edge %q importer: %w", edge.Specifier, err)
		}
		if _, err := artifact.require(edge.ResolvedPath, artifactEntryRegular); err != nil {
			return fmt.Errorf("program external edge %q resolved path: %w", edge.Specifier, err)
		}
		resolved, resolvedPath, err := resolveProgramArtifactPath(
			artifact,
			edge.LogicalPath,
		)
		if err != nil {
			return fmt.Errorf("program external edge %q logical path: %w", edge.Specifier, err)
		}
		if resolved.Kind != artifactEntryRegular || resolvedPath != edge.ResolvedPath {
			return fmt.Errorf(
				"program external edge %q logical path does not resolve to declared file",
				edge.Specifier,
			)
		}
	}
	return nil
}

func resolveProgramArtifactPath(
	artifact *inspectedArtifact,
	value string,
) (artifactEntry, string, error) {
	if err := validateArtifactPath(value, programArtifact); err != nil || value == "." {
		return artifactEntry{}, "", fmt.Errorf("program path %q is invalid", value)
	}
	pending := strings.Split(value, "/")
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
				return artifactEntry{}, "", fmt.Errorf("program path %q escapes the artifact", value)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		entry, exists := artifact.entries[candidate]
		if !exists {
			return artifactEntry{}, "", fmt.Errorf("program path %q is missing", candidate)
		}
		if entry.Kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return artifactEntry{}, "", fmt.Errorf(
					"program path %q exceeds %d symbolic-link hops",
					value,
					maxSymlinkHops,
				)
			}
			state := candidate + "\x00" + strings.Join(pending, "\x00")
			if _, exists := visited[state]; exists {
				return artifactEntry{}, "", fmt.Errorf("program path %q contains a link cycle", value)
			}
			visited[state] = struct{}{}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		resolved = candidateParts
		if len(pending) != 0 && entry.Kind != artifactEntryDirectory {
			return artifactEntry{}, "", fmt.Errorf(
				"program path %q traverses non-directory %q",
				value,
				candidate,
			)
		}
		if len(pending) == 0 {
			return entry, candidate, nil
		}
	}
	return artifactEntry{}, "", fmt.Errorf("program path %q is empty", value)
}

func validateProgramManifestLocators(
	modules []ProgramModule,
	locator DeclarationLocator,
) error {
	moduleSet := make(map[string]struct{}, len(modules))
	for _, module := range modules {
		if _, exists := moduleSet[module.ModulePath]; exists {
			return fmt.Errorf("program module %q is duplicated", module.ModulePath)
		}
		moduleSet[module.ModulePath] = struct{}{}
	}
	located := make(map[string]struct{}, len(locator.Declarations))
	for _, declaration := range locator.Declarations {
		located[declaration.ModulePath] = struct{}{}
	}
	if len(located) != len(moduleSet) {
		return errors.New("program module set does not match declaration locators")
	}
	for module := range located {
		if _, exists := moduleSet[module]; !exists {
			return errors.New("program module set does not match declaration locators")
		}
	}
	return nil
}

func programIndexDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return sha256sum.FormatDigest(digest[:])
}

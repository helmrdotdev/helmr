package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type ProgramBuildManifest struct {
	AggregateResultDigest string                     `json:"aggregateResultDigest"`
	Compiler              ProgramCompilerContract    `json:"compiler"`
	Config                ProgramBuildFile           `json:"config"`
	ConfigSource          ProgramBuildFile           `json:"configSource"`
	DiscoveryCandidates   []string                   `json:"discoveryCandidates"`
	Execution             ProgramBuildExecution      `json:"execution"`
	ExternalEdges         []ProgramBuildExternalEdge `json:"externalEdges"`
	Inputs                []ProgramBuildFile         `json:"inputs"`
	LocalPackages         []ProgramBuildLocalPackage `json:"localPackages"`
	Lockfile              ProgramBuildFile           `json:"lockfile"`
	Outputs               []ProgramBuildOutput       `json:"outputs"`
	ProgramIndexDigest    string                     `json:"programIndexDigest"`
	Selections            []ProgramBuildSelection    `json:"selections"`
	TSConfigs             []ProgramBuildFile         `json:"tsconfigs"`
}

type ProgramCompilerResult struct {
	AggregateResultDigest string                     `json:"aggregateResultDigest"`
	Compiler              ProgramCompilerContract    `json:"compiler"`
	Config                ProgramBuildFile           `json:"config"`
	DiscoveryCandidates   []string                   `json:"discoveryCandidates"`
	Execution             ProgramBuildExecution      `json:"execution"`
	ExternalEdges         []ProgramBuildExternalEdge `json:"externalEdges"`
	Inputs                []ProgramBuildFile         `json:"inputs"`
	LocalPackages         []ProgramBuildLocalPackage `json:"localPackages"`
	Outputs               []ProgramBuildOutput       `json:"outputs"`
	Selections            []ProgramBuildSelection    `json:"selections"`
	TSConfigs             []ProgramBuildFile         `json:"tsconfigs"`
}

type ProgramCompilerContract struct {
	APIVersion            string                 `json:"apiVersion"`
	EsbuildVersion        string                 `json:"esbuildVersion"`
	OptionsContractDigest string                 `json:"optionsContractDigest"`
	Output                CompilerOutputContract `json:"output"`
	Source                CompilerSourceContract `json:"source"`
}

type ProgramBuildExecution struct {
	NodeVersion   string `json:"nodeVersion"`
	OptionsDigest string `json:"optionsDigest"`
}

type ProgramBuildFile struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
}

type ProgramBuildOutput struct {
	ModuleDigest    string `json:"moduleDigest"`
	ModulePath      string `json:"modulePath"`
	SourceMapDigest string `json:"sourceMapDigest"`
	SourceMapPath   string `json:"sourceMapPath"`
	SourcePath      string `json:"sourcePath"`
}

type ProgramBuildLocalPackage struct {
	InstalledRoot string `json:"installedRoot"`
	Name          string `json:"name"`
	SourceRoot    string `json:"sourceRoot"`
}

type ProgramBuildExternalEdge struct {
	Importer     string `json:"importer"`
	Kind         string `json:"kind"`
	LogicalPath  string `json:"logicalPath"`
	ResolvedPath string `json:"resolvedPath"`
	RuntimePath  string `json:"runtimePath"`
	Specifier    string `json:"specifier"`
}

type ProgramBuildSelection struct {
	DeclaredID string          `json:"declaredId"`
	ExportName string          `json:"exportName"`
	Kind       DeclarationKind `json:"kind"`
	Slot       DeclarationSlot `json:"slot"`
	SourcePath string          `json:"sourcePath"`
}

func ParseProgramBuildManifest(raw []byte) (ProgramBuildManifest, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ProgramBuildManifest{}, fmt.Errorf(
			"Program build manifest size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramBuildManifest{}, fmt.Errorf(
			"canonicalize Program build manifest: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramBuildManifest{}, errors.New(
			"Program build manifest is not RFC 8785 canonical JSON",
		)
	}
	var manifest ProgramBuildManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ProgramBuildManifest{}, fmt.Errorf(
			"decode Program build manifest: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "Program build manifest"); err != nil {
		return ProgramBuildManifest{}, err
	}
	if err := validateProgramBuildManifest(manifest); err != nil {
		return ProgramBuildManifest{}, err
	}
	complete, err := canonicalProgramBuildManifest(manifest)
	if err != nil {
		return ProgramBuildManifest{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramBuildManifest{}, errors.New(
			"Program build manifest does not match the complete canonical v0 shape",
		)
	}
	return manifest, nil
}

func ParseProgramCompilerResult(raw []byte) (ProgramCompilerResult, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ProgramCompilerResult{}, fmt.Errorf(
			"Program compiler result size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramCompilerResult{}, fmt.Errorf(
			"canonicalize Program compiler result: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramCompilerResult{}, errors.New(
			"Program compiler result is not RFC 8785 canonical JSON",
		)
	}
	var result ProgramCompilerResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ProgramCompilerResult{}, fmt.Errorf(
			"decode Program compiler result: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "Program compiler result"); err != nil {
		return ProgramCompilerResult{}, err
	}
	if err := validateProgramCompilerResult(result); err != nil {
		return ProgramCompilerResult{}, err
	}
	complete, err := canonicalProgramCompilerResult(result)
	if err != nil {
		return ProgramCompilerResult{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramCompilerResult{}, errors.New(
			"Program compiler result does not match the complete canonical v0 shape",
		)
	}
	return result, nil
}

func canonicalProgramCompilerResult(
	result ProgramCompilerResult,
) ([]byte, error) {
	if err := validateProgramCompilerResult(result); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return jsoncanon.Transform(raw)
}

func canonicalProgramBuildManifest(
	manifest ProgramBuildManifest,
) ([]byte, error) {
	if err := validateProgramBuildManifest(manifest); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return jsoncanon.Transform(raw)
}

func validateProgramBuildManifest(manifest ProgramBuildManifest) error {
	if !sha256DigestPattern.MatchString(manifest.ProgramIndexDigest) {
		return errors.New("Program build manifest index digest is invalid")
	}
	if manifest.ConfigSource.Path != "helmr.config.ts" ||
		!sha256DigestPattern.MatchString(manifest.ConfigSource.Digest) {
		return errors.New("Program build manifest config source authority is invalid")
	}
	if !validProgramLockfileName(manifest.Lockfile.Path) ||
		!sha256DigestPattern.MatchString(manifest.Lockfile.Digest) {
		return errors.New("Program build manifest lockfile authority is invalid")
	}
	return validateProgramCompilerResult(compilerResultFromManifest(manifest))
}

func validateProgramCompilerResult(manifest ProgramCompilerResult) error {
	if manifest.Compiler.APIVersion != "helmr.compiler.v0" ||
		manifest.Compiler.EsbuildVersion == "" ||
		!sha256DigestPattern.MatchString(manifest.Compiler.OptionsContractDigest) {
		return errors.New("Program build manifest compiler contract is invalid")
	}
	if manifest.Compiler.Output.Aggregate != "analysis-only" ||
		manifest.Compiler.Output.FinalModules != "independent" ||
		manifest.Compiler.Output.SharedChunks ||
		manifest.Compiler.Output.SourceMaps != "external" ||
		manifest.Compiler.Source.PackageDependencies != "external" ||
		manifest.Compiler.Source.Semantics != "pinned-esbuild" ||
		manifest.Compiler.Source.WorkspaceDependencies != "bundled" ||
		!slices.Equal(
			manifest.Compiler.Source.DeclarationExtensions,
			[]string{".cjs", ".cts", ".js", ".jsx", ".mjs", ".mts", ".ts", ".tsx"},
		) {
		return errors.New("Program build manifest compiler contract is unsupported")
	}
	if _, _, _, ok := parseReleaseVersion(manifest.Execution.NodeVersion); !ok ||
		!sha256DigestPattern.MatchString(manifest.Execution.OptionsDigest) {
		return errors.New("Program build manifest execution authority is invalid")
	}
	if manifest.Config.Path != "helmr/config.json" ||
		!sha256DigestPattern.MatchString(manifest.Config.Digest) {
		return errors.New("Program build manifest config authority is invalid")
	}
	if !sha256DigestPattern.MatchString(manifest.AggregateResultDigest) {
		return errors.New("Program build manifest aggregate result digest is invalid")
	}
	if manifest.DiscoveryCandidates == nil || manifest.ExternalEdges == nil ||
		manifest.Inputs == nil || manifest.LocalPackages == nil ||
		manifest.Outputs == nil || manifest.Selections == nil ||
		manifest.TSConfigs == nil {
		return errors.New("Program build manifest collections must be arrays")
	}
	for index, candidate := range manifest.DiscoveryCandidates {
		if validateArtifactPath(candidate, programArtifact) != nil ||
			hasNodeModulesComponent(candidate) ||
			hasReservedOutputSegment(candidate) ||
			strings.HasPrefix(candidate, "helmr/") ||
			(index > 0 && manifest.DiscoveryCandidates[index-1] >= candidate) {
			return fmt.Errorf("Program build manifest discovery candidate %d is invalid", index)
		}
	}
	for index, edge := range manifest.ExternalEdges {
		if edge.Importer == "" || edge.Kind == "" || edge.Specifier == "" ||
			validateArtifactPath(edge.Importer, programArtifact) != nil ||
			validateArtifactPath(edge.LogicalPath, programArtifact) != nil ||
			validateArtifactPath(edge.ResolvedPath, programArtifact) != nil ||
			!strings.HasPrefix(edge.RuntimePath, "/opt/helmr/program/") ||
			validateArtifactPath(
				strings.TrimPrefix(edge.RuntimePath, "/opt/helmr/program/"),
				programArtifact,
			) != nil ||
			!hasNodeModulesComponent(edge.LogicalPath) ||
			!hasNodeModulesComponent(edge.ResolvedPath) ||
			!hasNodeModulesComponent(edge.RuntimePath) {
			return fmt.Errorf("Program build manifest external edge %d is invalid", index)
		}
		if strings.TrimPrefix(edge.RuntimePath, "/opt/helmr/program/") !=
			edge.LogicalPath {
			return fmt.Errorf(
				"Program build manifest external edge %d runtime path is invalid",
				index,
			)
		}
		if index > 0 && compareProgramBuildExternalEdge(
			manifest.ExternalEdges[index-1],
			edge,
		) >= 0 {
			return errors.New("Program build manifest external edges are not in canonical order")
		}
	}
	for index, localPackage := range manifest.LocalPackages {
		if localPackage.Name == "" ||
			validateArtifactPath(localPackage.SourceRoot, programArtifact) != nil ||
			hasNodeModulesComponent(localPackage.SourceRoot) ||
			hasReservedOutputSegment(localPackage.SourceRoot) ||
			strings.HasPrefix(localPackage.SourceRoot, "helmr/") ||
			validateArtifactPath(localPackage.InstalledRoot, programArtifact) != nil ||
			(!hasNodeModulesComponent(localPackage.InstalledRoot) &&
				localPackage.InstalledRoot != localPackage.SourceRoot) {
			return fmt.Errorf("Program build manifest local package %d is invalid", index)
		}
		if index > 0 &&
			manifest.LocalPackages[index-1].InstalledRoot >= localPackage.InstalledRoot {
			return errors.New("Program build manifest local packages are not in canonical order")
		}
	}
	for index, input := range manifest.Inputs {
		if err := validateProgramBuildFile(input); err != nil {
			return fmt.Errorf("Program build manifest input %d: %w", index, err)
		}
		if hasNodeModulesComponent(input.Path) &&
			!localPackageContains(manifest.LocalPackages, input.Path) {
			return fmt.Errorf(
				"Program build manifest input %d is not in a local package",
				index,
			)
		}
		if index > 0 && manifest.Inputs[index-1].Path >= input.Path {
			return errors.New("Program build manifest inputs are not in canonical order")
		}
	}
	for index, config := range manifest.TSConfigs {
		if err := validateProgramBuildFile(config); err != nil {
			return fmt.Errorf("Program build manifest tsconfig %d: %w", index, err)
		}
		if index > 0 && manifest.TSConfigs[index-1].Path >= config.Path {
			return errors.New("Program build manifest tsconfigs are not in canonical order")
		}
	}
	if len(manifest.Outputs) == 0 {
		return errors.New("Program build manifest outputs must not be empty")
	}
	for index, output := range manifest.Outputs {
		if err := validateDeclarationModulePath(output.ModulePath); err != nil {
			return fmt.Errorf("Program build manifest output %d modulePath: %w", index, err)
		}
		if output.SourceMapPath != output.ModulePath+".map" ||
			!sha256DigestPattern.MatchString(output.ModuleDigest) ||
			!sha256DigestPattern.MatchString(output.SourceMapDigest) ||
			validateArtifactPath(output.SourcePath, programArtifact) != nil {
			return fmt.Errorf("Program build manifest output %d is invalid", index)
		}
		if !slices.Contains(manifest.DiscoveryCandidates, output.SourcePath) {
			return fmt.Errorf(
				"Program build manifest output %d is not a discovery candidate",
				index,
			)
		}
		if index > 0 && manifest.Outputs[index-1].ModulePath >= output.ModulePath {
			return errors.New("Program build manifest outputs are not in canonical order")
		}
	}
	for index, selection := range manifest.Selections {
		if selection.DeclaredID == "" || selection.ExportName == "" ||
			(selection.Kind != DeclarationKindTask &&
				selection.Kind != DeclarationKindActor) ||
			selection.Slot == "" ||
			validateArtifactPath(selection.SourcePath, programArtifact) != nil {
			return fmt.Errorf("Program build manifest selection %d is invalid", index)
		}
		if !slices.Contains(manifest.DiscoveryCandidates, selection.SourcePath) {
			return fmt.Errorf(
				"Program build manifest selection %d is not a discovery candidate",
				index,
			)
		}
		if index > 0 && compareProgramBuildSelection(
			manifest.Selections[index-1],
			selection,
		) >= 0 {
			return errors.New("Program build manifest selections are not in canonical order")
		}
	}
	return nil
}

func compareProgramBuildExternalEdge(left, right ProgramBuildExternalEdge) int {
	return strings.Compare(
		left.Importer+"\x00"+left.Specifier+"\x00"+left.Kind+"\x00"+
			left.LogicalPath+"\x00"+left.ResolvedPath+"\x00"+left.RuntimePath,
		right.Importer+"\x00"+right.Specifier+"\x00"+right.Kind+"\x00"+
			right.LogicalPath+"\x00"+right.ResolvedPath+"\x00"+right.RuntimePath,
	)
}

func compareProgramBuildSelection(left, right ProgramBuildSelection) int {
	return strings.Compare(
		string(left.Kind)+"\x00"+left.DeclaredID+"\x00"+left.SourcePath+"\x00"+
			left.ExportName+"\x00"+string(left.Slot),
		string(right.Kind)+"\x00"+right.DeclaredID+"\x00"+right.SourcePath+"\x00"+
			right.ExportName+"\x00"+string(right.Slot),
	)
}

func compilerResultFromManifest(
	manifest ProgramBuildManifest,
) ProgramCompilerResult {
	return ProgramCompilerResult{
		AggregateResultDigest: manifest.AggregateResultDigest,
		Compiler:              manifest.Compiler,
		Config:                manifest.Config,
		DiscoveryCandidates:   manifest.DiscoveryCandidates,
		Execution:             manifest.Execution,
		ExternalEdges:         manifest.ExternalEdges,
		Inputs:                manifest.Inputs,
		LocalPackages:         manifest.LocalPackages,
		Outputs:               manifest.Outputs,
		Selections:            manifest.Selections,
		TSConfigs:             manifest.TSConfigs,
	}
}

func buildManifestFromCompilerResult(
	result ProgramCompilerResult,
	indexDigest string,
	configSource ProgramBuildFile,
	lockfile ProgramBuildFile,
) ProgramBuildManifest {
	return ProgramBuildManifest{
		AggregateResultDigest: result.AggregateResultDigest,
		Compiler:              result.Compiler,
		Config:                result.Config,
		ConfigSource:          configSource,
		DiscoveryCandidates:   result.DiscoveryCandidates,
		Execution:             result.Execution,
		ExternalEdges:         result.ExternalEdges,
		Inputs:                result.Inputs,
		LocalPackages:         result.LocalPackages,
		Lockfile:              lockfile,
		Outputs:               result.Outputs,
		ProgramIndexDigest:    indexDigest,
		Selections:            result.Selections,
		TSConfigs:             result.TSConfigs,
	}
}

func validateProgramBuildFile(file ProgramBuildFile) error {
	if !sha256DigestPattern.MatchString(file.Digest) {
		return errors.New("digest is not a lowercase SHA-256 digest")
	}
	if err := validateArtifactPath(file.Path, programArtifact); err != nil {
		return err
	}
	if file.Path == "helmr" || strings.HasPrefix(file.Path, "helmr/") {
		return errors.New("path is not a compiler input")
	}
	if hasReservedOutputSegment(file.Path) {
		return errors.New("path is reserved Platform output")
	}
	return nil
}

func validateProgramBuildAuthority(
	manifest ProgramCompilerResult,
	compiler CompilerInputs,
	nodeVersion string,
) error {
	if manifest.Compiler.APIVersion != compiler.APIVersion ||
		manifest.Compiler.EsbuildVersion != compiler.Esbuild.Version ||
		manifest.Compiler.OptionsContractDigest != compiler.OptionsContractDigest ||
		manifest.Compiler.Output != compiler.Output ||
		!slices.Equal(
			manifest.Compiler.Source.DeclarationExtensions,
			compiler.Source.DeclarationExtensions,
		) ||
		manifest.Compiler.Source.PackageDependencies != compiler.Source.PackageDependencies ||
		manifest.Compiler.Source.Semantics != compiler.Source.Semantics ||
		manifest.Compiler.Source.WorkspaceDependencies != compiler.Source.WorkspaceDependencies {
		return errors.New("Program build manifest compiler does not match Toolchain authority")
	}
	if manifest.Execution.NodeVersion != nodeVersion {
		return errors.New("Program build manifest Node version does not match Runtime authority")
	}
	expected, err := compilerOptionsDigest(compiler, nodeVersion)
	if err != nil {
		return err
	}
	if manifest.Execution.OptionsDigest != expected {
		return errors.New("Program build manifest options digest does not match compiler authority")
	}
	return nil
}

func compilerOptionsDigest(
	compiler CompilerInputs,
	nodeVersion string,
) (string, error) {
	input := struct {
		APIVersion            string   `json:"apiVersion"`
		Banner                string   `json:"banner"`
		Bundle                bool     `json:"bundle"`
		DeclarationExtensions []string `json:"declarationExtensions"`
		EsbuildVersion        string   `json:"esbuildVersion"`
		Format                string   `json:"format"`
		LegalComments         string   `json:"legalComments"`
		Metafile              bool     `json:"metafile"`
		Packages              string   `json:"packages"`
		Platform              string   `json:"platform"`
		PreserveSymlinks      bool     `json:"preserveSymlinks"`
		SourceMap             string   `json:"sourceMap"`
		SourceMapSources      string   `json:"sourceMapSources"`
		SourcesContent        bool     `json:"sourcesContent"`
		SourceSemantics       string   `json:"sourceSemantics"`
		Splitting             bool     `json:"splitting"`
		Target                string   `json:"target"`
		TreeShaking           bool     `json:"treeShaking"`
		Write                 bool     `json:"write"`
	}{
		APIVersion:            compiler.APIVersion,
		Banner:                `import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);`,
		Bundle:                true,
		DeclarationExtensions: compiler.Source.DeclarationExtensions,
		EsbuildVersion:        compiler.Esbuild.Version,
		Format:                "esm",
		LegalComments:         "none",
		Metafile:              true,
		Packages:              "bundle",
		Platform:              "node",
		PreserveSymlinks:      false,
		SourceMap:             "external",
		SourceMapSources:      "absolute-program-urls",
		SourcesContent:        false,
		SourceSemantics:       "pinned-esbuild",
		Splitting:             false,
		Target:                "node" + nodeVersion,
		TreeShaking:           true,
		Write:                 false,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	raw, err := jsoncanon.Transform(encoded)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func verifyProgramBuildFiles(
	ctx context.Context,
	artifact *inspectedArtifact,
	manifest ProgramCompilerResult,
) error {
	for _, localPackage := range manifest.LocalPackages {
		for _, root := range []string{
			localPackage.SourceRoot,
			localPackage.InstalledRoot,
		} {
			if _, err := artifact.require(root, artifactEntryDirectory); err != nil {
				return fmt.Errorf("Program local package %q: %w", localPackage.Name, err)
			}
			raw, err := artifact.read(
				ctx,
				path.Join(root, "package.json"),
				maxProgramFileSizeBytes,
			)
			if err != nil {
				return fmt.Errorf("Program local package %q: %w", localPackage.Name, err)
			}
			var packageDocument struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &packageDocument); err != nil ||
				packageDocument.Name != localPackage.Name {
				return fmt.Errorf(
					"Program local package root %q does not identify %q",
					root,
					localPackage.Name,
				)
			}
		}
	}
	for _, edge := range manifest.ExternalEdges {
		if _, err := artifact.require(
			edge.ResolvedPath,
			artifactEntryRegular,
		); err != nil {
			return fmt.Errorf(
				"Program external edge %q: %w",
				edge.Specifier,
				err,
			)
		}
	}
	files := append(
		append([]ProgramBuildFile(nil), manifest.Inputs...),
		manifest.TSConfigs...,
	)
	files = append(files, manifest.Config)
	for _, file := range files {
		if err := verifyProgramBuildFile(ctx, artifact, file); err != nil {
			return err
		}
	}
	for _, output := range manifest.Outputs {
		if err := verifyProgramBuildFile(ctx, artifact, ProgramBuildFile{
			Digest: output.ModuleDigest,
			Path:   output.ModulePath,
		}); err != nil {
			return err
		}
		if err := verifyProgramBuildFile(ctx, artifact, ProgramBuildFile{
			Digest: output.SourceMapDigest,
			Path:   output.SourceMapPath,
		}); err != nil {
			return err
		}
		if err := verifyProgramSourceMap(
			ctx,
			artifact,
			output.SourceMapPath,
			manifest.Inputs,
			manifest.LocalPackages,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateProgramBuildLocators(
	manifest ProgramCompilerResult,
	locator DeclarationLocator,
) error {
	outputs := make(map[string]struct{}, len(manifest.Outputs))
	for _, output := range manifest.Outputs {
		expected := generatedDeclarationModulePath(output.SourcePath)
		if output.ModulePath != expected {
			return fmt.Errorf(
				"Program build output %q does not match source path digest",
				output.ModulePath,
			)
		}
		if _, exists := outputs[output.ModulePath]; exists {
			return fmt.Errorf("Program build output %q is duplicated", output.ModulePath)
		}
		outputs[output.ModulePath] = struct{}{}
	}
	located := make(map[string]struct{}, len(locator.Declarations))
	for _, declaration := range locator.Declarations {
		located[declaration.ModulePath] = struct{}{}
	}
	if len(located) != len(outputs) {
		return errors.New("Program build output set does not match declaration locators")
	}
	for module := range located {
		if _, exists := outputs[module]; !exists {
			return errors.New("Program build output set does not match declaration locators")
		}
	}
	if len(manifest.Selections) != len(locator.Declarations) {
		return errors.New("Program compiler selections do not match declaration locators")
	}
	for index, selection := range manifest.Selections {
		expectedModule := generatedDeclarationModulePath(selection.SourcePath)
		declaration := locator.Declarations[index]
		if selection.DeclaredID != declaration.DeclaredID ||
			selection.ExportName != declaration.ExportName ||
			selection.Kind != declaration.Kind ||
			selection.Slot != declaration.Slot ||
			expectedModule != declaration.ModulePath {
			return errors.New(
				"Program compiler selections do not match declaration locators",
			)
		}
	}
	return nil
}

func generatedDeclarationModulePath(source string) string {
	directory := path.Dir(source)
	prefix := ""
	if directory != "." {
		prefix = directory + "/"
	}
	return prefix + ".helmr/modules/" +
		fmt.Sprintf("%x", sha256.Sum256([]byte(source))) +
		".mjs"
}

func validateProgramAggregateResult(
	result ProgramCompilerResult,
	plan BuildPlan,
) error {
	declarations := make([]LocatedDeclaration, len(result.Selections))
	for index, selection := range result.Selections {
		declarations[index] = LocatedDeclaration{
			DeclaredID: selection.DeclaredID,
			ExportName: selection.ExportName,
			Kind:       selection.Kind,
			ModulePath: selection.SourcePath,
			Slot:       selection.Slot,
		}
	}
	raw, err := json.Marshal(struct {
		Declarations []LocatedDeclaration `json:"declarations"`
		Plan         BuildPlan            `json:"plan"`
	}{
		Declarations: declarations,
		Plan:         plan,
	})
	if err != nil {
		return err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if result.AggregateResultDigest !=
		"sha256:"+hex.EncodeToString(digest[:]) {
		return errors.New(
			"Program compiler result aggregate digest does not match analysis",
		)
	}
	return nil
}

func verifyProgramSourceMap(
	ctx context.Context,
	artifact *inspectedArtifact,
	sourceMapPath string,
	inputs []ProgramBuildFile,
	localPackages []ProgramBuildLocalPackage,
) error {
	raw, err := artifact.read(ctx, sourceMapPath, maxProgramFileSizeBytes)
	if err != nil {
		return err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		return fmt.Errorf("Program source map %q is not canonical JSON", sourceMapPath)
	}
	var sourceMap struct {
		Mappings string   `json:"mappings"`
		Names    []string `json:"names"`
		Sources  []string `json:"sources"`
		Version  int      `json:"version"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sourceMap); err != nil {
		return fmt.Errorf("decode Program source map %q: %w", sourceMapPath, err)
	}
	if err := ensureEOF(decoder, "Program source map"); err != nil {
		return err
	}
	if sourceMap.Version != 3 || sourceMap.Names == nil ||
		sourceMap.Sources == nil || len(sourceMap.Sources) == 0 {
		return fmt.Errorf("Program source map %q has an invalid v3 shape", sourceMapPath)
	}
	inputSet := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		inputSet[input.Path] = struct{}{}
	}
	const prefix = "/opt/helmr/program/"
	for _, rawURL := range sourceMap.Sources {
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Scheme != "file" || parsed.Host != "" ||
			parsed.RawQuery != "" || parsed.Fragment != "" ||
			!strings.HasPrefix(parsed.Path, prefix) {
			return fmt.Errorf("Program source map %q contains an invalid source URL", sourceMapPath)
		}
		source := strings.TrimPrefix(parsed.Path, prefix)
		if err := validateArtifactPath(source, programArtifact); err != nil ||
			(hasNodeModulesComponent(source) &&
				!localPackageContains(localPackages, source)) ||
			hasReservedOutputSegment(source) ||
			strings.HasPrefix(source, "helmr/") {
			return fmt.Errorf("Program source map %q contains an invalid source path", sourceMapPath)
		}
		expected := (&url.URL{
			Scheme: "file",
			Path:   path.Join(prefix, source),
		}).String()
		if expected != rawURL {
			return fmt.Errorf("Program source map %q source URL is not canonical", sourceMapPath)
		}
		if _, exists := inputSet[source]; !exists {
			return fmt.Errorf(
				"Program source map %q source %q is not a compiler input",
				sourceMapPath,
				source,
			)
		}
		if _, err := artifact.require(source, artifactEntryRegular); err != nil {
			return fmt.Errorf("Program source map %q source: %w", sourceMapPath, err)
		}
	}
	return nil
}

func localPackageContains(
	localPackages []ProgramBuildLocalPackage,
	value string,
) bool {
	for _, localPackage := range localPackages {
		if value == localPackage.InstalledRoot ||
			strings.HasPrefix(value, localPackage.InstalledRoot+"/") {
			return true
		}
	}
	return false
}

func verifyProgramBuildFile(
	ctx context.Context,
	artifact *inspectedArtifact,
	file ProgramBuildFile,
) error {
	raw, err := artifact.read(ctx, file.Path, maxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("Program build file %q: %w", file.Path, err)
	}
	digest := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(digest[:]) != file.Digest {
		return fmt.Errorf("Program build file %q digest does not match manifest", file.Path)
	}
	return nil
}

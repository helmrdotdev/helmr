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

type ProgramCompilerResult struct {
	AggregateResultDigest string                     `json:"aggregateResultDigest"`
	Compiler              ProgramCompilerContract    `json:"compiler"`
	Config                ProgramPathDigest          `json:"config"`
	DiscoveryCandidates   []string                   `json:"discoveryCandidates"`
	Execution             ProgramCompilerExecution   `json:"execution"`
	ExternalEdges         []ProgramExternalEdge      `json:"externalEdges"`
	Inputs                []ProgramPathDigest        `json:"inputs"`
	LocalPackages         []ProgramLocalPackage      `json:"localPackages"`
	Outputs               []ProgramModule            `json:"outputs"`
	Selections            []ProgramCompilerSelection `json:"selections"`
	TSConfigs             []ProgramPathDigest        `json:"tsconfigs"`
}

type ProgramCompilerContract struct {
	APIVersion            string                 `json:"apiVersion"`
	EsbuildVersion        string                 `json:"esbuildVersion"`
	OptionsContractDigest string                 `json:"optionsContractDigest"`
	Output                CompilerOutputContract `json:"output"`
	Source                CompilerSourceContract `json:"source"`
}

type ProgramCompilerExecution struct {
	NodeVersion   string `json:"nodeVersion"`
	OptionsDigest string `json:"optionsDigest"`
}

type ProgramCompilerSelection struct {
	DeclaredID string          `json:"declaredId"`
	ExportName string          `json:"exportName"`
	Kind       DeclarationKind `json:"kind"`
	Slot       DeclarationSlot `json:"slot"`
	SourcePath string          `json:"sourcePath"`
}

func ParseProgramCompilerResult(raw []byte) (ProgramCompilerResult, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ProgramCompilerResult{}, fmt.Errorf(
			"program compiler result size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramCompilerResult{}, fmt.Errorf(
			"canonicalize program compiler result: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramCompilerResult{}, errors.New(
			"program compiler result is not RFC 8785 canonical JSON",
		)
	}
	var result ProgramCompilerResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ProgramCompilerResult{}, fmt.Errorf(
			"decode program compiler result: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "program compiler result"); err != nil {
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
			"program compiler result does not match the complete canonical v0 shape",
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

func validateProgramCompilerResult(manifest ProgramCompilerResult) error {
	if manifest.Compiler.APIVersion != "helmr.compiler.v0" ||
		manifest.Compiler.EsbuildVersion == "" ||
		!sha256DigestPattern.MatchString(manifest.Compiler.OptionsContractDigest) {
		return errors.New("program compiler result compiler contract is invalid")
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
		return errors.New("program compiler result compiler contract is unsupported")
	}
	if _, _, _, ok := parseReleaseVersion(manifest.Execution.NodeVersion); !ok ||
		!sha256DigestPattern.MatchString(manifest.Execution.OptionsDigest) {
		return errors.New("program compiler result execution authority is invalid")
	}
	if manifest.Config.Path != "helmr/config.json" ||
		!sha256DigestPattern.MatchString(manifest.Config.Digest) {
		return errors.New("program compiler result config authority is invalid")
	}
	if !sha256DigestPattern.MatchString(manifest.AggregateResultDigest) {
		return errors.New("program compiler result aggregate result digest is invalid")
	}
	if manifest.DiscoveryCandidates == nil || manifest.ExternalEdges == nil ||
		manifest.Inputs == nil || manifest.LocalPackages == nil ||
		manifest.Outputs == nil || manifest.Selections == nil ||
		manifest.TSConfigs == nil {
		return errors.New("program compiler result collections must be arrays")
	}
	for index, candidate := range manifest.DiscoveryCandidates {
		if validateArtifactPath(candidate, programArtifact) != nil ||
			hasNodeModulesComponent(candidate) ||
			hasReservedOutputSegment(candidate) ||
			strings.HasPrefix(candidate, "helmr/") ||
			(index > 0 && manifest.DiscoveryCandidates[index-1] >= candidate) {
			return fmt.Errorf("program compiler result discovery candidate %d is invalid", index)
		}
	}
	for index, edge := range manifest.ExternalEdges {
		if err := validateProgramExternalEdge(edge); err != nil {
			return fmt.Errorf("program compiler result external edge %d: %w", index, err)
		}
		if index > 0 && compareProgramExternalEdge(
			manifest.ExternalEdges[index-1],
			edge,
		) >= 0 {
			return errors.New("program compiler result external edges are not in canonical order")
		}
	}
	for index, localPackage := range manifest.LocalPackages {
		if err := validateProgramLocalPackage(localPackage); err != nil {
			return fmt.Errorf("program compiler result local package %d: %w", index, err)
		}
		if index > 0 &&
			manifest.LocalPackages[index-1].InstalledRoot >= localPackage.InstalledRoot {
			return errors.New("program compiler result local packages are not in canonical order")
		}
	}
	for index, input := range manifest.Inputs {
		if err := validateProgramPathDigest(input); err != nil {
			return fmt.Errorf("program compiler result input %d: %w", index, err)
		}
		if hasNodeModulesComponent(input.Path) &&
			!localPackageContains(manifest.LocalPackages, input.Path) {
			return fmt.Errorf(
				"program compiler result input %d is not in a local package",
				index,
			)
		}
		if index > 0 && manifest.Inputs[index-1].Path >= input.Path {
			return errors.New("program compiler result inputs are not in canonical order")
		}
	}
	for index, config := range manifest.TSConfigs {
		if err := validateProgramPathDigest(config); err != nil {
			return fmt.Errorf("program compiler result tsconfig %d: %w", index, err)
		}
		if index > 0 && manifest.TSConfigs[index-1].Path >= config.Path {
			return errors.New("program compiler result tsconfigs are not in canonical order")
		}
	}
	if len(manifest.Outputs) == 0 {
		return errors.New("program compiler result outputs must not be empty")
	}
	for index, output := range manifest.Outputs {
		if err := validateProgramModule(output); err != nil {
			return fmt.Errorf("program compiler result output %d: %w", index, err)
		}
		if !slices.Contains(manifest.DiscoveryCandidates, output.SourcePath) {
			return fmt.Errorf(
				"program compiler result output %d is not a discovery candidate",
				index,
			)
		}
		if index > 0 && manifest.Outputs[index-1].ModulePath >= output.ModulePath {
			return errors.New("program compiler result outputs are not in canonical order")
		}
	}
	for index, selection := range manifest.Selections {
		if selection.DeclaredID == "" || selection.ExportName == "" ||
			(selection.Kind != DeclarationKindTask &&
				selection.Kind != DeclarationKindActor) ||
			selection.Slot == "" ||
			validateArtifactPath(selection.SourcePath, programArtifact) != nil {
			return fmt.Errorf("program compiler result selection %d is invalid", index)
		}
		if !slices.Contains(manifest.DiscoveryCandidates, selection.SourcePath) {
			return fmt.Errorf(
				"program compiler result selection %d is not a discovery candidate",
				index,
			)
		}
		if index > 0 && compareProgramCompilerSelection(
			manifest.Selections[index-1],
			selection,
		) >= 0 {
			return errors.New("program compiler result selections are not in canonical order")
		}
	}
	return nil
}

func compareProgramExternalEdge(left, right ProgramExternalEdge) int {
	return strings.Compare(
		left.Importer+"\x00"+left.Specifier+"\x00"+left.Kind+"\x00"+
			left.LogicalPath+"\x00"+left.ResolvedPath+"\x00"+left.RuntimePath,
		right.Importer+"\x00"+right.Specifier+"\x00"+right.Kind+"\x00"+
			right.LogicalPath+"\x00"+right.ResolvedPath+"\x00"+right.RuntimePath,
	)
}

func compareProgramCompilerSelection(left, right ProgramCompilerSelection) int {
	return strings.Compare(
		string(left.Kind)+"\x00"+left.DeclaredID+"\x00"+left.SourcePath+"\x00"+
			left.ExportName+"\x00"+string(left.Slot),
		string(right.Kind)+"\x00"+right.DeclaredID+"\x00"+right.SourcePath+"\x00"+
			right.ExportName+"\x00"+string(right.Slot),
	)
}

func validateProgramPathDigest(file ProgramPathDigest) error {
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
		return errors.New("path is reserved platform output")
	}
	return nil
}

func validateProgramCompilerAuthority(
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
		return errors.New("program compiler result compiler does not match toolchain authority")
	}
	if manifest.Execution.NodeVersion != nodeVersion {
		return errors.New("program compiler result Node.js version does not match runtime authority")
	}
	expected, err := compilerOptionsDigest(compiler, nodeVersion)
	if err != nil {
		return err
	}
	if manifest.Execution.OptionsDigest != expected {
		return errors.New("program compiler result options digest does not match compiler authority")
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

func verifyProgramCompilerFiles(
	ctx context.Context,
	artifact *inspectedArtifact,
	manifest ProgramCompilerResult,
) error {
	if err := verifyProgramLocalPackages(ctx, artifact, manifest.LocalPackages); err != nil {
		return err
	}
	if err := verifyProgramExternalEdges(artifact, manifest.ExternalEdges); err != nil {
		return err
	}
	files := append(
		append([]ProgramPathDigest(nil), manifest.Inputs...),
		manifest.TSConfigs...,
	)
	files = append(files, manifest.Config)
	for _, file := range files {
		if err := verifyProgramPathDigest(ctx, artifact, file); err != nil {
			return err
		}
	}
	inputSet := make(map[string]struct{}, len(manifest.Inputs))
	for _, input := range manifest.Inputs {
		inputSet[input.Path] = struct{}{}
	}
	for _, output := range manifest.Outputs {
		if err := verifyProgramPathDigest(ctx, artifact, ProgramPathDigest{
			Digest: output.ModuleDigest,
			Path:   output.ModulePath,
		}); err != nil {
			return err
		}
		if err := verifyProgramPathDigest(ctx, artifact, ProgramPathDigest{
			Digest: output.SourceMapDigest,
			Path:   output.SourceMapPath,
		}); err != nil {
			return err
		}
		if err := verifyProgramSourceMap(
			ctx,
			artifact,
			output.SourceMapPath,
			manifest.LocalPackages,
			inputSet,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateProgramCompilerLocators(
	manifest ProgramCompilerResult,
	locator DeclarationLocator,
) error {
	outputs := make(map[string]struct{}, len(manifest.Outputs))
	for _, output := range manifest.Outputs {
		expected := generatedDeclarationModulePath(output.SourcePath)
		if output.ModulePath != expected {
			return fmt.Errorf(
				"program compiler output %q does not match source path digest",
				output.ModulePath,
			)
		}
		if _, exists := outputs[output.ModulePath]; exists {
			return fmt.Errorf("program compiler output %q is duplicated", output.ModulePath)
		}
		outputs[output.ModulePath] = struct{}{}
	}
	located := make(map[string]struct{}, len(locator.Declarations))
	for _, declaration := range locator.Declarations {
		located[declaration.ModulePath] = struct{}{}
	}
	if len(located) != len(outputs) {
		return errors.New("program compiler output set does not match declaration locators")
	}
	for module := range located {
		if _, exists := outputs[module]; !exists {
			return errors.New("program compiler output set does not match declaration locators")
		}
	}
	if len(manifest.Selections) != len(locator.Declarations) {
		return errors.New("program compiler selections do not match declaration locators")
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
				"program compiler selections do not match declaration locators",
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
			"program compiler result aggregate digest does not match analysis",
		)
	}
	return nil
}

func verifyProgramSourceMap(
	ctx context.Context,
	artifact *inspectedArtifact,
	sourceMapPath string,
	localPackages []ProgramLocalPackage,
	allowedSources map[string]struct{},
) error {
	raw, err := artifact.read(ctx, sourceMapPath, maxProgramFileSizeBytes)
	if err != nil {
		return err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		return fmt.Errorf("program source map %q is not canonical JSON", sourceMapPath)
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
		return fmt.Errorf("decode program source map %q: %w", sourceMapPath, err)
	}
	if err := ensureEOF(decoder, "program source map"); err != nil {
		return err
	}
	if sourceMap.Version != 3 || sourceMap.Names == nil ||
		sourceMap.Sources == nil || len(sourceMap.Sources) == 0 {
		return fmt.Errorf("program source map %q has an invalid v3 shape", sourceMapPath)
	}
	const prefix = "/opt/helmr/program/"
	for _, rawURL := range sourceMap.Sources {
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Scheme != "file" || parsed.Host != "" ||
			parsed.RawQuery != "" || parsed.Fragment != "" ||
			!strings.HasPrefix(parsed.Path, prefix) {
			return fmt.Errorf("program source map %q contains an invalid source URL", sourceMapPath)
		}
		source := strings.TrimPrefix(parsed.Path, prefix)
		if err := validateArtifactPath(source, programArtifact); err != nil ||
			(hasNodeModulesComponent(source) &&
				!localPackageContains(localPackages, source)) ||
			hasReservedOutputSegment(source) ||
			strings.HasPrefix(source, "helmr/") {
			return fmt.Errorf("program source map %q contains an invalid source path", sourceMapPath)
		}
		expected := (&url.URL{
			Scheme: "file",
			Path:   path.Join(prefix, source),
		}).String()
		if expected != rawURL {
			return fmt.Errorf("program source map %q source URL is not canonical", sourceMapPath)
		}
		if allowedSources != nil {
			if _, exists := allowedSources[source]; !exists {
				return fmt.Errorf(
					"program source map %q source %q is not a compiler input",
					sourceMapPath,
					source,
				)
			}
		}
		if _, err := artifact.require(source, artifactEntryRegular); err != nil {
			return fmt.Errorf("program source map %q source: %w", sourceMapPath, err)
		}
	}
	return nil
}

func localPackageContains(
	localPackages []ProgramLocalPackage,
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

func verifyProgramPathDigest(
	ctx context.Context,
	artifact *inspectedArtifact,
	file ProgramPathDigest,
) error {
	raw, err := artifact.read(ctx, file.Path, maxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("program file %q: %w", file.Path, err)
	}
	digest := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(digest[:]) != file.Digest {
		return fmt.Errorf("program file %q digest does not match authority", file.Path)
	}
	return nil
}

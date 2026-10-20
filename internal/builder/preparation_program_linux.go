//go:build linux

package builder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

// PrepareDeploymentPrograms independently bundles each selected Template from
// captured source. Only closed outputs cross into the final artifact builder.
func PrepareDeploymentPrograms(ctx context.Context, input ProgramInput, source, installed, output string) (returnErr error) {
	if source == "" || installed == "" || !filepath.IsAbs(source) || !filepath.IsAbs(installed) {
		return errors.New("preparation requires captured source and installed runtime packages")
	}
	if output == "" || !filepath.IsAbs(output) {
		return errors.New("prepared deployment output must be absolute")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("prepared deployment output already exists or is inaccessible")
	}
	work, err := os.MkdirTemp(input.WorkDirectory, ".helmr-preparations-")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(work)) }()
	stage := filepath.Join(work, "deployment")
	if err := PrepareProgram(ctx, input, stage); err != nil {
		return err
	}
	_, verification, err := readPreparedProgram(stage)
	if err != nil {
		return err
	}
	index, err := artifact.ParseDefinitionIndex(verification.DefinitionIndex())
	if err != nil {
		return err
	}
	raw, err := readBoundedRegularFile(input.BundlePath, compilerDocumentLimit)
	if err != nil {
		return err
	}
	var modules struct {
		Entries []struct {
			SourcePath string `json:"sourcePath"`
			ModulePath string `json:"modulePath"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &modules); err != nil {
		return err
	}
	sources := make(map[string]string, len(modules.Entries))
	for _, entry := range modules.Entries {
		sources[entry.ModulePath] = entry.SourcePath
	}
	if err := os.Mkdir(filepath.Join(stage, "preparations"), 0700); err != nil {
		return err
	}
	flags, err := artifact.NodeLanguageFlags(input.RuntimeMetadata.NodeVersion)
	if err != nil {
		return err
	}
	run := func(args ...string) error {
		_, err := runAnalysisCommand(ctx, analysisCommand{NodePath: input.NodePath, Arguments: append(append([]string{}, flags...), append([]string{input.ProgramCompiler}, args...)...), Directory: work, WorkDir: work})
		return err
	}
	plan, err := definition.ParseBuildPlan(verification.BuildPlan())
	if err != nil {
		return err
	}
	selectedTemplates := make(map[string]definition.Input)
	for _, d := range plan.Definitions {
		if d.Template != nil && d.Template.Prepare {
			selectedTemplates[d.DeclaredID] = d
		}
	}
	for _, entry := range index.Templates {
		expected, preparedTemplate := selectedTemplates[entry.ID]
		if !preparedTemplate {
			continue
		}
		path, ok := sources[entry.ModulePath]
		if !ok {
			return errors.New("preparation entry has no captured source")
		}
		selection := struct {
			SourcePath   string `json:"sourcePath"`
			ExportName   string `json:"exportName"`
			ThroughAgent bool   `json:"throughAgent"`
		}{path, entry.ExportName, entry.ThroughAgent}
		raw, err := json.Marshal(selection)
		if err != nil {
			return err
		}
		selectionPath := filepath.Join(work, "selection.json")
		if err := os.WriteFile(selectionPath, raw, 0600); err != nil {
			return err
		}
		bundlePath := filepath.Join(work, "bundle-"+entry.ID)
		if err := run("--bundle-template", source, input.ConfigPath, input.RuntimeMetadata.NodeVersion, bundlePath, selectionPath); err != nil {
			return fmt.Errorf("bundle preparation %q: %w", entry.ID, err)
		}
		payload := filepath.Join(work, "payload-"+entry.ID)
		if err := run("--assemble", bundlePath, installed, payload); err != nil {
			return err
		}
		selected := input
		selected.ProjectDirectory = payload
		selected.BundlePath = filepath.Join(bundlePath, "bundle.json")
		prepared := filepath.Join(stage, "preparations", entry.ID)
		if err := PrepareProgram(ctx, selected, prepared); err != nil {
			return fmt.Errorf("analyze preparation %q: %w", entry.ID, err)
		}
		_, separate, err := readPreparedProgram(prepared)
		if err != nil {
			return err
		}
		separatePlan, err := definition.ParseBuildPlan(separate.BuildPlan())
		if err != nil {
			return err
		}
		if len(separatePlan.Definitions) != 1 || !reflect.DeepEqual(separatePlan.Definitions[0], expected) {
			return errors.New("independent preparation Template differs from Session analysis")
		}
		separateIndex, err := artifact.ParseDefinitionIndex(separate.DefinitionIndex())
		if err != nil {
			return err
		}
		if len(separateIndex.Agents) != 0 || len(separateIndex.Templates) != 1 || separateIndex.Templates[0].ID != entry.ID || separateIndex.Templates[0].ThroughAgent {
			return errors.New("preparation must export exactly its selected Template")
		}
	}
	return os.Rename(stage, output)
}

// BuildPreparationPrograms encodes the closed Template outputs with the same
// immutable Runtime and resolved seed artifacts as the Session Program.
func BuildPreparationPrograms(ctx context.Context, input PreparedProgramInput) ([]ProgramResult, error) {
	_, verification, err := readPreparedProgram(input.PreparedDirectory)
	if err != nil {
		return nil, err
	}
	index, err := artifact.ParseDefinitionIndex(verification.DefinitionIndex())
	if err != nil {
		return nil, err
	}
	plan, err := definition.ParseBuildPlan(verification.BuildPlan())
	if err != nil {
		return nil, err
	}
	preparedIDs := make(map[string]bool)
	for _, d := range plan.Definitions {
		if d.Template != nil && d.Template.Prepare {
			preparedIDs[d.DeclaredID] = true
		}
	}
	selected := index.Templates[:0]
	for _, entry := range index.Templates {
		if preparedIDs[entry.ID] {
			selected = append(selected, entry)
		}
	}
	index.Templates = selected
	directory := filepath.Join(input.PreparedDirectory, "preparations")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	if len(entries) != len(index.Templates) {
		return nil, errors.New("prepared Template outputs do not match Session Program")
	}
	results := make([]ProgramResult, 0, len(index.Templates))
	for i, selected := range index.Templates {
		if entries[i].Name() != selected.ID || !entries[i].IsDir() {
			return nil, errors.New("invalid prepared Template directory")
		}
		child := input
		child.PreparedDirectory = filepath.Join(directory, selected.ID)
		child.ProgramObjectPath = filepath.Join(input.WorkDirectory, "preparation-"+selected.ID+".squashfs")
		child.ComputerSeeds = nil
		for _, seed := range input.ComputerSeeds {
			if seed.DeclaredID == selected.ID {
				child.ComputerSeeds = append(child.ComputerSeeds, seed)
			}
		}
		result, err := BuildPreparedProgram(ctx, child)
		if err != nil {
			return nil, err
		}
		if len(result.Program.Metadata.Definitions) != 1 || result.Program.Metadata.Definitions[0].Kind != definition.KindTemplate || result.Program.Metadata.Definitions[0].DeclaredID != selected.ID {
			return nil, errors.New("prepared artifact does not select its Template")
		}
		results = append(results, result)
	}
	return results, nil
}

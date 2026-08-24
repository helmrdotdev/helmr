package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "helmr bundle builder: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("helmr-bundle-builder", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	project := flags.String("project", "", "installed project tree")
	prepareOutput := flags.String("prepare-output", "", "new closed prepared Program directory")
	prepared := flags.String("prepared", "", "closed prepared Program directory")
	programProject := flags.String("program-project", "", "private writable Program project tree")
	work := flags.String("work", "", "private working directory")
	bundleOutput := flags.String("bundle-output", "", "new deployment bundle directory")
	analysisOutput := flags.String("analysis-output", "", "new canonical build-plan file")
	workspaceImageInput := flags.String("workspace-images", "", "workspace image input document")
	expectedPlanInput := flags.String("expected-plan", "", "canonical analysis build plan")
	runtimeDescriptor := flags.String("runtime-descriptor", "", "canonical Runtime descriptor")
	runtimeMetadata := flags.String("runtime-metadata", "", "canonical Runtime metadata")
	compilerDescriptor := flags.String("compiler-descriptor", "", "canonical compiler descriptor")
	node := flags.String("node", "", "pinned Node executable")
	nodeLoader := flags.String("node-loader", "", "pinned Runtime ELF loader")
	nodeLibraryPath := flags.String("node-library-path", "", "pinned Runtime library directory")
	configEvaluator := flags.String("config-evaluator", "", "pinned Config Evaluator")
	programCompiler := flags.String("program-compiler", "", "pinned Program Compiler")
	encoder := flags.String("encoder", "", "pinned mksquashfs executable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not supported")
	}
	runtimeRaw, err := os.ReadFile(*runtimeDescriptor)
	if err != nil {
		return fmt.Errorf("read Runtime descriptor: %w", err)
	}
	runtime, err := deployment.ParseRuntimeDescriptor(runtimeRaw)
	if err != nil {
		return err
	}
	metadataRaw, err := os.ReadFile(*runtimeMetadata)
	if err != nil {
		return fmt.Errorf("read Runtime metadata: %w", err)
	}
	metadata, err := deployment.ParseRuntimeMetadata(metadataRaw)
	if err != nil {
		return err
	}
	compilerRaw, err := os.ReadFile(*compilerDescriptor)
	if err != nil {
		return fmt.Errorf("read compiler descriptor: %w", err)
	}
	compiler, err := deployment.ParseCompilerInputs(compilerRaw)
	if err != nil {
		return err
	}
	compilerInput := builder.ProgramInput{
		ProjectDirectory: cleanAbsolute(*project),
		WorkDirectory:    cleanAbsolute(*work),
		NodePath:         cleanAbsolute(*node),
		NodeLoader:       cleanAbsolute(*nodeLoader),
		NodeLibraryPath:  cleanAbsolute(*nodeLibraryPath),
		ConfigEvaluator:  cleanAbsolute(*configEvaluator),
		ProgramCompiler:  cleanAbsolute(*programCompiler),
		SquashFSEncoder:  cleanAbsolute(*encoder),
		Compiler:         compiler,
		Runtime:          runtime,
		RuntimeMetadata:  metadata,
	}
	modes := 0
	for _, selected := range []bool{*analysisOutput != "", *prepareOutput != "", *bundleOutput != ""} {
		if selected {
			modes++
		}
	}
	if modes != 1 {
		return errors.New("exactly one of --analysis-output, --prepare-output, or --bundle-output is required")
	}
	if *analysisOutput != "" {
		analysis, err := builder.AnalyzeProgram(ctx, compilerInput)
		if err != nil {
			return err
		}
		canonical, err := deployment.CanonicalBuildPlan(analysis.Plan)
		if err != nil {
			return err
		}
		return writeExclusive(cleanAbsolute(*analysisOutput), canonical)
	}
	if *prepareOutput != "" {
		_, err := builder.PrepareProgram(ctx, compilerInput, cleanAbsolute(*prepareOutput))
		return err
	}
	images, objects, err := builder.ReadWorkspaceImageInputs(ctx, cleanAbsolute(*workspaceImageInput))
	if err != nil {
		return err
	}
	result, err := builder.BuildPreparedProgram(ctx, builder.PreparedProgramInput{
		PreparedDirectory: cleanAbsolute(*prepared),
		ProgramDirectory:  cleanAbsolute(*programProject),
		WorkDirectory:     cleanAbsolute(*work),
		ProgramObjectPath: filepath.Join(cleanAbsolute(*work), "program.squashfs"),
		SquashFSEncoder:   cleanAbsolute(*encoder),
		Compiler:          compiler,
		Runtime:           runtime,
		RuntimeMetadata:   metadata,
		WorkspaceImages:   images,
	})
	if err != nil {
		return err
	}
	defer os.Remove(result.ObjectPath)
	expectedPlanRaw, err := os.ReadFile(cleanAbsolute(*expectedPlanInput))
	if err != nil {
		return fmt.Errorf("read expected build plan: %w", err)
	}
	expectedPlan, err := deployment.ParseBuildPlan(expectedPlanRaw)
	if err != nil {
		return fmt.Errorf("parse expected build plan: %w", err)
	}
	actualPlan, err := deployment.ParseBuildPlan([]byte(result.Verification.Succeeded.Files[0].Content))
	if err != nil {
		return err
	}
	expectedCanonical, err := deployment.CanonicalBuildPlan(expectedPlan)
	if err != nil {
		return err
	}
	actualCanonical, err := deployment.CanonicalBuildPlan(actualPlan)
	if err != nil {
		return err
	}
	if !bytes.Equal(expectedCanonical, actualCanonical) {
		return errors.New("final installed tree build plan does not match analyzed plan")
	}
	objects = append(objects, builder.ObjectSource{
		Digest: result.Program.Artifact.Digest,
		Path:   result.ObjectPath,
	})
	_, err = builder.FinalizeBundle(ctx, cleanAbsolute(*bundleOutput), builder.BundleInput{
		Runtime:         runtime,
		Program:         result.Program,
		WorkspaceImages: images,
		Objects:         objects,
	})
	return err
}

func writeExclusive(path string, body []byte) (returnErr error) {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("analysis output must be an absolute path")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	written, err := file.Write(body)
	if err != nil {
		return err
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func cleanAbsolute(value string) string {
	if value == "" || !filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(value)
}

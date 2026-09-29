package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/definition"
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
	work := flags.String("work", "", "private working directory")
	bundleOutput := flags.String("bundle-output", "", "new deployment bundle directory")
	bundlePath := flags.String("bundle-manifest", "", "generated module manifest")
	computerImageInput := flags.String("computer-images", "", "computer image input document")
	expectedPlanInput := flags.String("expected-plan", "", "canonical analysis build plan")
	runtimeDescriptor := flags.String("runtime-descriptor", "", "canonical Runtime descriptor")
	runtimeMetadata := flags.String("runtime-metadata", "", "canonical Runtime metadata")
	compilerDescriptor := flags.String("compiler-descriptor", "", "canonical compiler descriptor")
	node := flags.String("node", "", "pinned Node executable")
	configPath := flags.String("config", "", "discovery config resolved on the invoking host")
	programCompiler := flags.String("program-compiler", "", "pinned Program Compiler")
	mkfs := flags.String("mkfs", "", "pinned filesystem generator")
	filesystemConfig := flags.String("filesystem-config", "", "pinned filesystem configuration")
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
		ConfigPath:       cleanAbsolute(*configPath),
		BundlePath:       cleanAbsolute(*bundlePath),
		ProgramCompiler:  cleanAbsolute(*programCompiler),
		Compiler:         compiler,
		Runtime:          runtime,
		RuntimeMetadata:  metadata,
	}
	modes := 0
	for _, selected := range []bool{*prepareOutput != "", *bundleOutput != ""} {
		if selected {
			modes++
		}
	}
	if modes != 1 {
		return errors.New("exactly one of --prepare-output or --bundle-output is required")
	}
	if *prepareOutput != "" {
		return builder.PrepareProgram(ctx, compilerInput, cleanAbsolute(*prepareOutput))
	}
	imageWork, err := os.MkdirTemp(cleanAbsolute(*work), "images-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(imageWork)
	images, objects, err := builder.ReadComputerImageInputs(ctx, cleanAbsolute(*computerImageInput), imageWork, cleanAbsolute(*mkfs), cleanAbsolute(*filesystemConfig))
	if err != nil {
		return err
	}
	result, err := builder.BuildPreparedProgram(ctx, builder.PreparedProgramInput{
		PreparedDirectory: cleanAbsolute(*prepared),
		WorkDirectory:     cleanAbsolute(*work),
		ProgramObjectPath: filepath.Join(cleanAbsolute(*work), "program.squashfs"),
		SquashFSEncoder:   cleanAbsolute(*encoder),
		Compiler:          compiler,
		Runtime:           runtime,
		RuntimeMetadata:   metadata,
		ComputerImages:    images,
	})
	if err != nil {
		return err
	}
	defer os.Remove(result.ObjectPath)
	expectedPlanRaw, err := os.ReadFile(cleanAbsolute(*expectedPlanInput))
	if err != nil {
		return fmt.Errorf("read expected build plan: %w", err)
	}
	expectedPlan, err := definition.ParseBuildPlan(expectedPlanRaw)
	if err != nil {
		return fmt.Errorf("parse expected build plan: %w", err)
	}
	actualPlan, err := definition.ParseBuildPlan([]byte(result.Verification.Succeeded.Files[0].Content))
	if err != nil {
		return err
	}
	expectedCanonical, err := definition.CanonicalBuildPlan(expectedPlan)
	if err != nil {
		return err
	}
	actualCanonical, err := definition.CanonicalBuildPlan(actualPlan)
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
		Runtime:        runtime,
		Program:        result.Program,
		ComputerImages: images,
		Objects:        objects,
	})
	return err
}

func cleanAbsolute(value string) string {
	if value == "" || !filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(value)
}

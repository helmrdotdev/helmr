package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	work := flags.String("work", "", "private working directory")
	bundleOutput := flags.String("bundle-output", "", "new deployment bundle directory")
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
	programObject := filepath.Join(cleanAbsolute(*work), "program.squashfs")
	result, err := builder.BuildProgram(ctx, builder.ProgramInput{
		ProjectDirectory:  cleanAbsolute(*project),
		WorkDirectory:     cleanAbsolute(*work),
		ProgramObjectPath: programObject,
		NodePath:          cleanAbsolute(*node),
		NodeLoader:        cleanAbsolute(*nodeLoader),
		NodeLibraryPath:   cleanAbsolute(*nodeLibraryPath),
		ConfigEvaluator:   cleanAbsolute(*configEvaluator),
		ProgramCompiler:   cleanAbsolute(*programCompiler),
		SquashFSEncoder:   cleanAbsolute(*encoder),
		Compiler:          compiler,
		Runtime:           runtime,
		RuntimeMetadata:   metadata,
	})
	if err != nil {
		return err
	}
	defer os.Remove(result.ObjectPath)
	_, err = builder.FinalizeBundle(ctx, cleanAbsolute(*bundleOutput), builder.BundleInput{
		Runtime:         runtime,
		Program:         result.Program,
		WorkspaceImages: []deployment.BundleWorkspaceImage{},
		Objects: []builder.ObjectSource{{
			Digest: result.Program.Artifact.Digest,
			Path:   result.ObjectPath,
		}},
	})
	return err
}

func cleanAbsolute(value string) string {
	if value == "" || !filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(value)
}

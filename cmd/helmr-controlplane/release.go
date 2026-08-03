package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func runReleaseCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("release command is required")
	}
	switch args[0] {
	case "install":
		return installRelease(ctx, args[1:])
	case "publish":
		return publishRelease(ctx, args[1:])
	default:
		return fmt.Errorf("unknown release command %q", args[0])
	}
}

func installRelease(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("release install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var storeURI, digest, output string
	flags.StringVar(&storeURI, "store", "", "release store URI")
	flags.StringVar(&digest, "digest", "", "build policy digest")
	flags.StringVar(&output, "output", "", "installed build policy path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("release install accepts no positional arguments")
	}
	if storeURI == "" || digest == "" || output == "" {
		return errors.New("release install requires --store, --digest, and --output")
	}
	store, err := cas.NewImmutableS3(ctx, storeURI)
	if err != nil {
		return fmt.Errorf("configure release store: %w", err)
	}
	return deployment.InstallBuildPolicy(ctx, store, digest, output)
}

func publishRelease(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("release publish", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var storeURI, input string
	flags.StringVar(&storeURI, "store", "", "Platform Artifact store URI")
	flags.StringVar(&input, "input", "", "Platform release directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || storeURI == "" || input == "" {
		return errors.New("release publish requires --store and --input")
	}
	store, err := cas.NewImmutableS3(ctx, storeURI)
	if err != nil {
		return fmt.Errorf("configure Platform Artifact store: %w", err)
	}
	return deployment.PublishPlatformRelease(ctx, store, input)
}

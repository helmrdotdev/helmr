package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func runReleaseCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "publish" {
		return errors.New("release publish is the only release command")
	}
	flags := flag.NewFlagSet("release publish", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var storeURI, input string
	flags.StringVar(&storeURI, "store", "", "Platform Artifact store URI")
	flags.StringVar(&input, "input", "", "Platform release directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || storeURI == "" || input == "" {
		return errors.New("release publish requires --store and --input")
	}
	store, err := cass3.NewImmutable(ctx, storeURI)
	if err != nil {
		return fmt.Errorf("configure platform artifact store: %w", err)
	}
	return deployment.PublishPlatformRelease(ctx, store, input)
}

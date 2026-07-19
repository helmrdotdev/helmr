package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func runReleaseCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("release command is required")
	}
	switch args[0] {
	case "install":
		return deployment.RunReleaseInstall(ctx, args[1:])
	default:
		return fmt.Errorf("unknown release command %q", args[0])
	}
}

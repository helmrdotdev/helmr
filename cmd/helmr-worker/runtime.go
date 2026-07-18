package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func runRuntimeCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("runtime command is required")
	}
	switch args[0] {
	case "install":
		return deployment.RunRuntimeInstall(ctx, args[1:])
	default:
		return fmt.Errorf("unknown runtime command %q", args[0])
	}
}

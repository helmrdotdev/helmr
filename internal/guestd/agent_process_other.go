//go:build !linux

package guestd

import (
	"context"
	"errors"
)

func createAgentProcess(context.Context, *computerMountEntry, agentProcessOptions) (agentSessionProcess, error) {
	return nil, errors.New("Session execution requires Linux")
}

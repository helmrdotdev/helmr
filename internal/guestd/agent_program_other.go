//go:build !linux

package guestd

import (
	"context"
	"errors"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"sync"
)

type mountedProgramImage struct{ root string }

func materializeAgentProgram(context.Context, string, agentv1.SessionProgramRequest_Kind, *agentv1.SessionProgramArtifact, agentProgramInput, *sync.Mutex) (*mountedProgramImage, error) {
	return nil, errors.New("Session Program mounting requires Linux")
}
func (*mountedProgramImage) close() error { return nil }
